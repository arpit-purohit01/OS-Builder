// A lightweight visual preview of the desktop look (accent, mode, layout,
// wallpaper, radius). The real thing is the live VM preview on the build page.
import type { Spec } from "../store/spec";
import { get } from "../lib/pointer";

const WALL: Record<string, string> = {
  aurora: "linear-gradient(135deg,#0f2027,#203a43 40%,#2c7a7b 70%,#9ae6b4)",
  dusk: "linear-gradient(160deg,#2d1b69,#a83279 60%,#f6a560)",
  forest: "linear-gradient(160deg,#0b3d2e,#1f6f50 55%,#a3c585)",
  graphite: "linear-gradient(160deg,#1f2328,#3b424c 60%,#6b7380)",
};

export default function Preview({ spec }: { spec: Spec }) {
  const profile = get(spec, "/foundation/profile") as string;
  const accent = (get(spec, "/look/theme/accent") as string) ?? "#4F46E5";
  const modeRaw = (get(spec, "/look/theme/mode") as string) ?? "auto";
  const dark = modeRaw === "dark" || (modeRaw === "auto" && window.matchMedia?.("(prefers-color-scheme: dark)").matches);
  const radius = { none: 0, small: 4, medium: 9, large: 16 }[(get(spec, "/look/theme/radius") as string) ?? "medium"] ?? 9;
  const desktop = profile === "desktop-kde" || profile === "desktop-gnome";
  const layout = (get(spec, "/look/layout/preset") as string) ?? (desktop ? "dock-bottom" : "none");
  const wall = WALL[(get(spec, "/look/wallpaper/catalog") as string) ?? "aurora"] ?? WALL.aurora;
  const name = (get(spec, "/branding/osName") as string) ?? (get(spec, "/metadata/displayName") as string) ?? "Your OS";
  const panel = dark ? "rgba(20,22,28,.82)" : "rgba(250,250,252,.82)";
  const ink = dark ? "#e8eaf0" : "#1b1e24";
  const icons = Array.from({ length: 6 }, (_, i) => (
    <span key={i} style={{ width: 18, height: 18, borderRadius: Math.min(radius, 6), background: i === 0 ? accent : dark ? "#3a3f4b" : "#cfd4dc", display: "inline-block" }} />
  ));
  const bar = (vertical: boolean) => (
    <div style={{ position: "absolute", background: panel, display: "flex", gap: 6, padding: 6, borderRadius: radius, flexDirection: vertical ? "column" : "row",
      ...(vertical ? { left: 8, top: "25%" } : layout === "taskbar-bottom" ? { left: 0, right: 0, bottom: 0, borderRadius: 0 } : { left: "50%", bottom: 8, transform: "translateX(-50%)" }) }}>
      {icons}
    </div>
  );
  if (!desktop) {
    return (
      <div className="preview" style={{ background: profile === "server" ? "#0b0d12" : "#fff", display: "grid", placeItems: "center", color: profile === "server" ? "#9fe6b0" : "#222" }}>
        {profile === "server" ? <code style={{ fontSize: 11 }}>{name} login: _</code> : <div style={{ textAlign: "center" }}><div style={{ width: 40, height: 40, borderRadius: radius, background: accent, margin: "0 auto 8px" }} />{profile === "kiosk-cage" ? "Full-screen web app" : "Remote desktop"}</div>}
      </div>
    );
  }
  return (
    <div className="preview" style={{ background: wall }} aria-label={`Preview of ${name}`} role="img">
      {layout !== "none" && <div style={{ position: "absolute", top: 0, left: 0, right: 0, height: 14, background: panel, color: ink, fontSize: 8, padding: "1px 6px" }}>{name}</div>}
      <div style={{ position: "absolute", left: "22%", top: "22%", width: "50%", height: "48%", background: dark ? "#1d2028" : "#fff", borderRadius: radius, boxShadow: "0 8px 24px rgba(0,0,0,.35)", overflow: "hidden" }}>
        <div style={{ height: 12, background: dark ? "#2a2e38" : "#eef0f4" }} />
        <div style={{ padding: 8, display: "grid", gap: 5 }}>
          <span style={{ height: 6, width: "60%", background: dark ? "#3a3f4b" : "#dfe3ea", borderRadius: 3 }} />
          <span style={{ height: 6, width: "80%", background: dark ? "#3a3f4b" : "#dfe3ea", borderRadius: 3 }} />
          <span style={{ height: 14, width: 54, background: accent, borderRadius: Math.min(radius, 7), marginTop: 4 }} />
        </div>
      </div>
      {layout === "dock-left" ? bar(true) : layout === "minimal" || layout === "none" ? null : bar(false)}
    </div>
  );
}
