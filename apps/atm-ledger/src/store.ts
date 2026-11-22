import { DurableObject } from "cloudflare:workers";
import { Buffer } from "node:buffer";
import {
	applyDelta,
	canonical,
	check,
	type Delta,
	digest,
	emptyLedger,
	hex,
	type Ledger,
	LedgerError,
	object as objectFields,
	type Principal,
	publication,
	type Snapshot,
	sha,
	triage,
	validateLedger,
} from "./ledger";

type Head = {
	version: number;
	digest: string;
	history_hash: string;
	ledger: string;
};
type EventRow = {
	version: number;
	hash: string;
	payload: string;
	operation: string;
	request_digest: string;
};
interface Event {
	schemaVersion: 1;
	source: string;
	version: number;
	previousHash: string;
	beforeDigest: string;
	afterDigest: string;
	kind: "import" | "publish" | "triage" | "recovery";
	operation: string;
	at: string;
	principal: Principal;
	change: Delta;
	legacy?: { commit: string; parent: string | null; at: string };
	requestDigest: string;
}
const ZERO = "0".repeat(64);
const MAX_ARCHIVED_BYTES = 2 * 1024 * 1024;
export const DAILY_REQUEST_QUOTA = 1000;
// Readers stop here so a flood of reads cannot starve publication or admin calls.
export const DAILY_READ_QUOTA = 800;
export const DAILY_EVENT_QUOTA = 200;
export const ARCHIVE_QUOTA_BYTES = 100 * 1024 * 1024;
const UPSERT_HEAD =
	"INSERT INTO head(id,version,digest,history_hash,ledger) VALUES(1,?,?,?,?) ON CONFLICT(id) DO UPDATE SET version=excluded.version,digest=excluded.digest,history_hash=excluded.history_hash,ledger=excluded.ledger";

const archiveBytes = (row: EventRow) =>
	canonical({ hash: row.hash, event: JSON.parse(row.payload) });

async function holds(object: R2ObjectBody | null, bytes: string) {
	return (
		object !== null &&
		object.size <= MAX_ARCHIVED_BYTES &&
		(await object.text()) === bytes
	);
}

export class LedgerStore extends DurableObject<Env> {
	async request(
		operation:
			| "snapshot"
			| "publish"
			| "triage"
			| "migrate"
			| "history"
			| "recover",
		body: unknown,
		principal: Principal,
	): Promise<
		| {
				ok: true;
				value: Snapshot | Awaited<ReturnType<LedgerStore["history"]>>;
		  }
		| { ok: false; status: number; detail: string }
	> {
		try {
			this.consumeRequest(principal.role);
			switch (operation) {
				case "snapshot":
					return { ok: true, value: await this.snapshot() };
				case "publish":
					return { ok: true, value: await this.publish(body, principal) };
				case "triage":
					return { ok: true, value: await this.classify(body, principal) };
				case "migrate":
					return { ok: true, value: await this.migrate(body, principal) };
				case "recover":
					return { ok: true, value: await this.recover(principal) };
				case "history": {
					check(
						principal.role === "admin",
						"History requires administrator identity",
						403,
					);
					check(typeof body === "number", "Invalid history cursor");
					return { ok: true, value: await this.history(body) };
				}
			}
		} catch (error) {
			if (error instanceof LedgerError)
				return { ok: false, status: error.status, detail: error.message };
			throw error;
		}
	}
	constructor(ctx: DurableObjectState, env: Env) {
		super(ctx, env);
		ctx.blockConcurrencyWhile(async () => {
			ctx.storage.sql.exec(`
        CREATE TABLE IF NOT EXISTS head (id INTEGER PRIMARY KEY CHECK (id=1), version INTEGER NOT NULL, digest TEXT NOT NULL, history_hash TEXT NOT NULL, ledger TEXT NOT NULL);
        CREATE TABLE IF NOT EXISTS events (version INTEGER PRIMARY KEY, hash TEXT NOT NULL, payload TEXT NOT NULL, operation TEXT NOT NULL UNIQUE, request_digest TEXT NOT NULL);
        CREATE TABLE IF NOT EXISTS budget (id INTEGER PRIMARY KEY CHECK(id=1), day TEXT NOT NULL, requests INTEGER NOT NULL, events INTEGER NOT NULL, archive_bytes INTEGER NOT NULL);
        INSERT OR IGNORE INTO budget VALUES(1,'',0,0,0);
        CREATE TABLE IF NOT EXISTS archive_pending (version INTEGER PRIMARY KEY REFERENCES events(version));
        CREATE TRIGGER IF NOT EXISTS events_no_update BEFORE UPDATE ON events BEGIN SELECT RAISE(ABORT, 'History is append-only'); END;
        CREATE TRIGGER IF NOT EXISTS events_no_delete BEFORE DELETE ON events BEGIN SELECT RAISE(ABORT, 'History is append-only'); END;
      `);
		});
	}

