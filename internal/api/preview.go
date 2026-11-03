package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"foundry.dev/foundry/internal/ids"
	"foundry.dev/foundry/internal/store"
	"foundry.dev/foundry/internal/ws"
)

// Live preview: the API authorizes, previewd runs the VM, and the browser's
// WebSocket is bridged here to previewd's raw RFB stream. Only signed
// (released) images can be previewed.

const previewPerOrg = 2

func (s *Server) previewCall(ctx context.Context, method, path string, body any, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(s.cfg.PreviewURL, "/")+path, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.PreviewToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return resp.StatusCode, fmt.Errorf("previewd: %s", strings.TrimSpace(string(b)))
	}
	if out != nil {
		return resp.StatusCode, json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode, nil
}

func (s *Server) startPreview(w http.ResponseWriter, r *http.Request) {
	if s.cfg.PreviewURL == "" {
		problem(w, r, 503, "preview-unavailable", "Live preview is not available on this installation", "")
		return
	}
	p := getPrincipal(r.Context())
	b, err := s.db.GetBuild(r.Context(), p.OrgID, r.PathValue("build"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if b.State != store.StatePublished && b.State != store.StateSigned {
		problem(w, r, 409, "not-released", "Only signed builds can be previewed", "")
		return
	}
	if ok, _ := s.limiter.Allow("preview:" + p.UserID); !ok {
		problem(w, r, 429, "rate-limited", "Too many preview requests", "")
		return
	}
	if n, err := s.db.ActivePreviews(r.Context(), p.OrgID); err == nil && n >= previewPerOrg {
		problem(w, r, 429, "preview-quota", "Your organization already has the maximum number of live previews running", "Stop a preview or wait for one to end.")
		return
	}
	var disk string
	as, err := s.db.Artifacts(r.Context(), p.OrgID, b.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for _, a := range as {
		if a.Kind == "disk.raw.zst" && a.Signed {
			disk = a.Digest
		}
	}
	if disk == "" {
		problem(w, r, 409, "no-image", "This build has no signed disk image", "")
		return
	}
	mem := 1024
	if sv, err := s.db.GetSpecVersion(r.Context(), p.OrgID, b.SpecVersionID); err == nil && strings.Contains(string(sv.Spec), `"desktop-`) {
		mem = 4096
	}
	var remote struct {
		ID        string    `json:"id"`
		ExpiresAt time.Time `json:"expiresAt"`
	}
	code, err := s.previewCall(r.Context(), "POST", "/v1/sessions", map[string]any{"orgId": p.OrgID, "buildId": b.ID, "disk": disk, "memoryMiB": mem}, &remote)
	if code == http.StatusTooManyRequests {
		problem(w, r, 503, "preview-busy", "All preview machines are busy", "Try again in a few minutes.")
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ps := &store.PreviewSession{ID: ids.New("pvw"), OrgID: p.OrgID, BuildID: b.ID, UserID: p.UserID, RemoteID: remote.ID, ExpiresAt: remote.ExpiresAt}
	if err := s.db.CreatePreview(r.Context(), ps); err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r.Context(), p.OrgID, p.UserID, "preview.start", b.ID, map[string]string{"session": ps.ID})
	writeJSON(w, 201, map[string]any{"session": ps, "ws": "/v1/orgs/" + p.OrgID + "/previews/" + ps.ID + "/ws"})
}

func (s *Server) loadPreview(w http.ResponseWriter, r *http.Request) (*store.PreviewSession, bool) {
	p := getPrincipal(r.Context())
	ps, err := s.db.GetPreview(r.Context(), p.OrgID, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return nil, false
	}
	return ps, true
}

func (s *Server) getPreview(w http.ResponseWriter, r *http.Request) {
	ps, ok := s.loadPreview(w, r)
	if !ok {
		return
	}
	var remote map[string]any
	if _, err := s.previewCall(r.Context(), "GET", "/v1/sessions/"+url.PathEscape(ps.RemoteID), nil, &remote); err != nil {
		remote = map[string]any{"state": "stopped"}
	}
	writeJSON(w, 200, map[string]any{"session": ps, "state": remote["state"], "reason": remote["reason"]})
}

func (s *Server) stopPreview(w http.ResponseWriter, r *http.Request) {
	ps, ok := s.loadPreview(w, r)
	if !ok {
		return
	}
	s.previewCall(r.Context(), "DELETE", "/v1/sessions/"+url.PathEscape(ps.RemoteID), nil, nil)
	s.db.StopPreview(r.Context(), ps.OrgID, ps.ID)
	w.WriteHeader(http.StatusNoContent)
}

// sameOrigin approves WebSocket upgrades only from our own web app (cookies
// are sent on cross-site WebSocket requests, so this check is required).
func (s *Server) sameOrigin(origin string) bool {
	u, err := url.Parse(s.cfg.BaseURL)
	if err != nil || origin == "" {
		return false
	}
	return strings.EqualFold(strings.TrimRight(origin, "/"), u.Scheme+"://"+u.Host)
}

func (s *Server) previewWS(w http.ResponseWriter, r *http.Request) {
	ps, ok := s.loadPreview(w, r)
	if !ok {
		return
	}
	if ps.StoppedAt != nil || time.Now().After(ps.ExpiresAt) {
		problem(w, r, 410, "preview-ended", "This preview has ended", "")
		return
	}
	// Connect to previewd first so failures are plain HTTP errors.
	u, err := url.Parse(s.cfg.PreviewURL)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	up, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(r.Context(), "tcp", u.Host)
	if err != nil {
		problem(w, r, 503, "preview-unavailable", "Preview host unreachable", "")
		return
	}
	defer up.Close()
	fmt.Fprintf(up, "GET /v1/sessions/%s/rfb HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nUpgrade: foundry-rfb\r\nConnection: Upgrade\r\n\r\n", url.PathEscape(ps.RemoteID), u.Host, s.cfg.PreviewToken)
	br := bufio.NewReader(up)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		problem(w, r, 410, "preview-ended", "This preview is no longer running", "")
		return
	}
	c, err := ws.Upgrade(w, r, s.sameOrigin)
	if err != nil {
		return
	}
	defer c.Close()
	deadline := time.Until(ps.ExpiresAt)
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	done := make(chan struct{}, 2)
	go func() { io.Copy(up, c); done <- struct{}{} }()
	go func() { io.Copy(c, br); done <- struct{}{} }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}
