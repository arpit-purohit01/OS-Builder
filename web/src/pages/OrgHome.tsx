import { useState, type FormEvent } from "react";
import { Link, useNavigate } from "react-router-dom";
import { api, ApiError, orgPath, currentOrg, type Build, type Project } from "../lib/api";
import { ago, useAsync } from "../lib/session";
import { Empty, ErrorNote, Risk, StateBadge, TierBadge } from "../components/ui";
import { TEMPLATES } from "../builder/templates";

export default function OrgHome({ buildsOnly }: { buildsOnly?: boolean }) {
  const projects = useAsync(() => api<{ items: Project[] }>("GET", orgPath("/projects")), []);
  const builds = useAsync(() => api<{ items: Build[] }>("GET", orgPath("/builds?limit=50")), []);
  const [creating, setCreating] = useState(false);
  const pname = (id: string) => projects.data?.items.find((p) => p.id === id)?.name ?? id;
  return (
    <div className="stack" style={{ gap: 24 }}>
      {!buildsOnly && (
        <>
          <div className="pagehead">
            <div>
              <h1>Projects</h1>
              <p className="muted">Each project is one operating system you build and ship.</p>
            </div>
            <button className="primary" onClick={() => setCreating(true)}>New OS</button>
          </div>
          {creating && <NewProject onClose={() => setCreating(false)} />}
          <ErrorNote error={projects.error} />
          <div className="grid2">
            {projects.data?.items.map((p) => (
              <Link key={p.id} to={`/o/${currentOrg()}/p/${p.id}`} className="card" style={{ color: "inherit", textDecoration: "none" }}>
                <h2 style={{ marginBottom: 4 }}>{p.name}</h2>
                <div className="muted small mono">{p.osId}</div>
                <div className="muted small">Created {ago(p.createdAt)}</div>
              </Link>
            ))}
          </div>
          {projects.data?.items.length === 0 && !creating && (
            <div className="card">
              <Empty>
                <h2>Create your first operating system</h2>
                <p>Start from a template and customise everything in the builder.</p>
                <button className="primary" onClick={() => setCreating(true)}>New OS</button>
              </Empty>
            </div>
          )}
        </>
      )}
      <div>
        <h2>{buildsOnly ? <span style={{ fontSize: 22 }}>Builds</span> : "Recent builds"}</h2>
        <BuildTable builds={builds.data?.items ?? []} projectName={pname} />
        <ErrorNote error={builds.error} />
      </div>
    </div>
  );
}

export function BuildTable({ builds, projectName }: { builds: Build[]; projectName?: (id: string) => string }) {
  const nav = useNavigate();
  if (builds.length === 0) return <div className="card"><Empty>No builds yet.</Empty></div>;
  return (
    <div className="card flush">
      <table>
        <thead>
          <tr><th>Build</th>{projectName && <th>Project</th>}<th>State</th><th>Trust</th><th>Started</th></tr>
        </thead>
        <tbody>
          {builds.map((b) => (
            <tr key={b.id} className="link" onClick={() => nav(`/o/${currentOrg()}/b/${b.id}`)}>
              <td><Link to={`/o/${currentOrg()}/b/${b.id}`} className="mono">{b.id}</Link></td>
              {projectName && <td>{projectName(b.projectId)}</td>}
              <td><StateBadge state={b.state} /></td>
              <td className="row" style={{ gap: 6 }}><TierBadge tier={b.tier} /><Risk score={b.riskScore} /></td>
              <td className="muted">{ago(b.createdAt)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function NewProject({ onClose }: { onClose: () => void }) {
  const nav = useNavigate();
  const [name, setName] = useState("");
  const [tpl, setTpl] = useState(TEMPLATES[0].id);
  const [err, setErr] = useState<string>();
  const osId = name.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^[^a-z]+|-$/g, "").slice(0, 40);
  async function submit(e: FormEvent) {
    e.preventDefault();
    try {
      const p = await api<Project>("POST", orgPath("/projects"), { name, osId });
      nav(`/o/${currentOrg()}/p/${p.id}/edit?template=${tpl}`);
    } catch (x) {
      setErr((x as ApiError).message);
    }
  }
  return (
    <form className="card stack" onSubmit={submit}>
      <h2>New operating system</h2>
      <label className="field">
        Name
        <input required autoFocus value={name} onChange={(e) => setName(e.target.value)} placeholder="Aurora OS" maxLength={60} />
        <span className="hint">OS id: <code>{osId || "…"}</code> (used in file names and on devices; cannot change later)</span>
      </label>
      <fieldset className="stack" style={{ border: 0, padding: 0, margin: 0 }}>
        <legend style={{ fontWeight: 500, marginBottom: 6 }}>Start from</legend>
        <div className="choices">
          {TEMPLATES.map((t) => (
            <button type="button" key={t.id} className="choice" aria-pressed={tpl === t.id} onClick={() => setTpl(t.id)}>
              <span className="t">{t.label}</span>
              <span className="d">{t.description}</span>
            </button>
          ))}
        </div>
      </fieldset>
      <div className="row">
        <button className="primary" disabled={osId.length < 3}>Create and open builder</button>
        <button type="button" className="ghost" onClick={onClose}>Cancel</button>
      </div>
      {err && <p className="err" role="alert">{err}</p>}
    </form>
  );
}
