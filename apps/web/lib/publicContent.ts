import { connection } from "next/server";
import type { Schema } from "./api";
import { donorApiBase } from "./donorApi";

/**
 * Spend by category across every published initiative, or null when the API cannot supply
 * it. Rendering waits for a request because the API address is runtime configuration, but
 * the response itself is shared for a minute.
 */
export async function loadOverallSpend(): Promise<Schema<"OverallSpend"> | null> {
  return loadPublic<Schema<"OverallSpend">>("/donor/public/spend");
}

/** Every published initiative, or null when the API cannot supply them. */
export async function loadInitiatives(): Promise<Schema<"PublicInitiative">[] | null> {
  return loadPublic<Schema<"PublicInitiative">[]>("/initiatives");
}

/** One published initiative; undefined when none is published under the slug, null when unavailable. */
export async function loadInitiative(slug: string): Promise<Schema<"PublicInitiative"> | undefined | null> {
  return loadPublic<Schema<"PublicInitiative">>(`/initiatives/${encodeURIComponent(slug)}`, true);
}

/** The parsed body, null when the API or its body is unavailable, and undefined for a 404 when `missing` is set. */
async function loadPublic<T>(path: string, missing: true): Promise<T | undefined | null>;
async function loadPublic<T>(path: string): Promise<T | null>;
async function loadPublic<T>(path: string, missing = false): Promise<T | undefined | null> {
  await connection();
  try {
    const response = await fetch(`${donorApiBase()}${path}`, {
      next: { revalidate: 60 },
      signal: AbortSignal.timeout(3000),
    });
    if (missing && response.status === 404) return undefined;
    return response.ok ? ((await response.json()) as T) : null;
  } catch {
    return null;
  }
}
