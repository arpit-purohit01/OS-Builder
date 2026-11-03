import { useEffect, useMemo, useRef, useState } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { api, ApiError, currentOrg, orgPath, type Build, type Project, type SpecVersion } from "../lib/api";
import { diff } from "../lib/jsonpatch";
import { useSession } from "../lib/session";
import { SpecStore, useSpec, type Spec } from "../store/spec";
import { STEPS, stepFor } from "./steps";
import { type Ctx, type Level } from "./fields";
import { useCatalog, useValidation } from "./validation";
import { instantiate } from "./templates";
import { get } from "../lib/pointer";

const FORMATS: [string, string][] = [["img.xz", "USB / disk image (.img.xz)"], ["qcow2", "QEMU / KVM (.qcow2)"], ["vmdk", "VMware (.vmdk)"], ["ova", "VirtualBox / OVA"], ["iso", "Installer DVD / ISO"]];

export default function BuilderPage() {
  const { project = "", step = "foundation" } = useParams();
  const [q] = useSearchParams();
  const [state, setState] = useState<{ project: Project; latest?: SpecVersion; store: SpecStore }>();
  const [err, setErr] = useState<string>();
  useEffect(() => {
    api<{ project: Project; latestSpec?: SpecVersion }>("GET", orgPath(`/projects/${project}`))
      .then(async (r) => {
        const v = q.get("version");
        const base = v ? await api<SpecVersion>("GET", orgPath(`/specs/${v}`)) : r.latestSpec;
        const doc: Spec = base ? (base.spec as Spec) : instantiate(q.get("template") ?? "office", r.project.name, r.project.osId);
        const store = new SpecStore(doc);
        if (!base) store.saved = {};
        setState({ project: r.project, latest: base, store });
      })
      .catch((e) => setErr(e.message));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [project]);
  if (err) return <p className="err">{err}</p>;
  if (!state) return <p className="muted">Loading…</p>;
  return <Builder key={state.project.id} project={state.project} latest={state.latest} store={state.store} step={step} />;
}

function Builder({ project, latest, store, step }: { project: Project; latest?: SpecVersion; store: SpecStore; step: string }) {
  const nav = useNavigate();
  const { memberships } = useSession();
  const expertAllowed = memberships.find((m) => m.orgId === currentOrg())?.org.expertAllowed ?? false;
  const spec = useSpec(store);
  const [level, setLevel] = useState<Level>(() => (get(spec, "/expert") ? "expert" : "simple"));
  const base = (get(spec, "/foundation/base") as string) ?? "ubuntu-24.04";
  const arch = ((get(spec, "/foundation/arch") as string[]) ?? ["x86_64"])[0];
  const { cat, formula } = useCatalog(base, arch);
  const v = useValidation(spec, cat, formula);
  const [server, setServer] = useState<{ errors: { pointer?: string; message: string; code: string }[]; warnings: { pointer?: string; message: string }[]; layer?: string }>({ errors: [], warnings: [] });
  const [parent, setParent] = useState<SpecVersion | undefined>(latest);
  const [busy, setBusy] = useState("");
  const [toast, setToast] = useState<string>();
  const heading = useRef<HTMLHeadingElement>(null);

  useEffect(() => heading.current?.focus(), [step]);
  useEffect(() => {
    if (!toast) return;
    const t = setTimeout(() => setToast(undefined), 3500);
    return () => clearTimeout(t);
  }, [toast]);
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "z" && !(e.target instanceof HTMLInputElement || e.target instanceof HTMLTextAreaElement)) {
        e.preventDefault();
        if (e.shiftKey) store.redo();
        else store.undo();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [store]);
  useEffect(() => {
    const warn = (e: BeforeUnloadEvent) => {
      if (store.dirty) e.preventDefault();
    };
    window.addEventListener("beforeunload", warn);
    return () => window.removeEventListener("beforeunload", warn);
  }, [store]);

  const implied = useMemo(() => new Set(v.implied ?? []), [v.implied]);
  const ctx: Ctx = { store, spec, options: cat?.options ?? [], level, issues: v.issues ?? [], implied, serverErrors: server.errors };
  const steps = STEPS.filter((s) => !s.level || (level === "expert" && (expertAllowed || !!get(spec, "/expert"))));
  const cur = steps.find((s) => s.id === step) ?? (step === "review" ? undefined : steps[0]);
  const go = (id: string) => nav(`/o/${currentOrg()}/p/${project.id}/edit/${id}`);
  const issueCount = (id: string) => (v.issues ?? []).filter((i) => stepFor(i.pointer) === id).length + server.errors.filter((e) => e.pointer && stepFor(e.pointer) === id).length;

  async function save(): Promise<SpecVersion | undefined> {
    setBusy("save");
    try {
      const body = parent ? { parent: parent.id, patch: diff(parent.spec, spec) } : { spec };
      const r = await api<{ specVersion: SpecVersion; created: boolean; warnings: { pointer?: string; message: string }[] }>("POST", orgPath(`/projects/${project.id}/specs`), body);
      setServer({ errors: [], warnings: r.warnings ?? [] });
      setParent(r.specVersion);
      store.saved = r.specVersion.spec as Spec;
      store.reset(r.specVersion.spec as Spec);
      setToast(r.created ? "Saved a new version." : "No changes since the last version.");
      return r.specVersion;
    } catch (e) {
      const x = e as ApiError;
      setServer({ errors: x.errors.length ? x.errors.map((f) => ({ ...f })) : [{ code: "ERR", message: x.message }], warnings: [], layer: x.layer });
      const first = x.errors.find((f) => f.pointer);
      if (first?.pointer) go(stepFor(first.pointer));
      return undefined;
    } finally {
      setBusy("");
    }
  }

  async function build(formats: string[]) {
    const sv = store.dirty || !parent ? await save() : parent;
    if (!sv) return;
    setBusy("build");
    try {
      const b = await api<Build>("POST", orgPath("/builds"), { specVersionId: sv.id, formats }, { "Idempotency-Key": crypto.randomUUID() });
      nav(`/o/${currentOrg()}/b/${b.id}`);
    } catch (e) {
      const x = e as ApiError;
      setServer({ errors: x.errors.length ? x.errors : [{ code: "ERR", message: x.message }], warnings: [] });
      setBusy("");
    }
  }

  // One-click fixes come from the worker, already verified to resolve the
  // conflict. Security-reducing values are not offered as one-click fixes.
  const RISKY = new Set(["developer", "off", "none", "relaxed", "allow-all"]);
  const fixes = (v.fixes ?? []).filter((f) => f.remove || !RISKY.has(String(f.value)));
  function applyFix(f: { pointer: string; value?: unknown; remove?: boolean; label: string }) {
    if (f.remove) store.applyPatch([{ op: "remove", path: f.pointer }], f.label);
    else store.set(f.pointer, f.value, f.label);
  }

  const conflicts = (v.issues ?? []).filter((i) => i.code === "CAT-409");
  const otherIssues = (v.issues ?? []).filter((i) => i.code !== "CAT-409");
  const hasExpert = !!get(spec, "/expert") || !!get(spec, "/apps/custom");

  return (
    <div className="stack" style={{ gap: 16 }}>
      <div className="pagehead">
        <div>
          <div className="small muted"><Link to={`/o/${currentOrg()}/p/${project.id}`}>{project.name}</Link> / Builder</div>
          <h1 ref={heading} tabIndex={-1}>{cur?.label ?? "Review & build"}</h1>
        </div>
        <div className="toolbar">
          <div className="seg" role="group" aria-label="Detail level">
            {(["simple", "custom", "expert"] as Level[]).map((l) => (
              <button key={l} aria-pressed={level === l} onClick={() => setLevel(l)}>
                {l[0].toUpperCase() + l.slice(1)}
              </button>
            ))}
          </div>
          <button onClick={() => store.undo()} disabled={!store.canUndo} aria-label="Undo" title="Undo (Ctrl+Z)">↶ Undo</button>
          <button onClick={() => store.redo()} disabled={!store.canRedo} aria-label="Redo" title="Redo (Ctrl+Shift+Z)">↷ Redo</button>
          <button onClick={() => void save()} disabled={!!busy || (!store.dirty && !!parent)}>{busy === "save" ? "Saving…" : store.dirty || !parent ? "Save version" : "Saved"}</button>
          <button className="primary" onClick={() => go("review")}>Review & build</button>
        </div>
      </div>

      <div className="builder">
        <nav className="steps" aria-label="Builder steps">
          {steps.map((s, i) => (
            <Link key={s.id} to={`/o/${currentOrg()}/p/${project.id}/edit/${s.id}`} className={cur?.id === s.id ? "active" : ""} aria-current={cur?.id === s.id ? "step" : undefined}>
              <span>{s.label}</span>
              {issueCount(s.id) > 0 ? <span className="badge bad">{issueCount(s.id)}</span> : <span className="n">{i + 1}</span>}
            </Link>
          ))}
          <Link to={`/o/${currentOrg()}/p/${project.id}/edit/review`} className={!cur ? "active" : ""}>
            <span>Review & build</span>
          </Link>
        </nav>

        <div className="card">
          {!cat ? <p className="muted">Loading catalog…</p> : cur ? cur.render(ctx) : (
            <Review spec={spec} parent={parent} store={store} busy={busy} onBuild={build} serverWarnings={server.warnings} hasExpert={hasExpert} valid={v.ok !== false && server.errors.length === 0} />
          )}
          {cur && (
            <div className="row spread" style={{ marginTop: 16, borderTop: "1px solid var(--line)", paddingTop: 16 }}>
              <button className="ghost" disabled={steps[0].id === cur.id} onClick={() => go(steps[steps.indexOf(cur) - 1].id)}>← Back</button>
              <button onClick={() => go(steps.indexOf(cur) + 1 < steps.length ? steps[steps.indexOf(cur) + 1].id : "review")}>Next →</button>
            </div>
          )}
        </div>

        <aside className="aside" aria-label="Validation">
          <div className="card stack" aria-live="polite">
            <div className="row spread">
              <h3 style={{ margin: 0 }}>Check</h3>
              {!v.ready ? <span className="badge">checking…</span> : v.ok ? <span className="badge ok">All choices work together</span> : <span className="badge bad">Needs attention</span>}
            </div>
            {conflicts.length > 0 && (
              <div className="issue">
                <span>{conflicts[0].message}</span>
                <div className="row" style={{ gap: 6 }}>
                  {fixes.map((f) => <button key={f.label} className="small" onClick={() => applyFix(f)}>{f.label}</button>)}
                  {conflicts.map((c) => (
                    <button key={c.pointer} className="ghost small" onClick={() => go(stepFor(c.pointer))}>Show {STEPS.find((s) => s.id === stepFor(c.pointer))?.label}</button>
                  ))}
                </div>
              </div>
            )}
            {otherIssues.map((i) => (
              <div key={i.pointer + i.code} className="issue"><span>{i.message}</span>
                <div><button className="small" onClick={() => go(stepFor(i.pointer))}>Show</button></div>
              </div>
            ))}
            {server.errors.length > 0 && (
              <div className="issue">
                <strong>The server rejected this version{server.layer ? ` (${server.layer})` : ""}</strong>
                {server.errors.map((e, i) => (
                  <button key={i} className="ghost small" style={{ textAlign: "left" }} onClick={() => e.pointer && go(stepFor(e.pointer))}>
                    {e.pointer && <code>{e.pointer}</code>} {e.message}
                  </button>
                ))}
              </div>
            )}
            {implied.size > 0 && (
              <div className="small">
                <div className="muted">Added automatically:</div>
                {[...implied].map((id) => <span key={id} className="badge info" style={{ margin: 2 }}>{cat?.options.find((o) => o.id === id)?.label ?? id}</span>)}
              </div>
            )}
            {v.ms !== undefined && <span className="muted small">checked in {v.ms.toFixed(1)} ms</span>}
          </div>
          {hasExpert && <div className="card small"><span className="badge warn">requires review</span> This OS contains expert content.</div>}
        </aside>
      </div>
      {toast && <div className="toast" role="status" onClick={() => setToast(undefined)}>{toast}</div>}
    </div>
  );
}

