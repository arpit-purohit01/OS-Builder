import { useState, useCallback } from "react";
import { Link } from "react-router-dom";
import { api, orgPath, type SpecVersion } from "../lib/api";
import { ago } from "../lib/session";
import { diff } from "../lib/jsonpatch";

export interface SpecVersionWithDiff extends SpecVersion {
  diff?: Array<{ op: string; path: string; value?: unknown }>;
  changesCount: number;
}

interface SpecVersionsTimelineProps {
  projectId: string;
  versions: SpecVersion[];
  currentVersionId?: string;
  onBuild: (versionId: string) => void;
  onSelect: (versionId: string) => void;
  currentOrgId: string;
}

interface DiffData {
  ops: any[];
  loading: boolean;
  error?: string;
}

// Type for the spec metadata
interface SpecMetadata {
  version?: string;
  displayName?: string;
  name?: string;
}

export default function SpecVersionsTimeline({
  projectId,
  versions,
  currentVersionId,
  onBuild,
  onSelect,
  currentOrgId,
}: SpecVersionsTimelineProps) {
  const [expandedVersion, setExpandedVersion] = useState<string | null>(null);
  const [diffs, setDiffs] = useState<Record<string, DiffData>>({});

  const loadDiff = useCallback(async (versionId: string) => {
    setDiffs(prev => ({ ...prev, [versionId]: { ops: [], loading: true, error: undefined } }));
    try {
      const version = versions.find(v => v.id === versionId);
      const parentId = version?.parentId;
      let ops: any[] = [];

      if (parentId) {
        const [child, parentSpec] = await Promise.all([
          api<{ spec: any }>("GET", orgPath(`/specs/${versionId}`)),
          api<{ spec: any }>("GET", orgPath(`/specs/${parentId}`)),
        ]);
        if (child?.spec && parentSpec?.spec) {
          ops = diff(parentSpec.spec, child.spec);
        }
      }

      setDiffs(prev => ({ ...prev, [versionId]: { ops, loading: false, error: undefined } }));
    } catch (error) {
      setDiffs(prev => ({ ...prev, [versionId]: { ops: [], loading: false, error: (error as Error).message } }));
    }
  }, [versions]);

  const toggleExpand = useCallback((versionId: string) => {
    setExpandedVersion(prev => prev === versionId ? null : versionId);
    if (expandedVersion !== versionId && !diffs[versionId]) {
      loadDiff(versionId);
    }
  }, [expandedVersion, diffs, loadDiff]);

  const getVersionLabel = (v: SpecVersion) => {
    const spec = v.spec as { metadata?: SpecMetadata };
    return spec?.metadata?.version ?? spec?.metadata?.displayName ?? spec?.metadata?.name ?? "—";
  };

  if (versions.length === 0) {
    return (
      <div className="card flush" style={{ padding: 32, textAlign: "center" }}>
        <p className="muted">No saved versions yet.</p>
        <button className="btn primary" style={{ marginTop: 16 }} onClick={() => window.location.href = `/o/${currentOrgId}/p/${projectId}/edit`}>
          Open builder to create one
        </button>
      </div>
    );
  }

  return (
    <div className="card flush">
      <table style={{ width: "100%", borderCollapse: "collapse" }}>
        <thead>
          <tr style={{ borderBottom: "2px solid var(--line)" }}>
            <th style={{ padding: "8px", textAlign: "left" }}>Version</th>
            <th style={{ padding: "8px", textAlign: "left" }}>Digest</th>
            <th style={{ padding: "8px", textAlign: "left" }}>Saved</th>
            <th style={{ padding: "8px", textAlign: "left" }}>Changes</th>
            <th style={{ padding: "8px", textAlign: "left" }}>Actions</th>
          </tr>
        </thead>
        <tbody>
          {versions.map((v, i) => (
            <>
              <tr
                key={v.id}
                style={{
                  background: v.id === currentVersionId ? "var(--primary-soft)" : i % 2 === 1 ? "var(--panel-2)" : "transparent",
                  borderBottom: "1px solid var(--line)",
                  cursor: "pointer",
                }}
                onClick={() => onSelect?.(v.id)}
              >
                <td style={{ padding: "12px 12px" }}>
                  <div className="row" style={{ gap: 8, alignItems: "center" }}>
                    <span style={{ fontWeight: 600 }}>{getVersionLabel(v)}</span>
                    {v.hasExpert && <span className="badge warn" style={{ fontSize: 10 }}>expert</span>}
                    {v.id === currentVersionId && <span className="badge ok" style={{ fontSize: 10 }}>current</span>}
                  </div>
                </td>
                <td style={{ padding: "12px" }}><code title={v.digest} style={{ fontSize: 11 }}>{v.digest.slice(7, 19)}</code></td>
                <td style={{ padding: "12px" }} className="muted">{v.createdAt ? ago(v.createdAt) : "—"}</td>
                <td style={{ padding: "12px" }}>
                  <button
                    className="ghost small"
                    onClick={(e) => { e.stopPropagation(); toggleExpand(v.id); }}
                    aria-expanded={expandedVersion === v.id}
                  >
                    {diffs[v.id] ? (diffs[v.id].loading ? "Loading…" : expandedVersion === v.id ? "Hide diff" : `Show diff (${diffs[v.id].ops?.length ?? 0})`) : "Show diff"}
                  </button>
                </td>
                <td style={{ padding: "12px" }}>
                  <div className="row" style={{ gap: 4 }}>
                    <Link className="btn small" to={`/o/${currentOrgId}/p/${projectId}/edit?version=${v.id}`}>Open</Link>
                    <button className="small" onClick={(e) => { e.stopPropagation(); onBuild(v.id); }}>Build</button>
                  </div>
                </td>
              </tr>
              {expandedVersion === v.id && (
                <tr style={{ background: "var(--panel-2)" }}>
                  <td colSpan={5} style={{ padding: 16 }}>
                    <div className="card stack" style={{ marginTop: 8 }}>
                      <div className="row spread" style={{ paddingBottom: 8, borderBottom: "1px solid var(--line)" }}>
                        <h4 style={{ margin: 0 }}>JSON Patch Diff (RFC 6902)</h4>
                        <span className="small muted">Parent: {getVersionLabel(versions.find(p => p.id === v.parentId) as SpecVersion)}</span>
                      </div>
                      <div style={{ maxHeight: 400, overflow: "auto" }}>
                        {diffs[v.id]?.loading ? (
                          <p className="muted" style={{ padding: 16, textAlign: "center" }}>Loading diff…</p>
                        ) : diffs[v.id]?.error ? (
                          <p className="err" style={{ padding: 16, textAlign: "center" }}>Failed to load diff: {diffs[v.id].error}</p>
                        ) : !diffs[v.id]?.ops?.length ? (
                          <p className="muted" style={{ padding: 16, textAlign: "center" }}>No changes</p>
                        ) : (
                          <pre style={{ padding: 12, margin: 0, background: "var(--panel-2)", color: "var(--text)", maxHeight: 300, overflow: "auto", fontSize: 11 }}>
                            {JSON.stringify(diffs[v.id].ops, null, 2)}
                          </pre>
                        )}
                      </div>
                    </div>
                  </td>
                </tr>
              )}
            </>
          ))}
        </tbody>
      </table>
    </div>
  );
}