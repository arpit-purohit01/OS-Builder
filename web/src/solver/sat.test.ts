// @vitest-environment node
import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { solve, mus, check, type Formula, type Lit } from "./sat";

interface Case {
  assumptions: Lit[];
  sat: boolean;
  model?: boolean[];
  implied?: Lit[];
  mus?: Lit[];
}

const fx = JSON.parse(
  readFileSync(fileURLToPath(new URL("./testdata/cases.json", import.meta.url)), "utf8"),
) as { formula: Formula; cases: Case[] };

describe("TS solver agrees with Go solver", () => {
  it("has 500 cases", () => expect(fx.cases.length).toBe(500));
  it("matches SAT/model/implied/MUS exactly", () => {
    let mismatches = 0;
    for (const c of fx.cases) {
      const r = solve(fx.formula.cnf, c.assumptions);
      if (r.sat !== c.sat) { mismatches++; continue; }
      if (r.sat) {
        expect(r.model).toEqual(c.model);
        expect(r.implied).toEqual(c.implied);
      } else {
        expect(mus(fx.formula.cnf, c.assumptions)).toEqual(c.mus);
      }
    }
    expect(mismatches).toBe(0);
  });
  it("check() names conflicting options", () => {
    const u = fx.cases.find((c) => !c.sat)!;
    const sel = u.assumptions.map((l) => ({ id: fx.formula.vars[Math.abs(l) - 1], on: l > 0 }));
    const r = check(fx.formula, sel);
    expect(r.ok).toBe(false);
    expect(r.conflict!.length).toBeGreaterThan(0);
  });
  it("solves in under 50ms per selection", () => {
    const t0 = performance.now();
    for (const c of fx.cases.slice(0, 100)) solve(fx.formula.cnf, c.assumptions);
    expect((performance.now() - t0) / 100).toBeLessThan(50);
  });
});
