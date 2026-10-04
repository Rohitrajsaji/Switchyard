"use client";
import {
  useCallback,
  useEffect,
  useImperativeHandle,
  useRef,
  useState,
} from "react";
import type { FormEvent, RefObject } from "react";
import { api, APIError, errorMessage } from "@/lib/api";
import type { Environment, Session } from "@/lib/api";
import type { Run } from "@/lib/experiments";
import type { Decision } from "@/lib/flags";
import { parseJSON, jsonText } from "@/lib/json";
import {
  listingFlow,
  exposureEvent,
  outcomeEvent,
  completionEvent,
  checkReceipts,
} from "@/lib/demo";
import type { MeasurementEvent, Receipt } from "@/lib/demo";
export type DemoHandle = { stop: () => Promise<void> };
type Credential = { id: string; token: string };
type Attempt = {
  decision: Decision;
  userID: string;
  attributes: unknown;
  exposure: MeasurementEvent;
  flow: "classic" | "simple";
};

function ListingAttempt({
  attempt,
  projectID,
  environmentID,
  credential,
}: {
  attempt: Attempt;
  projectID: string;
  environmentID: string;
  credential: Credential;
}) {
  const [exposed, setExposed] = useState(false);
  const [error, setError] = useState("");
  const [pending, setPending] = useState<MeasurementEvent[]>([]);
  const [busy, setBusy] = useState(false);
  const [completed, setCompleted] = useState(false);
  const [step, setStep] = useState(1);
  const [title, setTitle] = useState("Demo book");
  const [price, setPrice] = useState("500");
  const [category, setCategory] = useState("books");
  const deliver = useCallback(
    async (events: MeasurementEvent[], signal?: AbortSignal) => {
      const result = await api<{ receipts: Receipt[] }>("/v1/events", {
        method: "POST",
        bearer: credential.token,
        body: { project_id: projectID, environment_id: environmentID, events },
        signal,
      });
      checkReceipts(events, result.receipts);
    },
    [credential.token, projectID, environmentID],
  );
  useEffect(() => {
    const controller = new AbortController();
    // The selected product flow has committed to the DOM before this effect.
    void deliver([attempt.exposure], controller.signal)
      .then(() => {
        if (!controller.signal.aborted) setExposed(true);
      })
      .catch((error) => {
        if (!controller.signal.aborted) {
          setError(
            error instanceof APIError
              ? errorMessage(error)
              : error instanceof Error
                ? error.message
                : "Exposure delivery failed.",
          );
          setPending([attempt.exposure]);
        }
      });
    return () => controller.abort();
  }, [attempt.exposure, deliver]);
  async function retry() {
    setBusy(true);
    setError("");
    try {
      await deliver(pending);
      if (pending[0]?.kind === "exposure") setExposed(true);
      setPending([]);
    } catch (error) {
      setError(
        error instanceof APIError
          ? errorMessage(error)
          : error instanceof Error
            ? error.message
            : "Event delivery failed.",
      );
    } finally {
      setBusy(false);
    }
  }
  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (attempt.flow === "classic" && step === 1) {
      setStep(2);
      return;
    }
    setBusy(true);
    setError("");
    const start = performance.now();
    let failed = false;
    try {
      const response = await fetch("/api/demo/listings", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: jsonText({ title, price: Number(price), category }),
        signal: AbortSignal.timeout(8000),
      });
      if (!response.ok)
        throw new Error("The synthetic listing request failed.");
      await response.json();
      setCompleted(true);
    } catch {
      failed = true;
      setError(
        "The listing submission failed. The request outcome will still be recorded.",
      );
    }
    const time = new Date().toISOString();
    const outcome = outcomeEvent(
      attempt.exposure,
      `evt_${crypto.randomUUID()}`,
      time,
      failed,
      Math.min(60000, performance.now() - start),
    );
    const events = failed
      ? [outcome]
      : [
          outcome,
          completionEvent(attempt.exposure, `evt_${crypto.randomUUID()}`, time),
        ];
    try {
      await deliver(events);
      setPending([]);
    } catch (error) {
      setPending(events);
      setError(
        error instanceof APIError
          ? errorMessage(error)
          : error instanceof Error
            ? error.message
            : "Measurement delivery failed.",
      );
    } finally {
      setBusy(false);
    }
  }
  return (
    <section className="panel marketplace">
      <p className="eyebrow">MARKETPLACE DEMO</p>
      <div className="section-heading">
        <h2>
          {attempt.flow === "simple" ? "Quick listing" : "Create a listing"}
        </h2>
        <span className="badge">
          {attempt.decision.variant_id} · {attempt.flow} flow
        </span>
      </div>
      <p className="fine">
        Synthetic data only. This demo acknowledges a submission and stores no
        real marketplace listing.
      </p>
      <p role="status">
        {exposed
          ? "Exposure accepted after rendering."
          : "Recording the rendered exposure…"}
      </p>
      {error && (
        <p className="notice error" role="alert">
          {error}
        </p>
      )}
      {pending.length > 0 && (
        <button className="secondary" onClick={retry} disabled={busy}>
          Retry measurement delivery
        </button>
      )}
      {completed ? (
        <div className="listing-success">
          <h3>Demo listing completed</h3>
          <p>
            {pending.length
              ? "Submission succeeded; measurement acknowledgement is pending."
              : "Completion and request outcome accepted. Open Experiments to inspect the counts."}
          </p>
        </div>
      ) : (
        <form onSubmit={submit}>
          {attempt.flow === "classic" && (
            <p className="fine">
              Step {step} of 2 ·{" "}
              {step === 1 ? "Listing details" : "Price and review"}
            </p>
          )}
          {(attempt.flow === "simple" || step === 1) && (
            <>
              <label>
                Listing title
                <input
                  value={title}
                  onChange={(e) => setTitle(e.target.value)}
                  required
                  maxLength={120}
                />
              </label>
              <label>
                Category
                <select
                  aria-label="Category"
                  value={category}
                  onChange={(e) => setCategory(e.target.value)}
                >
                  <option value="books">Books</option>
                  <option value="electronics">Electronics</option>
                  <option value="home">Home</option>
                </select>
              </label>
            </>
          )}
          {(attempt.flow === "simple" || step === 2) && (
            <>
              <label>
                Price (JPY)
                <input
                  type="number"
                  value={price}
                  onChange={(e) => setPrice(e.target.value)}
                  required
                  min="1"
                  max="1000000"
                  step="1"
                />
              </label>
              {attempt.flow === "classic" && (
                <p className="fine">
                  Review: {title} · {category}
                </p>
              )}
            </>
          )}
          <div className="actions">
            {step === 2 && attempt.flow === "classic" && (
              <button
                className="secondary"
                type="button"
                onClick={() => setStep(1)}
                disabled={busy}
              >
                Back
              </button>
            )}
            <button
              type="submit"
              disabled={!exposed || busy || pending.length > 0}
            >
              {attempt.flow === "classic" && step === 1
                ? "Continue listing"
                : "Submit demo listing"}
            </button>
          </div>
        </form>
      )}
    </section>
  );
}
export default function DemoPanel({
  projectID,
  environment,
  session,
  cleanupRef,
}: {
  projectID: string;
  environment: Environment;
  session: Session;
  cleanupRef: RefObject<DemoHandle | null>;
}) {
  const [runs, setRuns] = useState<Run[]>([]);
  const [runID, setRunID] = useState("");
  const [loading, setLoading] = useState(true);
  const [refresh, setRefresh] = useState(0);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [enabled, setEnabled] = useState(false);
  const [attempt, setAttempt] = useState<Attempt | null>(null);
  const [userID, setUserID] = useState("");
  const key = useRef<Credential | null>(null);
  const creation = useRef<Promise<void> | null>(null);
  const mounted = useRef(true);
  const write =
    session.user.role !== "viewer" && environment.name !== "production";
  const revoke = useCallback(
    async (credential: Credential, keepalive = false) => {
      try {
        await api<void>(
          `/v1/projects/${projectID}/application-keys/${credential.id}`,
          { method: "DELETE", csrf: session.csrf_token, keepalive },
        );
      } catch (error) {
        if (!(error instanceof APIError && error.status === 409)) throw error;
      }
    },
    [projectID, session.csrf_token],
  );
  async function stop() {
    await creation.current;
    const credential = key.current;
    if (credential) await revoke(credential);
    key.current = null;
    if (mounted.current) {
      setEnabled(false);
      setAttempt(null);
    }
  }
  useImperativeHandle(cleanupRef, () => ({ stop }));
  useEffect(() => {
    mounted.current = true;
    setUserID(`demo_${crypto.randomUUID()}`);
    return () => {
      mounted.current = false;
      const credential = key.current;
      if (credential) void revoke(credential, true).catch(() => {});
    };
    // Browser exit cleanup is best effort. Normal navigation awaits stop().
  }, [revoke]);
  useEffect(() => {
    const controller = new AbortController();
    setLoading(true);
    setError("");
    api<Run[]>(
      `/v1/projects/${projectID}/experiments?environment_id=${environment.id}`,
      { signal: controller.signal },
    )
      .then((items) => {
        const running = items.filter((r) => r.state === "running");
        setRuns(running);
        setRunID((current) =>
          running.some((r) => r.id === current)
            ? current
            : (running[0]?.id ?? ""),
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
  async function enable() {
    setBusy(true);
    setError("");
    creation.current = (async () => {
      const credential = await api<Credential>(
        `/v1/projects/${projectID}/application-keys`,
        {
          method: "POST",
          csrf: session.csrf_token,
          body: {
            environment_id: environment.id,
            name: "Browser listing demo",
            permissions: ["evaluate", "events:write"],
          },
        },
      );
      key.current = credential;
      if (mounted.current) setEnabled(true);
      else {
        await revoke(credential, true);
        key.current = null;
      }
    })();
    try {
      await creation.current;
    } catch (error) {
      if (mounted.current) setError(errorMessage(error));
    } finally {
      creation.current = null;
      if (mounted.current) setBusy(false);
    }
  }
  async function evaluate(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const run = runs.find((r) => r.id === runID);
    const credential = key.current;
    if (!run || !credential) return;
    setBusy(true);
    setError("");
    setAttempt(null);
    const fields = new FormData(event.currentTarget);
    try {
      const attributes = parseJSON(String(fields.get("attributes")));
      const decision = await api<Decision>("/v1/evaluate", {
        method: "POST",
        bearer: credential.token,
        body: {
          project_id: projectID,
          environment_id: environment.id,
          key: run.definition.key,
          user_id: userID,
          attributes,
          fallback: run.definition.safe,
        },
      });
      if (decision.reason !== "experiment" || decision.run_id !== run.id) {
        setError(
          `This user was not randomized in the selected run (reason: ${decision.reason}). Change the demo user or attributes. No exposure was recorded.`,
        );
        return;
      }
      const flow = listingFlow(decision.value);
      const exposure = exposureEvent(
        decision,
        userID,
        attributes,
        `evt_${crypto.randomUUID()}`,
        new Date().toISOString(),
      );
      setAttempt({ decision, userID, attributes, exposure, flow });
    } catch (error) {
      setError(
        error instanceof APIError
          ? errorMessage(error)
          : error instanceof Error
            ? error.message
            : "Unable to render the demo flow.",
      );
    } finally {
      setBusy(false);
    }
  }
  return (
    <>
      <section className="panel">
        <div className="section-heading">
          <div>
            <h2>Listing demo</h2>
            <p className="muted">
              Render an assigned product flow, then measure an explicit listing
              completion.
            </p>
          </div>
          <button
            className="secondary"
            disabled={busy}
            onClick={() => setRefresh((n) => n + 1)}
          >
            Reload running experiments
          </button>
        </div>
        {error && (
          <p className="notice error" role="alert">
            {error}
          </p>
        )}
        {!write ? (
          <p className="notice">
            Running the demo requires developer/admin access in development or
            staging.
          </p>
        ) : loading ? (
          <p role="status">Loading running experiments…</p>
        ) : !runs.length ? (
          <p className="empty">
            Start an experiment before opening the listing demo.
          </p>
        ) : (
          <>
            {!enabled ? (
              <>
                <p className="fine">
                  Enable creates a scoped demo credential held only in this
                  browser’s memory. Leaving the screen revokes it.
                </p>
                <button onClick={enable} disabled={busy}>
                  Enable listing demo
                </button>
              </>
            ) : (
              <>
                <div className="actions">
                  <button
                    className="secondary"
                    disabled={busy}
                    onClick={() => {
                      setBusy(true);
                      void stop()
                        .catch((error) => setError(errorMessage(error)))
                        .finally(() => setBusy(false));
                    }}
                  >
                    Stop demo and revoke key
                  </button>
                </div>
                <form onSubmit={evaluate} className="experiment-form">
                  <label>
                    Running experiment
                    <select
                      aria-label="Running experiment"
                      value={runID}
                      disabled={busy}
                      onChange={(e) => {
                        setRunID(e.target.value);
                        setAttempt(null);
                      }}
                    >
                      {runs.map((run) => (
                        <option key={run.id} value={run.id}>
                          {run.name}
                        </option>
                      ))}
                    </select>
                  </label>
                  <div className="inline-form">
                    <label>
                      Demo user ID
                      <input
                        value={userID}
                        onChange={(e) => {
                          setUserID(e.target.value);
                          setAttempt(null);
                        }}
                        required
                        maxLength={128}
                      />
                    </label>
                    <button
                      className="secondary"
                      type="button"
                      disabled={busy}
                      onClick={() => {
                        setUserID(`demo_${crypto.randomUUID()}`);
                        setAttempt(null);
                      }}
                    >
                      New demo user
                    </button>
                  </div>
                  <label>
                    Demo attributes (JSON)
                    <textarea
                      aria-label="Demo attributes (JSON)"
                      name="attributes"
                      onChange={() => setAttempt(null)}
                      defaultValue={'{"country":"JP","device":"desktop"}'}
                      rows={3}
                      required
                      spellCheck={false}
                    />
                  </label>
                  <button type="submit" disabled={busy}>
                    Render assigned flow
                  </button>
                </form>
              </>
            )}
          </>
        )}
      </section>
      {attempt && key.current && (
        <ListingAttempt
          key={attempt.exposure.event_id}
          attempt={attempt}
          projectID={projectID}
          environmentID={environment.id}
          credential={key.current}
        />
      )}
    </>
  );
}
