import { useId, useState, type ReactNode } from "react";
import type { CatalogOption } from "../lib/api";
import { get } from "../lib/pointer";
import { Confirm } from "../components/ui";
import type { SpecStore, Spec } from "../store/spec";
import type { Issue } from "../workers/check";

export type Level = "simple" | "custom" | "expert";
const rank: Record<Level, number> = { simple: 0, custom: 1, expert: 2 };
export const visible = (opt: Level, cur: Level) => rank[opt] <= rank[cur];

export interface Ctx {
  store: SpecStore;
  spec: Spec;
  options: CatalogOption[];
  level: Level;
  issues: Issue[];
  implied: Set<string>;
  serverErrors: { pointer?: string; message: string }[];
}

export interface Danger { value: string; title: string; text: string }

function errorsAt(ctx: Ctx, ptr: string): string[] {
  const out = ctx.issues.filter((i) => i.pointer === ptr || i.pointer.startsWith(ptr + "/")).map((i) => i.message);
  for (const e of ctx.serverErrors) if (e.pointer && (e.pointer === ptr || e.pointer.startsWith(ptr + "/"))) out.push(e.message);
  return [...new Set(out)];
}

function FieldErrors({ errs, id }: { errs: string[]; id: string }) {
  if (!errs.length) return null;
  return (
    <div id={id} role="alert">
      {errs.map((e) => <p className="err" key={e}>{e}</p>)}
    </div>
  );
}

function optLevel(o: CatalogOption): Level {
  const l = o.levels ?? ["simple", "custom", "expert"];
  return (l.includes("simple") ? "simple" : l.includes("custom") ? "custom" : "expert") as Level;
}

// Choice: one-of from catalog options with a common id prefix.
export function Choice({ ctx, label, help, pointer, prefix, toValue = (s) => s, fromValue = (s) => s, def, dangers = [], exclude = [] }: {
  ctx: Ctx; label: string; help?: string; pointer: string; prefix: string; toValue?: (suffix: string) => string; fromValue?: (v: string) => string;
  def?: string; dangers?: Danger[]; exclude?: string[];
}) {
  const id = useId();
  const [pending, setPending] = useState<{ v: string; d: Danger }>();
  const raw = get(ctx.spec, pointer) as string | undefined;
  const cur = raw ?? def;
  const opts = ctx.options.filter((o) => o.id.startsWith(prefix) && !exclude.includes(o.id) && (visible(optLevel(o), ctx.level) || fromValue(cur ?? "") === o.id.slice(prefix.length)));
  const errs = errorsAt(ctx, pointer);
  const conflictIds = new Set(ctx.issues.flatMap((i) => (i.code === "CAT-409" ? i.options : [])));
  const pick = (v: string) => {
    const d = dangers.find((x) => x.value === v);
    if (d && v !== cur) setPending({ v, d });
    else ctx.store.set(pointer, v, label);
  };
  return (
    <fieldset className="stack" style={{ border: 0, padding: 0, margin: 0 }} aria-describedby={errs.length ? id + "e" : undefined}>
      <legend style={{ fontWeight: 600, marginBottom: 2 }}>{label}</legend>
      {help && <p className="muted small">{help}</p>}
      <div className="choices" role="radiogroup" aria-label={label}>
        {opts.map((o) => {
          const v = toValue(o.id.slice(prefix.length));
          const on = cur === v;
          return (
            <button type="button" role="radio" aria-checked={on} key={o.id} disabled={!o.available}
              className={`choice${o.available ? "" : " unavailable"}${on && conflictIds.has(o.id) ? " conflict" : ""}`} onClick={() => pick(v)}>
              <span className="t">{o.label}{optLevel(o) === "expert" && <span className="badge warn" style={{ marginLeft: 6 }}>expert</span>}</span>
              {o.help && <span className="d">{o.help}</span>}
              {!o.available && <span className="d">Not available for this base</span>}
            </button>
          );
        })}
      </div>
      <FieldErrors errs={errs} id={id + "e"} />
      <Confirm open={!!pending} title={pending?.d.title ?? ""} confirm="Yes, change it" danger
        onCancel={() => setPending(undefined)} onConfirm={() => { ctx.store.set(pointer, pending!.v, label); setPending(undefined); }}>
        <p>{pending?.d.text}</p>
      </Confirm>
    </fieldset>
  );
}

