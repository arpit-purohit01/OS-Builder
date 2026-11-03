import { Link, useNavigate, useParams } from "react-router-dom";
import { useState } from "react";
import { api, ApiError, currentOrg, orgPath, type Build, type Project, type SpecVersion } from "../lib/api";
import { ago, useAsync } from "../lib/session";
import { BuildTable } from "./OrgHome";
import { Empty, ErrorNote } from "../components/ui";

export default function ProjectPage() {
  const { project = "" } = useParams();
  const p = useAsync(() => api<{ project: Project }>("GET", orgPath(`/projects/${project}`)), [project]);
  const specs = useAsync(() => api<{ items: SpecVersion[] }>("GET", orgPath(`/projects/${project}/specs?limit=50`)), [project]);
  const builds = useAsync(() => api<{ items: Build[] }>("GET", orgPath(`/builds?limit=100`)), [project]);
  const mine = (builds.data?.items ?? []).filter((b) => b.projectId === project);
  const nav = useNavigate();
  const [err, setErr] = useState<string>();
  async function buildVersion(id: string) {
    setErr(undefined);
    try {
      const b = await api<Build>("POST", orgPath("/builds"), { specVersionId: id }, { "Idempotency-Key": crypto.randomUUID() });
      nav(`/o/${currentOrg()}/b/${b.id}`);
    } catch (e) {
      setErr((e as ApiError).message);
    }
  }
  return (
    <div className="stack" style={{ gap: 24 }}>
      <div className="pagehead">
        <div>
          <div className="small muted"><Link to={`/o/${currentOrg()}`}>Projects</Link></div>
          <h1>{p.data?.project.name ?? "…"}</h1>
          <code className="muted">{p.data?.project.osId}</code>
        </div>
        <Link className="btn primary" to={`/o/${currentOrg()}/p/${project}/edit`}>Open builder</Link>
      </div>
      <ErrorNote error={p.error} />
      {err && <p className="err" role="alert">{err}</p>}
      <div className="grid2" style={{ alignItems: "start" }}>
        <section>
          <h2>Builds</h2>
          <BuildTable builds={mine} />
        </section>
        <section>
          <h2>Versions</h2>
          <div className="card flush">
            {(specs.data?.items ?? []).length === 0 ? <Empty>No saved versions yet. Open the builder to create one.</Empty> : (
              <table>
                <thead><tr><th>Version</th><th>Digest</th><th>Saved</th><th></th></tr></thead>
                <tbody>
                  {specs.data!.items.map((s) => {
                    const meta = (s.spec as { metadata?: { version?: string } }).metadata;
                    return (
                      <tr key={s.id}>
                        <td>{meta?.version} {s.hasExpert && <span className="badge warn">expert</span>}</td>
                        <td><code title={s.digest}>{s.digest.slice(7, 19)}</code></td>
                        <td className="muted">{ago(s.createdAt)}</td>
                        <td className="row" style={{ gap: 4 }}>
                          <Link className="btn small" to={`/o/${currentOrg()}/p/${project}/edit?version=${s.id}`}>Open</Link>
                          <button className="small" onClick={() => void buildVersion(s.id)}>Build</button>
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            )}
          </div>
        </section>
      </div>
    </div>
  );
}
