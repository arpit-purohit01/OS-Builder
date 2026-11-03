// Minimal RFC 6902 JSON Patch: diff, apply (immutable), used for undo/redo and
// for sending small spec updates (POST .../specs with {parent, patch}).
import { format, parse } from "./pointer";

export type Op =
  | { op: "add"; path: string; value: unknown }
  | { op: "remove"; path: string }
  | { op: "replace"; path: string; value: unknown };

const isObj = (v: unknown): v is Record<string, unknown> => v !== null && typeof v === "object" && !Array.isArray(v);

export function equal(a: unknown, b: unknown): boolean {
  if (a === b) return true;
  if (Array.isArray(a) && Array.isArray(b)) return a.length === b.length && a.every((x, i) => equal(x, b[i]));
  if (isObj(a) && isObj(b)) {
    const ka = Object.keys(a), kb = Object.keys(b);
    return ka.length === kb.length && ka.every((k) => k in b && equal(a[k], b[k]));
  }
  return false;
}

export function diff(a: unknown, b: unknown, path: (string | number)[] = []): Op[] {
  if (equal(a, b)) return [];
  if (isObj(a) && isObj(b)) {
    const ops: Op[] = [];
    for (const k of Object.keys(a)) if (!(k in b)) ops.push({ op: "remove", path: format([...path, k]) });
    for (const k of Object.keys(b)) {
      if (!(k in a)) ops.push({ op: "add", path: format([...path, k]), value: clone(b[k]) });
      else ops.push(...diff(a[k], b[k], [...path, k]));
    }
    return ops;
  }
  return [{ op: "replace", path: format(path), value: clone(b) }];
}

export function clone<T>(v: T): T {
  return v === undefined ? v : (JSON.parse(JSON.stringify(v)) as T);
}

function setIn(doc: unknown, keys: string[], fn: (parent: unknown, key: string) => unknown): unknown {
  if (keys.length === 0) return fn(undefined, "");
  const [k, ...rest] = keys;
  if (Array.isArray(doc)) {
    const arr = doc.slice();
    if (rest.length === 0) return fn(arr, k);
    const i = Number(k);
    arr[i] = setIn(arr[i], rest, fn);
    return arr;
  }
  if (!isObj(doc)) throw new Error(`patch: path does not exist at ${k}`);
  const o: Record<string, unknown> = { ...doc };
  if (rest.length === 0) return fn(o, k);
  if (!(k in o)) throw new Error(`patch: missing ${k}`);
  o[k] = setIn(o[k], rest, fn);
  return o;
}

export function apply<T>(doc: T, ops: Op[]): T {
  let cur: unknown = doc;
  for (const op of ops) {
    const keys = parse(op.path);
    if (keys.length === 0) {
      if (op.op === "remove") throw new Error("patch: cannot remove root");
      cur = clone(op.value);
      continue;
    }
    cur = setIn(cur, keys, (parent, key) => {
      if (Array.isArray(parent)) {
        const i = key === "-" ? parent.length : Number(key);
        if (op.op === "add") parent.splice(i, 0, clone(op.value));
        else if (op.op === "remove") parent.splice(i, 1);
        else parent[i] = clone(op.value);
        return parent;
      }
      const o = parent as Record<string, unknown>;
      if (op.op === "remove") {
        if (!(key in o)) throw new Error(`patch: remove missing ${op.path}`);
        delete o[key];
      } else {
        if (op.op === "replace" && !(key in o)) throw new Error(`patch: replace missing ${op.path}`);
        o[key] = clone(op.value);
      }
      return o;
    });
  }
  return cur as T;
}

// setPath returns a new document with value at ptr, creating objects on the
// way; undefined removes the key and prunes empty parent objects.
export function setPath<T>(doc: T, ptr: string, value: unknown): T {
  const keys = parse(ptr);
  const rec = (node: unknown, i: number): unknown => {
    const o: Record<string, unknown> = isObj(node) ? { ...node } : {};
    const k = keys[i];
    if (i === keys.length - 1) {
      if (value === undefined) delete o[k];
      else o[k] = clone(value);
    } else {
      const child = rec(o[k], i + 1);
      if (isObj(child) && Object.keys(child).length === 0) delete o[k];
      else o[k] = child;
    }
    return o;
  };
  return rec(doc, 0) as T;
}
