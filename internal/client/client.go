// Package client is a Go client for the Foundry REST API (used by fctl and
// the end-to-end tests). It authenticates with an API token (Bearer) or a
// browser-style session (cookie + CSRF header).
package client

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"time"
)

// Client talks to one API.
type Client struct {
	BaseURL string
	Token   string
	CSRF    string
	Org     string
	HTTP    *http.Client
}

// New creates a client with a cookie jar.
func New(base string) *Client {
	jar, _ := cookiejar.New(nil)
	return &Client{BaseURL: strings.TrimRight(base, "/"), HTTP: &http.Client{Jar: jar, Timeout: 10 * time.Minute}}
}

// Problem is an RFC 9457 error response.
type Problem struct {
	Status int    `json:"status"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Errors []struct {
		Code    string `json:"code"`
		Pointer string `json:"pointer"`
		Message string `json:"message"`
	} `json:"errors"`
}

func (p *Problem) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d %s", p.Status, p.Title)
	if p.Detail != "" {
		b.WriteString(": " + p.Detail)
	}
	for _, e := range p.Errors {
		fmt.Fprintf(&b, "\n  %s %s %s", e.Code, e.Pointer, e.Message)
	}
	return b.String()
}

// Do sends a request; body may be nil, []byte, string or a JSON value.
func (c *Client) Do(ctx context.Context, method, path string, body any, out any, hdr ...string) (*http.Response, error) {
	var rd io.Reader
	ctype := "application/json"
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
		ctype = "application/yaml"
	case string:
		rd = strings.NewReader(b)
		ctype = "application/yaml"
	default:
		j, err := json.Marshal(b)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(j)
	}
	u := path
	if !strings.HasPrefix(path, "http") {
		u = c.BaseURL + path
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	if rd != nil {
		req.Header.Set("Content-Type", ctype)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if c.CSRF != "" {
		req.Header.Set("X-CSRF-Token", c.CSRF)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		p := &Problem{Status: resp.StatusCode, Title: http.StatusText(resp.StatusCode)}
		json.Unmarshal(b, p)
		return resp, p
	}
	if out != nil {
		defer resp.Body.Close()
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil && err != io.EOF {
			return resp, err
		}
	}
	return resp, nil
}

func (c *Client) org(path string) string { return "/v1/orgs/" + c.Org + path }

// ---------------------------------------------------------------- auth

// EmailLogin performs the email-code login; code is obtained from codeFn
// (prompt) unless the server returns a development code.
func (c *Client) EmailLogin(ctx context.Context, email string, codeFn func() (string, error)) error {
	var st struct {
		DevCode string `json:"devCode"`
	}
	if _, err := c.Do(ctx, "POST", "/v1/auth/email/start", map[string]string{"email": email}, &st); err != nil {
		return err
	}
	code := st.DevCode
	if code == "" {
		var err error
		if code, err = codeFn(); err != nil {
			return err
		}
	}
	var v struct {
		CSRF string `json:"csrf"`
	}
	if _, err := c.Do(ctx, "POST", "/v1/auth/email/verify", map[string]string{"email": email, "code": strings.TrimSpace(code)}, &v); err != nil {
		return err
	}
	c.CSRF = v.CSRF
	return nil
}

// Me returns the current user and memberships.
func (c *Client) Me(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	_, err := c.Do(ctx, "GET", "/v1/me", nil, &out)
	return out, err
}

// CreateOrg creates an organization.
func (c *Client) CreateOrg(ctx context.Context, name, slug string) (string, error) {
	var o struct{ ID string }
	_, err := c.Do(ctx, "POST", "/v1/orgs", map[string]string{"name": name, "slug": slug}, &o)
	return o.ID, err
}

// CreateToken creates an API token in the current org.
func (c *Client) CreateToken(ctx context.Context, name string, scopes []string, days int) (string, error) {
	var t struct{ Secret string }
	_, err := c.Do(ctx, "POST", c.org("/tokens"), map[string]any{"name": name, "scopes": scopes, "expiresInDays": days}, &t)
	return t.Secret, err
}

// ------------------------------------------------------------- projects

// Project is a project summary.
type Project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	OSID string `json:"osId"`
}

// Projects lists projects.
func (c *Client) Projects(ctx context.Context) ([]Project, error) {
	var out struct{ Items []Project }
	_, err := c.Do(ctx, "GET", c.org("/projects"), nil, &out)
	return out.Items, err
}

// CreateProject creates a project.
func (c *Client) CreateProject(ctx context.Context, name, osID string) (*Project, error) {
	var p Project
	_, err := c.Do(ctx, "POST", c.org("/projects"), map[string]string{"name": name, "osId": osID}, &p)
	return &p, err
}

// SpecVersion is a stored spec version.
type SpecVersion struct {
	ID       string          `json:"id"`
	Digest   string          `json:"digest"`
	ParentID *string         `json:"parentId"`
	Spec     json.RawMessage `json:"spec"`
}

// PushSpec uploads a YAML/JSON spec as a new version.
func (c *Client) PushSpec(ctx context.Context, projectID string, doc []byte) (*SpecVersion, bool, error) {
	var out struct {
		SpecVersion SpecVersion `json:"specVersion"`
		Created     bool        `json:"created"`
	}
	_, err := c.Do(ctx, "POST", c.org("/projects/"+projectID+"/specs"), doc, &out)
	return &out.SpecVersion, out.Created, err
}

// Validate validates a spec without storing it.
func (c *Client) Validate(ctx context.Context, doc []byte) (map[string]any, error) {
	var out map[string]any
	_, err := c.Do(ctx, "POST", c.org("/specs/validate"), doc, &out)
	return out, err
}

// --------------------------------------------------------------- builds

// Build is a build summary.
type Build struct {
	ID        string          `json:"id"`
	State     string          `json:"state"`
	Tier      *string         `json:"tier"`
	RiskScore *int            `json:"riskScore"`
	Failure   json.RawMessage `json:"failure"`
	Roothash  string          `json:"roothash"`
	CreatedAt time.Time       `json:"createdAt"`
}

// CreateBuild starts a build of a spec version.
func (c *Client) CreateBuild(ctx context.Context, specVersionID string, formats []string, idemKey string) (*Build, error) {
	var out struct{ Build Build }
	hdr := []string{}
	if idemKey != "" {
		hdr = append(hdr, "Idempotency-Key", idemKey)
	}
	_, err := c.Do(ctx, "POST", c.org("/builds"), map[string]any{"specVersionId": specVersionID, "formats": formats}, &out, hdr...)
	return &out.Build, err
}

// GetBuild returns a build.
func (c *Client) GetBuild(ctx context.Context, id string) (*Build, error) {
	var out struct{ Build Build }
	_, err := c.Do(ctx, "GET", c.org("/builds/"+id), nil, &out)
	return &out.Build, err
}

// Builds lists recent builds.
func (c *Client) Builds(ctx context.Context) ([]Build, error) {
	var out struct{ Items []Build }
	_, err := c.Do(ctx, "GET", c.org("/builds?limit=50"), nil, &out)
	return out.Items, err
}

// Cancel requests cancellation.
func (c *Client) Cancel(ctx context.Context, id string) error {
	_, err := c.Do(ctx, "POST", c.org("/builds/"+id+"/cancel"), nil, nil)
	return err
}

// LogLine is one build log line.
type LogLine struct {
	Seq    int    `json:"seq"`
	Stream string `json:"stream"`
	Line   string `json:"line"`
}

// FollowLogs streams logs (SSE) until the build reaches an end state; f
// receives each line, state receives state changes.
func (c *Client) FollowLogs(ctx context.Context, id string, after int, f func(LogLine), state func(string)) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", c.BaseURL+c.org("/builds/"+id+"/logs")+fmt.Sprintf("?after=%d", after), nil)
	req.Header.Set("Accept", "text/event-stream")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	hc := *c.HTTP
	hc.Timeout = 0
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("logs: HTTP %d", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 128<<10), 1<<20)
	event := ""
	for sc.Scan() {
		l := sc.Text()
		switch {
		case strings.HasPrefix(l, "event: "):
			event = strings.TrimPrefix(l, "event: ")
		case strings.HasPrefix(l, "data: "):
			data := strings.TrimPrefix(l, "data: ")
			switch event {
			case "log":
				var ll LogLine
				if json.Unmarshal([]byte(data), &ll) == nil && f != nil {
					f(ll)
				}
			case "state":
				var st struct{ State string }
				if json.Unmarshal([]byte(data), &st) == nil && state != nil {
					state(st.State)
				}
			case "end":
				return nil
			}
		}
	}
	return sc.Err()
}

// Artifact is a downloadable build output.
type Artifact struct {
	Kind   string `json:"kind"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
	Signed bool   `json:"signed"`
	Name   string `json:"name"`
	URL    string `json:"url"`
}

