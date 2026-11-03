import { describe, expect, it } from "vitest";
import { apply, diff, setPath } from "./jsonpatch";

describe("jsonpatch", () => {
  it("diff+apply round trips both ways", () => {
    const a = { x: 1, y: { z: [1, 2], w: "a" }, gone: true };
    const b = { x: 2, y: { z: [1, 3, 4], w: "a", n: null }, added: { k: "v/~" } };
    expect(apply(a, diff(a, b))).toEqual(b);
    expect(apply(b, diff(b, a))).toEqual(a);
    expect(a.y.z).toEqual([1, 2]); // immutable
  });
  it("setPath creates and prunes", () => {
    const d = setPath({}, "/look/theme/accent", "#000000");
    expect(d).toEqual({ look: { theme: { accent: "#000000" } } });
    expect(setPath(d, "/look/theme/accent", undefined)).toEqual({});
  });
  it("array ops", () => {
    expect(apply({ a: [1, 2] }, [{ op: "add", path: "/a/-", value: 3 }, { op: "remove", path: "/a/0" }])).toEqual({ a: [2, 3] });
  });
});
