import { jsonText, parseResponse } from "./json.ts";
export type Session = {
  user: { id: string; email: string; role: "viewer" | "developer" | "admin" };
  csrf_token: string;
  expires_at: string;
};
export type Project = { id: string; name: string };
export type Environment = {
  id: string;
  name: "development" | "staging" | "production";
};
export class APIError extends Error {
  constructor(
    public status: number,
    public code: string,
    public requestID?: string,
  ) {
    const messages: Record<string, string> = {
      unauthorized:
        "Your session has expired or the login details are incorrect.",
      forbidden: "You do not have permission for this action.",
      invalid_input: "Check the supplied values and try again.",
      conflict:
        "The configuration changed or this name already exists. Reload before retrying.",
      backend_unavailable:
        "The Go API is unavailable. Try again after it recovers.",
      rate_limited: "Too many requests. Wait a minute before retrying.",
      not_found: "This item is no longer available.",
    };
    super(messages[code] ?? "The request failed. Try again.");
  }
}
export async function api<T>(
  path: string,
  options: {
    method?: string;
    body?: unknown;
    csrf?: string;
    signal?: AbortSignal;
  } = {},
): Promise<T> {
  const headers: Record<string, string> = {};
  if (options.body !== undefined) headers["Content-Type"] = "application/json";
  if (options.csrf) headers["X-CSRF-Token"] = options.csrf;
  const response = await fetch(`/api/backend${path}`, {
    method: options.method ?? "GET",
    headers,
    body: options.body === undefined ? undefined : jsonText(options.body),
    signal: options.signal,
    credentials: "same-origin",
    cache: "no-store",
  });
  if (!response.ok) {
    const error = await response.json().catch(() => ({}));
    throw new APIError(
      response.status,
      error.error ?? "unknown",
      error.request_id,
    );
  }
  return response.status === 204
    ? (undefined as T)
    : parseResponse<T>(await response.text());
}
export function errorMessage(error: unknown): string {
  if (error instanceof APIError)
    return (
      error.message + (error.requestID ? ` Reference: ${error.requestID}` : "")
    );
  return "Unable to reach Switchyard. Check the connection and try again.";
}
