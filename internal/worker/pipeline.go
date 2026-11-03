package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"foundry.dev/foundry/internal/blob"
	"foundry.dev/foundry/internal/convert"
	"foundry.dev/foundry/internal/finalize"
	"foundry.dev/foundry/internal/imgutil"
	"foundry.dev/foundry/internal/jobs"
	"foundry.dev/foundry/internal/osspec"
	"foundry.dev/foundry/internal/queue"
	"foundry.dev/foundry/internal/scan"
	"foundry.dev/foundry/internal/store"
	"foundry.dev/foundry/internal/tester"
	"foundry.dev/foundry/internal/uki"
	"github.com/jackc/pgx/v5"
)

// Pipeline job kinds.
const (
	JobScan     = jobs.Scan
	JobTest     = jobs.Test
	JobFinalize = jobs.Finalize
	JobPublish  = jobs.Publish
)

type jobPayload struct {
	BuildID string `json:"buildId"`
}

// stageCtx is the common context of a pipeline job.
type stageCtx struct {
	b    *store.Build
	spec *osspec.Spec
	log  *logSink
}

// begin loads the build and moves it from → inProgress. A build already in
// inProgress (a previous attempt crashed) is resumed; any other state means
// the job is stale and is dropped.
func (w *Worker) begin(ctx context.Context, j *queue.Job, stream, from, inProgress string) (*stageCtx, error) {
	var p jobPayload
	if err := json.Unmarshal(j.Payload, &p); err != nil || p.BuildID == "" {
		return nil, queue.Permanent{Err: errors.New("bad payload")}
	}
	b, err := w.DB.GetBuildAny(ctx, p.BuildID)
	if err != nil {
		return nil, err
	}
	switch b.State {
	case from:
		if err := w.DB.Transition(ctx, b.ID, from, inProgress, store.TransitionOpts{Actor: "host:" + w.Cfg.HostID}); err != nil {
			return nil, err
		}
		b.State = inProgress
	case inProgress:
	default:
		return nil, nil
	}
	sv, err := w.DB.GetSpecVersion(ctx, b.OrgID, b.SpecVersionID)
	if err != nil {
		return nil, err
	}
	var spec osspec.Spec
	if err := json.Unmarshal(sv.Spec, &spec); err != nil {
		return nil, queue.Permanent{Err: err}
	}
	w.DB.Notify(ctx, "foundry_build", b.ID)
	return &stageCtx{b: b, spec: &spec, log: newLogSink(w.DB, b.ID, stream)}, nil
}

// fail moves a build to failed with a reason (not retryable at job level).
func (w *Worker) fail(ctx context.Context, sc *stageCtx, stage, reason string) error {
	sc.log.Line("foundry: " + stage + " failed: " + reason)
	err := w.DB.Transition(context.WithoutCancel(ctx), sc.b.ID, sc.b.State, store.StateFailed, store.TransitionOpts{Actor: "host:" + w.Cfg.HostID,
		Failure: map[string]any{"stage": stage, "reason": reason, "retryable": false}})
	w.DB.Notify(context.WithoutCancel(ctx), "foundry_build", sc.b.ID)
	return err
}

func (w *Worker) artifact(ctx context.Context, b *store.Build, kind string) (string, error) {
	as, err := w.DB.Artifacts(ctx, b.OrgID, b.ID)
	if err != nil {
		return "", err
	}
	for _, a := range as {
		if a.Kind == kind {
			return a.Digest, nil
		}
	}
	return "", fmt.Errorf("build %s has no %s artifact", b.ID, kind)
}

func (w *Worker) putArtifact(ctx context.Context, b *store.Build, kind, path string, signed bool) (string, error) {
	d, n, err := blob.PutFile(ctx, w.Blobs, path)
	if err != nil {
		return "", err
	}
	return d, w.DB.PutArtifact(ctx, b.OrgID, store.Artifact{BuildID: b.ID, Kind: kind, Digest: d, Size: n, Signed: signed})
}

func (w *Worker) putBytes(ctx context.Context, b *store.Build, kind string, data []byte, signed bool) (string, error) {
	d, n, err := w.Blobs.Put(ctx, strings.NewReader(string(data)), "", 0)
	if err != nil {
		return "", err
	}
	return d, w.DB.PutArtifact(ctx, b.OrgID, store.Artifact{BuildID: b.ID, Kind: kind, Digest: d, Size: n, Signed: signed})
}

func (w *Worker) readBlob(ctx context.Context, digest string, max int64) ([]byte, error) {
	r, _, err := w.Blobs.Open(ctx, digest)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(io.LimitReader(r, max))
}

