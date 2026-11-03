// @vitest-environment node
import { describe, expect, it } from "vitest";
import cases from "./testdata/selections.json";
import { selections } from "./selections";

describe("selections() matches Go catalog.Selections", () => {
  it(`agrees on ${cases.length} specs`, () => {
    expect(cases.length).toBeGreaterThan(300);
    for (const c of cases as { spec: unknown; selections: { id: string; pointer: string }[] }[]) {
      expect(selections(c.spec)).toEqual(c.selections);
    }
  });
});