	private budget() {
		const today = new Date().toISOString().slice(0, 10);
		this.ctx.storage.sql.exec(
			"UPDATE budget SET day=?,requests=0,events=0 WHERE day<>?",
			today,
			today,
		);
		return this.ctx.storage.sql
			.exec<{ requests: number; events: number; archive_bytes: number }>(
				"SELECT requests,events,archive_bytes FROM budget WHERE id=1",
			)
			.one();
	}

	private consumeRequest(role: Principal["role"]) {
		this.ctx.storage.transactionSync(() => {
			check(
				this.budget().requests <
					(role === "read" ? DAILY_READ_QUOTA : DAILY_REQUEST_QUOTA),
				"Daily ledger request quota reached",
				429,
			);
			this.ctx.storage.sql.exec(
				"UPDATE budget SET requests=requests+1 WHERE id=1",
			);
		});
	}

	private reserveEvent(payload: string, hash: string) {
		const budget = this.budget();
		const bytes =
			2 * Buffer.byteLength(`{"event":${payload},"hash":"${hash}"}`);
		check(
			budget.events < DAILY_EVENT_QUOTA,
			"Daily ledger event quota reached",
			429,
		);
		check(
			budget.archive_bytes + bytes <= ARCHIVE_QUOTA_BYTES,
			"Ledger archive quota reached",
			507,
		);
		this.ctx.storage.sql.exec(
			"UPDATE budget SET events=events+1,archive_bytes=archive_bytes+? WHERE id=1",
			bytes,
		);
	}

	private head(): Snapshot {
		const row = this.ctx.storage.sql
			.exec<Head>(
				"SELECT version,digest,history_hash,ledger FROM head WHERE id=1",
			)
			.toArray()[0];
		check(row, "Ledger has not been migrated", 503);
		const ledger: unknown = JSON.parse(row.ledger);
		validateLedger(ledger, this.env.SOURCE);
		check(digest(ledger) === row.digest, "Ledger integrity failure", 503);
		return {
			version: row.version,
			digest: row.digest,
			historyHash: row.history_hash,
			ledger,
		};
	}

	private key(version: number, hash: string) {
		return `history/${encodeURIComponent(this.env.SOURCE)}/${String(999999999999 - version).padStart(12, "0")}-${hash}.json`;
	}

	private eventKey(version: number, hash: string) {
		return `events/${encodeURIComponent(this.env.SOURCE)}/${String(version).padStart(12, "0")}-${hash}.json`;
	}

	private genesis(): Snapshot {
		const ledger = emptyLedger(this.env.SOURCE);
		return { version: 0, digest: digest(ledger), historyHash: ZERO, ledger };
	}

	private event(operation: string) {
		return this.ctx.storage.sql
			.exec<EventRow>("SELECT * FROM events WHERE operation=?", operation)
			.toArray()[0];
	}

	private eventCount() {
		return this.ctx.storage.sql
			.exec<{ n: number }>("SELECT COUNT(*) n FROM events")
			.one().n;
	}

	private async archive(): Promise<void> {
		const pending = this.ctx.storage.sql
			.exec<EventRow>(
				"SELECT e.* FROM events e JOIN archive_pending p USING(version) ORDER BY version LIMIT 10",
			)
			.toArray();
		for (const event of pending) {
			const bytes = archiveBytes(event);
			for (const key of [
				this.eventKey(event.version, event.hash),
				this.key(event.version, event.hash),
			]) {
				const existing = await this.env.ARCHIVE.get(key);
				if (existing) {
					check(await holds(existing, bytes), "Archive integrity failure", 503);
					continue;
				}
				const stored = await this.env.ARCHIVE.put(key, bytes, {
					onlyIf: { etagDoesNotMatch: "*" },
					httpMetadata: { contentType: "application/json" },
				});
				if (!stored)
					check(
						await holds(await this.env.ARCHIVE.get(key), bytes),
						"Archive collision",
						503,
					);
			}
			this.ctx.storage.sql.exec(
				"DELETE FROM archive_pending WHERE version=?",
				event.version,
			);
		}
		check(
			this.ctx.storage.sql
				.exec<{ n: number }>("SELECT COUNT(*) n FROM archive_pending")
				.one().n === 0,
			"Archive backlog exceeds request bound",
			503,
		);
	}

