// Package worker is the build-host agent (cmd/worker-agent). It registers
// the host, runs builds the scheduler leased to it (renewing the lease and
// honouring cancellation), and runs the post-build pipeline jobs from the
// queue: scan → boot test → decide → finalize (via the signer) → publish.
//
// Sandbox: in production each build runs in an ephemeral KVM build VM; on
// hosts without /dev/kvm the "process" sandbox runs mkosi directly (mkosi
// itself isolates the build in its own user/mount namespaces). See
// docs/context/build-pipeline.md §4.
package worker

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"foundry.dev/foundry/internal/blob"
	"foundry.dev/foundry/internal/builder"
	"foundry.dev/foundry/internal/compiler"
	"foundry.dev/foundry/internal/egress"
	"foundry.dev/foundry/internal/envelope"
	"foundry.dev/foundry/internal/finalize"
	"foundry.dev/foundry/internal/imgutil"
	"foundry.dev/foundry/internal/pgnotify"
	"foundry.dev/foundry/internal/queue"
	"foundry.dev/foundry/internal/sandbox"
	"foundry.dev/foundry/internal/store"
	"foundry.dev/foundry/internal/tester"
	"github.com/jackc/pgx/v5"
)

// Config for a worker host.
type Config struct {
	HostID        string
	Arch          string
	Slots         int      // concurrent builds
	Pools         []string // build, test, finalize
	Version       string
	WorkDir       string
	ComponentsDir string // platform binaries resolved into images (e.g. foundry-agent)
	LeaseTTL      time.Duration
	Builder       builder.Config
	// Sandbox runs builds (VM in production; process on hosts without KVM).
	Sandbox sandbox.Sandbox
	// Egress is the build egress proxy listening on EgressAddr.
	Egress      *egress.Proxy
	EgressAddr  string
	Tester      tester.Config
	Compression imgutil.Compression
	// Boot test scenario run before review (unsigned image, no Secure Boot).
	TestScenario tester.Scenario
	// VerifySecureBoot boots the signed image with Secure Boot in setup mode
	// before publishing (enrolls the tenant keys, then boots enforcing).
	VerifySecureBoot bool
	// AutoApproveMax: builds whose scan decision needs no reviewer are
	// approved automatically.
	Signer finalize.Signer
	// EnvelopeKeys are the API's trusted envelope keys. When set, a build
	// without a valid envelope matching its inputs is refused.
	EnvelopeKeys []ed25519.PublicKey
}

// Worker runs on one host.
type Worker struct {
	DB    *store.DB
	Blobs blob.Store
	Log   *slog.Logger
	Cfg   Config

	mu      sync.Mutex
	running map[string]context.CancelFunc
}

// New creates a worker.
func New(db *store.DB, bs blob.Store, log *slog.Logger, cfg Config) *Worker {
	if cfg.LeaseTTL == 0 {
		cfg.LeaseTTL = 2 * time.Minute
	}
	if cfg.Slots == 0 {
		cfg.Slots = 1
	}
	if cfg.Arch == "" {
		cfg.Arch = "x86_64"
	}
	if cfg.TestScenario.Name == "" {
		cfg.TestScenario = tester.Scenario{Name: "uefi-tpm-reboot", TPM: true, Reboot: true}
	}
	return &Worker{DB: db, Blobs: bs, Log: log, Cfg: cfg, running: map[string]context.CancelFunc{}}
}

func (w *Worker) has(pool string) bool {
	for _, p := range w.Cfg.Pools {
		if p == pool {
			return true
		}
	}
	return false
}

// Run heartbeats, executes leased builds and pipeline jobs until ctx ends.
func (w *Worker) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	if w.has("build") {
		wg.Add(1)
		go func() { defer wg.Done(); w.buildLoop(ctx) }()
	}
	r := &queue.Runner{Pool: w.DB.Pool, WorkerID: w.Cfg.HostID, Concurrency: 1, Lease: 10 * time.Minute, Log: w.Log, OnDead: w.deadJob}
	n := 0
	if w.has("build") {
		r.Handle(JobScan, w.jobScan)
		n++
	}
	if w.has("test") {
		r.Handle(JobTest, w.jobTest)
		n++
	}
	if w.has("finalize") {
		r.Handle(JobFinalize, w.jobFinalize)
		r.Handle(JobPublish, w.jobPublish)
		n++
	}
	if n > 0 {
		wg.Add(1)
		go func() { defer wg.Done(); r.Run(ctx) }()
	}
	wg.Wait()
	return ctx.Err()
}

