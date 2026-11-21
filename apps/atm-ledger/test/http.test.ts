import { env } from "cloudflare:workers";
import { afterEach, expect, it, vi } from "vitest";
import type { Principal } from "../src/ledger";

const asserted = vi.hoisted(() => ({ principal: undefined as unknown }));
vi.mock("../src/auth", async (original) => {
	const actual = await original<typeof import("../src/auth")>();
	return {
		...actual,
		authenticate: (...args: Parameters<typeof actual.authenticate>) =>
			asserted.principal
				? Promise.resolve(asserted.principal as Principal)
				: actual.authenticate(...args),
	};
});

import worker from "../src/index";

const principal = (role: Principal["role"]): Principal => ({
	role,
	actor: "CoachCoe",
	actorId: "17258192",
	execution: "123:1",
	revision: "a".repeat(40),
});
const send = (
	path: string,
	role: Principal["role"],
	init: RequestInit = { method: "POST" },
) => {
	asserted.principal = principal(role);
	return worker.fetch(
		new Request(`https://ledger.example.org${path}`, {
			...init,
			headers: { Authorization: "Bearer stubbed", ...init.headers },
		}),
		env,
	);
};
afterEach(() => {
	asserted.principal = undefined;
});

it.each([
	"/v1/ledger",
	"/v1/publications",
	"/v1/triage",
	"/v1/migration",
	"/v1/history",
	"/v1/recovery",
])("requires authentication on %s", async (route) => {
	const response = await worker.fetch(
		new Request(`https://ledger.example.org${route}`, {
			method:
				route.includes("ledger") || route.includes("history") ? "GET" : "POST",
		}),
		env,
	);
	expect(response.status).toBe(401);
	expect(response.headers.get("cache-control")).toBe("no-store");
	expect(await response.json()).toEqual({ detail: "Authentication required" });
});

it("refuses malformed bearer credentials without exposing token contents", async () => {
	const response = await worker.fetch(
		new Request("https://ledger.example.org/v1/ledger", {
			headers: { Authorization: "Bearer private-token-fixture" },
		}),
		env,
	);
	expect(response.status).toBe(403);
	expect(JSON.stringify(await response.json())).not.toContain(
		"private-token-fixture",
	);
});

it("rate limits ingress before authentication or durable storage access", async () => {
	const response = await worker.fetch(
		new Request("https://ledger.example.org/v1/ledger"),
		{ ...env, INGRESS: { limit: async () => ({ success: false }) } },
	);
	expect(response.status).toBe(429);
	expect(await response.json()).toEqual({
		detail: "Ledger ingress rate limit reached",
	});
});

it.each([
	["publish", "/v1/triage"],
	["publish", "/v1/migration"],
	["publish", "/v1/recovery"],
	["read", "/v1/publications"],
	["read", "/v1/triage"],
	["admin", "/v1/publications"],
] as const)("refuses %s role on POST %s", async (role, path) => {
	const response = await send(path, role, {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: "{}",
	});
	expect(response.status).toBe(403);
	expect(await response.json()).toEqual({
		detail: "Operation is not authorized",
	});
});

it("refuses history to non-administrators", async () => {
	const response = await send("/v1/history", "publish", { method: "GET" });
	expect(response.status).toBe(403);
});

it("requires a JSON content type on writes", async () => {
	const response = await send("/v1/publications", "publish", {
		method: "POST",
		headers: { "Content-Type": "text/plain" },
		body: "{}",
	});
	expect(response.status).toBe(415);
	expect(await response.json()).toEqual({ detail: "JSON required" });
});

it("rejects bodies over the size limit", async () => {
	const response = await send("/v1/publications", "publish", {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: new Uint8Array(16 * 1024 * 1024 + 1),
	});
	expect(response.status).toBe(413);
	expect(await response.json()).toEqual({ detail: "Body too large" });
});

it("keys the ingress limit on the caller address so one client cannot lock out others", async () => {
	const keys: string[] = [];
	const limited = {
		...env,
		INGRESS: {
			limit: async ({ key }: { key: string }) => {
				keys.push(key);
				return { success: key !== "203.0.113.9" };
			},
		},
	};
	const call = (ip?: string) =>
		worker.fetch(
			new Request("https://ledger.example.org/v1/ledger", {
				headers: ip ? { "cf-connecting-ip": ip } : {},
			}),
			limited,
		);
	expect((await call("203.0.113.9")).status).toBe(429);
	expect((await call("198.51.100.4")).status).toBe(401);
	expect((await call()).status).toBe(401);
	expect(keys).toEqual(["203.0.113.9", "198.51.100.4", "unknown"]);
});

it("distinguishes unknown routes, wrong methods and role mismatches", async () => {
	const missing = await send("/v1/nothing", "admin", { method: "GET" });
	expect(missing.status).toBe(404);
	const wrong = await send("/v1/ledger", "admin", { method: "POST" });
	expect(wrong.status).toBe(405);
	expect(wrong.headers.get("allow")).toBe("GET");
	const forbidden = await send("/v1/history", "read", { method: "GET" });
	expect(forbidden.status).toBe(403);
	expect((await send("/v1/toString", "admin", { method: "GET" })).status).toBe(
		404,
	);
});

it.each(["abc", "", "-1", "1.5", "99999999999999999999"])(
	"rejects history cursor %j",
	async (after) => {
		const response = await send(`/v1/history?after=${after}`, "admin", {
			method: "GET",
		});
		expect(response.status).toBe(400);
		expect(await response.json()).toEqual({ detail: "Invalid history cursor" });
	},
);

it("caps publication and triage bodies below the migration limit", async () => {
	const big = JSON.stringify({ pad: "x".repeat(1024 * 1024 + 70 * 1024) });
	for (const [path, role] of [
		["/v1/publications", "publish"],
		["/v1/triage", "admin"],
	] as const) {
		const response = await send(path, role, {
			method: "POST",
			headers: { "Content-Type": "application/json" },
			body: big,
		});
		expect(response.status).toBe(413);
	}
	const migration = await send("/v1/migration", "admin", {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: big,
	});
	expect(migration.status).toBe(400);
});

it("rejects an oversized declared length before reading the body", async () => {
	const response = await send("/v1/publications", "publish", {
		method: "POST",
		headers: {
			"Content-Type": "application/json",
			"Content-Length": String(2 * 1024 * 1024),
		},
		body: "{}",
	});
	expect(response.status).toBe(413);
});