	async snapshot(): Promise<Snapshot> {
		await this.archive();
		const head = this.head();
		const tail = await this.env.ARCHIVE.list({
			prefix: `history/${encodeURIComponent(this.env.SOURCE)}/`,
			limit: 1,
		});
		check(
			tail.objects[0]?.key === this.key(head.version, head.historyHash),
			"Archive and ledger disagree; retry or investigate recovery",
			503,
		);
		const archived = await this.env.ARCHIVE.get(tail.objects[0].key);
		const event = this.ctx.storage.sql
			.exec<EventRow>("SELECT * FROM events WHERE version=?", head.version)
			.one();
		check(
			await holds(archived, archiveBytes(event)),
			"Archive integrity failure",
			503,
		);
		check(
			event.hash === head.historyHash &&
				digest(JSON.parse(event.payload)) === event.hash &&
				JSON.parse(event.payload).afterDigest === head.digest,
			"History and projection disagree",
			503,
		);
		check(
			this.head().version === head.version,
			"Concurrent ledger update; retry",
			409,
		);
		return head;
	}

	private append(
		prior: Snapshot,
		next: Ledger,
		kind: Event["kind"],
		operation: string,
		requestDigest: string,
		principal: Principal,
		change: Delta,
		legacy?: Event["legacy"],
	) {
		check(
			prior.version < 999999999998 &&
				Buffer.byteLength(canonical(next)) <= 1024 * 1024,
			"Ledger capacity exceeded",
			503,
		);
		const event: Event = {
			schemaVersion: 1,
			source: this.env.SOURCE,
			version: prior.version + 1,
			previousHash: prior.historyHash,
			beforeDigest: prior.digest,
			afterDigest: digest(next),
			kind,
			operation,
			requestDigest,
			at: new Date().toISOString(),
			principal,
			change,
			...(legacy ? { legacy } : {}),
		};
		const hash = digest(event);
		const payload = canonical(event);
		check(
			Buffer.byteLength(payload) <= 512 * 1024,
			"Event capacity exceeded",
			413,
		);
		this.reserveEvent(payload, hash);
		this.ctx.storage.sql.exec(
			"INSERT INTO events(version,hash,payload,operation,request_digest) VALUES(?,?,?,?,?)",
			event.version,
			hash,
			payload,
			operation,
			requestDigest,
		);
		this.ctx.storage.sql.exec(
			"INSERT INTO archive_pending(version) VALUES(?)",
			event.version,
		);
		this.ctx.storage.sql.exec(
			UPSERT_HEAD,
			event.version,
			event.afterDigest,
			hash,
			canonical(next),
		);
	}

	// A command is keyed by its kind and GitHub execution, so a retry of the
	// same request is acknowledged once and a different one is refused.
	private async command(
		kind: "publish" | "triage",
		body: Record<string, unknown>,
		principal: Principal,
		conflict: string,
		apply: (prior: Snapshot) => { ledger: Ledger; delta: Delta },
	): Promise<Snapshot> {
		await this.snapshot();
		const operation = `${kind}:${principal.execution}`;
		const requestDigest = digest(body);
		this.ctx.storage.transactionSync(() => {
			const duplicate = this.event(operation);
			if (duplicate) {
				check(duplicate.request_digest === requestDigest, conflict, 409);
				return;
			}
			const prior = this.head();
			check(
				body.expectedVersion === prior.version &&
					body.previousDigest === prior.digest,
				"Stale ledger revision",
				409,
			);
			const { ledger, delta } = apply(prior);
			this.append(
				prior,
				ledger,
				kind,
				operation,
				requestDigest,
				principal,
				delta,
			);
		});
		return this.snapshot();
	}

	async publish(body: unknown, principal: Principal): Promise<Snapshot> {
		check(
			principal.role === "publish",
			"Publication requires publisher identity",
			403,
		);
		objectFields(body, ["expectedVersion", "previousDigest", "ledger"]);
		const ledger = body.ledger;
		validateLedger(ledger, this.env.SOURCE);
		return this.command(
			"publish",
			body,
			principal,
			"Conflicting publication retry",
			(prior) => ({
				ledger,
				delta: publication(prior.ledger, ledger, principal.execution),
			}),
		);
	}

