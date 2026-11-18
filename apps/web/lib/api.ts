import { observeAPI } from "./analytics";
import type { components, paths } from "./openapi";

/** A named schema from the API contract. */
export type Schema<Name extends keyof components["schemas"]> = components["schemas"][Name];

type HttpMethod = "get" | "post";
type Json<Content> = Content extends { content: { "application/json": infer Body } } ? Body : never;

/** The JSON body an operation takes. */
export type RequestBody<Path extends keyof paths, Method extends HttpMethod> = paths[Path][Method] extends {
  requestBody?: infer Body;
}
  ? Json<Body>
  : never;

/** The JSON a successful call to an operation returns. */
export type ResponseBody<Path extends keyof paths, Method extends HttpMethod> = paths[Path][Method] extends {
  responses: infer Responses;
}
  ? Json<Responses[keyof Responses & (200 | 201 | 202)]>
  : never;

export type FieldError = Schema<"PublicationProblem">;

export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
    readonly code?: string,
    readonly errors: FieldError[] = [],
  ) {
    super(message);
  }
}

async function toError(response: Response): Promise<ApiError> {
  const payload = await response.json().catch(() => null);
  const detail = payload?.detail;
  const message = (typeof detail === "string" ? detail : detail?.message) ?? `Request failed (${response.status})`;
  return new ApiError(
    message,
    response.status,
    payload?.code ?? detail?.code,
    Array.isArray(payload?.errors) ? payload.errors : [],
  );
}

/** Browser-side call. Always sends the session cookie. */
export async function api<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers: Record<string, string> =
    init.headers instanceof Headers || Array.isArray(init.headers)
      ? Object.fromEntries(init.headers)
      : { ...init.headers };
  if (typeof init.body === "string" && !Object.keys(headers).some((name) => name.toLowerCase() === "content-type")) {
    headers["Content-Type"] = "application/json";
  }
  const response = await fetch(`/api${path}`, { ...init, headers, credentials: "include" });
  if (!response.ok) throw await toError(response);
  const result = (await response.json()) as T;
  observeAPI(path, init.method ?? "GET", result, init.body);
  return result;
}

/** Browser-side call for a file, such as a PDF; a refusal throws the same ApiError as api(). */
export async function apiFile(path: string, init: RequestInit = {}): Promise<Blob> {
  const response = await fetch(`/api${path}`, { ...init, credentials: "include" });
  if (!response.ok) throw await toError(response);
  return response.blob();
}

type Failure =
  | "signedOut"
  | "forbidden"
  | "notFound"
  | "rateLimited"
  | "unconfigured"
  | "unavailable"
  | "rejected"
  | "offline";

export function classifyFailure(cause: unknown): Failure {
  if (cause instanceof ApiError) {
    // A coded 503 is a deliberate "switched off in this environment": retrying cannot help.
    if (cause.status === 503 && cause.code?.endsWith("_UNCONFIGURED")) return "unconfigured";
    if (cause.status === 401) return "signedOut";
    if (cause.status === 403) return "forbidden";
    if (cause.status === 404) return "notFound";
    if (cause.status === 429) return "rateLimited";
    return cause.status >= 500 ? "unavailable" : "rejected";
  }
  // fetch rejects only when no response arrived; an unreadable body means the server answered.
  return cause instanceof SyntaxError ? "unavailable" : "offline";
}

export const OFFLINE_MESSAGE = "You appear to be offline. Check your connection, then retry.";
export const RATE_LIMITED_MESSAGE = "Too many requests from this connection. Wait a minute, then retry.";
export const UNCONFIGURED_MESSAGE = "This isn’t available in this environment yet. Nothing was changed.";

/** Copy safe to show a donor: the API's own explanation for a rejected request, never a transport error. */
export function failureMessage(cause: unknown, fallback: string): string {
  const failure = classifyFailure(cause);
  if (failure === "offline") return OFFLINE_MESSAGE;
  if (failure === "rateLimited") return RATE_LIMITED_MESSAGE;
  if (failure === "unconfigured") return UNCONFIGURED_MESSAGE;
  return cause instanceof ApiError && failure !== "unavailable" ? cause.message : fallback;
}

/**
 * For a command whose outcome is unknown: keeps the "not confirmed" statement and adds why.
 * A command the environment refused outright had no outcome, so it gets no such statement.
 */
export function unconfirmedMessage(cause: unknown, statement: string): string {
  const failure = classifyFailure(cause);
  if (failure === "unconfigured") return UNCONFIGURED_MESSAGE;
  if (failure === "offline") return `${statement} ${OFFLINE_MESSAGE}`;
  if (failure === "rateLimited") return `${statement} ${RATE_LIMITED_MESSAGE}`;
  return failureMessage(cause, statement);
}
