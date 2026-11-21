import { authenticate } from "./auth";
import { LedgerError, type Principal } from "./ledger";

export { LedgerStore } from "./store";

const MIGRATION_LIMIT = 16 * 1024 * 1024;
// A ledger is capped at 1 MiB; the allowance covers the request envelope.
const COMMAND_LIMIT = 1024 * 1024 + 64 * 1024;
async function body(request: Request, limit: number): Promise<unknown> {
	if (!request.headers.get("content-type")?.startsWith("application/json"))
		throw new LedgerError("JSON required", 415);
	if (!request.body) throw new LedgerError("Body required");
	const declared = Number(request.headers.get("content-length") ?? "0");
	if (declared > limit) throw new LedgerError("Body too large", 413);
	const reader = request.body.getReader();
	const chunks: Uint8Array[] = [];
	let size = 0;
	for (;;) {
		const next = await reader.read();
		if (next.done) break;
		size += next.value.byteLength;
		if (size > limit) {
			await reader.cancel();
			throw new LedgerError("Body too large", 413);
		}
		chunks.push(next.value);
	}
	const bytes = new Uint8Array(size);
	let offset = 0;
	for (const chunk of chunks) {
		bytes.set(chunk, offset);
		offset += chunk.byteLength;
	}
	try {
		return JSON.parse(
			new TextDecoder("utf-8", { fatal: true, ignoreBOM: true }).decode(bytes),
		);
	} catch {
		throw new LedgerError("Invalid JSON");
	}
}

function cursor(value: string | null): number {
	if (value !== null && !/^\d+$/.test(value))
		throw new LedgerError("Invalid history cursor");
	return Number(value ?? "0");
}

export default {
	async fetch(request: Request, env: Env): Promise<Response> {
		const headers = {
			"Cache-Control": "no-store",
			"X-Content-Type-Options": "nosniff",
			"Content-Security-Policy": "default-src 'none'; frame-ancestors 'none'",
		};
		try {
			if (
				!(
					await env.INGRESS.limit({
						key: request.headers.get("cf-connecting-ip") ?? "unknown",
					})
				).success
			)
				throw new LedgerError("Ledger ingress rate limit reached", 429);
			const authorization = request.headers.get("authorization");
			if (!authorization?.startsWith("Bearer "))
				throw new LedgerError("Authentication required", 401);
			const principal = await authenticate(authorization.slice(7), env);
			const url = new URL(request.url);
			const store = env.LEDGERS.getByName(env.SOURCE);
			const call = async (
				operation: Parameters<typeof store.request>[0],
				input: unknown,
			) => {
				const result = await store.request(operation, input, principal);
				if (!result.ok) throw new LedgerError(result.detail, result.status);
				return Response.json(result.value, { headers });
			};
			const routes: Record<
				string,
				{
					method: string;
					role: Principal["role"] | null;
					run: () => Promise<Response>;
				}
			> = {
				"/v1/ledger": {
					method: "GET",
					role: null,
					run: () => call("snapshot", null),
				},
				"/v1/history": {
					method: "GET",
					role: "admin",
					run: () => call("history", cursor(url.searchParams.get("after"))),
				},
				"/v1/publications": {
					method: "POST",
					role: "publish",
					run: async () => call("publish", await body(request, COMMAND_LIMIT)),
				},
				"/v1/triage": {
					method: "POST",
					role: "admin",
					run: async () => call("triage", await body(request, COMMAND_LIMIT)),
				},
				"/v1/migration": {
					method: "POST",
					role: "admin",
					run: async () =>
						call("migrate", await body(request, MIGRATION_LIMIT)),
				},
				"/v1/recovery": {
					method: "POST",
					role: "admin",
					run: async () =>
						call("recover", await body(request, MIGRATION_LIMIT)),
				},
			};
			const route = Object.hasOwn(routes, url.pathname)
				? routes[url.pathname]
				: undefined;
			if (!route) throw new LedgerError("Not found", 404);
			if (request.method !== route.method)
				return Response.json(
					{ detail: "Method not allowed" },
					{ status: 405, headers: { ...headers, Allow: route.method } },
				);
			if (route.role && principal.role !== route.role)
				throw new LedgerError("Operation is not authorized", 403);
			return await route.run();
		} catch (error) {
			if (error instanceof LedgerError)
				return Response.json(
					{ detail: error.message },
					{ status: error.status, headers },
				);
			console.error(
				JSON.stringify({
					event: "ledger_request_failed",
					type: error instanceof Error ? error.name : "Unknown",
				}),
			);
			return Response.json(
				{ detail: "Ledger unavailable" },
				{ status: 503, headers },
			);
		}
	},
} satisfies ExportedHandler<Env>;