	async classify(body: unknown, principal: Principal): Promise<Snapshot> {
		check(
			principal.role === "admin",
			"Triage requires administrator identity",
			403,
		);
		objectFields(body, ["expectedVersion", "previousDigest", "change"]);
		return this.command(
			"triage",
			body,
			principal,
			"Conflicting triage retry",
			(prior) =>
				triage(
					prior.ledger,
					body.change,
					principal.actor,
					new Date().toISOString(),
				),
		);
	}

	async migrate(body: unknown, principal: Principal): Promise<Snapshot> {
		check(
			principal.role === "admin",
			"Migration requires administrator identity",
			403,
		);
		objectFields(body, ["source", "history"]);
		check(
			body.source === this.env.SOURCE &&
				Array.isArray(body.history) &&
				body.history.length > 0 &&
				body.history.length <= DAILY_EVENT_QUOTA,
			"Invalid migration history: between 1 and the daily event quota of entries required",
		);
		if (this.eventCount() === 0) {
			for (const index of ["events", "history"]) {
				const archived = await this.env.ARCHIVE.list({
					prefix: `${index}/${encodeURIComponent(this.env.SOURCE)}/`,
					limit: 1,
				});
				check(
					archived.objects.length === 0,
					"Retained archive requires recovery, not migration",
					409,
				);
			}
		}
		const history = body.history;
		const requestDigest = digest(body);
		this.ctx.storage.transactionSync(() => {
			const existing = this.event("migration:begin");
			if (existing) {
				check(
					existing.request_digest === requestDigest,
					"Ledger already initialized from a different history",
					409,
				);
				return;
			}
			check(this.eventCount() === 0, "Ledger already initialized", 409);
			let prior = this.genesis();
			let parent: string | null = null;
			const seen = new Set<string>();
			for (const [i, entry] of history.entries()) {
				objectFields(entry, ["commit", "parent", "at", "ledger"]);
				check(
					typeof entry.commit === "string" &&
						sha.test(entry.commit) &&
						entry.parent === parent &&
						typeof entry.at === "string" &&
						Number.isFinite(Date.parse(entry.at)),
					"Migration chain is incomplete",
				);
				check(!seen.has(entry.commit), "Migration chain repeats a commit");
				seen.add(entry.commit);
				validateLedger(entry.ledger, this.env.SOURCE);
				check(
					entry.ledger.sequence >= prior.ledger.sequence &&
						digest(entry.ledger.receipts.slice(0, prior.ledger.sequence)) ===
							digest(prior.ledger.receipts),
					"Migration rewrites receipt history",
				);
				for (const f of prior.ledger.findings) {
					const n = entry.ledger.findings.find((item) => item.id === f.id);
					check(
						n &&
							n.firstSeen === f.firstSeen &&
							n.check === f.check &&
							n.occurrences >= f.occurrences &&
							digest(n.audit.slice(0, f.audit.length)) === digest(f.audit),
						"Migration rewrites finding history",
					);
				}
				const changes: Delta = {
					findings: entry.ledger.findings.filter(
						(f) =>
							digest(f) !==
							digest(prior.ledger.findings.find((n) => n.id === f.id) ?? null),
					),
					receipts: entry.ledger.receipts.slice(prior.ledger.sequence),
				};
				check(
					digest(applyDelta(prior.ledger, changes)) === digest(entry.ledger),
					"Migration changes finding order or identity",
				);
				this.append(
					prior,
					entry.ledger,
					"import",
					i === 0 ? "migration:begin" : `migration:${entry.commit}`,
					requestDigest,
					principal,
					changes,
					{ commit: entry.commit, parent, at: entry.at },
				);
				prior = this.head();
				parent = entry.commit;
			}
		});
		return this.snapshot();
	}

	async history(
		after: number,
	): Promise<{ events: { hash: string; event: Event }[]; more: boolean }> {
		check(Number.isSafeInteger(after) && after >= 0, "Invalid history cursor");
		await this.archive();
		const rows = this.ctx.storage.sql
			.exec<EventRow>(
				"SELECT * FROM events WHERE version>? ORDER BY version LIMIT 101",
				after,
			)
			.toArray();
		return {
			events: rows
				.slice(0, 100)
				.map((row) => ({ hash: row.hash, event: JSON.parse(row.payload) })),
			more: rows.length > 100,
		};
	}

