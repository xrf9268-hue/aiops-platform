package gitea

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
