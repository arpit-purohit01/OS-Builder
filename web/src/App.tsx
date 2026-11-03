import { NavLink, Navigate, Route, Routes, useParams } from "react-router-dom";
import { SessionProvider, useSession } from "./lib/session";
import { setOrg } from "./lib/api";
import Login from "./pages/Login";
import Orgs from "./pages/Orgs";
import OrgHome from "./pages/OrgHome";
import ProjectPage from "./pages/Project";
import BuilderPage from "./builder/Builder";
import BuildPage from "./pages/Build";
import Reviews from "./pages/Reviews";
import Fleet from "./pages/Fleet";
import Settings from "./pages/Settings";

function OrgShell() {
  const { org = "" } = useParams();
  const { user, memberships, logout } = useSession();
  setOrg(org);
  const m = memberships.find((x) => x.orgId === org);
  if (!m) return <Navigate to="/" replace />;
  const base = `/o/${org}`;
  return (
    <div className="shell">
      <nav className="side" aria-label="Main">
        <div className="brand">
          <span className="brand-mark" aria-hidden>F</span> Foundry
        </div>
        <div className="nav">
          <NavLink to={base} end>Projects</NavLink>
          <NavLink to={`${base}/builds`}>Builds</NavLink>
          <NavLink to={`${base}/reviews`}>Reviews</NavLink>
          <NavLink to={`${base}/fleet`}>Fleet</NavLink>
          <NavLink to={`${base}/settings`}>Settings</NavLink>
        </div>
        <div className="foot">
          <div><strong>{m.org.name}</strong> · {m.role}</div>
          <div>{user?.email}</div>
          <div className="row" style={{ marginTop: 8 }}>
            <NavLink to="/?pick=1">Switch org</NavLink>
            <button className="ghost small" onClick={() => void logout()}>Sign out</button>
          </div>
        </div>
      </nav>
      <main id="main">
        <Routes>
          <Route index element={<OrgHome />} />
          <Route path="builds" element={<OrgHome buildsOnly />} />
          <Route path="p/:project" element={<ProjectPage />} />
          <Route path="p/:project/edit/:step?" element={<BuilderPage />} />
          <Route path="b/:build" element={<BuildPage />} />
          <Route path="reviews" element={<Reviews />} />
          <Route path="fleet/*" element={<Fleet />} />
          <Route path="settings" element={<Settings role={m.role} />} />
        </Routes>
      </main>
    </div>
  );
}

function Gate() {
  const { user, loading } = useSession();
  if (loading) return <div className="center muted">Loading…</div>;
  if (!user) return <Login />;
  return (
    <Routes>
      <Route path="/" element={<Orgs />} />
      <Route path="/o/:org/*" element={<OrgShell />} />
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  );
}

export default function App() {
  return (
    <SessionProvider>
      <Gate />
    </SessionProvider>
  );
}
