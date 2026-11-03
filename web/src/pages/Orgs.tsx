import { useState, type FormEvent } from "react";
import { Link, Navigate, useNavigate, useSearchParams } from "react-router-dom";
import { api, ApiError, type Org } from "../lib/api";
import { useSession } from "../lib/session";

export default function Orgs() {
  const { memberships, refresh, user, logout } = useSession();
  const nav = useNavigate();
  const [name, setName] = useState("");
  const [err, setErr] = useState<string>();
  const [q] = useSearchParams();
  if (memberships.length === 1 && !q.get("pick")) return <Navigate to={`/o/${memberships[0].orgId}`} replace />;

  async function create(e: FormEvent) {
    e.preventDefault();
    const slug = name.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-|-$/g, "").slice(0, 40) || "org";
    try {
      let o: Org;
      try {
        o = await api<Org>("POST", "/v1/orgs", { name, slug });
      } catch (x) {
        if ((x as ApiError).status !== 409) throw x;
        o = await api<Org>("POST", "/v1/orgs", { name, slug: `${slug.slice(0, 35)}-${Math.random().toString(36).slice(2, 6)}` });
      }
      await refresh();
      nav(`/o/${o.id}`);
    } catch (x) {
      setErr((x as ApiError).message);
    }
  }
  return (
    <div className="center">
      <div className="card auth stack" style={{ maxWidth: 460 }}>
        <h1>Your organizations</h1>
        <p className="muted small">Signed in as {user?.email} · <button className="ghost small" onClick={() => void logout()}>Sign out</button></p>
        {memberships.map((m) => (
          <Link key={m.orgId} className="choice" to={`/o/${m.orgId}`}>
            <span className="t">{m.org.name}</span>
            <span className="d">{m.role} · {m.org.plan}</span>
          </Link>
        ))}
        <form className="stack" onSubmit={create}>
          <h2 style={{ marginTop: 8 }}>Create an organization</h2>
          <label className="field">
            Name
            <input required maxLength={80} value={name} onChange={(e) => setName(e.target.value)} placeholder="Acme Corp" />
          </label>
          <button className="primary">Create</button>
          {err && <p className="err" role="alert">{err}</p>}
        </form>
      </div>
    </div>
  );
}