function Review({ spec, parent, store, busy, onBuild, serverWarnings, hasExpert, valid }: {
  spec: Spec; parent?: SpecVersion; store: SpecStore; busy: string; onBuild: (f: string[]) => void; serverWarnings: { pointer?: string; message: string }[]; hasExpert: boolean; valid: boolean;
}) {
  const [formats, setFormats] = useState<string[]>(["img.xz", "qcow2"]);
  const [view, setView] = useState<"summary" | "changes" | "json">("summary");
  const changes = parent ? diff(parent.spec, spec) : [];
  const S = (p: string, d = "default") => String(get(spec, p) ?? d);
  return (
    <div className="stack">
      <div className="seg" role="group" aria-label="View">
        {(["summary", "changes", "json"] as const).map((x) => <button key={x} aria-pressed={view === x} onClick={() => setView(x)}>{x === "json" ? "Spec" : x[0].toUpperCase() + x.slice(1)}</button>)}
      </div>
      {view === "summary" && (
        <dl className="kv">
          <dt>Name</dt><dd>{S("/branding/osName", S("/metadata/displayName"))} {S("/metadata/version")}</dd>
          <dt>Device type</dt><dd>{S("/foundation/profile")}</dd>
          <dt>Base</dt><dd>{S("/foundation/base", "ubuntu-24.04")}</dd>
          <dt>Security</dt><dd>{S("/security/level", "balanced")} · disk {S("/security/diskEncryption", "tpm2")} · Secure Boot {S("/security/secureBoot", "tenant-keys")}</dd>
          <dt>Admin</dt><dd>{S("/access/rootModel")}</dd>
          <dt>Apps</dt><dd>{((get(spec, "/apps/preinstalled") as string[]) ?? []).map((a) => a.replace(/^flatpak:/, "")).join(", ") || "none"}</dd>
          <dt>Fleet</dt><dd>{get(spec, "/ecosystem/fleet/enroll") ? `managed (${S("/ecosystem/fleet/group", "default group")})` : "not managed"}</dd>
        </dl>
      )}
      {view === "changes" && (
        changes.length === 0 ? <p className="muted">No changes since the last saved version.</p> : (
          <table><tbody>{changes.map((c, i) => <tr key={i}><td><span className="badge">{c.op}</span></td><td><code>{c.path}</code></td><td><code>{"value" in c ? JSON.stringify(c.value) : ""}</code></td></tr>)}</tbody></table>
        )
      )}
      {view === "json" && <pre className="log" style={{ height: 360, background: "var(--panel-2)", color: "var(--text)" }}>{JSON.stringify(spec, null, 2)}</pre>}
      {serverWarnings.length > 0 && <div className="small">{serverWarnings.map((w, i) => <p key={i} className="badge warn">{w.message}</p>)}</div>}
      <fieldset className="stack" style={{ border: 0, padding: 0 }}>
        <legend style={{ fontWeight: 600 }}>Download formats</legend>
        <div className="choices">
          {FORMATS.map(([f, l]) => (
            <button key={f} type="button" role="checkbox" aria-checked={formats.includes(f)} className="choice" onClick={() => setFormats((x) => (x.includes(f) ? x.filter((y) => y !== f) : [...x, f]))}>
              <span className="t">{l}</span>
            </button>
          ))}
        </div>
      </fieldset>
      {hasExpert && <p className="small"><span className="badge warn">requires review</span> A reviewer must approve this build before it is signed.</p>}
      <div className="row">
        <button className="primary" disabled={!valid || !!busy} onClick={() => onBuild(formats)}>{busy === "build" ? "Starting…" : store.dirty || !parent ? "Save and build" : "Build"}</button>
        {!valid && <span className="err">Fix the problems listed on the right first.</span>}
      </div>
    </div>
  );
}
