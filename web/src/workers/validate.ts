// Validation Web Worker: holds the catalog CNF and answers check requests so
// the UI thread never blocks on SAT solving.
import type { Formula } from "../solver/sat";
import { checkSpec } from "./check";

let formula: Formula | null = null;
let labels: Record<string, string> = {};
let available: Set<string> | undefined;

type Msg =
  | { type: "init"; formula: Formula; labels: Record<string, string>; available: string[] }
  | { type: "check"; seq: number; spec: unknown };

self.onmessage = (e: MessageEvent<Msg>) => {
  const m = e.data;
  if (m.type === "init") {
    formula = m.formula;
    labels = m.labels;
    available = new Set(m.available);
    return;
  }
  if (!formula) {
    (self as unknown as Worker).postMessage({ seq: m.seq, pending: true });
    return;
  }
  const r = checkSpec(formula, labels, m.spec, available);
  (self as unknown as Worker).postMessage({ seq: m.seq, ...r });
};
