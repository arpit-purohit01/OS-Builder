// Package svc holds small helpers shared by the service binaries: env
// configuration, logging and graceful HTTP serving.
package svc

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Env returns $key or def.
func Env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// EnvInt returns an integer env var.
func EnvInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

// EnvBool reads 1/true/yes.
func EnvBool(key string) bool {
	switch strings.ToLower(os.Getenv(key)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Logger returns a JSON logger tagged with the service name; FOUNDRY_LOG=debug enables debug.
func Logger(service string) *slog.Logger {
	lvl := slog.LevelInfo
	if os.Getenv("FOUNDRY_LOG") == "debug" {
		lvl = slog.LevelDebug
	}
	var h slog.Handler = slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	if os.Getenv("FOUNDRY_LOG_FORMAT") == "text" {
		h = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	}
	return slog.New(h).With("svc", service)
}

// DefaultDSN is the development database.
const DefaultDSN = "postgres://postgres@127.0.0.1:5432/foundry?sslmode=disable"

// Serve runs an HTTP server until ctx ends, then shuts down gracefully.
func Serve(ctx context.Context, log *slog.Logger, addr string, h http.Handler) error {
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info("listening", "addr", ln.Addr().String())
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return srv.Shutdown(sctx)
}