// Multi: set of catalog options (by id prefix or kind) stored as a list.
export function Multi({ ctx, label, help, pointer, match, toValue = (id) => id, fromValue = (v) => v }: {
  ctx: Ctx; label: string; help?: string; pointer: string; match: (o: CatalogOption) => boolean; toValue?: (id: string) => string; fromValue?: (v: string) => string;
}) {
  const id = useId();
  const list = ((get(ctx.spec, pointer) as string[] | undefined) ?? []).slice();
  const chosen = new Set(list.map(fromValue));
  const opts = ctx.options.filter((o) => match(o) && (visible(optLevel(o), ctx.level) || chosen.has(o.id)));
  const errs = errorsAt(ctx, pointer);
  const toggle = (o: CatalogOption) => {
    const v = toValue(o.id);
    const next = chosen.has(o.id) ? list.filter((x) => fromValue(x) !== o.id) : [...list, v];
    ctx.store.set(pointer, next.length ? next : undefined, label);
  };
  return (
    <fieldset className="stack" style={{ border: 0, padding: 0, margin: 0 }}>
      <legend style={{ fontWeight: 600, marginBottom: 2 }}>{label}</legend>
      {help && <p className="muted small">{help}</p>}
      <div className="choices">
        {opts.map((o) => (
          <button type="button" role="checkbox" aria-checked={chosen.has(o.id)} key={o.id} disabled={!o.available && !chosen.has(o.id)}
            className={`choice${o.available ? "" : " unavailable"}`} onClick={() => toggle(o)}>
            <span className="t">
              {o.label}
              {ctx.implied.has(o.id) && !chosen.has(o.id) && <span className="badge info" style={{ marginLeft: 6 }}>added automatically</span>}
              {o.hcl && <span className={`badge ${o.hcl.status === "certified" ? "ok" : ""}`} style={{ marginLeft: 6 }} title={o.hcl.notes ?? `Tested with ${o.hcl.tested.join(", ")}`}>{o.hcl.status}</span>}
            </span>
            {o.help && <span className="d">{o.help}</span>}
          </button>
        ))}
      </div>
      <FieldErrors errs={errs} id={id} />
    </fieldset>
  );
}

export function Text({ ctx, label, hint, pointer, placeholder, type = "text", pattern, readOnly, number }: {
  ctx: Ctx; label: string; hint?: ReactNode; pointer: string; placeholder?: string; type?: string; pattern?: string; readOnly?: boolean; number?: boolean;
}) {
  const id = useId();
  const v = get(ctx.spec, pointer);
  const errs = errorsAt(ctx, pointer);
  return (
    <label className="field">
      {label}
      <input type={type} value={v === undefined ? "" : String(v)} placeholder={placeholder} pattern={pattern} readOnly={readOnly}
        aria-invalid={errs.length > 0} aria-describedby={errs.length ? id : undefined}
        onChange={(e) => {
          const s = e.target.value;
          ctx.store.set(pointer, s === "" ? undefined : number ? Number(s) : s, label);
        }} />
      {hint && <span className="hint">{hint}</span>}
      <FieldErrors errs={errs} id={id} />
    </label>
  );
}

export function ListText({ ctx, label, hint, pointer, placeholder }: { ctx: Ctx; label: string; hint?: string; pointer: string; placeholder?: string }) {
  const id = useId();
  const v = (get(ctx.spec, pointer) as string[] | undefined) ?? [];
  const [text, setText] = useState(v.join("\n"));
  const errs = errorsAt(ctx, pointer);
  return (
    <label className="field">
      {label}
      <textarea rows={4} value={text} placeholder={placeholder} aria-invalid={errs.length > 0} aria-describedby={errs.length ? id : undefined}
        onChange={(e) => {
          setText(e.target.value);
          const items = e.target.value.split(/[\n,]/).map((s) => s.trim()).filter(Boolean);
          ctx.store.set(pointer, items.length ? items : undefined, label);
        }} />
      {hint && <span className="hint">{hint}</span>}
      <FieldErrors errs={errs} id={id} />
    </label>
  );
}

