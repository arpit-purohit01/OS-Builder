import { Choice, Color, ListText, Multi, Segmented, Select, Text, Toggle, type Ctx } from "./fields";
import { get } from "../lib/pointer";
import AssetUpload, { mediaFor } from "../components/Upload";
import Preview from "./Preview";
import LayoutEditor from "./LayoutEditor";

export interface Step { id: string; label: string; prefixes: string[]; render: (ctx: Ctx) => JSX.Element; level?: "expert" }

const isDesktop = (ctx: Ctx) => ["desktop-kde", "desktop-gnome"].includes(get(ctx.spec, "/foundation/profile") as string);

const Foundation = (ctx: Ctx) => (
  <>
    <div className="section">
      <Choice ctx={ctx} label="What kind of device is this for?" pointer="/foundation/profile" prefix="profile." />
    </div>
    <div className="section grid2">
      <Text ctx={ctx} label="Display name" pointer="/metadata/displayName" placeholder="Aurora OS" />
      <Text ctx={ctx} label="Version" pointer="/metadata/version" pattern="\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?" hint="Semantic version, e.g. 1.4.0. Increase it for every release." />
    </div>
    {ctx.level !== "simple" && (
      <div className="section">
        <Select ctx={ctx} label="Base system" pointer="/foundation/base" def="ubuntu-24.04"
          options={[["ubuntu-24.04", "Ubuntu 24.04 LTS (recommended)"], ["ubuntu-26.04", "Ubuntu 26.04 LTS"], ["debian-13", "Debian 13"]]}
          hint="All supported bases receive security updates through Foundry." />
        <Multi ctx={ctx} label="Hardware" help="Pick the devices you will install on. Generic UEFI works on most PCs." pointer="/hardware/targets"
          match={(o) => o.id.startsWith("hw.")} toValue={(id) => id.slice(3)} fromValue={(v) => "hw." + v} />
        <Choice ctx={ctx} label="Kernel" pointer="/kernel/flavor" prefix="kernel." def="standard" />
        <Choice ctx={ctx} label="Memory tuning" pointer="/memory/profile" prefix="memory." def="auto" />
      </div>
    )}
    {ctx.level === "expert" && (
      <div className="section">
        <Multi ctx={ctx} label="Extra firmware" pointer="/hardware/extraFirmware" match={(o) => o.kind === "firmware"} />
        <Select ctx={ctx} label="Boot mode" pointer="/foundation/bootMode" def="uefi" options={[["uefi", "UEFI only (recommended)"], ["uefi+bios-compat", "UEFI + legacy BIOS media"]]} />
        <ListText ctx={ctx} label="Extra kernel command line" pointer="/kernel/extraCmdline" placeholder="quiet&#10;mitigations=auto" hint="One parameter per line. Security-reducing parameters are rejected." />
      </div>
    )}
  </>
);

