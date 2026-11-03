import { Link } from "react-router-dom";
import { api, currentOrg, orgPath, type Build } from "../lib/api";
import { ago, useAsync } from "../lib/session";
import { Empty, ErrorNote, Risk, StateBadge } from "../components/ui";

export default function Reviews() {
  const q = useAsync(() => api<{ items: Build[] }>("GET", orgPath("/reviews")), []);
  return (
    <div className="stack">
      <div className="pagehead">
        <div>
          <h1>Security reviews</h1>
          <p className="muted">Builds with expert content or risky findings wait here until a reviewer approves them. Highest risk first.</p>
        </div>
        <button onClick={q.reload}>Refresh</button>
      </div>
      <ErrorNote error={q.error} />
      <div className="card flush">
        {(q.data?.items ?? []).length === 0 ? <Empty>Nothing waiting for review.</Empty> : (
          <table>
            <thead><tr><th>Build</th><th>Risk</th><th>State</th><th>Waiting</th></tr></thead>
            <tbody>
              {[...q.data!.items].sort((a, b) => (b.riskScore ?? 0) - (a.riskScore ?? 0)).map((b) => (
                <tr key={b.id}>
                  <td><Link className="mono" to={`/o/${currentOrg()}/b/${b.id}`}>{b.id}</Link></td>
                  <td><Risk score={b.riskScore} /></td>
                  <td><StateBadge state={b.state} /></td>
                  <td className="muted">{ago(b.finishedAt ?? b.createdAt)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </div>
  );
}
