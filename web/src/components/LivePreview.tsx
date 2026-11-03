import { useEffect, useRef, useState } from "react";
import { api, ApiError, orgPath } from "../lib/api";

interface Started { session: { id: string; expiresAt: string }; ws: string }

// LivePreview runs the signed image in a short-lived VM and shows its screen
// (noVNC over the API's authenticated WebSocket). Reconnects with backoff.
export default function LivePreview({ buildId }: { buildId: string }) {
  const [sess, setSess] = useState<Started>();
  const [status, setStatus] = useState("");
  const [err, setErr] = useState<string>();
  const [left, setLeft] = useState(0);
  const screen = useRef<HTMLDivElement>(null);
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const rfb = useRef<any>(null);

  async function start() {
    setErr(undefined);
    setStatus("Starting a preview machine… the first start of a build takes a little longer.");
    try {
      setSess(await api<Started>("POST", orgPath(`/builds/${buildId}/preview`)));
    } catch (e) {
      setErr((e as ApiError).message);
      setStatus("");
    }
  }
  async function stop() {
    if (!sess) return;
    rfb.current?.disconnect();
    await api("DELETE", orgPath(`/previews/${sess.session.id}`)).catch(() => undefined);
    setSess(undefined);
    setStatus("");
  }

  useEffect(() => {
    if (!sess) return;
    const t = setInterval(() => setLeft(Math.max(0, Math.round((new Date(sess.session.expiresAt).getTime() - Date.now()) / 1000))), 1000);
    return () => clearInterval(t);
  }, [sess]);

  useEffect(() => {
    if (!sess || !screen.current) return;
    let alive = true;
    let attempt = 0;
    let timer: ReturnType<typeof setTimeout>;
    const connect = async () => {
      const { default: RFB } = await import("@novnc/novnc/core/rfb.js");
      if (!alive || !screen.current) return;
      const url = `${location.protocol === "https:" ? "wss" : "ws"}://${location.host}${sess.ws}`;
      const r = new RFB(screen.current, url, { wsProtocols: ["binary"] });
      r.scaleViewport = true;
      r.focusOnClick = true;
      rfb.current = r;
      r.addEventListener("connect", () => {
        attempt = 0;
        setStatus("");
      });
      r.addEventListener("disconnect", () => {
        if (!alive) return;
        if (new Date(sess.session.expiresAt).getTime() < Date.now()) {
          setStatus("The preview time is over.");
          return;
        }
        attempt++;
        const delay = Math.min(15000, 500 * 2 ** attempt);
        setStatus(attempt === 1 ? "Waiting for the machine's screen…" : `Reconnecting in ${Math.round(delay / 1000)} s…`);
        timer = setTimeout(() => void connect(), delay);
      });
    };
    void connect();
    return () => {
      alive = false;
      clearTimeout(timer);
      rfb.current?.disconnect();
    };
  }, [sess]);

  if (!sess) {
    return (
      <div className="card stack">
        <h3>Try it live</h3>
        <p className="small muted">Boots this exact signed image in a temporary virtual machine for 15 minutes. Nothing you do there is kept.</p>
        <div><button className="primary" onClick={() => void start()}>Start live preview</button></div>
        {status && <p className="small" aria-live="polite">{status}</p>}
        {err && <p className="err" role="alert">{err}</p>}
      </div>
    );
  }
  const mm = Math.floor(left / 60), ss = String(left % 60).padStart(2, "0");
  return (
    <div className="stack">
      <div className="row spread">
        <span className="small muted" aria-live="polite">{status || "Click into the screen to use the keyboard."}</span>
        <div className="row">
          <span className="badge info">{mm}:{ss} left</span>
          <button onClick={() => rfb.current?.sendCtrlAltDel()}>Ctrl+Alt+Del</button>
          <button className="danger" onClick={() => void stop()}>Stop</button>
        </div>
      </div>
      <div ref={screen} className="preview" style={{ background: "#000", height: 560, aspectRatio: "auto" }} aria-label="Live preview screen" />
    </div>
  );
}