const Look = (ctx: Ctx) => (
  <>
    <div className="section">
      <Preview spec={ctx.spec} />
      <div className="grid2">
        <Segmented ctx={ctx} label="Appearance" pointer="/look/theme/mode" def="auto" options={[["light", "Light"], ["dark", "Dark"], ["auto", "Automatic"]]} />
        {ctx.level !== "simple" && <Segmented ctx={ctx} label="Corner style" pointer="/look/theme/radius" def="medium" options={[["none", "Square"], ["small", "Small"], ["medium", "Medium"], ["large", "Round"]]} />}
      </div>
      <Color ctx={ctx} label="Accent colour" pointer="/look/theme/accent" def="#4F46E5" />
    </div>
    {isDesktop(ctx) && (
      <div className="section">
        {!get(ctx.spec, "/look/layout/custom") && <Choice ctx={ctx} label="Layout" pointer="/look/layout/preset" prefix="layout." exclude={["layout.custom", "layout.none"]} />}
        {ctx.level !== "simple" && <LayoutEditor ctx={ctx} profile={get(ctx.spec, "/foundation/profile") as string} />}
        <Choice ctx={ctx} label="Wallpaper" pointer="/look/wallpaper/catalog" prefix="wallpaper." />
        {ctx.level !== "simple" && (
          <AssetUpload kind="image" accept="image/png,image/jpeg,image/svg+xml" mediaType={mediaFor} label="Or upload your own wallpaper (PNG, JPEG or SVG, up to 20 MB)"
            onDone={(d) => ctx.store.set("/look/wallpaper", { asset: d }, "Wallpaper")} />
        )}
        {(get(ctx.spec, "/look/wallpaper/asset") as string) && <p className="small muted">Custom wallpaper: <code>{(get(ctx.spec, "/look/wallpaper/asset") as string).slice(0, 19)}…</code></p>}
      </div>
    )}
    {ctx.level !== "simple" && (
      <div className="section">
        {isDesktop(ctx) && <Choice ctx={ctx} label="Font" pointer="/look/theme/font" prefix="font." def="inter" />}
        {isDesktop(ctx) && <Choice ctx={ctx} label="Icons" pointer="/look/theme/iconTheme" prefix="icons." />}
        <Color ctx={ctx} label="Boot screen background" pointer="/look/bootSplash/background" def="#0B1020" swatches={["#0B1020", "#000000", "#FFFFFF", "#111827"]} />
        <AssetUpload kind="image" accept="image/png,image/svg+xml" mediaType={mediaFor} label="Boot logo (PNG or SVG)" onDone={(d) => ctx.store.set("/look/bootSplash/logo", d, "Boot logo")} />
        <AssetUpload kind="image" accept="image/png,image/jpeg" mediaType={mediaFor} label="Login screen background (PNG or JPEG)" onDone={(d) => ctx.store.set("/look/loginScreen/background", d, "Login screen background")} />
        {(get(ctx.spec, "/look/loginScreen/background") as string) && <p className="small muted">Custom login background: <code>{(get(ctx.spec, "/look/loginScreen/background") as string).slice(0, 19)}…</code></p>}
        <Toggle ctx={ctx} label="Hide systemd-boot menu (silent boot)" pointer="/look/bootSplash/hideMenu" def={true}
          danger={{ on: false, title: "Show boot menu?", text: "Showing the menu allows selecting fallback entries but leaks the OS name before authentication." }} />
      </div>
    )}
  </>
);

const Apps = (ctx: Ctx) => (
  <>
    <div className="section">
      {isDesktop(ctx) ? (
        <Multi ctx={ctx} label="Apps people get on day one" help="Apps are sandboxed and updated automatically." pointer="/apps/preinstalled" match={(o) => o.kind === "flatpak"} />
      ) : (
        <p className="muted">This device type has no desktop, so desktop apps are not offered.</p>
      )}
    </div>
    <div className="section">
      <Multi ctx={ctx} label="System features" pointer="/apps/system" match={(o) => o.kind === "app" && o.id !== "app.kiosk-browser"} />
    </div>
  </>
);