func (w *Worker) heartbeat(ctx context.Context) {
	err := w.DB.UpsertHost(ctx, store.Host{ID: w.Cfg.HostID, Pool: "build", Arch: w.Cfg.Arch, Slots: w.Cfg.Slots, Version: w.Cfg.Version})
	if err != nil && ctx.Err() == nil {
		w.Log.Error("heartbeat", "err", err)
	}
}

func (w *Worker) buildLoop(ctx context.Context) {
	wake := pgnotify.Wake(ctx, w.DB.Pool, "foundry_lease")
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		w.heartbeat(ctx)
		leased, err := w.DB.LeasedTo(ctx, w.Cfg.HostID)
		if err != nil && ctx.Err() == nil {
			w.Log.Error("leased builds", "err", err)
		}
		for _, b := range leased {
			w.mu.Lock()
			_, busy := w.running[b.ID]
			if !busy {
				bctx, cancel := context.WithCancel(ctx)
				w.running[b.ID] = cancel
				wg.Add(1)
				go func(b store.Build) {
					defer wg.Done()
					defer func() { w.mu.Lock(); delete(w.running, b.ID); w.mu.Unlock(); cancel() }()
					w.runBuild(bctx, b)
				}(b)
			}
			w.mu.Unlock()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-wake:
		}
	}
}

// infraError marks failures worth retrying on another attempt.
type infraError struct{ err error }

func (e infraError) Error() string { return e.err.Error() }
func (e infraError) Unwrap() error { return e.err }

func (w *Worker) runBuild(ctx context.Context, b store.Build) {
	log := newLogSink(w.DB, b.ID, "build")
	defer log.Close()
	actor := "host:" + w.Cfg.HostID
	if err := w.DB.Transition(ctx, b.ID, store.StateLeased, store.StateBuilding, store.TransitionOpts{Actor: actor}); err != nil {
		w.Log.Warn("start build", "build", b.ID, "err", err)
		return
	}
	w.DB.Notify(ctx, "foundry_build", b.ID)
	bctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var cancelled, lost bool
	var mu sync.Mutex
	go func() {
		t := time.NewTicker(w.Cfg.LeaseTTL / 4)
		defer t.Stop()
		for {
			select {
			case <-bctx.Done():
				return
			case <-t.C:
				c, err := w.DB.RenewLease(bctx, b.ID, w.Cfg.HostID, w.Cfg.LeaseTTL)
				mu.Lock()
				if errors.Is(err, store.ErrNotFound) {
					lost = true
				}
				if c {
					cancelled = true
				}
				mu.Unlock()
				if c || lost {
					cancel()
					return
				}
			}
		}
	}()
	start := time.Now()
	log.Line(fmt.Sprintf("foundry: build %s started on %s", b.ID, w.Cfg.HostID))
	err := w.build(bctx, &b, log)
	mu.Lock()
	wasCancelled, wasLost := cancelled, lost
	mu.Unlock()
	bg := context.WithoutCancel(ctx)
	switch {
	case wasLost:
		w.Log.Warn("lease lost", "build", b.ID)
	case wasCancelled:
		log.Line("foundry: build cancelled")
		w.DB.Transition(bg, b.ID, store.StateBuilding, store.StateCancelled, store.TransitionOpts{Actor: actor})
	case err != nil:
		log.Line("foundry: build failed: " + firstLines(err.Error(), 40))
		var ie infraError
		if errors.As(err, &ie) && b.Attempt+1 < 3 {
			w.DB.Transition(bg, b.ID, store.StateBuilding, store.StateQueued, store.TransitionOpts{Actor: actor, Detail: map[string]string{"retry": err.Error()}})
		} else {
			w.DB.Transition(bg, b.ID, store.StateBuilding, store.StateFailed, store.TransitionOpts{Actor: actor,
				Failure: map[string]any{"stage": "build", "reason": lastLines(err.Error(), 12), "retryable": errors.As(err, &ie)}})
		}
	default:
		log.Line(fmt.Sprintf("foundry: build finished in %s", time.Since(start).Round(time.Second)))
		err := w.DB.Tx(bg, func(tx pgx.Tx) error {
			if err := store.Transition(bg, tx, b.ID, store.StateBuilding, store.StateBuilt, store.TransitionOpts{Actor: actor}); err != nil {
				return err
			}
			return queue.Enqueue(bg, tx, queue.Job{Kind: JobScan, OrgID: b.OrgID, Payload: payload(b.ID), UniqueKey: "scan:" + b.ID})
		})
		if err != nil {
			w.Log.Error("finish build", "build", b.ID, "err", err)
		}
	}
	w.DB.Notify(bg, "foundry_build", b.ID)
}

