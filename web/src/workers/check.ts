// Pure validation logic shared by the Web Worker and tests.
import { solve, mus, type Formula, type Lit } from "../solver/sat";
import { selections } from "../solver/selections";

export interface Issue {
  pointer: string;
  code: string;
  message: string;
  options: string[]; // option ids involved
}

// Fix is a one-click change the solver has verified makes the spec satisfiable.
export interface Fix {
  label: string;
  pointer: string;
  value?: unknown; // new value for the pointer
  remove?: boolean; // remove the array element at pointer
}

export interface CheckResult {
  ok: boolean;
  issues: Issue[];
  fixes: Fix[];
  implied: string[];
  ms: number;
}

// Scalar spec fields backed by a one-of group of catalog options.
export const FIELD_PREFIX: Record<string, string> = {
  "/foundation/profile": "profile.",
  "/kernel/flavor": "kernel.",
  "/security/level": "security.level.",
  "/security/diskEncryption": "disk.",
  "/security/secureBoot": "secureboot.",
  "/security/usbStorage": "usb.",
  "/access/rootModel": "access.",
  "/memory/profile": "memory.",
  "/ecosystem/identity/provider": "identity.",
  "/ecosystem/sync/provider": "ecosystem.sync.",
  "/look/layout/preset": "layout.",
  "/look/theme/font": "font.",
  "/look/theme/iconTheme": "icons.",
  "/look/wallpaper/catalog": "wallpaper.",
};
const BOOL_FIELDS = new Set(["/ecosystem/appStore/enabled", "/ecosystem/handoff/enabled", "/ecosystem/fleet/enroll"]);

const valueOf = (ptr: string, id: string) => {
  const v = id.slice(FIELD_PREFIX[ptr].length);
  return ptr === "/security/diskEncryption" ? v.replace("tpm2-pin", "tpm2+pin") : v;
};

export function checkSpec(f: Formula, labels: Record<string, string>, spec: unknown, available?: Set<string>): CheckResult {
  const t0 = performance.now();
  const idx = new Map(f.vars.map((n, i) => [n, i + 1]));
  const issues: Issue[] = [];
  const lits: Lit[] = [];
  const ptrOf = new Map<Lit, string>();
  const chosen = new Set<string>();
  const done = (r: Omit<CheckResult, "ms">): CheckResult => ({ ...r, ms: performance.now() - t0 });
  for (const s of selections(spec)) {
    if (s.id.endsWith(".")) continue; // field not chosen yet
    const v = idx.get(s.id);
    if (!v || (available && !available.has(s.id))) {
      issues.push({ pointer: s.pointer, code: v ? "CAT-410" : "CAT-404", message: v ? `${labels[s.id] ?? s.id} is not available for this base.` : `"${s.id}" is not in the catalog.`, options: [s.id] });
      continue;
    }
    chosen.add(s.id);
    if (!ptrOf.has(v)) {
      lits.push(v);
      ptrOf.set(v, s.pointer);
    }
  }
  if (issues.length) {
    const fixes: Fix[] = issues.filter((i) => /\/\d+$/.test(i.pointer)).map((i) => ({ label: `Remove ${labels[i.options[0]] ?? i.options[0]}`, pointer: i.pointer, remove: true }));
    return done({ ok: false, issues, fixes, implied: [] });
  }
  const r = solve(f.cnf, lits);
  if (r.sat) {
    const implied = (r.implied ?? []).filter((l) => l > 0).map((l) => f.vars[l - 1]).filter((n) => !chosen.has(n) && labels[n] !== undefined);
    return done({ ok: true, issues, fixes: [], implied });
  }
  const core = mus(f.cnf, lits) ?? [];
  const ids = core.map((l) => f.vars[l - 1]);
  const msg = "These choices cannot be combined: " + ids.map((i) => labels[i] ?? i).join(", ") + ". Change one of them.";
  for (const l of core) issues.push({ pointer: ptrOf.get(l) ?? "", code: "CAT-409", message: msg, options: ids });

  // Candidate fixes, most recently chosen first; keep only verified ones.
  const fixes: Fix[] = [];
  const without = (l: Lit) => lits.filter((x) => x !== l);
  for (const l of [...core].reverse()) {
    const ptr = ptrOf.get(l) ?? "";
    const id = f.vars[l - 1];
    if (/\/\d+$/.test(ptr)) {
      if (solve(f.cnf, without(l)).sat) fixes.push({ label: `Remove ${labels[id] ?? id}`, pointer: ptr, remove: true });
    } else if (BOOL_FIELDS.has(ptr)) {
      if (solve(f.cnf, without(l)).sat) fixes.push({ label: `Turn off ${labels[id] ?? id}`, pointer: ptr, value: false });
    } else if (FIELD_PREFIX[ptr]) {
      const prefix = FIELD_PREFIX[ptr];
      for (const alt of f.vars) {
        if (!alt.startsWith(prefix) || alt === id || (available && !available.has(alt)) || labels[alt] === undefined) continue;
        if (solve(f.cnf, [...without(l), idx.get(alt)!]).sat) fixes.push({ label: `Use ${labels[alt]}`, pointer: ptr, value: valueOf(ptr, alt) });
      }
    }
    if (fixes.length >= 4) break;
  }
  return done({ ok: false, issues, fixes: fixes.slice(0, 4), implied: [] });
}