	async recover(principal: Principal): Promise<Snapshot> {
		check(
			principal.role === "admin",
			"Recovery requires administrator identity",
			403,
		);
		await this.archive();
		const prefix = `events/${encodeURIComponent(this.env.SOURCE)}/`;
		let head = this.ctx.storage.sql
			.exec<{ n: number }>("SELECT COUNT(*) n FROM head")
			.one().n
			? this.head()
			: this.genesis();
		const startingVersion = head.version;
		const listing = await this.env.ARCHIVE.list({
			prefix,
			limit: 10,
			startAfter: `${prefix}${String(head.version).padStart(12, "0")}~`,
		});
		for (const object of listing.objects) {
			const position = Number(
				object.key.slice(prefix.length, prefix.length + 12),
			);
			if (position <= head.version) continue;
			const archived = await this.env.ARCHIVE.get(object.key);
			check(
				archived && archived.size <= MAX_ARCHIVED_BYTES,
				"Missing or oversized archive entry",
				503,
			);
			const entry: unknown = JSON.parse(await archived.text());
			objectFields(entry, ["hash", "event"]);
			const event = entry.event;
			objectFields(event, [
				"schemaVersion",
				"source",
				"version",
				"previousHash",
				"beforeDigest",
				"afterDigest",
				"kind",
				"operation",
				"requestDigest",
				"at",
				"principal",
				"change",
				"legacy",
			]);
			check(
				event.schemaVersion === 1 &&
					event.source === this.env.SOURCE &&
					event.version === head.version + 1 &&
					event.previousHash === head.historyHash &&
					event.beforeDigest === head.digest &&
					digest(event) === entry.hash &&
					object.key === this.eventKey(head.version + 1, String(entry.hash)),
				"Broken archive chain",
				503,
			);
			objectFields(event.change, ["findings", "receipts"]);
			// applyDelta validates every resulting finding and receipt before it becomes live state.
			const delta = event.change;
			check(
				Array.isArray(delta.findings) && Array.isArray(delta.receipts),
				"Invalid archived change",
				503,
			);
			const changes: unknown = {
				schemaVersion: 1,
				source: this.env.SOURCE,
				findings: delta.findings,
				receipts: delta.receipts,
				sequence: delta.receipts.length,
			};
			validateLedger(changes, this.env.SOURCE);
			const next = applyDelta(head.ledger, {
				findings: changes.findings,
				receipts: changes.receipts,
			});
			check(
				digest(next) === event.afterDigest &&
					typeof event.operation === "string" &&
					typeof event.requestDigest === "string" &&
					hex.test(event.requestDigest),
				"Archive projection mismatch",
				503,
			);
			this.ctx.storage.transactionSync(() => {
				const latest =
					this.ctx.storage.sql
						.exec<{ version: number }>("SELECT version FROM head WHERE id=1")
						.toArray()[0]?.version ?? 0;
				check(latest === head.version, "Concurrent recovery change", 409);
				const existing = this.ctx.storage.sql
					.exec<EventRow>("SELECT * FROM events WHERE version=?", event.version)
					.toArray()[0];
				const payload = canonical(event);
				if (existing)
					check(
						existing.hash === entry.hash && existing.payload === payload,
						"Existing history contradicts archive",
						503,
					);
				else {
					this.reserveEvent(payload, String(entry.hash));
					this.ctx.storage.sql.exec(
						"INSERT INTO events(version,hash,payload,operation,request_digest) VALUES(?,?,?,?,?)",
						event.version,
						entry.hash,
						payload,
						event.operation,
						event.requestDigest,
					);
				}
				this.ctx.storage.sql.exec(
					UPSERT_HEAD,
					event.version,
					event.afterDigest,
					entry.hash,
					canonical(next),
				);
			});
			head = this.head();
		}
		check(
			!listing.truncated,
			"Recovery made bounded progress; retry to continue",
			503,
		);
		if (head.version > startingVersion) {
			this.ctx.storage.transactionSync(() => {
				check(
					this.head().version === head.version,
					"Concurrent recovery change",
					409,
				);
				this.append(
					head,
					head.ledger,
					"recovery",
					`recovery:${principal.execution}`,
					digest({ recoveredFromVersion: startingVersion }),
					principal,
					{ findings: [], receipts: [] },
				);
			});
		}
		return this.snapshot();
	}
}
