"use client";
import { useEffect, useState } from "react";
import type { FormEvent } from "react";
import { api, APIError, errorMessage } from "@/lib/api";
import type { Session, Project, Environment } from "@/lib/api";
import FlagsPanel from "./flags-panel";
import AuditPanel from "./audit-panel";

function Brand() {
  return (
    <div className="brand">
      <span className="brand-mark" aria-hidden="true">
        ⇄
      </span>
      <span>
        Switchyard<small>Experimentation platform</small>
      </span>
    </div>
  );
}
function ErrorNotice({ message }: { message: string }) {
  return message ? (
    <p className="notice error" role="alert">
      {message}
    </p>
  ) : null;
}
function Login({ onLogin }: { onLogin: (session: Session) => void }) {
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setBusy(true);
    setError("");
    const fields = new FormData(event.currentTarget);
    try {
      onLogin(
        await api<Session>("/v1/session", {
          method: "POST",
          body: {
            email: fields.get("email"),
            password: fields.get("password"),
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
    <main className="login">
      <section className="login-card">
        <Brand />
        <p className="eyebrow">TEAM WORKSPACE</p>
        <h1>Make change measurable.</h1>
        <p className="muted">
          Sign in to manage flags and inspect your experiments.
        </p>
        <form onSubmit={submit}>
          <label>
            Email
            <input
              name="email"
              type="email"
              autoComplete="username"
              required
              maxLength={254}
            />
          </label>
          <label>
            Password
            <input
              name="password"
              type="password"
              autoComplete="current-password"
              required
              maxLength={72}
            />
          </label>
          <ErrorNotice message={error} />
          <button disabled={busy} type="submit">
            {busy ? "Signing in…" : "Sign in"}
          </button>
        </form>
        <p className="fine">
          Local demo accounts must be seeded explicitly. Use the credentials
          from your setup guide.
        </p>
      </section>
    </main>
  );
}
function Workspace({
  session,
  onLogout,
}: {
  session: Session;
  onLogout: () => void;
}) {
  const [projects, setProjects] = useState<Project[]>([]);
  const [projectID, setProjectID] = useState("");
  const [environments, setEnvironments] = useState<Environment[]>([]);
  const [environmentID, setEnvironmentID] = useState("");
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [refresh, setRefresh] = useState(0);
  const [tab, setTab] = useState<"flags" | "audit">("flags");
  useEffect(() => {
    const controller = new AbortController();
    setLoading(true);
    setError("");
    api<Project[]>("/v1/projects", { signal: controller.signal })
      .then((items) => {
        setProjects(items);
        setProjectID((current) =>
          items.some((p) => p.id === current) ? current : (items[0]?.id ?? ""),
        );
      })
      .catch((error) => {
        if (!controller.signal.aborted) setError(errorMessage(error));
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
    return () => controller.abort();
  }, [refresh]);
  useEffect(() => {
    const controller = new AbortController();
    setEnvironments([]);
    setEnvironmentID("");
    if (projectID)
      api<Environment[]>(`/v1/projects/${projectID}/environments`, {
        signal: controller.signal,
      })
        .then((items) => {
          setEnvironments(items);
          setEnvironmentID(
            items.find((e) => e.name === "development")?.id ??
              items[0]?.id ??
              "",
          );
        })
        .catch((error) => {
          if (!controller.signal.aborted) setError(errorMessage(error));
        });
    return () => controller.abort();
  }, [projectID]);
  async function createProject(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const form = event.currentTarget;
    setBusy(true);
    setError("");
    try {
      const project = await api<Project>("/v1/projects", {
        method: "POST",
        csrf: session.csrf_token,
        body: { name: new FormData(form).get("name") },
      });
      setProjects((items) => [...items, project]);
      setProjectID(project.id);
      form.reset();
    } catch (error) {
      setError(errorMessage(error));
    } finally {
      setBusy(false);
    }
  }
  async function logout() {
    setBusy(true);
    setError("");
    try {
      await api<void>("/v1/session", {
        method: "DELETE",
        csrf: session.csrf_token,
      });
      onLogout();
    } catch (error) {
      if (error instanceof APIError && error.status === 401) onLogout();
      else setError(errorMessage(error));
    } finally {
      setBusy(false);
    }
  }
  const environment = environments.find((e) => e.id === environmentID);
  return (
    <div className="workspace">
      <header className="topbar">
        <Brand />
        <div className="account">
          <span>{session.user.email}</span>
          <span className="badge">{session.user.role}</span>
          <button className="secondary" onClick={logout} disabled={busy}>
            Sign out
          </button>
        </div>
      </header>
      <main className="content">
        <div className="page-heading">
          <div>
            <p className="eyebrow">WORKSPACE</p>
            <h1>Projects & environments</h1>
            <p className="muted">
              Choose the scope for your flags and experiments.
            </p>
          </div>
          <button
            className="secondary"
            onClick={() => setRefresh((n) => n + 1)}
          >
            Reload projects
          </button>
        </div>
        <ErrorNotice message={error} />
        <section className="panel">
          <div className="scope">
            <label>
              Project
              <select
                value={projectID}
                onChange={(e) => {
                  setProjectID(e.target.value);
                  setError("");
                }}
                disabled={loading || !projects.length}
              >
                <option value="">
                  {loading ? "Loading projects…" : "Select a project"}
                </option>
                {projects.map((p) => (
                  <option key={p.id} value={p.id}>
                    {p.name}
                  </option>
                ))}
              </select>
            </label>
            <label>
              Environment
              <select
                value={environmentID}
                onChange={(e) => setEnvironmentID(e.target.value)}
                disabled={!environments.length}
              >
                <option value="">Select an environment</option>
                {environments.map((e) => (
                  <option key={e.id} value={e.id}>
                    {e.name}
                  </option>
                ))}
              </select>
            </label>
          </div>
          {!loading && !projects.length && (
            <p className="empty">
              No projects are available to your account.
              {session.user.role === "viewer"
                ? " Ask a project admin to add your membership."
                : " Create your first project below."}
            </p>
          )}
          {environment && (
            <div className="scope-detail">
              <span
                className={`dot ${environment.name === "production" ? "amber" : ""}`}
                aria-hidden="true"
              />
              <strong>{projects.find((p) => p.id === projectID)?.name}</strong>
              <span>/</span>
              <span>{environment.name}</span>
            </div>
          )}
          {environment?.name === "production" && (
            <p className="notice">
              Production is read-only until reviewed change controls are
              available.
            </p>
          )}
          {session.user.role === "viewer" && (
            <p className="notice">
              Viewer access: you can inspect project data. Configuration changes
              require a developer or admin.
            </p>
          )}
        </section>
        {environment && (
          <div key={`${projectID}-${environment.id}`}>
            <nav className="tabs" aria-label="Project sections">
              <button
                className={tab === "flags" ? "active" : ""}
                aria-current={tab === "flags" ? "page" : undefined}
                onClick={() => setTab("flags")}
              >
                Flags
              </button>
              <button
                className={tab === "audit" ? "active" : ""}
                aria-current={tab === "audit" ? "page" : undefined}
                onClick={() => setTab("audit")}
              >
                Audit
              </button>
            </nav>
            {tab === "flags" ? (
              <FlagsPanel
                projectID={projectID}
                environment={environment}
                session={session}
              />
            ) : (
              <AuditPanel projectID={projectID} />
            )}
          </div>
        )}
        {session.user.role !== "viewer" && (
          <section className="panel narrow">
            <h2>Create a project</h2>
            <p className="muted">
              Every project starts with development, staging and production
              environments.
            </p>
            <form className="inline-form" onSubmit={createProject}>
              <label>
                Project name
                <input
                  name="name"
                  required
                  maxLength={120}
                  placeholder="Marketplace"
                />
              </label>
              <button type="submit" disabled={busy}>
                Create project
              </button>
            </form>
          </section>
        )}
      </main>
      <footer>Switchyard · Local workspace</footer>
    </div>
  );
}
export default function Dashboard() {
  const [session, setSession] = useState<Session | null>(null);
  const [ready, setReady] = useState(false);
  const [error, setError] = useState("");
  const [retry, setRetry] = useState(0);
  useEffect(() => {
    const controller = new AbortController();
    setReady(false);
    setError("");
    api<Session>("/v1/session", { signal: controller.signal })
      .then(setSession)
      .catch((error) => {
        if (
          !controller.signal.aborted &&
          !(error instanceof APIError && error.status === 401)
        )
          setError(errorMessage(error));
      })
      .finally(() => {
        if (!controller.signal.aborted) setReady(true);
      });
    return () => controller.abort();
  }, [retry]);
  if (!ready)
    return (
      <main className="login">
        <p role="status">Loading your workspace…</p>
      </main>
    );
  if (error)
    return (
      <main className="login">
        <section className="login-card">
          <Brand />
          <ErrorNotice message={error} />
          <button onClick={() => setRetry((n) => n + 1)}>
            Retry connection
          </button>
        </section>
      </main>
    );
  return session ? (
    <Workspace session={session} onLogout={() => setSession(null)} />
  ) : (
    <Login onLogin={setSession} />
  );
}
