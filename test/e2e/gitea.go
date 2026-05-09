//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

type giteaEnv struct {
	baseURL    string
	mappedPort string
	botUser    string
	botPass    string
	botToken   string
	container  testcontainers.Container
}

const giteaImage = "gitea/gitea:1.21.11-rootless"

// startGitea boots Gitea with admin credentials injected via env, then
// exchanges basic auth for an access token. Returns a fully usable client
// envelope. The clone-URL host mismatch (container-internal vs host-mapped)
// is handled by services.go via a rewriter on the trigger-api Store, NOT
// here.
func startGitea(ctx context.Context) (*giteaEnv, error) {
	pass := randomHex(16)
	secret := randomHex(32)

	req := testcontainers.ContainerRequest{
		Image:        giteaImage,
		ExposedPorts: []string{"3000/tcp"},
		Env: map[string]string{
			// NOTE: GITEA_ADMIN_USER env var is NOT processed by Gitea's
			// docker entrypoint in 1.21.x; we create the admin user via
			// `gitea admin user create` after the server is ready.
			"GITEA__security__INSTALL_LOCK": "true",
			"GITEA__security__SECRET_KEY":   secret,
			"GITEA__database__DB_TYPE":      "sqlite3",
			"GITEA__server__DISABLE_SSH":    "true",
			// Allow webhook deliveries to host.docker.internal (the test server
			// runs on the host; Gitea blocks private IPs by default).
			"GITEA__webhook__ALLOWED_HOST_LIST": "external,loopback,private",
		},
		ExtraHosts: []string{"host.docker.internal:host-gateway"},
		WaitingFor: wait.ForHTTP("/api/v1/version").WithPort("3000/tcp").WithStartupTimeout(90 * time.Second),
	}

	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return nil, fmt.Errorf("start gitea: %w", err)
	}

	// Create the admin user via the gitea CLI now that the server is ready.
	code, _, execErr := c.Exec(ctx, []string{
		"gitea", "admin", "user", "create",
		"--admin",
		"--username", "aiops-bot",
		"--password", pass,
		"--email", "aiops-bot@example.invalid",
		"-c", "/etc/gitea/app.ini",
	})
	if execErr != nil || code != 0 {
		_ = c.Terminate(ctx)
		return nil, fmt.Errorf("create gitea admin user: exit %d err %v", code, execErr)
	}

	host, err := c.Host(ctx)
	if err != nil {
		_ = c.Terminate(ctx)
		return nil, err
	}
	port, err := c.MappedPort(ctx, "3000/tcp")
	if err != nil {
		_ = c.Terminate(ctx)
		return nil, err
	}
	baseURL := fmt.Sprintf("http://%s:%s", host, port.Port())

	env := &giteaEnv{
		baseURL:    baseURL,
		mappedPort: port.Port(),
		botUser:    "aiops-bot",
		botPass:    pass,
		container:  c,
	}

	tok, err := env.createToken(ctx, "e2e")
	if err != nil {
		_ = c.Terminate(ctx)
		return nil, fmt.Errorf("create token: %w", err)
	}
	env.botToken = tok

	return env, nil
}

