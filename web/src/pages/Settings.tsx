import { useState, type FormEvent } from "react";
import { api, ApiError, orgPath } from "../lib/api";
import { ago, useAsync, useSession } from "../lib/session";
import { Confirm, ErrorNote } from "../components/ui";
import { currentOrg } from "../lib/api";

interface Member { userId: string; email: string; name: string; role: string }

export default function Settings({ role }: { role: string }) {
  const members = useAsync(() => api<{ items: Member[] }>("GET", orgPath("/members")), []);
  const [email, setEmail] = useState("");
  const [mrole, setMrole] = useState("builder");
  const [tname, setTname] = useState("ci");
  const [scopes, setScopes] = useState("specs:write builds:write builds:read");
  const [token, setToken] = useState<string>();
  const [err, setErr] = useState<string>();
  const admin = role === "owner" || role === "admin";
  const { memberships, refresh } = useSession();
  const expert = memberships.find((m) => m.orgId === currentOrg())?.org.expertAllowed ?? false;
  const [askExpert, setAskExpert] = useState(false);
  async function setExpert(v: boolean) {
    setAskExpert(false);
    try {
      await api("PATCH", orgPath(""), { expertAllowed: v });
      await refresh();
    } catch (x) {
      setErr((x as ApiError).message);
    }
  }

  async function invite(e: FormEvent) {
    e.preventDefault();
    try {
      await api("POST", orgPath("/members"), { email, role: mrole });
      setEmail("");
      members.reload();
    } catch (x) {
      setErr((x as ApiError).message);
    }
  }
  async function mint(e: FormEvent) {
    e.preventDefault();
    try {
      const t = await api<{ secret: string }>("POST", orgPath("/tokens"), { name: tname, scopes: scopes.split(/\s+/).filter(Boolean), expiresInDays: 90 });
      setToken(t.secret);
    } catch (x) {
      setErr((x as ApiError).message);
    }
  }
  return (
    <div className="stack" style={{ gap: 24 }}>
      <h1>Settings</h1>
      {err && <p className="err" role="alert">{err}</p>}
      <section className="card stack">
        <h2>Members</h2>
        <ErrorNote error={members.error} />
        <table>
          <thead><tr><th>Name</th><th>Email</th><th>Role</th></tr></thead>
          <tbody>{members.data?.items.map((m) => <tr key={m.userId}><td>{m.name}</td><td>{m.email}</td><td>{m.role}</td></tr>)}</tbody>
        </table>
        {admin && (
          <form className="row" onSubmit={invite}>
            <input type="email" required placeholder="colleague@example.com" value={email} onChange={(e) => setEmail(e.target.value)} style={{ maxWidth: 280 }} />
            <select value={mrole} onChange={(e) => setMrole(e.target.value)} style={{ maxWidth: 160 }}>
              {["viewer", "builder", "reviewer", "admin"].map((r) => <option key={r}>{r}</option>)}
            </select>
            <button>Add member</button>
          </form>
        )}
      </section>
      <section className="card stack">
        <h2>Expert content</h2>
        <p className="small muted">Lets builders add custom packages, files, scripts and package repositories. Every build with expert content is scanned and needs a security review before it is signed.</p>
        <div className="row">
          <span className={`badge ${expert ? "warn" : ""}`}>{expert ? "Enabled" : "Disabled"}</span>
          {role === "owner" ? (
            <button onClick={() => (expert ? void setExpert(false) : setAskExpert(true))}>{expert ? "Disable expert content" : "Enable expert content"}</button>
          ) : <span className="small muted">Only owners can change this.</span>}
        </div>
        <Confirm open={askExpert} title="Enable expert content?" confirm="Enable" onCancel={() => setAskExpert(false)} onConfirm={() => void setExpert(true)}>
          <p>Builders will be able to put their own software into the OS. Reviewers must approve each such build. This change is recorded in the audit log.</p>
        </Confirm>
      </section>
      {admin && (
        <section className="card stack">
          <h2>API tokens</h2>
          <p className="small muted">For CI and the <code>fctl</code> command line tool. Tokens are shown once and expire after 90 days.</p>
          <form className="row" onSubmit={mint}>
            <input value={tname} onChange={(e) => setTname(e.target.value)} style={{ maxWidth: 160 }} aria-label="Token name" />
            <input value={scopes} onChange={(e) => setScopes(e.target.value)} style={{ maxWidth: 360 }} aria-label="Scopes" />
            <button>Create token</button>
          </form>
          {token && <p>New token (copy it now): <code style={{ wordBreak: "break-all" }}>{token}</code></p>}
          <p className="small muted">Created {ago(new Date().toISOString())}</p>
        </section>
      )}
    </div>
  );
}
