// Command previewd runs live-preview VMs on a (KVM) host. Only the API talks
// to it, with a shared bearer token; browsers reach the VNC stream through
// the API's authenticated WebSocket, never directly.
//
//	FOUNDRY_PREVIEW_LISTEN   127.0.0.1:7444
//	FOUNDRY_PREVIEW_TOKEN    shared secret with the API (required)
//	FOUNDRY_PREVIEW_DIR      /var/lib/foundry/preview
//	FOUNDRY_BLOBS            blob store directory
//	FOUNDRY_PREVIEW_MAX      sessions per host (4), FOUNDRY_PREVIEW_PER_ORG (2)
//	FOUNDRY_PREVIEW_MINUTES  session length (15)
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"foundry.dev/foundry/internal/blob"
	"foundry.dev/foundry/internal/preview"
	"foundry.dev/foundry/internal/svc"
	"foundry.dev/foundry/internal/tester"
)

func main() {
	log := svc.Logger("previewd")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	token := os.Getenv("FOUNDRY_PREVIEW_TOKEN")
	if len(token) < 16 {
		log.Error("FOUNDRY_PREVIEW_TOKEN must be set (≥16 characters)")
		os.Exit(1)
	}
	bs, err := blob.FromEnv(os.Getenv, "/var/lib/foundry/blobs")
	if err != nil {
		log.Error("blobs", "err", err)
		os.Exit(1)
	}
	dir := svc.Env("FOUNDRY_PREVIEW_DIR", "/var/lib/foundry/preview")
	m := &preview.Manager{VM: tester.DefaultConfig(dir), Blobs: bs, Dir: dir,
		TTL:         time.Duration(svc.EnvInt("FOUNDRY_PREVIEW_MINUTES", 15)) * time.Minute,
		MaxSessions: svc.EnvInt("FOUNDRY_PREVIEW_MAX", 4), PerOrg: svc.EnvInt("FOUNDRY_PREVIEW_PER_ORG", 2)}
	go func() {
		t := time.NewTicker(time.Minute)
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.Reap()
			}
		}
	}()
	if err := svc.Serve(ctx, log, svc.Env("FOUNDRY_PREVIEW_LISTEN", "127.0.0.1:7444"), Handler(m, token)); err != nil {
		log.Error("serve", "err", err)
	}
	m.Shutdown()
}

// Handler is previewd's HTTP API.
func Handler(m *preview.Manager, token string) http.Handler {
	mux := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}
	reply := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("POST /v1/sessions", auth(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			OrgID     string `json:"orgId"`
			BuildID   string `json:"buildId"`
			Disk      string `json:"disk"`
			MemoryMiB int    `json:"memoryMiB"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil || req.OrgID == "" || req.Disk == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		s, err := m.Start(r.Context(), req.OrgID, req.BuildID, req.Disk, min(max(req.MemoryMiB, 1024), 4096))
		if errors.Is(err, preview.ErrQuota) {
			http.Error(w, err.Error(), http.StatusTooManyRequests)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		reply(w, 201, s)
	}))
	mux.HandleFunc("GET /v1/sessions/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		s, err := m.Get(r.PathValue("id"))
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		reply(w, 200, s)
	}))
	mux.HandleFunc("DELETE /v1/sessions/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		if err := m.Stop(r.PathValue("id")); err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	// Raw RFB stream after an HTTP/1.1 upgrade (the API bridges it to WebSocket).
	mux.HandleFunc("GET /v1/sessions/{id}/rfb", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "foundry-rfb" {
			http.Error(w, "upgrade required", http.StatusUpgradeRequired)
			return
		}
		vnc, err := m.Attach(r.Context(), r.PathValue("id"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		defer vnc.Close()
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		c, rw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: foundry-rfb\r\nConnection: Upgrade\r\n\r\n")
		done := make(chan struct{}, 2)
		go func() { io.Copy(vnc, rw); done <- struct{}{} }()
		go func() { io.Copy(c, vnc); done <- struct{}{} }()
		<-done
	}))
	return mux
}
