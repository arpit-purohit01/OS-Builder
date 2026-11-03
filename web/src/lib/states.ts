export const PIPELINE = ["queued", "building", "scanning", "testing", "awaiting_review", "finalizing", "published"] as const;

const pos: Record<string, number> = {
  queued: 0, leased: 0, building: 1, built: 1, scanning: 2, scanned: 2, testing: 3, tested: 3,
  awaiting_review: 4, changes_requested: 4, rejected: 4, approved: 5, finalizing: 5, signed: 5, published: 6, revoked: 6,
};
export const stageIndex = (s: string) => pos[s] ?? 0;

export const LABEL: Record<string, string> = {
  queued: "Queued", leased: "Starting", building: "Building", built: "Built", scanning: "Security scan", scanned: "Scanned",
  testing: "Boot test", tested: "Tested", awaiting_review: "Awaiting review", approved: "Approved", changes_requested: "Changes requested",
  rejected: "Rejected", finalizing: "Signing", signed: "Signed", published: "Ready", revoked: "Revoked", failed: "Failed", cancelled: "Cancelled",
};

export function stateTone(s: string): "ok" | "warn" | "bad" | "info" | "" {
  if (s === "published" || s === "signed") return "ok";
  if (s === "failed" || s === "rejected" || s === "revoked") return "bad";
  if (s === "awaiting_review" || s === "changes_requested") return "warn";
  if (s === "cancelled") return "";
  return "info";
}

export const TERMINAL = new Set(["published", "failed", "cancelled", "rejected", "revoked"]);
