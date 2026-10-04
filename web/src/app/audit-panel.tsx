"use client";
import { useEffect, useState } from "react";
import { api, errorMessage } from "@/lib/api";
import { jsonText } from "@/lib/json";
type Entry = {
  id: number;
  action: string;
  actor_id: string;
  source: string;
  reason: string;
  environment_id?: string;
  created_at: string;
  before_revision?: number;
  after_revision?: number;
  details: unknown;
};
export default function AuditPanel({ projectID }: { projectID: string }) {
  const [entries, setEntries] = useState<Entry[]>([]);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [refresh, setRefresh] = useState(0);
  const [before, setBefore] = useState<number | undefined>();
  useEffect(() => {
    const controller = new AbortController();
    setLoading(true);
    setError("");
    api<Entry[]>(
      `/v1/projects/${projectID}/audit${before ? `?before_id=${before}` : ""}`,
      { signal: controller.signal },
    )
      .then(setEntries)
      .catch((error) => {
        if (!controller.signal.aborted) setError(errorMessage(error));
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
    return () => controller.abort();
  }, [projectID, before, refresh]);
  return (
    <section className="panel">
      <div className="section-heading">
        <div>
          <h2>Audit history</h2>
          <p className="muted">
            Committed changes across all project environments.
          </p>
        </div>
        <button
          className="secondary"
          onClick={() => {
            setBefore(undefined);
            setRefresh((n) => n + 1);
          }}
        >
          Latest changes
        </button>
      </div>
      {error && (
        <p className="notice error" role="alert">
          {error}
        </p>
      )}
      {loading ? (
        <p role="status">Loading audit history…</p>
      ) : entries.length ? (
        <div className="table-scroll">
          <table>
            <thead>
              <tr>
                <th>Change</th>
                <th>Actor</th>
                <th>Reason</th>
                <th>Time</th>
              </tr>
            </thead>
            <tbody>
              {entries.map((entry) => (
                <tr key={entry.id}>
                  <td>
                    <strong>{entry.action}</strong>
                    <small>
                      {entry.source}
                      {entry.after_revision !== undefined
                        ? ` · revision ${entry.after_revision}`
                        : ""}
                    </small>
                    <details>
                      <summary>Details</summary>
                      <pre>{jsonText(entry.details, true)}</pre>
                      {entry.environment_id && (
                        <small>{entry.environment_id}</small>
                      )}
                    </details>
                  </td>
                  <td>{entry.actor_id}</td>
                  <td>{entry.reason}</td>
                  <td>
                    <time dateTime={entry.created_at}>
                      {new Date(entry.created_at).toLocaleString()}
                    </time>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        <p className="empty">No more audit entries.</p>
      )}
      {entries.length === 100 && (
        <button
          className="secondary"
          disabled={loading}
          onClick={() => setBefore(entries.at(-1)?.id)}
        >
          Older changes
        </button>
      )}
    </section>
  );
}
