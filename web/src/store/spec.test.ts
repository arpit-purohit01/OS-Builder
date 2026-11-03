import { describe, expect, it } from "vitest";
import { SpecStore } from "./spec";

describe("SpecStore", () => {
  it("undo/redo restores exact documents", () => {
    const s = new SpecStore({ metadata: { name: "a" } });
    s.set("/look/theme/accent", "#111111", "accent");
    s.set("/security/level", "hardened", "level");
    expect(s.get()).toEqual({ metadata: { name: "a" }, look: { theme: { accent: "#111111" } }, security: { level: "hardened" } });
    s.undo();
    expect(s.get()).toEqual({ metadata: { name: "a" }, look: { theme: { accent: "#111111" } } });
    s.undo();
    expect(s.get()).toEqual({ metadata: { name: "a" } });
    expect(s.canUndo).toBe(false);
    s.redo();
    s.redo();
    expect(s.get()).toEqual({ metadata: { name: "a" }, look: { theme: { accent: "#111111" } }, security: { level: "hardened" } });
  });
  it("coalesces typing into one undo step", () => {
    const s = new SpecStore({});
    for (const v of ["A", "Au", "Aur", "Auro"]) s.set("/branding/osName", v, "name");
    s.set("/branding/osName", "Aurora", "name");
    expect(s.history.length).toBe(1);
    s.undo();
    expect(s.get()).toEqual({});
    s.redo();
    expect(s.get()).toEqual({ branding: { osName: "Aurora" } });
  });
  it("tracks dirty state", () => {
    const s = new SpecStore({ a: 1 });
    expect(s.dirty).toBe(false);
    s.set("/a", 2);
    expect(s.dirty).toBe(true);
    s.markSaved();
    expect(s.dirty).toBe(false);
  });
  it("caps history at 200", () => {
    const s = new SpecStore({});
    for (let i = 0; i < 250; i++) s.set(`/k${i}`, i, `k${i}`);
    expect(s.history.length).toBe(200);
  });
});