func (w *Worker) timed(ctx context.Context, b *store.Build, stage string, f func() error) error {
	st := store.StageRun{Stage: stage, StartedAt: time.Now(), CacheKey: b.TreeDigest, CacheHit: "miss"}
	w.DB.PutStageRun(context.WithoutCancel(ctx), b.ID, st)
	err := f()
	now, code := time.Now(), 0
	if err != nil {
		code = 1
	}
	st.FinishedAt, st.ExitCode = &now, &code
	w.DB.PutStageRun(context.WithoutCancel(ctx), b.ID, st)
	return err
}

// ------------------------------------------------------------------- scan

func (w *Worker) jobScan(ctx context.Context, j *queue.Job) error {
	sc, err := w.begin(ctx, j, "scan", store.StateBuilt, store.StateScanning)
	if err != nil || sc == nil {
		return err
	}
	defer sc.log.Close()
	b, spec := sc.b, sc.spec
	var dec scan.Decision
	err = w.timed(ctx, b, "scan", func() error {
		sc.log.Line("foundry: scanning build " + b.ID)
		md, err := w.artifact(ctx, b, "manifest-mkosi")
		if err != nil {
			return err
		}
		mb, err := w.readBlob(ctx, md, 32<<20)
		if err != nil {
			return err
		}
		pkgs, err := scan.ParseManifest(mb)
		if err != nil {
			return err
		}
		distro, release := "ubuntu", "noble"
		switch spec.Foundation.Base {
		case "ubuntu-26.04":
			release = "resolute"
		case "debian-13":
			distro, release = "debian", "trixie"
		}
		sbom, err := scan.SBOM(scan.SBOMInput{BuildID: b.ID, OSName: spec.Branding.OSName, OSID: spec.Branding.OSID, Version: spec.Metadata.Version,
			Distro: distro, Release: release, Arch: b.Arch, Roothash: b.Roothash, Packages: pkgs, SourceEpoch: b.CreatedAt.Unix()})
		if err != nil {
			return err
		}
		sd, err := w.putBytes(ctx, b, "sbom", sbom, false)
		if err != nil {
			return err
		}
		w.DB.RecordScan(ctx, store.Scan{BuildID: b.ID, Scanner: "sbom", ToolVersion: "foundry-scanner-1", Status: "passed", Report: sd}, nil)
		sc.log.Line(fmt.Sprintf("foundry: SBOM with %d packages", len(pkgs)))
		findings, err := scan.AnalyzeSpec(spec, func(d string) (io.ReadCloser, error) {
			rc, _, err := w.Blobs.Open(ctx, d)
			return rc, err
		})
		if err != nil {
			return err
		}
		// Image analysis: every file of /usr against the package snapshot.
		sdir, done, err := w.scratch(b, "scan")
		if err != nil {
			return err
		}
		imgFindings, err := w.imageScan(ctx, b, spec, sdir, sc.log.Line)
		done()
		if err != nil {
			return err
		}
		findings = append(findings, imgFindings...)
		// Egress outside the allowlist during the build is a hard blocker.
		if denied, err := w.DB.Egress(ctx, b.ID, true); err == nil {
			for _, e := range denied {
				if e.Reason == "unauthenticated" {
					continue
				}
				findings = append(findings, scan.Finding{Rule: "BEH-EGRESS", Severity: scan.SevBlocker, Scanner: "behavior",
					Detail: fmt.Sprintf("build tried to reach %s:%d (%s)", e.Host, e.Port, e.Reason)})
			}
		}
		dec = scan.Score(findings, spec.HasExpert(), nil)
		rep, _ := json.MarshalIndent(dec, "", " ")
		rd, err := w.putBytes(ctx, b, "scan-report", rep, false)
		if err != nil {
			return err
		}
		status := "passed"
		switch {
		case dec.Blocked:
			status = "blocked"
		case len(findings) > 0:
			status = "flagged"
		}
		rows := make([]store.FindingRow, 0, len(findings))
		for _, f := range findings {
			det, _ := json.Marshal(map[string]string{"detail": f.Detail, "feature": f.Feature, "scanner": f.Scanner})
			rows = append(rows, store.FindingRow{Rule: f.Rule, Severity: f.Severity, Path: f.Path, Weight: f.Weight, Detail: det})
		}
		_, err = w.DB.RecordScan(ctx, store.Scan{BuildID: b.ID, Scanner: "diff", ToolVersion: "foundry-scanner-1", Status: status, Report: rd}, rows)
		for _, e := range dec.Explanation {
			sc.log.Line("foundry: risk: " + e)
		}
		sc.log.Line(fmt.Sprintf("foundry: risk score %d, reviewers required %d", dec.Score, dec.Reviewers))
		return err
	})
	if err != nil {
		sc.log.Line("foundry: scan error: " + err.Error())
		return err // retried by the queue
	}
	if dec.Blocked {
		return w.fail(ctx, sc, "scan", strings.Join(dec.Explanation, "; "))
	}
	risk := dec.Score
	return w.DB.Tx(ctx, func(tx pgx.Tx) error {
		if err := store.Transition(ctx, tx, b.ID, store.StateScanning, store.StateScanned, store.TransitionOpts{Actor: "scanner", Risk: &risk,
			Detail: map[string]any{"score": dec.Score, "reviewers": dec.Reviewers}}); err != nil {
			return err
		}
		return queue.Enqueue(ctx, tx, queue.Job{Kind: JobTest, OrgID: b.OrgID, Payload: payload(b.ID), UniqueKey: "test:" + b.ID})
	})
}

