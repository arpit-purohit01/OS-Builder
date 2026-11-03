import { useState } from "react";
import { upload } from "../lib/api";
import { get } from "../lib/pointer";
import type { Ctx } from "./fields";

type Edge = "top" | "bottom" | "left" | "right";
interface Panel { edge: Edge; style: "dock" | "bar"; size: number; autohide?: boolean; items: string[] }
export interface LayoutDoc { version: 1; panels: Panel[]; favorites?: string[] }

const ITEMS: Record<string, string> = {
  launcher: "App launcher", favorites: "Favorite apps", tasks: "Open windows", tray: "Status icons", clock: "Clock",
  search: "Search", spacer: "Spacer", desktops: "Workspaces", trash: "Trash",
};

const PRESETS: Record<string, LayoutDoc> = {
  "dock-bottom": { version: 1, panels: [{ edge: "bottom", style: "dock", size: 48, items: ["launcher", "favorites", "tasks", "tray", "clock"] }] },
  "dock-left": { version: 1, panels: [{ edge: "left", style: "dock", size: 48, items: ["launcher", "favorites", "tasks"] }, { edge: "top", style: "bar", size: 28, items: ["spacer", "clock", "tray"] }] },
  "taskbar-bottom": { version: 1, panels: [{ edge: "bottom", style: "bar", size: 40, items: ["launcher", "tasks", "spacer", "tray", "clock"] }] },
};

export default function LayoutEditor({ ctx, profile }: { ctx: Ctx; profile: string }) {
  const preset = (get(ctx.spec, "/look/layout/preset") as string) ?? "dock-bottom";
  const custom = get(ctx.spec, "/look/layout/custom") as string | undefined;
  const apps = ((get(ctx.spec, "/apps/preinstalled") as string[]) ?? []).filter((a) => a.startsWith("flatpak:")).map((a) => a.slice(8) + ".desktop");
  const [doc, setDoc] = useState<LayoutDoc>(() => JSON.parse(JSON.stringify(PRESETS[preset] ?? PRESETS["dock-bottom"])));
  const [state, setState] = useState<string>();
  const [open, setOpen] = useState(!!custom);
  const upd = (f: (d: LayoutDoc) => void) => setDoc((d) => {
    const n = JSON.parse(JSON.stringify(d)) as LayoutDoc;
    f(n);
    return n;
  });
  const used = new Set(doc.panels.map((p) => p.edge));
  async function apply() {
    setState("Saving layout…");
    try {
      const body = JSON.stringify(doc);
      const a = await upload("layout", "application/json", new File([body], "layout.json", { type: "application/json" }));
      if (a.status !== "valid") throw new Error("The layout was rejected");
      ctx.store.commit(applyCustom(ctx.spec, a.digest), "Custom layout");
      setState("Layout applied.");
    } catch (e) {
      setState((e as Error).message);
    }
  }
  if (!open) {
    return (
      <div className="row">
        <button onClick={() => setOpen(true)}>Design your own layout…</button>
        {custom && <span className="badge info">custom layout in use</span>}
      </div>
    );
  }
  return (
    <div className="card stack">
      <div className="row spread">
        <h3 style={{ margin: 0 }}>Layout editor</h3>
        <div className="row">
          {custom && <button className="ghost small" onClick={() => ctx.store.set("/look/layout", { preset: "dock-bottom" }, "Layout preset")}>Use a preset instead</button>}
          <button className="ghost small" onClick={() => setOpen(false)}>Close</button>
        </div>
      </div>
      <LayoutCanvas doc={doc} />
      {profile === "desktop-gnome" && <p className="small muted">GNOME keeps its top bar; the first other panel becomes the dock.</p>}
      {doc.panels.map((p, i) => (
        <fieldset key={i} className="card stack" style={{ padding: 12 }}>
          <legend style={{ fontWeight: 600 }}>Panel {i + 1}</legend>
          <div className="row">
            <label className="field" style={{ width: 140 }}>Edge
              <select value={p.edge} onChange={(e) => upd((d) => void (d.panels[i].edge = e.target.value as Edge))}>
                {(["bottom", "top", "left", "right"] as Edge[]).filter((x) => x === p.edge || !used.has(x)).map((x) => <option key={x}>{x}</option>)}
              </select>
            </label>
            <label className="field" style={{ width: 160 }}>Style
              <select value={p.style} onChange={(e) => upd((d) => void (d.panels[i].style = e.target.value as "dock" | "bar"))}>
                <option value="dock">Dock (centered)</option><option value="bar">Bar (full width)</option>
              </select>
            </label>
            <label className="field" style={{ width: 180 }}>Size {p.size}px
              <input type="range" min={24} max={96} value={p.size} onChange={(e) => upd((d) => void (d.panels[i].size = Number(e.target.value)))} />
            </label>
            <label className="row small"><input type="checkbox" checked={!!p.autohide} onChange={(e) => upd((d) => void (d.panels[i].autohide = e.target.checked))} /> Hide automatically</label>
            {doc.panels.length > 1 && <button className="danger small" onClick={() => upd((d) => void d.panels.splice(i, 1))}>Remove panel</button>}
          </div>
          <ol className="stack" style={{ margin: 0, paddingLeft: 20 }} aria-label={`Items of panel ${i + 1}`}>
            {p.items.map((it, j) => (
              <li key={it + j} className="row spread">
                <span>{ITEMS[it]}</span>
                <span className="row" style={{ gap: 4 }}>
                  <button className="ghost small" aria-label={`Move ${ITEMS[it]} up`} disabled={j === 0} onClick={() => upd((d) => { const a = d.panels[i].items; [a[j - 1], a[j]] = [a[j], a[j - 1]]; })}>↑</button>
                  <button className="ghost small" aria-label={`Move ${ITEMS[it]} down`} disabled={j === p.items.length - 1} onClick={() => upd((d) => { const a = d.panels[i].items; [a[j + 1], a[j]] = [a[j], a[j + 1]]; })}>↓</button>
                  <button className="ghost small" aria-label={`Remove ${ITEMS[it]}`} disabled={p.items.length === 1} onClick={() => upd((d) => void d.panels[i].items.splice(j, 1))}>✕</button>
                </span>
              </li>
            ))}
          </ol>
          <label className="field" style={{ maxWidth: 260 }}>Add item
            <select value="" onChange={(e) => e.target.value && upd((d) => void d.panels[i].items.push(e.target.value))}>
              <option value="">Choose…</option>
              {Object.entries(ITEMS).filter(([k]) => k === "spacer" || !p.items.includes(k)).map(([k, l]) => <option key={k} value={k}>{l}</option>)}
            </select>
          </label>
        </fieldset>
      ))}
      {doc.panels.length < 4 && <div><button onClick={() => upd((d) => void d.panels.push({ edge: (["top", "bottom", "left", "right"] as Edge[]).find((x) => !used.has(x))!, style: "bar", size: 32, items: ["clock"] }))}>Add panel</button></div>}
      {apps.length > 0 && (
        <fieldset className="stack" style={{ border: 0, padding: 0 }}>
          <legend style={{ fontWeight: 600 }}>Favorite apps</legend>
          <div className="row">
            {apps.map((a) => (
              <label key={a} className="row small">
                <input type="checkbox" checked={(doc.favorites ?? []).includes(a)} onChange={(e) => upd((d) => {
                  const f = new Set(d.favorites ?? []);
                  if (e.target.checked) f.add(a); else f.delete(a);
                  d.favorites = [...f];
                })} /> {a.replace(/\.desktop$/, "").split(".").pop()}
              </label>
            ))}
          </div>
        </fieldset>
      )}
      <div className="row">
        <button className="primary" onClick={() => void apply()}>Use this layout</button>
        {state && <span className="small" aria-live="polite">{state}</span>}
      </div>
    </div>
  );
}

