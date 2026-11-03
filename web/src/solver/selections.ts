// Port of catalog.Selections + the osspec defaults it depends on. The Go test
// TestWebSelectionsFixture writes testdata/selections.json from the golden
// specs; selections.test.ts requires identical output.
/* eslint-disable @typescript-eslint/no-explicit-any */
export interface Sel {
  id: string;
  pointer: string;
}

const str = (v: unknown, d: string) => (typeof v === "string" && v !== "" ? v : d);

export function selections(spec: any): Sel[] {
  const out: Sel[] = [];
  const add = (id: string, pointer: string) => out.push({ id, pointer });
  const f = spec?.foundation ?? {};
  const profile = str(f.profile, "");
  const kiosk = profile === "kiosk-cage" || profile === "thin-client";
  const desktop = profile === "desktop-kde" || profile === "desktop-gnome";
  const arch = Array.isArray(f.arch) && f.arch.length ? f.arch[0] : "x86_64";
  const sec = spec?.security ?? {};
  const acc = spec?.access ?? {};
  const eco = spec?.ecosystem ?? {};
  const look = spec?.look ?? {};
  const apps = spec?.apps ?? {};
  const hw = spec?.hardware ?? {};

  add("profile." + profile, "/foundation/profile");
  add("kernel." + str(spec?.kernel?.flavor, "standard"), "/kernel/flavor");
  add("security.level." + str(sec.level, "balanced"), "/security/level");
  add("disk." + str(sec.diskEncryption, "tpm2").replace(/\+/g, "-"), "/security/diskEncryption");
  add("secureboot." + str(sec.secureBoot, "tenant-keys"), "/security/secureBoot");
  add("usb." + str(sec.usbStorage, "allow"), "/security/usbStorage");
  add("access." + str(acc.rootModel, kiosk ? "locked" : "admin-password"), "/access/rootModel");
  add("memory." + str(spec?.memory?.profile, "auto"), "/memory/profile");
  add("identity." + str(eco.identity?.provider, "local"), "/ecosystem/identity/provider");
  add("ecosystem.sync." + str(eco.sync?.provider, "none"), "/ecosystem/sync/provider");
  if (eco.appStore?.enabled) add("ecosystem.appstore", "/ecosystem/appStore/enabled");
  if (eco.handoff?.enabled) {
    add("ecosystem.handoff", "/ecosystem/handoff/enabled");
    if (profile === "desktop-kde") add("ecosystem.handoff.kde", "/ecosystem/handoff/enabled");
    if (profile === "desktop-gnome") add("ecosystem.handoff.gnome", "/ecosystem/handoff/enabled");
  }
  if (eco.fleet?.enroll) add("ecosystem.fleet", "/ecosystem/fleet/enroll");
  const layout = look.layout ?? {};
  if (layout.custom) add("layout.custom", "/look/layout/custom");
  else add("layout." + str(layout.preset, desktop ? "dock-bottom" : "none"), "/look/layout/preset");
  add("font." + str(look.theme?.font, "inter"), "/look/theme/font");
  if (look.theme?.iconTheme) add("icons." + look.theme.iconTheme, "/look/theme/iconTheme");
  if (look.wallpaper?.catalog) add("wallpaper." + look.wallpaper.catalog, "/look/wallpaper/catalog");
  (apps.preinstalled ?? []).forEach((a: string, i: number) => add(a.startsWith("catalog:") ? a.slice(8) : a, `/apps/preinstalled/${i}`));
  (apps.system ?? []).forEach((a: string, i: number) => add(a, `/apps/system/${i}`));
  (sec.firewall?.allow ?? []).forEach((a: string, i: number) => add("fw.allow." + a, `/security/firewall/allow/${i}`));
  const targets: string[] = Array.isArray(hw.targets) && hw.targets.length ? hw.targets : ["generic-uefi-" + arch];
  targets.forEach((h, i) => add("hw." + h, `/hardware/targets/${i}`));
  (hw.extraFirmware ?? []).forEach((h: string, i: number) => add(h, `/hardware/extraFirmware/${i}`));
  return out;
}
