"use client";
import { useEffect, useState } from "react";
import type { FormEvent } from "react";
import { api, errorMessage } from "@/lib/api";
import type { Environment, Session } from "@/lib/api";
import type { Flag } from "@/lib/flags";
import type { Run } from "@/lib/experiments";
import { basisPoints } from "@/lib/experiments";
import { jsonText, parseJSON } from "@/lib/json";
import ResultsPanel from "./results-panel";

function CreateExperiment({
  flags,
  projectID,
  environment,
  session,
  onCreated,
  onCancel,
}: {
  flags: Flag[];
  projectID: string;
  environment: Environment;
  session: Session;
  onCreated: (run: Run) => void;
  onCancel: () => void;
}) {
  const [key, setKey] = useState(flags[0]?.key ?? "");
  const flag = flags.find((f) => f.key === key);
  return (
    <section className="panel">
      <h2>Create an A/B experiment</h2>
      <p className="muted">
        Starting a run freezes its population and values. Explicit targeting
        overrides randomized assignment.
      </p>
      <label>
        Experiment flag
        <select
          aria-label="Experiment flag"
          value={key}
          onChange={(e) => setKey(e.target.value)}
        >
          {flags.map((f) => (
            <option key={f.key} value={f.key}>
              {f.key}
            </option>
          ))}
        </select>
      </label>
      {flag && (
        <ExperimentForm
          key={`${flag.key}-${flag.revision}`}
          flag={flag}
          projectID={projectID}
          environment={environment}
          session={session}
          onCreated={onCreated}
          onCancel={onCancel}
        />
      )}
    </section>
  );
}
function ExperimentForm({
  flag,
  projectID,
  environment,
  session,
  onCreated,
  onCancel,
}: {
  flag: Flag;
  projectID: string;
  environment: Environment;
  session: Session;
  onCreated: (run: Run) => void;
  onCancel: () => void;
}) {
  const [control, setControl] = useState(jsonText(flag.default.data, true));
  const [treatment, setTreatment] = useState(
    jsonText(
      flag.type === "boolean" ? !flag.default.data : flag.default.data,
      true,
    ),
  );
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  async function create(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setBusy(true);
    setError("");
    const fields = new FormData(event.currentTarget);
    try {
      const controlBP = basisPoints(String(fields.get("control_weight")));
      const trafficBP = basisPoints(String(fields.get("traffic")));
      onCreated(
        await api<Run>(`/v1/projects/${projectID}/experiments`, {
          method: "POST",
          csrf: session.csrf_token,
          body: {
            environment_id: environment.id,
            flag_key: flag.key,
            expected_revision: flag.revision,
            name: fields.get("name"),
            control_variant_id: "control",
            traffic_bp: trafficBP,
            variants: [
              {
                id: "control",
                ordinal: 0,
                weight_bp: controlBP,
                value: { type: flag.type, data: parseJSON(control) },
              },
              {
                id: "treatment",
                ordinal: 1,
                weight_bp: 10000 - controlBP,
                value: { type: flag.type, data: parseJSON(treatment) },
              },
            ],
            reason: fields.get("reason"),
          },
        }),
      );
    } catch (error) {
      setError(
        error instanceof RangeError
          ? error.message
          : error instanceof SyntaxError
            ? "Enter valid JSON variant values."
            : errorMessage(error),
      );
    } finally {
      setBusy(false);
    }
  }
  return (
    <form onSubmit={create} className="experiment-form">
      <label>
        Experiment name
        <input
          name="name"
          required
          maxLength={128}
          defaultValue="Simplified listing flow"
        />
      </label>
      <div className="field-grid">
        <label>
          Eligible traffic (%)
          <input
            name="traffic"
            type="number"
            min="0"
            max="100"
            step="0.01"
            defaultValue="100"
            required
          />
        </label>
        <label>
          Control allocation (%)
          <input
            name="control_weight"
            type="number"
            min="0.01"
            max="99.99"
            step="0.01"
            defaultValue="50"
            required
          />
        </label>
      </div>
      <div className="field-grid">
        {[
          { name: "Control value", text: control, set: setControl },
          { name: "Treatment value", text: treatment, set: setTreatment },
        ].map((field) => (
          <label key={field.name}>
            {field.name}
            {flag.type === "boolean" ? (
              <select
                aria-label={field.name}
                value={field.text}
                onChange={(e) => field.set(e.target.value)}
              >
                <option value="false">false</option>
                <option value="true">true</option>
              </select>
            ) : (
              <textarea
                aria-label={field.name}
                value={field.text}
                onChange={(e) => field.set(e.target.value)}
                rows={4}
                required
                spellCheck={false}
              />
            )}
          </label>
        ))}
      </div>
      <p className="fine">
        Treatment receives the remaining allocation. The two allocations total
        100% of eligible users.
      </p>
      <label>
        Experiment reason
        <input
          name="reason"
          required
          maxLength={512}
          placeholder="What hypothesis are you testing?"
        />
      </label>
      {error && (
        <p className="notice error" role="alert">
          {error}
        </p>
      )}
      <div className="actions">
        <button disabled={busy} type="submit">
          {busy ? "Creating…" : "Create draft"}
        </button>
        <button
          type="button"
          className="secondary"
          disabled={busy}
          onClick={onCancel}
        >
          Cancel
        </button>
      </div>
    </form>
  );
}
export default function ExperimentsPanel({
  projectID,
  environment,
  session,
}: {
  projectID: string;
  environment: Environment;
  session: Session;
}) {
  const [runs, setRuns] = useState<Run[]>([]);
  const [flags, setFlags] = useState<Flag[]>([]);
  const [selected, setSelected] = useState("");
  const [creating, setCreating] = useState(false);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [refresh, setRefresh] = useState(0);
  const write =
    session.user.role !== "viewer" && environment.name !== "production";
  useEffect(() => {
    const controller = new AbortController();
    setLoading(true);
    setError("");
    Promise.all([
      api<Run[]>(
        `/v1/projects/${projectID}/experiments?environment_id=${environment.id}`,
        { signal: controller.signal },
      ),
      api<Flag[]>(
        `/v1/projects/${projectID}/flags?environment_id=${environment.id}`,
        { signal: controller.signal },
      ),
    ])
      .then(([items, definitions]) => {
        setRuns(items);
        setFlags(definitions);
        setSelected((current) =>
          items.some((r) => r.id === current) ? current : (items[0]?.id ?? ""),
        );
      })
      .catch((error) => {
        if (!controller.signal.aborted) setError(errorMessage(error));
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
    return () => controller.abort();
  }, [projectID, environment.id, refresh]);
  const run = runs.find((r) => r.id === selected);
  const eligible = flags.filter(
    (f) =>
      !f.killed &&
      !f.rollout &&
      !f.experiment &&
      !runs.some((r) => r.definition.key === f.key && r.state !== "completed"),
  );
  async function transition(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!run) return;
    setBusy(true);
    setError("");
    const fields = new FormData(
      event.currentTarget,
      (event.nativeEvent as SubmitEvent).submitter,
    );
    try {
      const updated = await api<Run>(
        `/v1/projects/${projectID}/experiments/${run.id}/transitions`,
        {
          method: "POST",
          csrf: session.csrf_token,
          body: {
            action: fields.get("action"),
            expected_revision: run.configuration_revision,
            reason: fields.get("reason"),
          },
        },
      );
      setRuns((items) =>
        items.map((item) => (item.id === updated.id ? updated : item)),
      );
      setRefresh((n) => n + 1);
    } catch (error) {
      setError(errorMessage(error));
    } finally {
      setBusy(false);
    }
  }
  return (
    <>
      <section className="panel">
        <div className="section-heading">
          <div>
            <h2>Experiments</h2>
            <p className="muted">Randomized runs in {environment.name}.</p>
          </div>
          <div className="actions">
            <button
              className="secondary"
              disabled={busy}
              onClick={() => {
                setCreating(false);
                setRefresh((n) => n + 1);
              }}
            >
              Reload experiments
            </button>
            {write && (
              <button
                disabled={loading || !eligible.length || busy}
                onClick={() => setCreating(true)}
              >
                Create experiment
              </button>
            )}
          </div>
        </div>
        {error && (
          <p className="notice error" role="alert">
            {error}
          </p>
        )}
        {write && !loading && !eligible.length && (
          <p className="fine">
            Create an enabled flag without a rollout or reserved experiment
            before creating a run.
          </p>
        )}
        {loading ? (
          <p role="status">Loading experiments…</p>
        ) : runs.length ? (
          <div className="table-scroll">
            <table aria-label="Experiment runs">
              <thead>
                <tr>
                  <th>Experiment</th>
                  <th>Flag</th>
                  <th>State</th>
                  <th>Traffic</th>
                </tr>
              </thead>
              <tbody>
                {runs.map((item) => (
                  <tr
                    key={item.id}
                    className={item.id === selected ? "selected" : ""}
                  >
                    <td>
                      <button
                        className="link"
                        onClick={() => {
                          setSelected(item.id);
                          setCreating(false);
                        }}
                      >
                        {item.name}
                      </button>
                    </td>
                    <td>{item.definition.key}</td>
                    <td>{item.state}</td>
                    <td>
                      {(item.definition.experiment.traffic_bp / 100).toFixed(2)}
                      %
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <p className="empty">No experiments in this environment yet.</p>
        )}
      </section>
      {creating && write && !!eligible.length && (
        <CreateExperiment
          flags={eligible}
          projectID={projectID}
          environment={environment}
          session={session}
          onCreated={(item) => {
            setCreating(false);
            setSelected(item.id);
            setRuns((items) => [item, ...items]);
            setRefresh((n) => n + 1);
          }}
          onCancel={() => setCreating(false)}
        />
      )}
      {!creating && run && (
        <>
          <section className="panel">
            <div className="section-heading">
              <div>
                <h2>{run.name}</h2>
                <p className="muted">
                  {run.definition.key} · <strong>{run.state}</strong> · current
                  revision {run.configuration_revision}
                </p>
              </div>
              <span className="badge">
                {(run.definition.experiment.traffic_bp / 100).toFixed(2)}%
                traffic
              </span>
            </div>
            <div className="field-grid">
              {run.definition.experiment.variants.map((v) => (
                <div key={v.id}>
                  <h3>
                    {v.id}
                    {v.id === run.control_variant_id ? " · control" : ""} ·{" "}
                    {(v.weight_bp / 100).toFixed(2)}%
                  </h3>
                  <pre>{jsonText(v.value.data, true)}</pre>
                </div>
              ))}
            </div>
            {write && run.state !== "completed" && (
              <form className="transition-form" onSubmit={transition}>
                <label>
                  Lifecycle reason
                  <input
                    name="reason"
                    required
                    maxLength={512}
                    placeholder="Record why this run is changing"
                  />
                </label>
                <div className="actions">
                  {run.state !== "running" && (
                    <button
                      disabled={busy}
                      name="action"
                      value="start"
                      type="submit"
                    >
                      {run.state === "paused"
                        ? "Resume experiment"
                        : "Start experiment"}
                    </button>
                  )}
                  {run.state === "running" && (
                    <button
                      className="secondary"
                      disabled={busy}
                      name="action"
                      value="pause"
                      type="submit"
                    >
                      Pause experiment
                    </button>
                  )}
                  <button
                    className="secondary"
                    disabled={busy}
                    name="action"
                    value="complete"
                    type="submit"
                  >
                    Complete experiment
                  </button>
                </div>
              </form>
            )}
            {run.state === "completed" && (
              <p className="fine">
                This run is complete. Historical assignment and results are
                preserved.
              </p>
            )}
          </section>
          <ResultsPanel key={run.id} projectID={projectID} runID={run.id} />
        </>
      )}
    </>
  );
}
