"use client";
import { useEffect, useState } from "react";
import { api, errorMessage } from "@/lib/api";
import { label, percent } from "@/lib/experiments";
import type { Cohort, Results } from "@/lib/experiments";
export default function ResultsPanel({
  projectID,
  runID,
}: {
  projectID: string;
  runID: string;
}) {
  const [results, setResults] = useState<Results | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(true);
  const [refresh, setRefresh] = useState(0);
  const [cohort, setCohort] = useState<Cohort>("provisional");
  useEffect(() => {
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout>;
    async function read() {
      setBusy(true);
      try {
        const value = await api<Results>(
          `/v1/projects/${projectID}/experiments/${runID}/results`,
          { signal: controller.signal },
        );
        if (!controller.signal.aborted) {
          setResults(value);
          setError("");
        }
      } catch (error) {
        if (!controller.signal.aborted) setError(errorMessage(error));
      } finally {
        if (!controller.signal.aborted) {
          setBusy(false);
          timer = setTimeout(read, 5000);
        }
      }
    }
    void read();
    return () => {
      controller.abort();
      clearTimeout(timer);
    };
  }, [projectID, runID, refresh]);
  return (
    <section className="panel">
      <div className="section-heading">
        <div>
          <h2>Experiment results</h2>
          <p className="muted">
            Listing completion within 30 minutes of a user’s first explicit
            exposure.
          </p>
        </div>
        <button
          className="secondary"
          disabled={busy}
          onClick={() => setRefresh((n) => n + 1)}
        >
          Refresh results
        </button>
      </div>
      {error && (
        <p className="notice error" role="alert">
          {error}
          {results && " The last successful snapshot is shown below."}
        </p>
      )}
      {!results ? (
        <p role="status">
          {busy ? "Loading results…" : "Results unavailable."}
        </p>
      ) : (
        <>
          <div className="results-context">
            <label>
              Measurement cohort
              <select
                aria-label="Measurement cohort"
                value={cohort}
                onChange={(e) => setCohort(e.target.value as Cohort)}
              >
                <option value="provisional">Provisional</option>
                <option value="finalized">Finalized</option>
                <option value="total">Total</option>
              </select>
            </label>
            <p className="fine">
              Snapshot:{" "}
              <time dateTime={results.as_of}>
                {new Date(results.as_of).toLocaleString()}
              </time>
              <br />
              Refreshes every 5 seconds while this screen is open.
            </p>
          </div>
          <p className="notice">
            Provisional counts can change as late events arrive. Finalized
            cohorts mature after the 30-minute window plus the 24-hour lateness
            allowance.
          </p>
          <div className="table-scroll">
            <table aria-label="Variant conversion results">
              <thead>
                <tr>
                  <th>Variant</th>
                  <th>Allocation</th>
                  <th>Exposed users</th>
                  <th>Completed users</th>
                  <th>Conversion</th>
                  <th>95% Wilson interval</th>
                </tr>
              </thead>
              <tbody>
                {results.variants.map((v) => {
                  const rate = v[`${cohort}_rate`];
                  return (
                    <tr key={v.id}>
                      <td>
                        <strong>{v.id}</strong>
                        {v.id === results.control_variant_id && (
                          <small>Control</small>
                        )}
                      </td>
                      <td>{(v.weight_bp / 100).toFixed(2)}%</td>
                      <td>{v[cohort].exposed}</td>
                      <td>{v[cohort].converted}</td>
                      <td>
                        {percent(rate.rate)}
                        {rate.rate !== null && (
                          <div className="rate-track" aria-hidden="true">
                            <span style={{ width: `${rate.rate * 100}%` }} />
                          </div>
                        )}
                      </td>
                      <td>
                        {rate.confidence_interval
                          ? `${percent(rate.confidence_interval.lower)} – ${percent(rate.confidence_interval.upper)}`
                          : "—"}
                        <small>{label(rate.status)}</small>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
          <div className="section-heading result-section">
            <h3>Comparison with control</h3>
            <span
              className={`badge ${results.sample_ratio[cohort].status === "mismatch" ? "warning" : ""}`}
            >
              Sample ratio: {label(results.sample_ratio[cohort].status)}
            </span>
          </div>
          <div className="table-scroll">
            <table aria-label="Variant comparisons">
              <thead>
                <tr>
                  <th>Treatment</th>
                  <th>Absolute lift</th>
                  <th>Relative lift</th>
                  <th>p-value</th>
                  <th>Inference</th>
                </tr>
              </thead>
              <tbody>
                {results.comparisons.map((c) => {
                  const stat = c[cohort];
                  return (
                    <tr key={c.variant_id}>
                      <td>{c.variant_id}</td>
                      <td>
                        {stat.absolute_lift === null
                          ? "—"
                          : `${(100 * stat.absolute_lift).toFixed(2)} pp`}
                      </td>
                      <td>
                        {percent(stat.relative_lift)}
                        {stat.relative_lift_reason && (
                          <small>{label(stat.relative_lift_reason)}</small>
                        )}
                      </td>
                      <td>
                        {stat.p_value === null
                          ? "—"
                          : stat.p_value.toPrecision(4)}
                      </td>
                      <td>
                        {label(stat.status)}
                        {stat.reason && <small>{label(stat.reason)}</small>}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
          <details>
            <summary>Measurement notes and quality</summary>
            <ul className="fine">
              {results.inference_notes.map((note) => (
                <li key={note}>{note}</li>
              ))}
            </ul>
            <dl className="quality">
              {Object.entries(results.quality).map(([name, count]) => (
                <div key={name}>
                  <dt>{label(name)}</dt>
                  <dd>{count}</dd>
                </div>
              ))}
            </dl>
            <p className="fine">
              p-values are descriptive and unadjusted. They do not authorize
              automatic promotion or stopping.
            </p>
          </details>
          <h3 className="result-section">Product request guardrails</h3>
          <p className="fine">
            Requests linked to valid exposures, across all cohorts. These
            measure listing operations, not the Switchyard API.
          </p>
          <div className="table-scroll">
            <table aria-label="Product request metrics">
              <thead>
                <tr>
                  <th>Variant</th>
                  <th>Requests</th>
                  <th>Errors</th>
                  <th>Error rate</th>
                  <th>Approx. p95 upper bound</th>
                </tr>
              </thead>
              <tbody>
                {results.variants.map((v) => (
                  <tr key={v.id}>
                    <td>{v.id}</td>
                    <td>{v.requests.count}</td>
                    <td>{v.requests.errors}</td>
                    <td>{percent(v.requests.error_rate)}</td>
                    <td>
                      {v.requests.p95_upper_bound_ms === null
                        ? "—"
                        : `${v.requests.p95_upper_bound_ms} ms`}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </>
      )}
    </section>
  );
}