func (g *giteaEnv) close(ctx context.Context) {
	_ = g.container.Terminate(ctx)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func encodeBase64(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}

// --- HTTP plumbing ---

func (g *giteaEnv) doJSON(ctx context.Context, method, path string, body any, basicAuth bool) (*http.Response, []byte, error) {
	var bodyReader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, nil, err
		}
		bodyReader = bytes.NewReader(buf)
	}
	u := g.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, u, bodyReader)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if basicAuth {
		req.SetBasicAuth(g.botUser, g.botPass)
	} else if g.botToken != "" {
		req.Header.Set("Authorization", "token "+g.botToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return resp, respBody, nil
}

func (g *giteaEnv) createToken(ctx context.Context, name string) (string, error) {
	type tokReq struct {
		Name   string   `json:"name"`
		Scopes []string `json:"scopes"`
	}
	resp, body, err := g.doJSON(ctx, "POST",
		"/api/v1/users/"+url.PathEscape(g.botUser)+"/tokens",
		tokReq{Name: name, Scopes: []string{"write:repository", "write:admin", "write:user", "write:issue"}},
		true)
	if err != nil {
		return "", err
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("createToken: status %d body %s", resp.StatusCode, body)
	}
	var out struct {
		Sha1 string `json:"sha1"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	if out.Sha1 == "" {
		return "", fmt.Errorf("createToken: empty sha1 in body %s", body)
	}
	return out.Sha1, nil
}

// --- domain helpers ---

func (g *giteaEnv) createRepo(ctx context.Context, name string) (cloneURL string, err error) {
	type req struct {
		Name     string `json:"name"`
		AutoInit bool   `json:"auto_init"`
		Private  bool   `json:"private"`
	}
	resp, body, err := g.doJSON(ctx, "POST", "/api/v1/user/repos",
		req{Name: name, AutoInit: true, Private: false}, false)
	if err != nil {
		return "", err
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("createRepo: status %d body %s", resp.StatusCode, body)
	}
	var out struct {
		CloneURL string `json:"clone_url"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	return out.CloneURL, nil
}

func (g *giteaEnv) putFile(ctx context.Context, owner, repo, path string, content []byte, msg string) error {
	type req struct {
		Message string `json:"message"`
		Content string `json:"content"`
	}
	resp, body, err := g.doJSON(ctx, "POST",
		fmt.Sprintf("/api/v1/repos/%s/%s/contents/%s", owner, repo, path),
		req{Message: msg, Content: encodeBase64(content)},
		false)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("putFile: status %d body %s", resp.StatusCode, body)
	}
	return nil
}

func (g *giteaEnv) createWebhook(ctx context.Context, owner, repo, hookURL, secret string) error {
	type req struct {
		Type   string            `json:"type"`
		Config map[string]string `json:"config"`
		Events []string          `json:"events"`
		Active bool              `json:"active"`
	}
	resp, body, err := g.doJSON(ctx, "POST",
		fmt.Sprintf("/api/v1/repos/%s/%s/hooks", owner, repo),
		req{
			Type: "gitea",
			Config: map[string]string{
				"content_type": "json",
				"url":          hookURL,
				"secret":       secret,
			},
			Events: []string{"issue_comment"},
			Active: true,
		},
		false)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("createWebhook: status %d body %s", resp.StatusCode, body)
	}
	return nil
}

func (g *giteaEnv) createIssue(ctx context.Context, owner, repo, title, body string) (int, error) {
	type req struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	resp, respBody, err := g.doJSON(ctx, "POST",
		fmt.Sprintf("/api/v1/repos/%s/%s/issues", owner, repo),
		req{Title: title, Body: body}, false)
	if err != nil {
		return 0, err
	}
	if resp.StatusCode/100 != 2 {
		return 0, fmt.Errorf("createIssue: status %d body %s", resp.StatusCode, respBody)
	}
	var out struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return 0, err
	}
	return out.Number, nil
}

func (g *giteaEnv) commentIssue(ctx context.Context, owner, repo string, issue int, body string) error {
	type req struct {
		Body string `json:"body"`
	}
	resp, respBody, err := g.doJSON(ctx, "POST",
		fmt.Sprintf("/api/v1/repos/%s/%s/issues/%d/comments", owner, repo, issue),
		req{Body: body}, false)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("commentIssue: status %d body %s", resp.StatusCode, respBody)
	}
	return nil
}

type prSummary struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	Draft  bool   `json:"draft"`
	Head   struct {
		Ref string `json:"ref"`
	} `json:"head"`
}

func (g *giteaEnv) listOpenPRs(ctx context.Context, owner, repo string) ([]prSummary, error) {
	resp, body, err := g.doJSON(ctx, "GET",
		fmt.Sprintf("/api/v1/repos/%s/%s/pulls?state=open", owner, repo),
		nil, false)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("listOpenPRs: status %d body %s", resp.StatusCode, body)
	}
	var out []prSummary
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (g *giteaEnv) getBranch(ctx context.Context, owner, repo, branch string) (bool, error) {
	resp, _, err := g.doJSON(ctx, "GET",
		fmt.Sprintf("/api/v1/repos/%s/%s/branches/%s", owner, repo, url.PathEscape(branch)),
		nil, false)
	if err != nil {
		return false, err
	}
	if resp.StatusCode == 404 {
		return false, nil
	}
	if resp.StatusCode/100 != 2 {
		return false, fmt.Errorf("getBranch: unexpected status %d", resp.StatusCode)
	}
	return true, nil
}
