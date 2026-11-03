// Command signer is the only program that touches signing keys (CLAUDE.md
// invariant 3).
//
//	signer serve      -keys DIR -blobs DIR -db DSN -listen unix:///run/foundry/signer.sock
//	signer keys       -keys DIR -tenant NAME            # create/show a tenant key set
//	signer sign-image -keys DIR -tenant NAME -disk X.raw -uki X.efi -roothash H -out DIR
//
// "serve" authorizes every request against the database (build must be in
// finalizing). "sign-image" is the offline path for local/dev builds: it runs
// the finalize stage in-process for a single disk image.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"foundry.dev/foundry/internal/blob"
	"foundry.dev/foundry/internal/signer"
	"foundry.dev/foundry/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: signer serve|keys|sign-image [flags]")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(ctx, os.Args[2:])
	case "keys":
		err = keys(os.Args[2:])
	case "sign-image":
		err = signImage(ctx, os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "signer:", err)
		os.Exit(1)
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func serve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	keysDir := fs.String("keys", env("FOUNDRY_SIGNER_KEYS", "/var/lib/foundry/keys"), "key directory (0700)")
	blobsDir := fs.String("blobs", env("FOUNDRY_BLOBS", "/var/lib/foundry/blobs"), "blob store root")
	dsn := fs.String("db", env("FOUNDRY_DB", "postgres://postgres@127.0.0.1:5432/foundry?sslmode=disable"), "database DSN")
	listen := fs.String("listen", env("FOUNDRY_SIGNER_LISTEN", "127.0.0.1:7443"), "host:port or unix:///path")
	fs.Parse(args)
	token := os.Getenv("FOUNDRY_SIGNER_TOKEN")
	if len(token) < 16 {
		return fmt.Errorf("FOUNDRY_SIGNER_TOKEN must be set (≥16 characters)")
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("svc", "signer")
	if err := os.MkdirAll(*keysDir, 0o700); err != nil {
		return err
	}
	db, err := store.Open(ctx, *dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	var bs blob.Store
	if os.Getenv("FOUNDRY_S3_BUCKET") != "" {
		bs, err = blob.FromEnv(os.Getenv, *blobsDir)
	} else {
		bs, err = blob.NewFS(*blobsDir)
	}
	if err != nil {
		return err
	}
	svc := &signer.Service{DB: db, Blobs: bs, KeysRoot: *keysDir, Tools: tools(""), Token: token, Log: log}
	var ln net.Listener
	if p, ok := strings.CutPrefix(*listen, "unix://"); ok {
		os.Remove(p)
		ln, err = net.Listen("unix", p)
		if err == nil {
			os.Chmod(p, 0o660)
		}
	} else {
		ln, err = net.Listen("tcp", *listen)
	}
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: svc.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { <-ctx.Done(); srv.Shutdown(context.Background()) }()
	log.Info("listening", "addr", *listen)
	if err := srv.Serve(ln); err != http.ErrServerClosed {
		return err
	}
	return nil
}

func keys(args []string) error {
	fs := flag.NewFlagSet("keys", flag.ExitOnError)
	dir := fs.String("keys", env("FOUNDRY_SIGNER_KEYS", "/var/lib/foundry/keys"), "key directory")
	tenant := fs.String("tenant", "", "tenant (org slug)")
	fs.Parse(args)
	k, err := signer.Open(*dir, *tenant, true)
	if err != nil {
		return err
	}
	out := map[string]string{"dir": k.Dir, "efiOwner": k.OwnerGUID()}
	for _, r := range []string{signer.RolePK, signer.RoleKEK, signer.RoleDB} {
		c, err := k.Cert(r)
		if err != nil {
			return err
		}
		out[r] = c.Subject.CommonName + " (expires " + c.NotAfter.Format("2006-01-02") + ")"
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
	return nil
}

// tools returns the signing tools; extra db certificates come from the flag
// or FOUNDRY_SIGNER_EXTRA_DB_CERTS (comma separated paths).
func tools(extra string) signer.Tools {
	t := signer.DefaultTools()
	if extra == "" {
		extra = os.Getenv("FOUNDRY_SIGNER_EXTRA_DB_CERTS")
	}
	for _, p := range strings.Split(extra, ",") {
		if p = strings.TrimSpace(p); p != "" {
			t.ExtraDBCerts = append(t.ExtraDBCerts, p)
		}
	}
	return t
}

func abs(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return a
}
