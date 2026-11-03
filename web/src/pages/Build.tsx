import { useEffect, useRef, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { api, ApiError, currentOrg, orgPath, type Artifact, type Build, type EgressEvent, type Finding, type Review } from "../lib/api";
import { PIPELINE, LABEL, stageIndex, TERMINAL } from "../lib/states";
import { ago, fmtBytes, useSession } from "../lib/session";
import { Confirm, Risk, StateBadge, TierBadge } from "../components/ui";
import LivePreview from "../components/LivePreview";

interface LogLine { seq: number; stream: string; line: string }
interface Security { build: Build; findings: Finding[]; egress: EgressEvent[]; reviews: Review[]; reviewersRequired: number; scans: { scanner: string; status: string }[] }

export default function BuildPage() {
  const { build = "" } = useParams();
  const [b, setB] = useState<Build>();
  const [lines, setLines] = useState<LogLine[]>([]);
  const [arts, setArts] = useState<{ items: Artifact[]; downloadable: boolean }>();
  const [sec, setSec] = useState<Security>();
  const [tab, setTab] = useState<"logs" | "downloads" | "security" | "preview">("logs");
  const [err, setErr] = useState<string>();
  const [askCancel, setAskCancel] = useState(false);
  const logRef = useRef<HTMLDivElement>(null);
  const stick = useRef(true);

  const load = () => {
    api<{ build: Build }>("GET", orgPath(`/builds/${build}`)).then((r) => setB(r.build), (e) => setErr(e.message));
    api<{ items: Artifact[]; downloadable: boolean }>("GET", orgPath(`/builds/${build}/artifacts`)).then(setArts, () => undefined);
    api<Security>("GET", orgPath(`/builds/${build}/security`)).then(setSec, () => undefined);
  };
  useEffect(load, [build]);

  // Live logs over SSE; EventSource resumes with Last-Event-ID after drops.
  useEffect(() => {
    setLines([]);
    const es = new EventSource(orgPath(`/builds/${build}/logs`), { withCredentials: true });
    const buf: LogLine[] = [];
    let raf = 0;
    const flush = () => {
      raf = 0;
      const add = buf.splice(0);
      setLines((l) => (l.length + add.length > 20000 ? [...l, ...add].slice(-20000) : [...l, ...add]));
    };
    es.addEventListener("log", (e) => {
      buf.push(JSON.parse((e as MessageEvent).data));
      if (!raf) raf = requestAnimationFrame(flush);
    });
    es.addEventListener("state", (e) => {
      const s = JSON.parse((e as MessageEvent).data);
      setB((x) => (x ? { ...x, state: s.state, tier: s.tier, riskScore: s.risk } : x));
      load();
    });
    es.addEventListener("end", () => {
      es.close();
      load();
    });
    return () => {
      es.close();
      cancelAnimationFrame(raf);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [build]);

  useEffect(() => {
    const el = logRef.current;
    if (el && stick.current) el.scrollTop = el.scrollHeight;
  }, [lines]);

  async function cancel() {
    setAskCancel(false);
    try {
      await api("POST", orgPath(`/builds/${build}/cancel`));
      load();
    } catch (e) {
      setErr((e as ApiError).message);
    }
  }

  if (err && !b) return <p className="err">{err}</p>;
  if (!b) return <p className="muted">Loading…</p>;
  const idx = stageIndex(b.state);
  const failed = b.state === "failed" || b.state === "cancelled" || b.state === "rejected";
  const blockers = (sec?.findings ?? []).filter((f) => f.severity === "blocker" && !f.acceptedBy).length;
  return (
    <div className="stack" style={{ gap: 16 }}>
      <div className="pagehead">
        <div>
          <div className="small muted"><Link to={`/o/${currentOrg()}/p/${b.projectId}`}>Project</Link> / Build</div>
          <h1 className="mono" style={{ fontSize: 18 }}>{b.id}</h1>
          <div className="row" style={{ gap: 6 }}>
            <StateBadge state={b.state} /><TierBadge tier={b.tier} /><Risk score={b.riskScore} />
            <span className="muted small">started {ago(b.createdAt)} · {b.arch}{b.reproduced != null && ` · ${b.reproduced ? "reproducible ✓" : "not reproducible"}`}</span>
          </div>
        </div>
        {!TERMINAL.has(b.state) && b.state !== "awaiting_review" && <button className="danger" onClick={() => setAskCancel(true)}>Cancel build</button>}
      </div>
      <div className="timeline" aria-label="Progress">
        {PIPELINE.map((s, i) => (
          <span key={s} className={`st ${i < idx || b.state === "published" ? "done" : i === idx ? (failed ? "fail" : "cur") : ""}`}>{LABEL[s]}</span>
        ))}
      </div>
      {b.failure && (
        <div className="issue">
          <strong>{b.failure.stage ? `Failed during ${b.failure.stage}` : "Build failed"}</strong>
          <span>{b.failure.message ?? JSON.stringify(b.failure)}</span>
        </div>
      )}
      {b.state === "awaiting_review" && (
        <div className="issue" style={{ background: "var(--warn-soft)", borderColor: "var(--warn)" }}>
          <strong>Waiting for security review</strong>
          <span className="small">{blockers > 0 ? `${blockers} blocking finding(s) must be fixed or accepted.` : `${sec?.reviewersRequired ?? 1} approval(s) needed.`} <button className="ghost small" onClick={() => setTab("security")}>Open review</button></span>
        </div>
      )}
      <div className="seg" role="tablist">
        {(["logs", "downloads", "security", ...(b.state === "published" || b.state === "signed" ? (["preview"] as const) : [])] as const).map((t) => (
          <button key={t} role="tab" aria-selected={tab === t} aria-pressed={tab === t} onClick={() => setTab(t)}>
            {t === "logs" ? "Logs" : t === "downloads" ? "Downloads" : t === "preview" ? "Live preview" : `Security${sec?.findings.length ? ` (${sec.findings.length})` : ""}`}
          </button>
        ))}
      </div>
      {tab === "logs" && (
        <div ref={logRef} className="log" role="log" aria-label="Build log" onScroll={(e) => {
          const el = e.currentTarget;
          stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40;
        }}>
          {lines.length === 0 && <span className="n">Waiting for output…</span>}
          {lines.map((l) => (
            <div key={l.seq} className={l.stream === "stderr" ? "stderr" : ""}><span className="n">{l.seq}</span>{l.line}</div>
          ))}
        </div>
      )}
      {tab === "downloads" && <Downloads arts={arts} />}
      {tab === "security" && sec && <SecurityPanel sec={sec} reload={load} />}
      {tab === "preview" && <LivePreview buildId={b.id} />}
      <Confirm open={askCancel} title="Cancel this build?" confirm="Cancel build" danger onCancel={() => setAskCancel(false)} onConfirm={() => void cancel()}>
        <p>The build machine is stopped and nothing is published.</p>
      </Confirm>
    </div>
  );
}

const KIND_LABEL: Record<string, string> = {
  "img.xz": "Disk image for USB / internal disk", qcow2: "QEMU / KVM image", vmdk: "VMware disk", ova: "VirtualBox appliance", iso: "Installer ISO",
  "disk.raw.zst": "Raw disk image (zstd)", uki: "Unified kernel image", sha256sums: "Checksums", "sha256sums.sig": "Checksum signature",
  sbom: "Software bill of materials (SPDX)", provenance: "Build provenance (SLSA)", "scan-report": "Security scan report", "boot-log": "Boot test log",
  "manifest-mkosi": "Package list", "pubkey-artifact": "Artifact signing key", "pubkey-db": "Secure Boot certificate", sources: "Source archive (GPL)",
  "usr.zst": "Update payload", "usr-verity": "Update verity data", "usr-verity-sig": "Update verity signature", cidx: "Chunk index",
};

function Downloads({ arts }: { arts?: { items: Artifact[]; downloadable: boolean } }) {
  if (!arts) return <p className="muted">Loading…</p>;
  const images = arts.items.filter((a) => ["img.xz", "qcow2", "vmdk", "ova", "iso", "disk.raw.zst"].includes(a.kind));
  const rest = arts.items.filter((a) => !images.includes(a));
  return (
    <div className="stack">
      {!arts.downloadable && <p className="muted">Images can be downloaded once the build is signed. Reports are available now.</p>}
      <div className="card flush">
        <table>
          <thead><tr><th>File</th><th>Size</th><th>SHA-256</th><th></th></tr></thead>
          <tbody>
            {[...images, ...rest].map((a) => (
              <tr key={a.kind + a.digest}>
                <td><div>{KIND_LABEL[a.kind] ?? a.kind}</div><code className="muted small">{a.name}</code></td>
                <td>{fmtBytes(a.size)}</td>
                <td><code className="small" title={a.digest}>{a.digest.slice(7, 23)}…</code> {a.signed && <span className="badge ok">signed</span>}</td>
                <td>{a.url ? <a className="btn" href={a.url} download={a.name}>Download</a> : <span className="muted small">after signing</span>}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {arts.downloadable && images.length > 0 && (
        <div className="card stack small">
          <h3>Install</h3>
          <p><strong>Windows / macOS:</strong> write the <code>.img.xz</code> to a USB stick with balenaEtcher or Rufus (DD mode), then boot from it.</p>
          <p><strong>Linux:</strong> <code>xzcat {images.find((i) => i.kind === "img.xz")?.name ?? "image.img.xz"} | sudo dd of=/dev/sdX bs=4M conv=fsync status=progress</code></p>
          <p><strong>Verify:</strong> <code>sha256sum -c *.SHA256SUMS</code> and <code>openssl pkeyutl -verify -pubin -inkey *.artifact.pub.pem -rawin -in *.SHA256SUMS -sigfile *.SHA256SUMS.sig</code></p>
          <p><strong>Secure Boot:</strong> enroll the <code>.db.crt</code> certificate in your firmware, or use the installer, which enrolls it for you.</p>
        </div>
      )}
    </div>
  );
}

function SecurityPanel({ sec, reload }: { sec: Security; reload: () => void }) {
  const { user } = useSession();
  const [comment, setComment] = useState("");
  const [err, setErr] = useState<string>();
  const approvals = sec.reviews.filter((r) => r.decision === "approve").length;
  const canReview = sec.build.state === "awaiting_review";
  async function decide(decision: string) {
    setErr(undefined);
    try {
      await api("POST", orgPath(`/builds/${sec.build.id}/reviews`), { decision, comment });
      setComment("");
      reload();
    } catch (e) {
      setErr((e as ApiError).message);
    }
  }
  const [accepting, setAccepting] = useState<Finding>();
  const [reason, setReason] = useState("");
  async function accept() {
    const f = accepting!;
    try {
      await api("POST", orgPath(`/builds/${sec.build.id}/findings/${f.id}/accept`), { reason });
      setAccepting(undefined);
      setReason("");
      reload();
    } catch (e) {
      setErr((e as ApiError).message);
    }
  }
  const sevTone = (s: string) => (s === "blocker" || s === "high" ? "bad" : s === "medium" ? "warn" : "");
  const denied = sec.egress.filter((e) => !e.allowed);
  return (
    <div className="stack">
      <div className="grid2">
        <div className="card">
          <h3>Scans</h3>
          {sec.scans.length === 0 ? <p className="muted">Not scanned yet.</p> : sec.scans.map((s) => <div key={s.scanner} className="row spread"><span>{s.scanner}</span><span className={`badge ${s.status === "ok" ? "ok" : "warn"}`}>{s.status}</span></div>)}
        </div>
        <div className="card">
          <h3>Network during build</h3>
          {denied.length === 0 ? <p className="muted">No blocked connections.</p> : (
            <ul className="small">{denied.map((e, i) => <li key={i}><code>{e.method} {e.host}:{e.port}</code> — {e.reason}</li>)}</ul>
          )}
        </div>
      </div>
      <div className="card flush">
        <table>
          <thead><tr><th>Finding</th><th>Severity</th><th>Where</th><th></th></tr></thead>
          <tbody>
            {sec.findings.length === 0 && <tr><td colSpan={4} className="muted">No findings.</td></tr>}
            {sec.findings.map((f) => (
              <tr key={f.id}>
                <td><code>{f.rule}</code><div className="small muted">{typeof f.detail === "string" ? f.detail : JSON.stringify(f.detail)}</div></td>
                <td><span className={`badge ${sevTone(f.severity)}`}>{f.severity}</span></td>
                <td><code className="small">{f.path}</code></td>
                <td>{f.acceptedBy ? <span className="badge ok" title={f.acceptedReason ?? ""}>accepted</span> : canReview && <button className="small" onClick={() => setAccepting(f)}>Accept risk</button>}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <div className="card stack">
        <h3>Review</h3>
        <p className="small">{approvals} of {sec.reviewersRequired} approval(s). {sec.reviewersRequired > 1 && "High-risk builds need two different reviewers."} The person who started the build cannot approve it.</p>
        {sec.reviews.map((r) => <div key={r.id} className="small"><span className={`badge ${r.decision === "approve" ? "ok" : "bad"}`}>{r.decision}</span> {r.reviewer === user?.id ? "you" : r.reviewer} · {ago(r.createdAt)} {r.comment && `— ${r.comment}`}</div>)}
        {canReview && (
          <>
            <label className="field">Comment<textarea rows={2} value={comment} onChange={(e) => setComment(e.target.value)} style={{ fontFamily: "inherit" }} /></label>
            <div className="row">
              <button className="primary" onClick={() => void decide("approve")}>Approve</button>
              <button onClick={() => void decide("changes_requested")}>Request changes</button>
              <button className="danger" onClick={() => void decide("reject")}>Reject</button>
            </div>
          </>
        )}
        {err && <p className="err" role="alert">{err}</p>}
      </div>
      <Confirm open={!!accepting} title={`Accept ${accepting?.rule ?? ""}?`} confirm="Accept risk" onCancel={() => setAccepting(undefined)} onConfirm={() => reason.trim() && void accept()}>
        <p className="small">Accepting a finding is recorded in the audit log with your name and reason.</p>
        <label className="field">Reason<textarea rows={3} value={reason} onChange={(e) => setReason(e.target.value)} style={{ fontFamily: "inherit" }} /></label>
      </Confirm>
    </div>
  );
}