const Security = (ctx: Ctx) => (
  <>
    <div className="section">
      <Choice ctx={ctx} label="Security level" pointer="/security/level" prefix="security.level." def="balanced"
        dangers={[{ value: "relaxed", title: "Lower the security level?", text: "Relaxed turns off several hardening settings (kernel lockdown, strict sysctls). Use it only for development devices." }]} />
      <Choice ctx={ctx} label="Disk encryption" pointer="/security/diskEncryption" prefix="disk." def="tpm2" toValue={(s) => s.replace("tpm2-pin", "tpm2+pin")} fromValue={(v) => v.replace("+", "-")}
        dangers={[{ value: "none", title: "Turn off disk encryption?", text: "Anyone who takes the device can read all files on it. Encryption with the TPM costs nothing for users." }]} />
      <Choice ctx={ctx} label="USB storage" pointer="/security/usbStorage" prefix="usb." def="allow" />
    </div>
    <div className="section">
      <Choice ctx={ctx} label="Admin access" pointer="/access/rootModel" prefix="access." def={["kiosk-cage", "thin-client"].includes(get(ctx.spec, "/foundation/profile") as string) ? "locked" : "admin-password"}
        dangers={[{ value: "developer", title: "Give full root access?", text: "Developer mode lets users change the system freely: verified boot can no longer guarantee the OS is untouched, and the build always needs a security review." }]} />
      {ctx.level !== "simple" && (
        <Select ctx={ctx} label="User accounts" pointer="/access/users/mode" options={[["", "Default for this device"], ["firstboot", "Create on first boot"], ["directory", "From the company directory"], ["predefined", "Predefined accounts"], ["autologin-kiosk", "Automatic sign-in (kiosk)"]]} />
      )}
    </div>
    {ctx.level !== "simple" && (
      <div className="section">
        <Choice ctx={ctx} label="Secure Boot" pointer="/security/secureBoot" prefix="secureboot." def="tenant-keys"
          dangers={[{ value: "off", title: "Turn off Secure Boot?", text: "Without Secure Boot, malware can replace the bootloader or kernel without being detected. Only use this for hardware that cannot enroll keys." }]} />
        <Multi ctx={ctx} label="Allow incoming connections" help="Everything else is blocked by the firewall." pointer="/security/firewall/allow"
          match={(o) => o.id.startsWith("fw.allow.")} toValue={(id) => id.slice(9)} fromValue={(v) => "fw.allow." + v} />
        <Toggle ctx={ctx} label="Automatic security updates" pointer="/security/autoUpdates" def={true}
          danger={{ on: false, title: "Turn off automatic updates?", text: "Devices will only update when you push an update. Security fixes may arrive late." }} />
      </div>
    )}
    {ctx.level === "expert" && (
      <div className="section">
        <Select ctx={ctx} label="Firewall default" pointer="/security/firewall/default" def="deny-inbound" options={[["deny-inbound", "Block incoming (recommended)"], ["allow-all", "Allow everything"]]} />
        <Toggle ctx={ctx} label="Text console login (Ctrl+Alt+F2)" pointer="/access/ttyConsole" />
      </div>
    )}
  </>
);

const Ecosystem = (ctx: Ctx) => {
  const prov = (get(ctx.spec, "/ecosystem/identity/provider") as string) ?? "local";
  const sync = (get(ctx.spec, "/ecosystem/sync/provider") as string) ?? "none";
  const profile = get(ctx.spec, "/foundation/profile") as string;
  return (
    <>
      <div className="section">
        <Toggle ctx={ctx} label="Manage devices from Foundry" hint="Updates, inventory, remote lock and wipe." pointer="/ecosystem/fleet/enroll" />
        {(get(ctx.spec, "/ecosystem/fleet/enroll") as boolean) && <Text ctx={ctx} label="Device group" pointer="/ecosystem/fleet/group" placeholder="front-office" />}
        {isDesktop(ctx) && <Toggle ctx={ctx} label="Company app store" hint="People can install apps you approve." pointer="/ecosystem/appStore/enabled" />}
      </div>
      {profile === "kiosk-cage" && (
        <div className="section">
          <Text ctx={ctx} label="Kiosk web address" type="url" pointer="/ecosystem/kiosk/url" placeholder="https://kiosk.example.com/" />
          <Text ctx={ctx} label="Reset after idle (minutes)" pointer="/ecosystem/kiosk/idleResetMinutes" number />
          <ListText ctx={ctx} label="Allowed domains" pointer="/ecosystem/kiosk/allowedDomains" placeholder="kiosk.example.com" />
        </div>
      )}
      {ctx.level !== "simple" && (
        <div className="section">
          <Choice ctx={ctx} label="Sign-in" pointer="/ecosystem/identity/provider" prefix="identity." def="local" />
          {prov === "oidc" && (
            <div className="grid2">
              <Text ctx={ctx} label="Issuer URL" type="url" pointer="/ecosystem/identity/issuer" placeholder="https://login.example.com" />
              <Text ctx={ctx} label="Client ID" pointer="/ecosystem/identity/clientId" />
            </div>
          )}
          {(prov === "ad" || prov === "ldap") && <Text ctx={ctx} label="Domain" pointer="/ecosystem/identity/domain" placeholder="corp.example.com" />}
          {isDesktop(ctx) && <Choice ctx={ctx} label="File sync" pointer="/ecosystem/sync/provider" prefix="ecosystem.sync." def="none" />}
          {sync === "webdav" && <Text ctx={ctx} label="WebDAV URL" type="url" pointer="/ecosystem/sync/url" />}
          {isDesktop(ctx) && <Toggle ctx={ctx} label="Phone & device handoff" pointer="/ecosystem/handoff/enabled" />}
        </div>
      )}
    </>
  );
};

