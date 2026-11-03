// Command worker-agent runs on build/test/finalize hosts.
//
//	FOUNDRY_DB, FOUNDRY_BLOBS, FOUNDRY_WORK (scratch), FOUNDRY_COMPONENTS (bin/components)
//	FOUNDRY_POOLS=build,test,finalize   FOUNDRY_SLOTS=1   FOUNDRY_HOST_ID (default hostname)
//	FOUNDRY_TOOLS_TREE (prebuilt mkosi tools tree)   FOUNDRY_DIRECT_MIRRORS=1 (drop *_proxy for mkosi)
//	FOUNDRY_SIGNER_URL, FOUNDRY_SIGNER_TOKEN (finalize pool)
//	FOUNDRY_SANDBOX=vm|process, FOUNDRY_BUILDER_IMAGE (golden qcow2), FOUNDRY_EGRESS_ALLOW (extra hosts)
//	FOUNDRY_ZSTD_LEVEL (19)   FOUNDRY_VERIFY_SECURE_BOOT=1   FOUNDRY_BOOT_TIMEOUT (15m)
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"foundry.dev/foundry/internal/procreg"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"foundry.dev/foundry/internal/blob"
	"foundry.dev/foundry/internal/builder"
	"foundry.dev/foundry/internal/egress"
	"foundry.dev/foundry/internal/envelope"
	"foundry.dev/foundry/internal/imgutil"
	"foundry.dev/foundry/internal/sandbox"
	"foundry.dev/foundry/internal/signclient"
	"foundry.dev/foundry/internal/store"
	"foundry.dev/foundry/internal/svc"
	"foundry.dev/foundry/internal/tester"
	"foundry.dev/foundry/internal/worker"
)

var version = "dev"