func payload(buildID string) []byte { return []byte(`{"buildId":"` + buildID + `"}`) }

// blobResolver serves asset refs from the blob store and components from disk.
type blobResolver struct {
	ctx        context.Context
	bs         blob.Store
	components string
}

func (r blobResolver) Asset(d string) (io.ReadCloser, error) {
	rc, _, err := r.bs.Open(r.ctx, d)
	return rc, err
}

func (r blobResolver) Component(name string) (io.ReadCloser, error) {
	if strings.ContainsAny(name, "/\\") || name == "" || name[0] == '.' {
		return nil, fmt.Errorf("invalid component %q", name)
	}
	return os.Open(filepath.Join(r.components, name))
}

func (w *Worker) build(ctx context.Context, b *store.Build, log *logSink) error {
	stage := func(name string, f func() error) error {
		st := store.StageRun{Stage: name, StartedAt: time.Now(), CacheKey: b.TreeDigest, CacheHit: "miss"}
		w.DB.PutStageRun(context.WithoutCancel(ctx), b.ID, st)
		err := f()
		now := time.Now()
		code := 0
		if err != nil {
			code = 1
		}
		st.FinishedAt, st.ExitCode = &now, &code
		w.DB.PutStageRun(context.WithoutCancel(ctx), b.ID, st)
		return err
	}
	var env *envelope.Envelope
	if len(w.Cfg.EnvelopeKeys) > 0 {
		raw, err := w.DB.Envelope(ctx, b.ID)
		if err != nil || len(raw) == 0 {
			return fmt.Errorf("build has no signed job envelope")
		}
		var signed envelope.Signed
		if err := json.Unmarshal(raw, &signed); err != nil {
			return err
		}
		if env, err = envelope.Verify(w.Cfg.EnvelopeKeys, &signed, time.Now()); err != nil {
			return fmt.Errorf("job envelope rejected: %w", err)
		}
		if env.BuildID != b.ID || env.OrgID != b.OrgID || env.TreeDigest != b.TreeDigest {
			return fmt.Errorf("job envelope does not match the build record")
		}
		log.Line("foundry: job envelope verified (key " + signed.KeyID + ")")
	}
	rc, _, err := w.Blobs.Open(ctx, b.TreeDigest)
	if err != nil {
		return infraError{fmt.Errorf("fetch compiled tree: %w", err)}
	}
	tree, err := compiler.ReadTar(rc)
	rc.Close()
	if err != nil {
		return err
	}
	if d := tree.Digest(); d != b.TreeDigest {
		return fmt.Errorf("compiled tree digest mismatch: %s != %s", d, b.TreeDigest)
	}
	if env != nil {
		allowed := map[string]bool{}
		for _, a := range env.Assets {
			allowed[a] = true
		}
		for _, f := range tree.Files {
			if f.AssetRef != "" && !allowed[f.AssetRef] {
				return fmt.Errorf("tree references asset %s that the envelope does not list", f.AssetRef)
			}
		}
	}
	stem, err := builder.ImageNameFromTree(tree)
	if err != nil {
		return err
	}
	sv, err := w.DB.GetSpecVersion(ctx, b.OrgID, b.SpecVersionID)
	if err != nil {
		return infraError{err}
	}
	sb := w.Cfg.Sandbox
	if sb == nil {
		bc := w.Cfg.Builder
		bc.WorkDir = filepath.Join(w.Cfg.WorkDir, "builds")
		sb = &sandbox.Process{Config: bc}
	}
	job := sandbox.Job{BuildID: b.ID, SpecDigest: sv.Digest, Tree: tree, ImageName: stem,
		Resolver: blobResolver{ctx: ctx, bs: w.Blobs, components: w.Cfg.ComponentsDir}, Log: log}
	if w.Cfg.Egress != nil {
		user, pass := w.Cfg.Egress.Credentials(b.ID)
		job.ProxyURL, job.ProxyAddr, job.ProxyCred = w.Cfg.Egress.URL(w.Cfg.EgressAddr, b.ID), w.Cfg.EgressAddr, user+":"+pass
	}
	log.Line("foundry: sandbox " + sb.Name())
	var res *builder.Result
	err = stage("mkosi", func() error {
		var err error
		res, err = sb.Run(ctx, job)
		return err
	})
	if err != nil {
		return err
	}
	defer os.RemoveAll(res.Dir)
	return stage("package", func() error {
		log.Line("foundry: compressing and storing build outputs")
		zst := filepath.Join(res.Dir, stem+".raw.zst")
		if a, ok := res.Artifacts["disk.zst"]; ok { // VM sandbox returns it compressed
			zst = a.Path
		} else {
			if err := imgutil.Zstd(ctx, w.Cfg.Compression, res.Artifacts["disk"].Path, zst); err != nil {
				return infraError{err}
			}
			os.Remove(res.Artifacts["disk"].Path)
		}
		put := func(kind, path string) (string, error) {
			d, n, err := blob.PutFile(ctx, w.Blobs, path)
			if err != nil {
				return "", infraError{err}
			}
			return d, w.DB.PutArtifact(ctx, b.OrgID, store.Artifact{BuildID: b.ID, Kind: kind, Digest: d, Size: n})
		}
		if _, err := put("disk-unsigned.raw.zst", zst); err != nil {
			return err
		}
		if _, err := put("uki-unsigned", res.Artifacts["uki"].Path); err != nil {
			return err
		}
		if a, ok := res.Artifacts["pkgdb"]; ok {
			if _, err := put("pkgdb", a.Path); err != nil {
				return err
			}
		}
		md := ""
		if a, ok := res.Artifacts["manifest-mkosi"]; ok {
			if md, err = put("manifest-mkosi", a.Path); err != nil {
				return err
			}
		}
		if len(res.Roothash) != 64 {
			return errors.New("build produced no usr verity root hash")
		}
		log.Line("foundry: usr root hash " + res.Roothash)
		if err := w.DB.SetBuildResult(ctx, b.ID, res.Roothash, md); err != nil {
			return err
		}
		// Two builds of the same compiled tree must produce the same /usr.
		if other, same, err := w.DB.CompareReproduced(ctx, b.ID); err == nil && same != nil {
			if *same {
				log.Line("foundry: reproducible: usr root hash matches build " + other)
			} else {
				log.Line("foundry: NOT reproducible: usr root hash differs from build " + other)
			}
		}
		return nil
	})
}

