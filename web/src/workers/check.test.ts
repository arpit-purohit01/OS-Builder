// @vitest-environment node
import { describe, expect, it } from "vitest";
import fx from "../solver/testdata/cases.json";
import { checkSpec } from "./check";
import { apply } from "../lib/jsonpatch";
import { setPath } from "../lib/jsonpatch";
import type { Formula } from "../solver/sat";

const f = (fx as unknown as { formula: Formula }).formula;
const labels: Record<string, string> = Object.fromEntries(f.vars.map((v) => [v, v]));

const lab = {
  apiVersion: "foundry.dev/v1alpha1", kind: "OSSpec", metadata: { name: "lab", version: "1.0.0" },
  foundation: { base: "ubuntu-24.04", profile: "desktop-gnome" },
  access: { rootModel: "locked" },
  apps: { preinstalled: ["flatpak:org.mozilla.firefox", "flatpak:com.github.tchx84.Flatseal"] },
};

describe("checkSpec", () => {
  it("accepts a valid spec and reports implied options", () => {
    const r = checkSpec(f, labels, { ...lab, apps: { preinstalled: ["flatpak:org.mozilla.firefox"] } });
    expect(r.ok).toBe(true);
    expect(r.implied).toContain("app.flatpak");
  });
  it("explains a conflict and every offered fix resolves it", () => {
    const r = checkSpec(f, labels, lab);
    expect(r.ok).toBe(false);
    expect(r.issues[0].options.sort()).toEqual(["access.locked", "flatpak:com.github.tchx84.Flatseal"].sort());
    expect(r.fixes.length).toBeGreaterThan(0);
    for (const fix of r.fixes) {
      let next: unknown;
      if (fix.remove) next = apply(lab, [{ op: "remove", path: fix.pointer }]);
      else next = setPath(lab, fix.pointer, fix.value);
      expect(checkSpec(f, labels, next).ok, fix.label).toBe(true);
    }
    expect(r.fixes.map((x) => x.label)).toContain("Remove flatpak:com.github.tchx84.Flatseal");
    expect(r.fixes.map((x) => x.label)).toContain("Use access.developer");
  });
});