function applyCustom(spec: Record<string, unknown>, digest: string) {
  const s = JSON.parse(JSON.stringify(spec));
  s.look = s.look ?? {};
  s.look.layout = { custom: digest };
  return s;
}

export function LayoutCanvas({ doc }: { doc: LayoutDoc }) {
  const W = 320, H = 200;
  return (
    <svg viewBox={`0 0 ${W} ${H}`} role="img" aria-label="Layout preview" style={{ width: "100%", maxWidth: 480, background: "linear-gradient(135deg,#203a43,#2c7a7b)", borderRadius: 10 }}>
      {doc.panels.map((p, i) => {
        const t = Math.max(10, p.size / 3);
        const horizontal = p.edge === "top" || p.edge === "bottom";
        const len = p.style === "bar" ? (horizontal ? W : H) : Math.min(horizontal ? W : H, p.items.length * (t + 4) + 8);
        const x = horizontal ? (W - len) / 2 : p.edge === "left" ? 4 : W - t - 4;
        const y = horizontal ? (p.edge === "top" ? (p.style === "bar" ? 0 : 4) : H - t - (p.style === "bar" ? 0 : 4)) : (H - len) / 2;
        return (
          <g key={i} opacity={p.autohide ? 0.55 : 1}>
            <rect x={x} y={y} width={horizontal ? len : t} height={horizontal ? t : len} rx={p.style === "dock" ? 6 : 0} fill="rgba(15,17,22,.78)" />
            {p.items.map((it, j) => (
              <rect key={j} x={horizontal ? x + 6 + j * (t + 4) : x + 2} y={horizontal ? y + 2 : y + 6 + j * (t + 4)} width={t - 4} height={t - 4} rx={3}
                fill={it === "spacer" ? "transparent" : it === "launcher" ? "#8b85ff" : "rgba(255,255,255,.55)"} />
            ))}
          </g>
        );
      })}
    </svg>
  );
}
