"use client";
import { useEffect, useState } from "react";
import type { FormEvent } from "react";
import { api, errorMessage } from "@/lib/api";
import type { Session, Environment } from "@/lib/api";
import type { Flag, Value, Rule, Decision } from "@/lib/flags";
import { jsonText, parseJSON } from "@/lib/json";

function ValueField({
  label,
  type,
  text,
  onChange,
}: {
  label: string;
  type: Value["type"];
  text: string;
  onChange: (text: string) => void;
}) {
  return (
    <label>
      {label}
      {type === "boolean" ? (
        <select
          aria-label={label}
          value={text}
          onChange={(e) => onChange(e.target.value)}
        >
          <option value="false">false</option>
          <option value="true">true</option>
        </select>
      ) : (
        <textarea
          aria-label={label}
          value={text}
          onChange={(e) => onChange(e.target.value)}
          required
          rows={3}
          spellCheck={false}
        />
      )}
    </label>
  );
}
function FlagEditor({
  flag,
  projectID,
  environment,
  session,
  onSaved,
  onCancel,
}: {
  flag?: Flag;
  projectID: string;
  environment: Environment;
  session: Session;
  onSaved: (flag: Flag) => void;
  onCancel: () => void;
}) {
  const [type, setType] = useState<Value["type"]>(flag?.type ?? "boolean");
  const [normal, setNormal] = useState(
    jsonText(flag ? flag.default.data : false, true),
  );
  const [safe, setSafe] = useState(
    jsonText(flag ? flag.safe.data : false, true),
  );
  const [rules, setRules] = useState(jsonText(flag?.rules ?? [], true));
  const [rollout, setRollout] = useState(jsonText(flag?.rollout ?? null, true));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setBusy(true);
    setError("");
    const form = new FormData(event.currentTarget);
    try {
      const config = {
        environment_id: environment.id,
        default: { type, data: parseJSON(normal) },
        safe: { type, data: parseJSON(safe) },
        rules: parseJSON(rules) as Rule[],
        rollout: parseJSON(rollout),
        killed: flag?.killed ?? false,
        reason: form.get("reason"),
      };
      const saved = await api<Flag>(
        `/v1/projects/${projectID}/flags${flag ? `/${flag.key}` : ""}`,
        {
          method: flag ? "PUT" : "POST",
          csrf: session.csrf_token,
          body: flag
            ? { ...config, expected_revision: flag.revision }
            : { ...config, key: form.get("key"), type },
        },
      );
      onSaved(saved);
    } catch (error) {
      setError(
        error instanceof SyntaxError
          ? "Enter valid JSON for values, rules and rollout."
          : errorMessage(error),
      );
    } finally {
      setBusy(false);
    }
  }
  return (
    <section className="panel">
      <h2>{flag ? `Edit ${flag.key}` : "Create a flag"}</h2>
      <p className="muted">
        The safe value is returned when the kill switch is on. Go validates the
        complete configuration.
      </p>
      {flag?.experiment && (
        <p className="notice">
          This flag has an experiment. Its population and values are frozen; use
          the experiment controls or kill switch.
        </p>
      )}
      <form onSubmit={submit}>
        <div className="field-grid">
          <label>
            Flag key
            <input
              name="key"
              defaultValue={flag?.key}
              disabled={!!flag}
              required
              pattern="[a-z][a-z0-9_-]{0,63}"
              maxLength={64}
              placeholder="listing_flow"
            />
          </label>
          <label>
            Value type
            <select
              aria-label="Value type"
              value={type}
              disabled={!!flag}
              onChange={(e) => {
                setType(e.target.value as Value["type"]);
                setNormal(e.target.value === "boolean" ? "false" : "{}");
                setSafe(e.target.value === "boolean" ? "false" : "{}");
              }}
            >
              <option value="boolean">Boolean</option>
              <option value="json">JSON</option>
            </select>
          </label>
        </div>
        <div className="field-grid">
          <ValueField
            label="Default value"
            type={type}
            text={normal}
            onChange={setNormal}
          />
          <ValueField
            label="Emergency safe value"
            type={type}
            text={safe}
            onChange={setSafe}
          />
        </div>
        <details>
          <summary>Targeting and gradual rollout</summary>
          <p className="fine">
            Rules are evaluated in order before experiment assignment. Use
            non-sensitive demo attributes.
          </p>
          <label>
            Targeting rules (JSON)
            <textarea
              aria-label="Targeting rules (JSON)"
              value={rules}
              onChange={(e) => setRules(e.target.value)}
              rows={7}
              spellCheck={false}
            />
          </label>
          <p className="fine">
            Example:{" "}
            <code>
              {
                '[{"attribute":"country","operator":"eq","values":["JP"],"value":{"type":"boolean","data":true}}]'
              }
            </code>
          </p>
          <label>
            Gradual rollout (JSON or null)
            <textarea
              aria-label="Gradual rollout (JSON or null)"
              value={rollout}
              onChange={(e) => setRollout(e.target.value)}
              rows={4}
              spellCheck={false}
            />
          </label>
          <p className="fine">
            Example:{" "}
            <code>
              {
                '{"traffic_bp":1000,"salt":"standalone-v1","value":{"type":"boolean","data":true}}'
              }
            </code>{" "}
            enables 10% of users. Use null for none.
          </p>
        </details>
        <label>
          Change reason
          <input
            name="reason"
            required
            maxLength={500}
            placeholder="Explain the change for audit history"
          />
        </label>
        {error && (
          <p className="notice error" role="alert">
            {error}
          </p>
        )}
        <div className="actions">
          <button type="submit" disabled={busy}>
            {busy ? "Saving…" : "Save flag"}
          </button>
          <button
            type="button"
            className="secondary"
            onClick={onCancel}
            disabled={busy}
          >
            Cancel
          </button>
        </div>
      </form>
    </section>
  );
}
function Preview({
  flag,
  projectID,
  environment,
}: {
  flag: Flag;
  projectID: string;
  environment: Environment;
}) {
  const [decision, setDecision] = useState<Decision | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setBusy(true);
    setError("");
    setDecision(null);
    const fields = new FormData(event.currentTarget);
    try {
      setDecision(
        await api<Decision>(
          `/v1/projects/${projectID}/flags/${flag.key}/preview`,
          {
            method: "POST",
            body: {
              environment_id: environment.id,
              user_id: fields.get("user_id"),
              attributes: parseJSON(String(fields.get("attributes"))),
              fallback: flag.safe,
            },
          },
        ),
      );
    } catch (error) {
      setError(
        error instanceof SyntaxError
          ? "Enter valid JSON attributes."
          : errorMessage(error),
      );
    } finally {
      setBusy(false);
    }
  }
  return (
    <section className="panel">
      <h2>Evaluation preview</h2>
      <p className="muted">Inspect a decision without recording an exposure.</p>
      <form onSubmit={submit}>
        <label>
          User ID
          <input
            name="user_id"
            required
            maxLength={128}
            defaultValue="demo_user_1"
          />
        </label>
        <label>
          User attributes (JSON)
          <textarea
            aria-label="User attributes (JSON)"
            name="attributes"
            defaultValue={'{"country":"JP","device":"desktop"}'}
            rows={3}
            required
            spellCheck={false}
          />
        </label>
        <button disabled={busy} type="submit">
          {busy ? "Evaluating…" : "Evaluate user"}
        </button>
      </form>
      {error && (
        <p className="notice error" role="alert">
          {error}
        </p>
      )}
      {decision && (
        <div className="decision">
          <dl>
            <dt>Reason</dt>
            <dd>{decision.reason}</dd>
            <dt>Revision</dt>
            <dd>{decision.revision}</dd>
            {decision.variant_id && (
              <>
                <dt>Variant</dt>
                <dd>{decision.variant_id}</dd>
              </>
            )}
          </dl>
          <pre aria-label="Evaluation value">
            {jsonText(decision.value.data, true)}
          </pre>
        </div>
      )}
    </section>
  );
}
export default function FlagsPanel({
  projectID,
  environment,
  session,
}: {
  projectID: string;
  environment: Environment;
  session: Session;
}) {
  const [flags, setFlags] = useState<Flag[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [refresh, setRefresh] = useState(0);
  const [selected, setSelected] = useState("");
  const [editing, setEditing] = useState<"create" | "edit" | null>(null);
  const [busy, setBusy] = useState(false);
  const write =
    session.user.role !== "viewer" && environment.name !== "production";
  useEffect(() => {
    const controller = new AbortController();
    setLoading(true);
    setError("");
    api<Flag[]>(
      `/v1/projects/${projectID}/flags?environment_id=${environment.id}`,
      { signal: controller.signal },
    )
      .then((items) => {
        setFlags(items);
        setSelected((current) =>
          items.some((f) => f.key === current)
            ? current
            : (items[0]?.key ?? ""),
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
  const flag = flags.find((f) => f.key === selected);
  function saved(item: Flag) {
    setSelected(item.key);
    setEditing(null);
    setRefresh((n) => n + 1);
  }
  async function kill() {
    if (!flag) return;
    setBusy(true);
    setError("");
    try {
      saved(
        await api<Flag>(`/v1/projects/${projectID}/flags/${flag.key}`, {
          method: "PUT",
          csrf: session.csrf_token,
          body: {
            environment_id: environment.id,
            expected_revision: flag.revision,
            default: flag.default,
            safe: flag.safe,
            rules: flag.rules,
            rollout: flag.rollout,
            killed: true,
            reason: "Emergency kill from dashboard",
          },
        }),
      );
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
            <h2>Feature flags</h2>
            <p className="muted">Configuration for {environment.name}.</p>
          </div>
          <div className="actions">
            <button
              className="secondary"
              onClick={() => {
                setEditing(null);
                setRefresh((n) => n + 1);
              }}
            >
              Reload flags
            </button>
            {write && (
              <button onClick={() => setEditing("create")}>Create flag</button>
            )}
          </div>
        </div>
        {error && (
          <p className="notice error" role="alert">
            {error}
          </p>
        )}
        {loading ? (
          <p role="status">Loading flags…</p>
        ) : flags.length ? (
          <div className="table-scroll">
            <table>
              <thead>
                <tr>
                  <th>Flag</th>
                  <th>Type</th>
                  <th>Revision</th>
                  <th>Status</th>
                </tr>
              </thead>
              <tbody>
                {flags.map((f) => (
                  <tr
                    key={f.key}
                    className={selected === f.key ? "selected" : ""}
                  >
                    <td>
                      <button
                        className="link"
                        onClick={() => {
                          setSelected(f.key);
                          setEditing(null);
                        }}
                      >
                        {f.key}
                      </button>
                    </td>
                    <td>{f.type}</td>
                    <td>{f.revision}</td>
                    <td>
                      {f.killed
                        ? "Killed"
                        : f.experiment
                          ? "Experiment"
                          : "Enabled"}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <p className="empty">No flags in this environment yet.</p>
        )}
        {!editing && flag && (
          <div className="flag-detail">
            <div className="section-heading">
              <h3>{flag.key}</h3>
              <div className="actions">
                {write && (
                  <>
                    <button
                      className="secondary"
                      onClick={() => setEditing("edit")}
                    >
                      Edit flag
                    </button>
                    <button
                      className="danger"
                      disabled={busy || flag.killed}
                      onClick={kill}
                    >
                      Kill switch
                    </button>
                  </>
                )}
              </div>
            </div>
            <div className="field-grid">
              <div>
                <p className="eyebrow">DEFAULT</p>
                <pre>{jsonText(flag.default.data, true)}</pre>
              </div>
              <div>
                <p className="eyebrow">EMERGENCY SAFE</p>
                <pre>{jsonText(flag.safe.data, true)}</pre>
              </div>
            </div>
          </div>
        )}
      </section>
      {editing && write && (
        <FlagEditor
          key={editing === "create" ? "new" : `${flag?.key}-${flag?.revision}`}
          flag={editing === "edit" ? flag : undefined}
          projectID={projectID}
          environment={environment}
          session={session}
          onSaved={saved}
          onCancel={() => setEditing(null)}
        />
      )}
      {!editing && flag && (
        <Preview
          key={`${flag.key}-${flag.revision}`}
          flag={flag}
          projectID={projectID}
          environment={environment}
        />
      )}
    </>
  );
}