const Branding = (ctx: Ctx) => (
  <>
    <div className="section grid2">
      <Text ctx={ctx} label="OS name" pointer="/branding/osName" hint="Shown at boot, sign-in and in Settings → About." />
      <Text ctx={ctx} label="OS id" pointer="/branding/osId" readOnly hint="Fixed by the project." />
      <Text ctx={ctx} label="Home page" type="url" pointer="/branding/homeUrl" placeholder="https://example.com" />
      <Text ctx={ctx} label="Support page" type="url" pointer="/branding/supportUrl" placeholder="https://example.com/help" />
    </div>
    <div className="section grid2">
      <Select ctx={ctx} label="Language" pointer="/localization/locale" def="en_US.UTF-8"
        options={[["en_US.UTF-8", "English (US)"], ["en_GB.UTF-8", "English (UK)"], ["en_IN.UTF-8", "English (India)"], ["hi_IN.UTF-8", "Hindi"], ["de_DE.UTF-8", "German"], ["fr_FR.UTF-8", "French"], ["es_ES.UTF-8", "Spanish"], ["pt_BR.UTF-8", "Portuguese (Brazil)"], ["ja_JP.UTF-8", "Japanese"], ["zh_CN.UTF-8", "Chinese (Simplified)"]]} />
      <Select ctx={ctx} label="Keyboard" pointer="/localization/keymap" def="us" options={[["us", "US"], ["gb", "UK"], ["de", "German"], ["fr", "French"], ["es", "Spanish"], ["in", "Indian"], ["jp", "Japanese"]]} />
      <Text ctx={ctx} label="Time zone" pointer="/localization/timezone" placeholder="UTC" hint="e.g. Asia/Kolkata, Europe/Berlin" />
    </div>
  </>
);

