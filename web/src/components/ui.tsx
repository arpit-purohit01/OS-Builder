import { useEffect, useRef, type ReactNode } from "react";
import { LABEL, stateTone } from "../lib/states";

export function StateBadge({ state }: { state: string }) {
  return <span className={`badge ${stateTone(state)}`}>{LABEL[state] ?? state}</span>;
}

export function TierBadge({ tier }: { tier?: string | null }) {
  if (!tier) return null;
  const tone = tier === "verified" ? "ok" : tier === "reviewed" ? "info" : "warn";
  return <span className={`badge ${tone}`} title="Trust tier">{tier}</span>;
}

export function Risk({ score }: { score?: number | null }) {
  if (score == null) return <span className="muted">—</span>;
  const tone = score > 60 ? "bad" : score > 30 ? "warn" : "ok";
  return <span className={`badge ${tone}`} title="Risk score (0–100)">risk {score}</span>;
}

export function Confirm({ open, title, children, confirm, onConfirm, onCancel, danger }: {
  open: boolean; title: string; children: ReactNode; confirm: string; onConfirm: () => void; onCancel: () => void; danger?: boolean;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const d = ref.current;
    if (!d) return;
    if (open && !d.open) d.showModal?.();
    if (!open && d.open) d.close();
  }, [open]);
  return (
    <dialog ref={ref} onCancel={onCancel} aria-labelledby="dlg-title">
      <h2 id="dlg-title">{title}</h2>
      <div className="stack">{children}</div>
      <div className="row" style={{ justifyContent: "flex-end", marginTop: 16 }}>
        <button onClick={onCancel}>Cancel</button>
        <button className={danger ? "danger" : "primary"} onClick={onConfirm}>{confirm}</button>
      </div>
    </dialog>
  );
}

export function Empty({ children }: { children: ReactNode }) {
  return <div className="empty">{children}</div>;
}

export function ErrorNote({ error }: { error?: Error }) {
  if (!error) return null;
  return <p className="err" role="alert">{error.message}</p>;
}
