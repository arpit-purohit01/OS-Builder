// RFC 6901 JSON pointers.
export type JSONValue = null | boolean | number | string | JSONValue[] | { [k: string]: JSONValue };

export function parse(ptr: string): string[] {
  if (ptr === "") return [];
  if (!ptr.startsWith("/")) throw new Error(`invalid pointer ${ptr}`);
  return ptr.slice(1).split("/").map((s) => s.replace(/~1/g, "/").replace(/~0/g, "~"));
}

export function format(parts: (string | number)[]): string {
  return parts.map((p) => "/" + String(p).replace(/~/g, "~0").replace(/\//g, "~1")).join("");
}

export function get(doc: unknown, ptr: string): unknown {
  let cur: unknown = doc;
  for (const k of parse(ptr)) {
    if (cur === null || typeof cur !== "object") return undefined;
    cur = (cur as Record<string, unknown>)[k];
  }
  return cur;
}
