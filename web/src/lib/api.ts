// Fetch wrapper for the Foundry API: cookie session + CSRF header, RFC 9457
// problem documents surfaced as ApiError with field errors at JSON pointers.
export interface FieldError {
  pointer?: string;
  code: string;
  message: string;
}

export class ApiError extends Error {
  constructor(
    public status: number,
    public title: string,
    public detail?: string,
    public errors: FieldError[] = [],
    public layer?: string,
  ) {
    super(detail ? `${title}: ${detail}` : title);
  }
}

let csrf = "";
let org = "";
export const setCSRF = (t: string) => (csrf = t);
export const setOrg = (o: string) => (org = o);
export const currentOrg = () => org;

export async function api<T = unknown>(method: string, path: string, body?: unknown, headers: Record<string, string> = {}): Promise<T> {
  const h: Record<string, string> = { Accept: "application/json", ...headers };
  let payload: BodyInit | undefined;
  if (body !== undefined) {
    if (typeof body === "string") {
      payload = body;
      h["Content-Type"] ??= "application/yaml";
    } else {
      payload = JSON.stringify(body);
      h["Content-Type"] = "application/json";
    }
  }
  if (method !== "GET" && method !== "HEAD" && csrf) h["X-CSRF-Token"] = csrf;
  const res = await fetch(path, { method, headers: h, body: payload, credentials: "same-origin" });
  if (res.status === 204) return undefined as T;
  const ct = res.headers.get("Content-Type") ?? "";
  const data = ct.includes("json") ? await res.json() : await res.text();
  if (!res.ok) {
    const p = (typeof data === "object" ? data : {}) as Record<string, unknown>;
    throw new ApiError(res.status, (p.title as string) ?? res.statusText, p.detail as string | undefined, (p.errors as FieldError[]) ?? [], p.layer as string | undefined);
  }
  return data as T;
}

export const orgPath = (p: string) => `/v1/orgs/${org}${p}`;

// ---- shapes ---------------------------------------------------------------
export interface Org { id: string; name: string; slug: string; plan: string; expertAllowed: boolean }
export interface Membership { orgId: string; role: string; org: Org }
export interface User { id: string; email: string; name: string }
export interface Project { id: string; name: string; osId: string; createdAt: string }
export interface SpecVersion { id: string; digest: string; parentId?: string | null; spec: Record<string, unknown>; createdAt: string; hasExpert?: boolean; createdBy?: string }
export interface Build {
  id: string; projectId: string; specVersionId: string; arch: string; state: string; tier?: string | null;
  riskScore?: number | null; failure?: { code?: string; message?: string; stage?: string } | null; roothash?: string;
  reproduced?: boolean | null; createdAt: string; startedAt?: string; finishedAt?: string; predictedMinutes: number; attempt: number;
}
export interface Artifact { kind: string; digest: string; size: number; signed: boolean; name: string; url?: string }
export interface Finding { id: string; rule: string; severity: string; path?: string; weight: number; detail: unknown; acceptedBy?: string | null; acceptedReason?: string | null }
export interface Review { id: string; reviewer: string; decision: string; comment?: string; createdAt: string }
export interface EgressEvent { ts: string; method: string; host: string; port: number; allowed: boolean; reason: string; bytes: number }
export interface CatalogOption { id: string; kind: string; label: string; oneOf?: string; available: boolean; levels?: string[]; help?: string; requires?: string[]; conflicts?: string[]; risk?: string[]; hcl?: { status: string; tested: string[]; notes?: string } }
export interface Catalog { version: string; base: string; arch: string; options: CatalogOption[]; bases: { id: string; tested: boolean }[] }

export async function sha256Hex(data: ArrayBuffer): Promise<string> {
  const d = await crypto.subtle.digest("SHA-256", data);
  return Array.from(new Uint8Array(d), (b) => b.toString(16).padStart(2, "0")).join("");
}

// upload sends a file through the two-step signed-URL flow and returns the asset.
export async function upload(kind: string, mediaType: string, file: File): Promise<{ digest: string; status: string; validation?: unknown }> {
  const buf = await file.arrayBuffer();
  const hex = await sha256Hex(buf);
  const st = await api<{ uploadId: string; url: string; digest: string }>("POST", orgPath("/assets/uploads"), { kind, mediaType, size: file.size, sha256: hex });
  const res = await fetch(st.url, { method: "PUT", body: buf, headers: { "Content-Type": "application/octet-stream" } });
  const data = await res.json();
  if (!res.ok && res.status !== 422) throw new ApiError(res.status, data.title ?? "Upload failed", data.detail);
  return data;
}
