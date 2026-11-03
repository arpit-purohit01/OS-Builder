import { useState, type FormEvent } from "react";
import { api, ApiError } from "../lib/api";
import { useSession } from "../lib/session";

export default function Login() {
  const { refresh } = useSession();
  const [email, setEmail] = useState("");
  const [code, setCode] = useState("");
  const [sent, setSent] = useState(false);
  const [devCode, setDevCode] = useState<string>();
  const [err, setErr] = useState<string>();
  const [busy, setBusy] = useState(false);

  async function start(e: FormEvent) {
    e.preventDefault();
    setErr(undefined);
    setBusy(true);
    try {
      const r = await api<{ sent: boolean; devCode?: string }>("POST", "/v1/auth/email/start", { email });
      setSent(true);
      setDevCode(r.devCode);
    } catch (x) {
      setErr((x as ApiError).message);
    } finally {
      setBusy(false);
    }
  }
  async function verify(e: FormEvent) {
    e.preventDefault();
    setErr(undefined);
    setBusy(true);
    try {
      await api("POST", "/v1/auth/email/verify", { email, code: code.trim() });
      await refresh();
    } catch (x) {
      setErr((x as ApiError).message);
      setBusy(false);
    }
  }
  return (
    <div className="center">
      <div className="card auth stack">
        <div className="brand" style={{ padding: 0 }}>
          <span className="brand-mark" aria-hidden>F</span> Foundry
        </div>
        <div>
          <h1>Sign in</h1>
          <p className="muted">Build, sign and manage your own operating system.</p>
        </div>
        {!sent ? (
          <form className="stack" onSubmit={start}>
            <label className="field">
              Work email
              <input type="email" required autoFocus value={email} onChange={(e) => setEmail(e.target.value)} autoComplete="email" />
            </label>
            <button className="primary" disabled={busy}>Email me a code</button>
            <a className="small" href="/v1/auth/oidc/start">Sign in with your company account (SSO)</a>
          </form>
        ) : (
          <form className="stack" onSubmit={verify}>
            <label className="field">
              6-digit code sent to {email}
              <input inputMode="numeric" pattern="[0-9]{6}" required autoFocus value={code} onChange={(e) => setCode(e.target.value)} autoComplete="one-time-code" />
              {devCode && <span className="hint">Development mode code: <code>{devCode}</code></span>}
            </label>
            <button className="primary" disabled={busy}>Sign in</button>
            <button type="button" className="ghost" onClick={() => setSent(false)}>Use a different email</button>
          </form>
        )}
        {err && <p className="err" role="alert">{err}</p>}
      </div>
    </div>
  );
}