func firstLines(s string, n int) string {
	ls := strings.Split(s, "\n")
	if len(ls) > n {
		ls = ls[:n]
	}
	return strings.Join(ls, "\n")
}

func lastLines(s string, n int) string {
	ls := strings.Split(strings.TrimSpace(s), "\n")
	if len(ls) > n {
		ls = ls[len(ls)-n:]
	}
	return strings.Join(ls, "\n")
}

// deadJob fails the build whose job ran out of attempts, so no build is left
// in an in-progress state without a job.
func (w *Worker) deadJob(ctx context.Context, j *queue.Job, cause error) {
	var p struct {
		BuildID string `json:"buildId"`
	}
	if json.Unmarshal([]byte(j.Payload), &p) != nil || p.BuildID == "" {
		return
	}
	b, err := w.DB.GetBuildAny(ctx, p.BuildID)
	if err != nil || store.Terminal(b.State) || b.State == store.StatePublished || b.State == store.StateAwaitingReview {
		return
	}
	stage := strings.TrimPrefix(j.Kind, "build.")
	reason := cause.Error()
	if len(reason) > 500 {
		reason = reason[:500] + "…"
	}
	if err := w.DB.Transition(ctx, b.ID, b.State, store.StateFailed, store.TransitionOpts{Actor: "host:" + w.Cfg.HostID,
		Failure: map[string]any{"stage": stage, "reason": reason, "retryable": true}}); err == nil {
		w.DB.AppendLogs(ctx, b.ID, "stderr", []string{"foundry: " + stage + " failed after " + fmt.Sprint(j.Attempt) + " attempts: " + reason})
		w.DB.Notify(ctx, "foundry_build", b.ID)
	}
}