const Expert = (ctx: Ctx) => {
  const repos = (get(ctx.spec, "/expert/extraRepos") as Record<string, unknown>[] | undefined) ?? [];
  const list = (ptr: string) => (get(ctx.spec, ptr) as string[] | undefined) ?? [];
  const addTo = (ptr: string, d: string, label: string) => ctx.store.set(ptr, [...list(ptr).filter((x) => x !== d), d], label);
  const rm = (ptr: string, d: string, label: string) => {
    const n = list(ptr).filter((x) => x !== d);
    ctx.store.set(ptr, n.length ? n : undefined, label);
  };
  const Assets = ({ ptr, label }: { ptr: string; label: string }) => (
    <ul className="small">
      {list(ptr).map((d) => (
        <li key={d} className="row"><code>{d.slice(0, 23)}…</code><button className="ghost small" onClick={() => rm(ptr, d, label)}>Remove</button></li>
      ))}
    </ul>
  );
  return (
    <>
      <div className="issue" style={{ background: "var(--warn-soft)", borderColor: "var(--warn)" }}>
        <strong>Expert content always needs a security review.</strong>
        <span className="small">Builds with custom packages, files or scripts are scanned, run in an isolated build machine without network for your scripts, and need approval before they are signed. They can never get the “verified” trust tier.</span>
      </div>
      <div className="section">
        <ListText ctx={ctx} label="Extra packages from the base archive" pointer="/expert/packages" placeholder="htop&#10;jq" hint="Package names, one per line." />
      </div>
      <div className="section">
        <h3>Custom .deb packages</h3>
        <AssetUpload kind="deb" accept=".deb" mediaType={mediaFor} label="Upload a .deb" onDone={(d) => addTo("/apps/custom", d, "Custom package")} />
        <Assets ptr="/apps/custom" label="Custom package" />
        <p className="muted small">Installed with dependency resolution from the base archive. Maintainer scripts are flagged in the security review.</p>
      </div>
      <div className="section">
        <h3>Files added to the image</h3>
        <AssetUpload kind="overlay" accept=".tar,.tar.gz,.tgz" mediaType={mediaFor} label="Upload a .tar archive (extracted at /)" onDone={(d) => addTo("/expert/overlays", d, "Overlay")} />
        <Assets ptr="/expert/overlays" label="Overlay" />
      </div>
      <div className="section">
        <h3>Scripts run while building</h3>
        <AssetUpload kind="script" accept=".sh" mediaType={mediaFor} label="Upload a shell script (runs as root in the image, no network, 10 minute limit)" onDone={(d) => addTo("/expert/postinstScripts", d, "Script")} />
        <Assets ptr="/expert/postinstScripts" label="Script" />
      </div>
      <div className="section">
        <h3>Image tool settings</h3>
        <AssetUpload kind="mkosi-dropin" accept=".conf" mediaType={mediaFor} label="Upload an mkosi drop-in (.conf, allowlisted keys only)" onDone={(d) => addTo("/expert/mkosiDropins", d, "Drop-in")} />
        <Assets ptr="/expert/mkosiDropins" label="Drop-in" />
      </div>
      <div className="section">
        <h3>Extra package repositories</h3>
        {repos.map((_, i) => (
          <div key={i} className="card stack">
            <div className="grid2">
              <Text ctx={ctx} label="Repository URL" type="url" pointer={`/expert/extraRepos/${i}/url`} />
              <Text ctx={ctx} label="Suite" pointer={`/expert/extraRepos/${i}/suite`} placeholder="noble" />
              <Text ctx={ctx} label="Signing key fingerprint" pointer={`/expert/extraRepos/${i}/keyFingerprint`} hint="40 hex characters, uppercase." />
              <Text ctx={ctx} label="Pin" pointer={`/expert/extraRepos/${i}/pin`} placeholder="origin repo.example.com" hint="Packages from this repo only replace base packages when pinned." />
            </div>
            <AssetUpload kind="gpg-key" accept=".asc,.gpg" mediaType={mediaFor} label="Signing key (public, ASCII-armored)"
              onDone={(d, info) => {
                ctx.store.set(`/expert/extraRepos/${i}/keyAsset`, d, "Repo key");
                if (info.fingerprint) ctx.store.set(`/expert/extraRepos/${i}/keyFingerprint`, info.fingerprint, "Repo key");
              }} />
            <div><button className="danger small" onClick={() => { const n = repos.filter((_, j) => j !== i); ctx.store.set("/expert/extraRepos", n.length ? n : undefined, "Remove repo"); }}>Remove repository</button></div>
          </div>
        ))}
        <div><button onClick={() => ctx.store.set("/expert/extraRepos", [...repos, { url: "https://", keyFingerprint: "", keyAsset: "", pin: "origin " }], "Add repo")}>Add repository</button></div>
      </div>
    </>
  );
};

export const STEPS: Step[] = [
  { id: "foundation", label: "Foundation", prefixes: ["/foundation", "/metadata", "/hardware", "/kernel", "/memory"], render: Foundation },
  { id: "look", label: "Look & feel", prefixes: ["/look"], render: Look },
  { id: "apps", label: "Apps", prefixes: ["/apps"], render: Apps },
  { id: "security", label: "Security & access", prefixes: ["/security", "/access"], render: Security },
  { id: "ecosystem", label: "Ecosystem", prefixes: ["/ecosystem"], render: Ecosystem },
  { id: "branding", label: "Branding", prefixes: ["/branding", "/localization"], render: Branding },
  { id: "expert", label: "Expert", prefixes: ["/expert"], render: Expert, level: "expert" },
];

export const stepFor = (ptr: string) => STEPS.find((s) => s.prefixes.some((p) => ptr === p || ptr.startsWith(p + "/")))?.id ?? "foundation";