export function Toggle({ ctx, label, hint, pointer, def = false, danger }: { ctx: Ctx; label: string; hint?: string; pointer: string; def?: boolean; danger?: { on: boolean; title: string; text: string } }) {
  const v = (get(ctx.spec, pointer) as boolean | undefined) ?? def;
  const [ask, setAsk] = useState(false);
  const flip = () => {
    if (danger && !v === danger.on) setAsk(true);
    else ctx.store.set(pointer, !v, label);
  };
  return (
    <div className="row spread card" style={{ padding: "10px 14px" }}>
      <div>
        <div style={{ fontWeight: 600 }}>{label}</div>
        {hint && <div className="muted small">{hint}</div>}
      </div>
      <button type="button" role="switch" aria-checked={v} aria-label={label} onClick={flip} className={v ? "primary" : ""} style={{ minWidth: 64, justifyContent: "center" }}>
        {v ? "On" : "Off"}
      </button>
      {danger && (
        <Confirm open={ask} title={danger.title} confirm="Yes" danger onCancel={() => setAsk(false)} onConfirm={() => { ctx.store.set(pointer, !v, label); setAsk(false); }}>
          <p>{danger.text}</p>
        </Confirm>
      )}
    </div>
  );
}

export function Select({ ctx, label, hint, pointer, options, def }: { ctx: Ctx; label: string; hint?: string; pointer: string; options: [string, string][]; def?: string }) {
  const v = (get(ctx.spec, pointer) as string | undefined) ?? def ?? "";
  const errs = errorsAt(ctx, pointer);
  return (
    <label className="field">
      {label}
      <select value={v} aria-invalid={errs.length > 0} onChange={(e) => ctx.store.set(pointer, e.target.value || undefined, label)}>
        {options.map(([val, l]) => <option key={val} value={val}>{l}</option>)}
      </select>
      {hint && <span className="hint">{hint}</span>}
      {errs.map((e) => <span className="err" key={e}>{e}</span>)}
    </label>
  );
}

export function Segmented({ ctx, label, pointer, options, def }: { ctx: Ctx; label: string; pointer: string; options: [string, string][]; def: string }) {
  const v = (get(ctx.spec, pointer) as string | undefined) ?? def;
  return (
    <div className="field">
      <span style={{ fontWeight: 600 }}>{label}</span>
      <div className="seg" role="group" aria-label={label}>
        {options.map(([val, l]) => (
          <button type="button" key={val} aria-pressed={v === val} onClick={() => ctx.store.set(pointer, val, label)}>{l}</button>
        ))}
      </div>
    </div>
  );
}

export const ACCENTS = ["#4F46E5", "#2563EB", "#0F766E", "#16A34A", "#CA8A04", "#EA580C", "#DC2626", "#DB2777", "#7C3AED", "#334155"];

export function Color({ ctx, label, pointer, def, swatches = ACCENTS }: { ctx: Ctx; label: string; pointer: string; def: string; swatches?: string[] }) {
  const v = ((get(ctx.spec, pointer) as string | undefined) ?? def).toUpperCase();
  return (
    <div className="field">
      <span style={{ fontWeight: 600 }}>{label}</span>
      <div className="row" style={{ gap: 8 }}>
        {swatches.map((c) => (
          <button type="button" key={c} className="swatch" style={{ background: c, padding: 0 }} aria-pressed={v === c} aria-label={c} onClick={() => ctx.store.set(pointer, c, label)} />
        ))}
        <input type="color" aria-label={`${label} (custom)`} value={v} onChange={(e) => ctx.store.set(pointer, e.target.value.toUpperCase(), label)} style={{ width: 40, height: 30, padding: 0, border: 0, background: "none" }} />
        <code>{v}</code>
      </div>
    </div>
  );
}