// ------------------------------------------------------------------- test

func (w *Worker) scratch(b *store.Build, name string) (string, func(), error) {
	dir := filepath.Join(w.Cfg.WorkDir, "jobs", b.ID+"-"+name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, err
	}
	return dir, func() { os.RemoveAll(dir) }, nil
}

func expectations(spec *osspec.Spec, sc tester.Scenario) tester.Scenario {
	sc.Version = spec.Metadata.Version
	sc.Slot = spec.Branding.OSID + "_" + spec.Metadata.Version
	if spec.Security.DiskEncryption != "none" {
		sc.Crypt = "yes"
		if sc.TPM {
			sc.TPM2 = "yes"
		}
	} else {
		sc.Crypt = "no"
	}
	return sc
}

func (w *Worker) jobTest(ctx context.Context, j *queue.Job) error {
	sc, err := w.begin(ctx, j, "test", store.StateScanned, store.StateTesting)
	if err != nil || sc == nil {
		return err
	}
	defer sc.log.Close()
	b := sc.b
	dir, done, err := w.scratch(b, "test")
	if err != nil {
		return err
	}
	defer done()
	var rep *tester.Report
	err = w.timed(ctx, b, "boot-test", func() error {
		dd, err := w.artifact(ctx, b, "disk-unsigned.raw.zst")
		if err != nil {
			return err
		}
		zst := filepath.Join(dir, "disk.raw.zst")
		if err := fetchTo(ctx, w.Blobs, dd, zst); err != nil {
			return err
		}
		disk := filepath.Join(dir, "disk.raw")
		if err := imgutil.Unzstd(ctx, zst, disk); err != nil {
			return err
		}
		os.Remove(zst)
		tc := w.Cfg.Tester
		tc.WorkDir = dir
		scen := expectations(sc.spec, w.Cfg.TestScenario)
		sc.log.Line(fmt.Sprintf("foundry: boot test %s (Secure Boot %v, TPM %v, reboot %v)", scen.Name, scen.SecureBoot, scen.TPM, scen.Reboot))
		rep, err = tester.Run(ctx, tc, disk, scen, sc.log)
		if err != nil {
			return err
		}
		if _, err := w.putArtifact(ctx, b, "boot-log", rep.Serial, false); err != nil {
			return err
		}
		return w.DB.SetTestReport(ctx, b.ID, []*tester.Report{rep})
	})
	if err != nil {
		sc.log.Line("foundry: boot test error: " + err.Error())
		return err
	}
	if !rep.Passed {
		return w.fail(ctx, sc, "test", strings.Join(rep.Failures, "; "))
	}
	sc.log.Line(fmt.Sprintf("foundry: boot test passed in %.0fs", rep.Seconds))
	return w.decide(ctx, sc)
}