// Artifacts lists a build's artifacts.
func (c *Client) Artifacts(ctx context.Context, id string) ([]Artifact, error) {
	var out struct{ Items []Artifact }
	_, err := c.Do(ctx, "GET", c.org("/builds/"+id+"/artifacts"), nil, &out)
	return out.Items, err
}

// Download fetches an artifact to path and verifies its content digest.
func (c *Client) Download(ctx context.Context, a Artifact, path string) error {
	if a.URL == "" {
		return errors.New("artifact is not downloadable yet")
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", a.URL, nil)
	hc := *c.HTTP
	hc.Timeout = 0
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}
	tmp := path + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		f.Close()
		return err
	}
	f.Close()
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != a.Digest {
		os.Remove(tmp)
		return fmt.Errorf("download: digest mismatch (%s != %s)", got, a.Digest)
	}
	return os.Rename(tmp, path)
}

// WaitState polls until the build reaches one of the states (or a terminal one).
func (c *Client) WaitState(ctx context.Context, id string, states ...string) (*Build, error) {
	want := map[string]bool{"failed": true, "cancelled": true, "rejected": true, "published": true}
	for _, s := range states {
		want[s] = true
	}
	for {
		b, err := c.GetBuild(ctx, id)
		if err != nil {
			return nil, err
		}
		if want[b.State] {
			return b, nil
		}
		select {
		case <-ctx.Done():
			return b, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// Review submits a review decision (approve|reject|changes_requested).
func (c *Client) Review(ctx context.Context, buildID, decision, comment string) error {
	_, err := c.Do(ctx, "POST", c.org("/builds/"+buildID+"/reviews"), map[string]string{"decision": decision, "comment": comment}, nil)
	return err
}

// URLWithQuery is a helper for list filters.
func URLWithQuery(path string, q url.Values) string {
	if len(q) == 0 {
		return path
	}
	return path + "?" + q.Encode()
}
