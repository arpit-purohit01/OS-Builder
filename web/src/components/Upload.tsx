import { useState } from "react";
import { upload } from "../lib/api";

// AssetUpload uploads a file through the signed URL flow and reports the digest.
export default function AssetUpload({ kind, accept, mediaType, label, onDone }: {
  kind: string; accept: string; mediaType: (f: File) => string; label: string; onDone: (digest: string, info: Record<string, unknown>) => void;
}) {
  const [state, setState] = useState<string>();
  const [err, setErr] = useState<string>();
  return (
    <label className="field">
      {label}
      <input type="file" accept={accept} onChange={async (e) => {
        const f = e.target.files?.[0];
        if (!f) return;
        setErr(undefined);
        setState(`Uploading ${f.name}…`);
        try {
          const a = await upload(kind, mediaType(f), f);
          if (a.status !== "valid") {
            const v = a.validation as { findings?: { detail?: string; rule?: string }[] } | undefined;
            setErr(`Rejected: ${(v?.findings ?? []).map((x) => `${x.rule}: ${x.detail ?? ""}`).join("; ") || "validation failed"}`);
            setState(undefined);
            return;
          }
          setState(`Uploaded ${f.name}`);
          onDone(a.digest, (a.validation as Record<string, unknown>) ?? {});
        } catch (x) {
          setErr((x as Error).message);
          setState(undefined);
        }
        e.target.value = "";
      }} />
      {state && <span className="hint" aria-live="polite">{state}</span>}
      {err && <span className="err" role="alert">{err}</span>}
    </label>
  );
}

export const mediaFor = (f: File): string => {
  const n = f.name.toLowerCase();
  if (n.endsWith(".png")) return "image/png";
  if (n.endsWith(".jpg") || n.endsWith(".jpeg")) return "image/jpeg";
  if (n.endsWith(".svg")) return "image/svg+xml";
  if (n.endsWith(".deb")) return "application/vnd.debian.binary-package";
  if (n.endsWith(".tar")) return "application/x-tar";
  if (n.endsWith(".tar.gz") || n.endsWith(".tgz")) return "application/gzip";
  if (n.endsWith(".asc") || n.endsWith(".gpg") || n.endsWith(".pgp")) return "application/pgp-keys";
  if (n.endsWith(".sh")) return "text/x-shellscript";
  if (n.endsWith(".conf")) return "text/plain";
  if (n.endsWith(".json")) return "application/json";
  return f.type || "application/octet-stream";
};