func main() {
	if len(os.Args) == 3 && os.Args[1] == "egress-pipe" { // invoked by QEMU per guest connection
		if err := sandbox.Pipe(os.Args[2]); err != nil {
			os.Exit(1)
		}
		return
	}
	log := svc.Logger("worker")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	host, _ := os.Hostname()
	work := svc.Env("FOUNDRY_WORK", "/var/lib/foundry/work")
	db, err := store.Open(ctx, svc.Env("FOUNDRY_DB", svc.DefaultDSN))
	if err != nil {
		log.Error("db", "err", err)
		os.Exit(1)
	}
	defer db.Close()
	bs, err := blob.FromEnv(os.Getenv, "/var/lib/foundry/blobs")
	if err != nil {
		log.Error("blobs", "err", err)
		os.Exit(1)
	}
	tt := svc.Env("FOUNDRY_TOOLS_TREE", "default")
	// Kill whatever a crashed predecessor left running (builds, VMs).
	procs := procreg.Dir(filepath.Join(work, "procs"))
	if n := procs.Reap(log); n > 0 {
		log.Warn("reaped orphaned process groups", "count", n)
	}
	// Scratch space of builds and VMs that died with it (the mkosi package
	// cache under cache/ is kept).
	for _, pat := range []string{"jobs/*", "builds/*", "vm-*", "*/vm-*"} {
		stale, _ := filepath.Glob(filepath.Join(work, pat))
		for _, p := range stale {
			if filepath.Base(filepath.Dir(p)) != "cache" {
				os.RemoveAll(p)
			}
		}
	}
	tc := tester.DefaultConfig(work)
	tc.Procs = procs
	if d, err := time.ParseDuration(svc.Env("FOUNDRY_BOOT_TIMEOUT", "15m")); err == nil {
		tc.BootTimeout = d
	}
	cfg := worker.Config{
		HostID:        svc.Env("FOUNDRY_HOST_ID", host),
		Pools:         strings.Split(svc.Env("FOUNDRY_POOLS", "build,test,finalize"), ","),
		Slots:         svc.EnvInt("FOUNDRY_SLOTS", 1),
		Version:       version,
		WorkDir:       work,
		ComponentsDir: svc.Env("FOUNDRY_COMPONENTS", "bin/components"),
		Builder: builder.Config{CacheDir: svc.Env("FOUNDRY_MKOSI_CACHE", work+"/cache"), ToolsTree: tt, ToolsTreeDist: "ubuntu",
			ToolsTreeRelease: "resolute", ToolsTreeMirror: "http://archive.ubuntu.com/ubuntu", Mkosi: svc.Env("FOUNDRY_MKOSI", "mkosi"),
			DropProxyEnv: svc.EnvBool("FOUNDRY_DIRECT_MIRRORS"), Timeout: 60 * time.Minute, Procs: procs},
		Tester:           tc,
		Compression:      imgutil.Compression{ZstdLevel: svc.EnvInt("FOUNDRY_ZSTD_LEVEL", 19), XzLevel: svc.EnvInt("FOUNDRY_XZ_LEVEL", 6)},
		VerifySecureBoot: svc.EnvBool("FOUNDRY_VERIFY_SECURE_BOOT"),
	}
	if pk := os.Getenv("FOUNDRY_ENVELOPE_PUBKEYS"); pk != "" {
		keys, err := envelope.PublicKeys(pk)
		if err != nil {
			log.Error("envelope keys", "err", err)
			os.Exit(1)
		}
		cfg.EnvelopeKeys = keys
	} else if svc.EnvBool("FOUNDRY_DEV") {
		cfg.EnvelopeKeys = []ed25519.PublicKey{envelope.DevKey("dev").Public().(ed25519.PublicKey)}
	} else {
		log.Error("FOUNDRY_ENVELOPE_PUBKEYS must be set (comma-separated base64 Ed25519 public keys)")
		os.Exit(1)
	}
	if u := os.Getenv("FOUNDRY_SIGNER_URL"); u != "" {
		cfg.Signer = signclient.New(u, os.Getenv("FOUNDRY_SIGNER_TOKEN"))
	}
	// Egress proxy: the only network path of build sandboxes.
	secret := make([]byte, 32)
	rand.Read(secret)
	allow := egress.DefaultAllowlist
	if extra := os.Getenv("FOUNDRY_EGRESS_ALLOW"); extra != "" { // host[:port],... (ports default 80,443)
		for _, h := range strings.Split(extra, ",") {
			if h = strings.TrimSpace(h); h != "" {
				allow = append(allow, egress.Rule{Host: h, Ports: []int{80, 443}})
			}
		}
	}
	px := &egress.Proxy{Allow: allow, Secret: secret, Log: log, DenyPrivate: !svc.EnvBool("FOUNDRY_EGRESS_ALLOW_PRIVATE"),
		OnEvent: func(e egress.Event) {
			if e.BuildID == "" || (e.Allowed && (e.Reason == "" || e.Reason == "cache hit")) { // store denials and upstream errors
				return
			}
			db.RecordEgress(context.Background(), store.EgressEvent{BuildID: e.BuildID, TS: e.Time, Method: e.Method, Host: e.Host,
				Port: e.Port, Allowed: e.Allowed, Reason: e.Reason, Bytes: e.Bytes})
		}}
	// Package cache tier (immutable mirror files), shared by all builds on the host.
	if dir := svc.Env("FOUNDRY_PKG_CACHE", "/var/lib/foundry/pkgcache"); dir != "off" {
		gib, _ := strconv.Atoi(svc.Env("FOUNDRY_PKG_CACHE_GIB", "20"))
		if pc, err := egress.NewPkgCache(dir, int64(gib)<<30); err == nil {
			px.Cache = pc
			log.Info("package cache", "dir", dir, "gib", gib, "used", pc.Used())
		} else {
			log.Warn("package cache disabled", "err", err)
		}
	}
	ln, err := net.Listen("tcp", svc.Env("FOUNDRY_EGRESS_LISTEN", "127.0.0.1:0"))
	if err != nil {
		log.Error("egress proxy", "err", err)
		os.Exit(1)
	}
	go (&http.Server{Handler: px, ReadHeaderTimeout: 30 * time.Second}).Serve(ln)
	cfg.Egress, cfg.EgressAddr = px, ln.Addr().String()
	switch mode := svc.Env("FOUNDRY_SANDBOX", "process"); mode {
	case "vm":
		cfg.Sandbox = &sandbox.VM{Image: os.Getenv("FOUNDRY_BUILDER_IMAGE"), WorkDir: work, ZstdLevel: cfg.Compression.ZstdLevel,
			MemoryMiB: svc.EnvInt("FOUNDRY_VM_MEMORY_MIB", 8192), CPUs: svc.EnvInt("FOUNDRY_VM_CPUS", 4), Procs: procs}
	case "process":
		bc := cfg.Builder
		bc.WorkDir = work + "/builds"
		cfg.Sandbox = &sandbox.Process{Config: bc}
		log.Warn("process sandbox: mkosi runs on this host (use FOUNDRY_SANDBOX=vm in production)")
	default:
		log.Error("unknown FOUNDRY_SANDBOX", "mode", mode)
		os.Exit(1)
	}
	if err := os.MkdirAll(work, 0o700); err != nil {
		log.Error("work dir", "err", err)
		os.Exit(1)
	}
	log.Info("starting", "host", cfg.HostID, "pools", cfg.Pools, "slots", cfg.Slots)
	w := worker.New(db, bs, log, cfg)
	w.Run(ctx)
}