// decide moves tested builds to approved (auto) or awaiting_review.
func (w *Worker) decide(ctx context.Context, sc *stageCtx) error {
	b := sc.b
	var dec scan.Decision
	if rd, err := w.artifact(ctx, b, "scan-report"); err == nil {
		if data, err := w.readBlob(ctx, rd, 8<<20); err == nil {
			json.Unmarshal(data, &dec)
		}
	} else {
		dec.Reviewers = 1 // no scan report: never auto-approve
	}
	return w.DB.Tx(ctx, func(tx pgx.Tx) error {
		if err := store.Transition(ctx, tx, b.ID, store.StateTesting, store.StateTested, store.TransitionOpts{Actor: "tester"}); err != nil {
			return err
		}
		if dec.Reviewers > 0 {
			sc.log.Line(fmt.Sprintf("foundry: awaiting review (%d reviewer(s) required)", dec.Reviewers))
			return store.Transition(ctx, tx, b.ID, store.StateTested, store.StateAwaitingReview, store.TransitionOpts{Actor: "policy",
				Detail: map[string]any{"reviewers": dec.Reviewers, "score": dec.Score}})
		}
		tier := dec.Tier
		if tier == "" {
			tier = "private-unverified"
		}
		sc.log.Line("foundry: approved automatically (tier " + tier + ")")
		if err := store.Transition(ctx, tx, b.ID, store.StateTested, store.StateApproved, store.TransitionOpts{Actor: "policy", Tier: &tier,
			Detail: map[string]any{"auto": true, "score": dec.Score}}); err != nil {
			return err
		}
		return queue.Enqueue(ctx, tx, queue.Job{Kind: JobFinalize, OrgID: b.OrgID, Payload: payload(b.ID), UniqueKey: "finalize:" + b.ID})
	})
}

