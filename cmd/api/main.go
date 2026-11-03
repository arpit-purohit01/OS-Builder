// Command api serves the public REST API and the web app.
//
// Configuration (environment):
//
//	FOUNDRY_DB            Postgres DSN
//	FOUNDRY_LISTEN        listen address (:8080)
//	FOUNDRY_BASE_URL      public URL (http://localhost:8080)
//	FOUNDRY_BLOBS         blob store directory
//	FOUNDRY_CATALOG       catalog directory
//	FOUNDRY_WEB           built web app directory (optional)
//	FOUNDRY_PREVIEW_URL   previewd base URL, FOUNDRY_PREVIEW_TOKEN shared secret (live preview)
//	FOUNDRY_URL_KEY       HMAC key for signed download/upload URLs (≥32 chars)
//	FOUNDRY_DEV=1         development mode: login codes in responses, insecure cookies, dev envelope key
//	FOUNDRY_ENVELOPE_KEY  base64 Ed25519 seed signing build job envelopes
//	FOUNDRY_OIDC_ISSUER / FOUNDRY_OIDC_CLIENT_ID / FOUNDRY_OIDC_CLIENT_SECRET
//	FOUNDRY_SMTP_ADDR / FOUNDRY_SMTP_FROM / FOUNDRY_SMTP_USER / FOUNDRY_SMTP_PASSWORD
//	FOUNDRY_TENANT / FOUNDRY_FLEET_URL / FOUNDRY_UPDATE_URL / FOUNDRY_FLATPAK_REMOTE
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"foundry.dev/foundry/internal/api"
	"foundry.dev/foundry/internal/blob"
	"foundry.dev/foundry/internal/catalog"
	"foundry.dev/foundry/internal/compiler"
	"foundry.dev/foundry/internal/envelope"
	"foundry.dev/foundry/internal/mail"
	"foundry.dev/foundry/internal/oidc"
	"foundry.dev/foundry/internal/pgnotify"
	"foundry.dev/foundry/internal/store"
	"foundry.dev/foundry/internal/svc"
)

func main() {
	migrate := flag.Bool("migrate", true, "apply database migrations at start")
	flag.Parse()
	log := svc.Logger("api")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *migrate); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, migrate bool) error {
	log := svc.Logger("api")
	dev := svc.EnvBool("FOUNDRY_DEV")
	db, err := store.Open(ctx, svc.Env("FOUNDRY_DB", svc.DefaultDSN))
	if err != nil {
		return err
	}
	defer db.Close()
	if migrate {
		if err := db.Migrate(ctx); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	cat, err := catalog.LoadDir(svc.Env("FOUNDRY_CATALOG", "catalog"))
	if err != nil {
		return err
	}
	cnf, _ := json.Marshal(cat.Compile("ubuntu-24.04", "x86_64"))
	sum := sha256.Sum256(cnf)
	if err := db.EnsureCatalogVersion(ctx, cat.Version, sum[:], true); err != nil {
		return err
	}
	bs, err := blob.FromEnv(os.Getenv, "/var/lib/foundry/blobs")
	if err != nil {
		return err
	}
	key := os.Getenv("FOUNDRY_URL_KEY")
	if len(key) < 32 {
		if !dev {
			return fmt.Errorf("FOUNDRY_URL_KEY must be at least 32 characters")
		}
		key = "development-only-url-signing-key-0123456789"
	}
	baseURL := strings.TrimRight(svc.Env("FOUNDRY_BASE_URL", "http://localhost:8080"), "/")
	signer := &blob.Signer{Key: []byte(key), BaseURL: baseURL}
	var mailer mail.Mailer = mail.LogMailer{Log: log}
	if addr := os.Getenv("FOUNDRY_SMTP_ADDR"); addr != "" {
		mailer = mail.SMTP{Addr: addr, From: svc.Env("FOUNDRY_SMTP_FROM", "noreply@foundry.local"), User: os.Getenv("FOUNDRY_SMTP_USER"), Password: os.Getenv("FOUNDRY_SMTP_PASSWORD")}
	}
	cfg := api.Config{BaseURL: baseURL, CookieSecure: strings.HasPrefix(baseURL, "https://"), DevLogin: dev, WebDir: os.Getenv("FOUNDRY_WEB"),
		Platform: compiler.Platform{Tenant: svc.Env("FOUNDRY_TENANT", "foundry"), FleetURL: os.Getenv("FOUNDRY_FLEET_URL"),
			UpdateURL: os.Getenv("FOUNDRY_UPDATE_URL"), SyncURL: os.Getenv("FOUNDRY_SYNC_URL"), FlatpakRemote: os.Getenv("FOUNDRY_FLATPAK_REMOTE")},
		PresignDownloads: svc.EnvBool("FOUNDRY_S3_PRESIGN"), PreviewURL: os.Getenv("FOUNDRY_PREVIEW_URL"), PreviewToken: os.Getenv("FOUNDRY_PREVIEW_TOKEN")}
	if iss := os.Getenv("FOUNDRY_OIDC_ISSUER"); iss != "" {
		dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		p := &oidc.Provider{Issuer: iss, ClientID: os.Getenv("FOUNDRY_OIDC_CLIENT_ID"), ClientSecret: os.Getenv("FOUNDRY_OIDC_CLIENT_SECRET"),
			RedirectURL: baseURL + "/v1/auth/oidc/callback"}
		err := p.Discover(dctx)
		cancel()
		if err != nil {
			return fmt.Errorf("oidc: %w", err)
		}
		cfg.OIDC = p
	}
	if seed := os.Getenv("FOUNDRY_ENVELOPE_KEY"); seed != "" {
		if cfg.EnvelopeKey, err = envelope.KeyFromSeed(seed); err != nil {
			return err
		}
	} else if dev {
		cfg.EnvelopeKey = envelope.DevKey("dev")
	} else {
		return fmt.Errorf("FOUNDRY_ENVELOPE_KEY must be set (base64 Ed25519 seed)")
	}
	srv := api.New(cfg, db, bs, signer, cat, mailer, log)
	srv.SetHooks(api.Hooks{BuildCreated: func(id string) { db.Notify(context.Background(), "foundry_sched", id) }})
	go pgnotify.Listen(ctx, db.Pool, "foundry_build", srv.Notify)
	if dev {
		log.Warn("development mode: login codes are returned by the API")
	}
	return svc.Serve(ctx, log, svc.Env("FOUNDRY_LISTEN", ":8080"), srv.Handler())
}
