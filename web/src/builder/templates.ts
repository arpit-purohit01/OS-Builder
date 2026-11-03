// Starting points for a new OS. Every template is valid with catalog defaults;
// metadata and branding ids are filled from the project.
import type { Spec } from "../store/spec";

export interface Template { id: string; label: string; description: string; spec: Spec }

const base = (profile: string, extra: Spec): Spec => ({
  apiVersion: "foundry.dev/v1alpha1",
  kind: "OSSpec",
  foundation: { base: "ubuntu-24.04", arch: ["x86_64"], profile },
  ...extra,
});

export const TEMPLATES: Template[] = [
  {
    id: "office",
    label: "Office laptop",
    description: "KDE desktop, single sign-on, app store, encrypted disk.",
    spec: base("desktop-kde", {
      look: { theme: { mode: "auto", accent: "#4F46E5", radius: "medium", font: "inter", iconTheme: "breeze" }, layout: { preset: "dock-bottom" } },
      apps: { preinstalled: ["flatpak:org.mozilla.firefox", "flatpak:org.libreoffice.LibreOffice"], system: ["printing.cups"] },
      security: { level: "balanced", diskEncryption: "tpm2", secureBoot: "tenant-keys", firewall: { default: "deny-inbound", allow: ["mdns"] } },
      access: { rootModel: "admin-password", users: { mode: "firstboot" } },
      ecosystem: { appStore: { enabled: true }, fleet: { enroll: true } },
    }),
  },
  {
    id: "school",
    label: "School computer lab",
    description: "GNOME desktop, no admin rights, USB read-only.",
    spec: base("desktop-gnome", {
      look: { theme: { mode: "auto", accent: "#2563EB" }, layout: { preset: "dock-left" } },
      apps: { preinstalled: ["flatpak:org.mozilla.firefox", "flatpak:org.libreoffice.LibreOffice", "flatpak:org.geogebra.GeoGebra"], system: ["printing.cups"] },
      security: { level: "balanced", diskEncryption: "tpm2", usbStorage: "read-only", firewall: { default: "deny-inbound", allow: ["printing"] } },
      access: { rootModel: "locked", users: { mode: "firstboot" } },
      ecosystem: { fleet: { enroll: true } },
    }),
  },
  {
    id: "kiosk",
    label: "Kiosk",
    description: "One full-screen web app, locked down, resets when idle.",
    spec: base("kiosk-cage", {
      look: { theme: { mode: "light", accent: "#0F766E" } },
      security: { level: "hardened", diskEncryption: "tpm2", usbStorage: "block" },
      access: { rootModel: "locked", users: { mode: "autologin-kiosk" } },
      kernel: { flavor: "hardened" },
      memory: { profile: "low-ram" },
      ecosystem: { fleet: { enroll: true }, kiosk: { url: "https://example.com/", idleResetMinutes: 5, allowedDomains: ["example.com"] } },
    }),
  },
  {
    id: "thin",
    label: "Thin client",
    description: "Boots straight into a remote desktop session.",
    spec: base("thin-client", {
      security: { level: "hardened", diskEncryption: "tpm2", usbStorage: "block" },
      access: { rootModel: "locked", users: { mode: "autologin-kiosk" } },
      memory: { profile: "low-ram" },
      ecosystem: { fleet: { enroll: true } },
    }),
  },
  {
    id: "developer",
    label: "Developer workstation",
    description: "KDE, full admin rights, SSH and tools, hardened kernel off.",
    spec: base("desktop-kde", {
      look: { theme: { mode: "dark", accent: "#7C3AED", font: "inter", iconTheme: "papirus" }, layout: { preset: "taskbar-bottom" } },
      apps: { preinstalled: ["flatpak:org.mozilla.firefox", "flatpak:org.chromium.Chromium"], system: ["app.openssh-client", "vpn.wireguard"] },
      security: { level: "balanced", diskEncryption: "tpm2", firewall: { default: "deny-inbound", allow: ["ssh", "mdns"] } },
      access: { rootModel: "developer", users: { mode: "firstboot" } },
      memory: { profile: "workstation" },
      ecosystem: { fleet: { enroll: true } },
    }),
  },
  {
    id: "server",
    label: "Server / VM",
    description: "Headless, minimal, encrypted. Fast to build and boot.",
    spec: base("server", {
      security: { level: "balanced", diskEncryption: "tpm2" },
      access: { rootModel: "admin-password", users: { mode: "firstboot" } },
      hardware: { targets: ["generic-vm-x86_64"] },
    }),
  },
];

export function instantiate(id: string, name: string, osId: string): Spec {
  const t = TEMPLATES.find((x) => x.id === id) ?? TEMPLATES[0];
  const s = JSON.parse(JSON.stringify(t.spec)) as Spec;
  s.metadata = { name: osId, displayName: name, version: "1.0.0" };
  s.branding = { ...(s.branding as object), osName: name, osId };
  return s;
}