func fetchTo(ctx context.Context, bs blob.Store, digest, dst string) error {
	r, _, err := bs.Open(ctx, digest)
	if err != nil {
		return err
	}
	defer r.Close()
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// --------------------------------------------------------------- finalize

type platformInfo struct {
	Virtual bool `json:"virtual"`
}

func (w *Worker) jobFinalize(ctx context.Context, j *queue.Job) error {
	sc, err := w.begin(ctx, j, "finalize", store.StateApproved, store.StateFinalizing)
	if err != nil || sc == nil {
		return err
	}
	defer sc.log.Close()
	b, spec := sc.b, sc.spec
	if w.Cfg.Signer == nil {
		return errors.New("no signer configured")
	}
	dir, done, err := w.scratch(b, "finalize")
	if err != nil {
		return err
	}
	defer done()
	var pi platformInfo
	json.Unmarshal(b.Platform, &pi)
	enroll := "manual"
	if pi.Virtual {
		enroll = "force"
	}
	formats, _ := w.DB.BuildFormats(ctx, b.ID)
	if len(formats) == 0 {
		formats = convert.Default(pi.Virtual, !pi.Virtual)
	}
	var conv []string
	for _, f := range formats {
		if f != "raw.zst" && convert.Valid(f) {
			conv = append(conv, f)
		}
	}
	convertFn := func(ctx context.Context, disk, cdir string) (map[string]string, error) {
		if len(conv) == 0 {
			return nil, nil
		}
		sc.log.Line("foundry: converting to " + strings.Join(conv, ", "))
		files, err := convert.Run(ctx, convert.Options{Compression: w.Cfg.Compression, Name: spec.Branding.OSName, Epoch: b.CreatedAt}, disk, cdir, conv)
		var fe convert.FormatError
		if errors.As(err, &fe) {
			// A format that cannot be produced is reported, not fatal: the
			// signed disk and the other formats are still released.
			for f, e := range fe {
				sc.log.Line("foundry: WARNING: could not produce " + f + ": " + e.Error())
			}
			return files, nil
		}
		return files, err
	}
	var out *finalize.Output
	err = w.timed(ctx, b, "finalize", func() error {
		dd, err := w.artifact(ctx, b, "disk-unsigned.raw.zst")
		if err != nil {
			return err
		}
		ud, err := w.artifact(ctx, b, "uki-unsigned")
		if err != nil {
			return err
		}
		out, err = finalize.Run(ctx, finalize.Job{BuildID: b.ID, Base: spec.Foundation.Base, Arch: b.Arch, LabelPrefix: spec.Branding.OSID,
			Version: spec.Metadata.Version, DiskDigest: dd, UKIDigest: ud, Roothash: b.Roothash, EnrollMode: enroll,
			Compression: w.Cfg.Compression, SourceEpoch: b.CreatedAt.Unix()}, finalize.Deps{Blobs: w.Blobs, Signer: w.Cfg.Signer, WorkDir: dir, Log: sc.log,
			Record: func(ctx context.Context, kind, digest string, size int64) error {
				return w.DB.PutArtifact(ctx, b.OrgID, store.Artifact{BuildID: b.ID, Kind: kind, Digest: digest, Size: size})
			}, Convert: convertFn})
		if err != nil {
			return err
		}
		// Signed: produced by the signer, or listed in the signed SHA256SUMS.
		signedKinds := map[string]bool{"uki": true, "disk.raw.zst": true, "usr-verity-sig": true, "sha256sums.sig": true, "usr.zst": true, "usr-verity": true}
		for _, f := range conv {
			signedKinds[f] = true
		}
		for kind, d := range out.Artifacts {
			size := out.Sizes[kind]
			if size == 0 {
				size, _ = w.Blobs.Stat(ctx, d)
			}
			if err := w.DB.PutArtifact(ctx, b.OrgID, store.Artifact{BuildID: b.ID, Kind: kind, Digest: d, Size: size, Signed: signedKinds[kind]}); err != nil {
				return err
			}
		}
		// PCR 11 predictions for remote attestation.
		if p, ok := w.Blobs.Path(out.Artifacts["uki"]); ok {
			if pred, err := uki.PredictPCR11(ctx, "", p); err == nil {
				w.DB.SetPCR11(ctx, b.ID, pred)
				sc.log.Line("foundry: PCR 11 (ready) " + pred["enter-initrd:leave-initrd:sysinit:ready"])
			} else {
				sc.log.Line("foundry: PCR 11 prediction unavailable: " + err.Error())
			}
		}
		return nil
	})
	if err != nil {
		// Integrity problems and signer refusals are final; anything else is retried.
		if errors.Is(err, finalize.ErrIntegrity) || strings.Contains(err.Error(), "refused") {
			return w.fail(ctx, sc, "finalize", err.Error())
		}
		sc.log.Line("foundry: finalize error: " + err.Error())
		return err
	}
	if w.Cfg.VerifySecureBoot {
		var rep *tester.Report
		err = w.timed(ctx, b, "secure-boot-test", func() error {
			tc := w.Cfg.Tester
			tc.WorkDir = dir
			scen := expectations(spec, tester.Scenario{Name: "sb-tpm", SecureBoot: true, TPM: true, SBEnforced: true})
			sc.log.Line("foundry: verifying the signed image boots with Secure Boot enforced")
			var err error
			rep, err = tester.Run(ctx, tc, out.DiskPath, scen, sc.log)
			if err != nil {
				return err
			}
			var prev []*tester.Report
			json.Unmarshal(b.TestReport, &prev)
			w.DB.SetTestReport(ctx, b.ID, append(prev, rep))
			if pcr, ok := rep.Boots[len(rep.Boots)-1].Fields["pcr11"]; ok && len(rep.Boots) > 0 {
				sc.log.Line("foundry: measured PCR 11 " + pcr)
			}
			return nil
		})
		if err != nil {
			return err
		}
		if !rep.Passed {
			return w.fail(ctx, sc, "secure-boot-test", strings.Join(rep.Failures, "; "))
		}
	}
	return w.DB.Tx(ctx, func(tx pgx.Tx) error {
		if err := store.Transition(ctx, tx, b.ID, store.StateFinalizing, store.StateSigned, store.TransitionOpts{Actor: "host:" + w.Cfg.HostID}); err != nil {
			return err
		}
		return queue.Enqueue(ctx, tx, queue.Job{Kind: JobPublish, OrgID: b.OrgID, Payload: payload(b.ID), UniqueKey: "publish:" + b.ID})
	})
}

// ---------------------------------------------------------------- publish

func (w *Worker) jobPublish(ctx context.Context, j *queue.Job) error {
	var p jobPayload
	if err := json.Unmarshal(j.Payload, &p); err != nil {
		return queue.Permanent{Err: err}
	}
	b, err := w.DB.GetBuildAny(ctx, p.BuildID)
	if err != nil {
		return err
	}
	if b.State != store.StateSigned {
		return nil
	}
	sv, err := w.DB.GetSpecVersion(ctx, b.OrgID, b.SpecVersionID)
	if err != nil {
		return err
	}
	var meta struct {
		Metadata struct{ Version string } `json:"metadata"`
	}
	json.Unmarshal(sv.Spec, &meta)
	rel, err := w.DB.Publish(ctx, b, meta.Metadata.Version)
	if err != nil {
		return err
	}
	log := newLogSink(w.DB, b.ID, "publish")
	log.Line("foundry: published release " + rel.ID + " (" + rel.Version + ", tier " + rel.Tier + ")")
	log.Close()
	w.DB.Notify(ctx, "foundry_build", b.ID)
	w.DB.Audit(ctx, b.OrgID, "publisher", "release.publish", rel.ID, map[string]string{"build": b.ID, "version": rel.Version})
	return nil
}
