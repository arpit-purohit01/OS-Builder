// useValidation keeps a Web Worker loaded with the catalog CNF and checks the
// spec 150 ms after each edit. Results are advisory; the server is authoritative.
import { useEffect, useRef, useState } from "react";
import { api, type Catalog } from "../lib/api";
import type { Formula } from "../solver/sat";
import type { CheckResult } from "../workers/check";

export interface Validation extends Partial<CheckResult> {
  ready: boolean;
}

export function useCatalog(base: string, arch: string) {
  const [cat, setCat] = useState<Catalog>();
  const [formula, setFormula] = useState<Formula>();
  useEffect(() => {
    let live = true;
    (async () => {
      const c = await api<Catalog>("GET", `/v1/catalog?base=${encodeURIComponent(base)}&arch=${encodeURIComponent(arch)}`);
      const f = await api<Formula>("GET", `/v1/catalog/${c.version}/cnf?base=${encodeURIComponent(base)}&arch=${encodeURIComponent(arch)}`);
      if (live) {
        setCat(c);
        setFormula(f);
      }
    })().catch(() => undefined);
    return () => {
      live = false;
    };
  }, [base, arch]);
  return { cat, formula };
}

export function useValidation(spec: unknown, cat?: Catalog, formula?: Formula): Validation {
  const worker = useRef<Worker>();
  const seq = useRef(0);
  const [res, setRes] = useState<Validation>({ ready: false });
  useEffect(() => {
    const w = new Worker(new URL("../workers/validate.ts", import.meta.url), { type: "module" });
    worker.current = w;
    w.onmessage = (e) => {
      if (e.data.seq === seq.current && !e.data.pending) setRes({ ready: true, ...e.data });
    };
    return () => w.terminate();
  }, []);
  useEffect(() => {
    if (!cat || !formula || !worker.current) return;
    const labels: Record<string, string> = {};
    for (const o of cat.options) labels[o.id] = o.label;
    worker.current.postMessage({ type: "init", formula, labels, available: cat.options.filter((o) => o.available).map((o) => o.id) });
  }, [cat, formula]);
  useEffect(() => {
    if (!cat || !formula) return;
    const t = setTimeout(() => {
      seq.current++;
      worker.current?.postMessage({ type: "check", seq: seq.current, spec });
    }, 150);
    return () => clearTimeout(t);
  }, [spec, cat, formula]);
  return res;
}
