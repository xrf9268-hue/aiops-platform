package gitea

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// captureBody decodes the JSON body of a CreatePullRequest call into a generic
// map so tests can assert on which keys were sent.
func captureBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	return out
}

func TestCreatePullRequest_DraftTrueSendsDraftField(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = captureBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"number": 7, "html_url": "http://gitea.local/o/r/pulls/7", "title": "t"}`))
	}))
	defer srv.Close()

	c := Client{BaseURL: srv.URL, Token: "fake"}
	pr, err := c.CreatePullRequest(context.Background(), CreatePullRequestInput{
		Owner: "o", Repo: "r", Title: "t", Body: "b", Head: "feat", Base: "main", Draft: true,
	})
	if err != nil {
		t.Fatalf("CreatePullRequest: %v", err)
	}
	if pr.Number != 7 {
		t.Fatalf("pr number: got %d want 7", pr.Number)
	}
	v, ok := got["draft"]
	if !ok {
		t.Fatalf("payload missing draft field: %#v", got)
	}
	if b, _ := v.(bool); !b {
		t.Fatalf("draft field should be true, got %#v", v)
	}
}

func TestCreatePullRequest_DraftFalseOmitsDraftField(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = captureBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"number": 8, "html_url": "http://gitea.local/o/r/pulls/8", "title": "t"}`))
	}))
	defer srv.Close()

	c := Client{BaseURL: srv.URL, Token: "fake"}
	if _, err := c.CreatePullRequest(context.Background(), CreatePullRequestInput{
		Owner: "o", Repo: "r", Title: "t", Body: "b", Head: "feat", Base: "main", Draft: false,
	}); err != nil {
		t.Fatalf("CreatePullRequest: %v", err)
	}
	if _, present := got["draft"]; present {
		t.Fatalf("draft key should be omitted when false; got %#v", got)
	}
	// Sanity check that the existing fields are still sent.
	for _, k := range []string{"title", "body", "head", "base"} {
		if _, present := got[k]; !present {
			t.Fatalf("payload missing %q: %#v", k, got)
		}
	}
}

func TestFindOpenPullRequest_ReturnsMatchByHeadRef(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		if r.URL.Path != "/api/v1/repos/o/r/pulls" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[
			{"number": 11, "html_url": "http://gitea.local/o/r/pulls/11", "title": "other", "head": {"ref": "ai/tsk_other"}},
			{"number": 12, "html_url": "http://gitea.local/o/r/pulls/12", "title": "match", "head": {"ref": "ai/tsk_42"}}
		]`))
	}))
	defer srv.Close()

	c := Client{BaseURL: srv.URL, Token: "fake"}
	pr, err := c.FindOpenPullRequest(context.Background(), FindOpenPullRequestInput{
		Owner: "o", Repo: "r", Head: "ai/tsk_42",
	})
	if err != nil {
		t.Fatalf("FindOpenPullRequest: %v", err)
	}
	if pr == nil {
		t.Fatal("expected match, got nil")
	}
	if pr.Number != 12 {
		t.Fatalf("pr number: got %d want 12", pr.Number)
	}
	q, _ := url.ParseQuery(gotQuery)
	if got := q.Get("state"); got != "open" {
		t.Fatalf("query state: got %q want open", got)
	}
}

func TestFindOpenPullRequest_NoMatchReturnsNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[
			{"number": 11, "html_url": "http://gitea.local/o/r/pulls/11", "title": "other", "head": {"ref": "ai/tsk_other"}}
		]`))
	}))
	defer srv.Close()

	c := Client{BaseURL: srv.URL, Token: "fake"}
	pr, err := c.FindOpenPullRequest(context.Background(), FindOpenPullRequestInput{
		Owner: "o", Repo: "r", Head: "ai/tsk_42",
	})
	if err != nil {
		t.Fatalf("FindOpenPullRequest: %v", err)
	}
	if pr != nil {
		t.Fatalf("expected nil, got %#v", pr)
	}
}

func TestFindOpenPullRequest_PaginatesUntilMatch(t *testing.T) {
	var pages int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		page := r.URL.Query().Get("page")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		switch page {
		case "1":
			// First page: 50 unrelated PRs, signalling more results follow.
			out := make([]map[string]any, 0, 50)
			for i := 0; i < 50; i++ {
				out = append(out, map[string]any{
					"number":   i + 100,
					"html_url": "http://gitea.local/x",
					"title":    "x",
					"head":     map[string]any{"ref": "ai/tsk_other"},
				})
			}
			_ = json.NewEncoder(w).Encode(out)
		case "2":
			_, _ = w.Write([]byte(`[
				{"number": 200, "html_url": "http://gitea.local/o/r/pulls/200", "title": "match", "head": {"ref": "ai/tsk_42"}}
			]`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer srv.Close()

	c := Client{BaseURL: srv.URL, Token: "fake"}
	pr, err := c.FindOpenPullRequest(context.Background(), FindOpenPullRequestInput{
		Owner: "o", Repo: "r", Head: "ai/tsk_42",
	})
	if err != nil {
		t.Fatalf("FindOpenPullRequest: %v", err)
	}
	if pr == nil || pr.Number != 200 {
		t.Fatalf("expected match #200, got %#v", pr)
	}
	if pages < 2 {
		t.Fatalf("expected at least 2 pages requested, got %d", pages)
	}
}

func TestFindOpenPullRequest_NonSuccessReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := Client{BaseURL: srv.URL, Token: "fake"}
	if _, err := c.FindOpenPullRequest(context.Background(), FindOpenPullRequestInput{
		Owner: "o", Repo: "r", Head: "ai/tsk_42",
	}); err == nil {
		t.Fatal("expected error on 500, got nil")
	}
}
