import { reset, runInDurableObject } from "cloudflare:test";
import { env } from "cloudflare:workers";
import { beforeEach, describe, expect, it, vi } from "vitest";
import {
	digest,
	emptyLedger,
	type Finding,
	type Ledger,
	type MigrationEntry,
	type Principal,
} from "../src/ledger";
import {
	ARCHIVE_QUOTA_BYTES,
	DAILY_EVENT_QUOTA,
	DAILY_READ_QUOTA,
	DAILY_REQUEST_QUOTA,
} from "../src/store";

const admin: Principal = {
	role: "admin",
	actor: "CoachCoe",
	actorId: "17258192",
	execution: "123:1",
	revision: "a".repeat(40),
};
const publisher: Principal = { ...admin, role: "publish", execution: "124:1" };
const source = "github:pre-dad/DAD-Main";
const migration = (
	ledger = emptyLedger(source),
): { source: string; history: MigrationEntry[] } => ({
	source,
	history: [
		{
			commit: "b".repeat(40),
			parent: null,
			at: "2026-10-13T00:00:00Z",
			ledger,
		},
	],
});
beforeEach(reset);

async function call(
	stub: DurableObjectStub<import("../src/store").LedgerStore>,
	operation: "snapshot" | "migrate" | "publish" | "triage" | "recover",
	body: unknown,
	principal: Principal = admin,
) {
	const result = await stub.request(operation, body, principal);
	if (!result.ok) throw new Error(result.detail);
	if (!("ledger" in result.value)) throw new Error("Expected ledger snapshot");
	return result.value;
}
function collected(prior: Ledger, execution = publisher.execution): Ledger {
	return {
		...structuredClone(prior),
		sequence: prior.sequence + 1,
		receipts: [
			...prior.receipts,
			{
				execution,
				runId: "atm-20261013000000-aabbccdd",
				revision: "a".repeat(40),
				bundleSha256: "b".repeat(64),
				artifactId: "1234",
				artifactDigest: "c".repeat(64),
				eligible: true,
			},
		],
	};
}
// Each archived event stores the event and its envelope object.
const objectsFor = (events: number) => 2 * events;

async function setup(ledger = emptyLedger(source)) {
	const stub = env.LEDGERS.getByName(source);
	const snapshot = await call(stub, "migrate", migration(ledger), admin);
	return { stub, snapshot };
}

describe("durable ledger", () => {
	it("requires explicit migration and never invents missing state", async () => {
		await expect(
			call(env.LEDGERS.getByName(source), "snapshot", null),
		).rejects.toThrow("not been migrated");
	});

	it("preserves migration history and refuses to replace it", async () => {
		const ledger = collected(emptyLedger(source), "100:1");
		const m = migration();
		m.history.push({
			commit: "c".repeat(40),
			parent: "b".repeat(40),
			at: "2026-10-13T01:00:00Z",
			ledger,
		});
		const stub = env.LEDGERS.getByName(source);
		const snapshot = await call(stub, "migrate", m, admin);
		expect(snapshot.version).toBe(2);
		expect(snapshot.ledger).toEqual(ledger);
		expect(await call(stub, "migrate", m, admin)).toEqual(snapshot);
		await expect(call(stub, "migrate", migration(), admin)).rejects.toThrow(
			"different history",
		);
		const log = await stub.history(0);
		expect(log.events.map((e) => e.event.legacy?.commit)).toEqual([
			"b".repeat(40),
			"c".repeat(40),
		]);
		expect(log.events[1].event.previousHash).toBe(log.events[0].hash);
		expect(log.events.every((e) => digest(e.event) === e.hash)).toBe(true);
	});

	it("does not allow two writers to publish from the same revision", async () => {
		const { stub, snapshot } = await setup();
		const requests = [publisher, { ...publisher, execution: "125:1" }].map(
			(p) =>
				call(
					stub,
					"publish",
					{
						expectedVersion: snapshot.version,
						previousDigest: snapshot.digest,
						ledger: collected(snapshot.ledger, p.execution),
					},
					p,
				),
		);
		const results = await Promise.allSettled(requests);
		expect(results.filter((r) => r.status === "fulfilled")).toHaveLength(1);
		expect(results.filter((r) => r.status === "rejected")).toHaveLength(1);
		expect((await call(stub, "snapshot", null)).ledger.receipts).toHaveLength(
			1,
		);
	});

	it("acknowledges identical retries without duplicating receipts, and rejects conflicting retries", async () => {
		const { stub, snapshot } = await setup();
		const body = {
			expectedVersion: snapshot.version,
			previousDigest: snapshot.digest,
			ledger: collected(snapshot.ledger),
		};
		const saved = await call(stub, "publish", body, publisher);
		expect(await call(stub, "publish", body, publisher)).toEqual(saved);
		await expect(
			call(
				stub,
				"publish",
				{
					...body,
					ledger: {
						...body.ledger,
						receipts: [{ ...body.ledger.receipts[0], artifactId: "99" }],
					},
				},
				publisher,
			),
		).rejects.toThrow("Conflicting publication");
		expect((await call(stub, "snapshot", null)).version).toBe(2);
	});

	it("rejects finding reordering that would make archive replay ambiguous", async () => {
		const finding: Finding = {
			id: "d".repeat(24),
			source,
			check: "build",
			severity: "medium",
			disposition: "confirmed",
			firstSeen: "2026-10-13T00:00:00Z",
			lastSeen: "2026-10-13T00:00:00Z",
			lastRunId: "atm-20261013000000-aabbccdd",
			occurrences: 1,
			audit: [],
		};
		const { stub, snapshot } = await setup({
			...emptyLedger(source),
			findings: [finding, { ...finding, id: "e".repeat(24), check: "lint" }],
		});
		const next = collected(snapshot.ledger);
		next.findings.reverse();
		await expect(
			call(
				stub,
				"publish",
				{
					expectedVersion: snapshot.version,
					previousDigest: snapshot.digest,
					ledger: next,
				},
				publisher,
			),
		).rejects.toThrow("finding order");
		expect((await call(stub, "snapshot", null)).digest).toBe(snapshot.digest);
	});

	it("prevents mutation or deletion of stored events at the SQLite layer", async () => {
		const { stub } = await setup();
		await runInDurableObject(stub, async (_instance, state) => {
			expect(() =>
				state.storage.sql.exec(
					"UPDATE events SET payload='{}' WHERE version=1",
				),
			).toThrow("append-only");
			expect(() =>
				state.storage.sql.exec("DELETE FROM events WHERE version=1"),
			).toThrow("append-only");
		});
	});

	it("fails closed on archive tampering or a database restored behind its archive", async () => {
		const { stub, snapshot } = await setup();
		const key = (await env.ARCHIVE.list()).objects[0].key;
		await env.ARCHIVE.put(key, "tampered");
		await runInDurableObject(stub, async (_instance, state) => {
			state.storage.sql.exec(
				"INSERT INTO archive_pending(version) VALUES(?)",
				snapshot.version,
			);
		});
		await expect(call(stub, "snapshot", null)).rejects.toThrow(
			"Archive integrity",
		);
	});

	it("denies publication, migration and triage to a read identity", async () => {
		const { stub, snapshot } = await setup();
		const reader: Principal = { ...admin, role: "read" };
		await expect(call(stub, "migrate", migration(), reader)).rejects.toThrow(
			"administrator",
		);
		await expect(
			call(
				stub,
				"publish",
				{
					expectedVersion: snapshot.version,
					previousDigest: snapshot.digest,
					ledger: collected(snapshot.ledger),
				},
				reader,
			),
		).rejects.toThrow("publisher");
		await expect(call(stub, "triage", {}, reader)).rejects.toThrow(
			"administrator",
		);
		await expect(call(stub, "recover", {}, reader)).rejects.toThrow(
			"administrator",
		);
	});

	it("binds a publication to the authenticated GitHub execution", async () => {
		const { stub, snapshot } = await setup();
		await expect(
			call(
				stub,
				"publish",
				{
					expectedVersion: snapshot.version,
					previousDigest: snapshot.digest,
					ledger: collected(snapshot.ledger, "999:1"),
				},
				publisher,
			),
		).rejects.toThrow("Execution does not match identity");
		expect((await call(stub, "snapshot", null)).version).toBe(snapshot.version);
	});

	it("allows audited owner triage without altering receipts, and prevents publisher triage", async () => {
		const finding: Finding = {
			id: "d".repeat(24),
			source,
			check: "build",
			severity: "unassessed",
			disposition: "untriaged",
			firstSeen: "2026-10-13T00:00:00Z",
			lastSeen: "2026-10-13T00:00:00Z",
			lastRunId: "atm-20261013000000-aabbccdd",
			occurrences: 1,
			audit: [],
		};
		const { stub, snapshot } = await setup({
			...emptyLedger(source),
			findings: [finding],
		});
		const classified = await call(stub, "triage", {
			expectedVersion: snapshot.version,
			previousDigest: snapshot.digest,
			change: {
				finding: finding.id,
				severity: "medium",
				disposition: "resolved",
				noteDigest: "e".repeat(64),
			},
		});
		expect(classified.ledger.receipts).toEqual(snapshot.ledger.receipts);
		expect(classified.ledger.findings[0].audit[0]).toMatchObject({
			actor: "CoachCoe",
			noteDigest: "e".repeat(64),
			disposition: "resolved",
		});
		await expect(
			call(
				stub,
				"publish",
				{
					expectedVersion: snapshot.version,
					previousDigest: snapshot.digest,
					ledger: collected(snapshot.ledger),
				},
				publisher,
			),
		).rejects.toThrow("Stale ledger");
		const changed = collected(classified.ledger);
		changed.findings[0].severity = "low";
		await expect(
			call(
				stub,
				"publish",
				{
					expectedVersion: classified.version,
					previousDigest: classified.digest,
					ledger: changed,
				},
				publisher,
			),
		).rejects.toThrow("Publisher cannot change triage");
		const recurrence = collected(classified.ledger);
		recurrence.receipts[0].eligible = false;
		recurrence.findings[0] = {
			...recurrence.findings[0],
			disposition: "reopened",
			occurrences: 2,
			lastRunId: "atm-20261013010000-11223344",
			lastSeen: "2026-10-13T01:00:00Z",
		};
		const saved = await call(
			stub,
			"publish",
			{
				expectedVersion: classified.version,
				previousDigest: classified.digest,
				ledger: recurrence,
			},
			publisher,
		);
		expect(saved.ledger.findings[0].audit).toEqual(
			classified.ledger.findings[0].audit,
		);
		expect(saved.ledger.findings[0].disposition).toBe("reopened");
	});

	it("refuses a projection restored behind the retained archive", async () => {
		const { stub, snapshot } = await setup();
		await call(
			stub,
			"publish",
			{
				expectedVersion: snapshot.version,
				previousDigest: snapshot.digest,
				ledger: collected(snapshot.ledger),
			},
			publisher,
		);
		await runInDurableObject(stub, async (_instance, state) => {
			state.storage.sql.exec(
				"UPDATE head SET version=?,digest=?,history_hash=?,ledger=? WHERE id=1",
				snapshot.version,
				snapshot.digest,
				snapshot.historyHash,
				JSON.stringify(snapshot.ledger),
			);
		});
		await expect(call(stub, "snapshot", null)).rejects.toThrow(
			"Archive and ledger disagree",
		);
		await expect(
			call(
				stub,
				"publish",
				{
					expectedVersion: snapshot.version,
					previousDigest: snapshot.digest,
					ledger: collected(snapshot.ledger, "126:1"),
				},
				{ ...publisher, execution: "126:1" },
			),
		).rejects.toThrow("Archive and ledger disagree");
		await expect(call(stub, "triage", {})).rejects.toThrow(
			"Archive and ledger disagree",
		);
		expect((await stub.history(0)).events).toHaveLength(2);
		expect((await env.ARCHIVE.list()).objects).toHaveLength(objectsFor(2));
		const recovered = await call(stub, "recover", {});
		expect(recovered.ledger.receipts).toEqual(
			collected(snapshot.ledger).receipts,
		);
		expect(recovered.version).toBe(3);
		expect((await stub.history(0)).events.at(-1)?.event.kind).toBe("recovery");
		expect(await call(stub, "recover", {})).toEqual(recovered);
	});

	it("retains committed events when archive acknowledgement fails and safely retries", async () => {
		const stub = env.LEDGERS.getByName(source);
		await runInDurableObject(stub, async (instance) => {
			const archive = (instance as unknown as { env: Env }).env.ARCHIVE;
			const fault = vi
				.spyOn(archive, "put")
				.mockRejectedValue(new Error("Archive unavailable"));
			try {
				await expect(instance.migrate(migration(), admin)).rejects.toThrow(
					"Archive unavailable",
				);
			} finally {
				fault.mockRestore();
			}
		});
		const saved = await call(stub, "migrate", migration());
		expect(saved.version).toBe(1);
		expect((await stub.history(0)).events).toHaveLength(1);
		expect((await env.ARCHIVE.list()).objects).toHaveLength(objectsFor(1));
	});

	it("refuses bootstrap over retained history after namespace loss", async () => {
		const key = `events/${encodeURIComponent(source)}/000000000001-retained.json`;
		await env.ARCHIVE.put(key, "retained history");
		const stub = env.LEDGERS.getByName(source);
		await expect(call(stub, "migrate", migration())).rejects.toThrow(
			"requires recovery",
		);
		await runInDurableObject(stub, async (_instance, state) => {
			expect(
				state.storage.sql
					.exec<{ n: number }>("SELECT COUNT(*) n FROM events")
					.one().n,
			).toBe(0);
		});
		expect(await (await env.ARCHIVE.get(key))?.text()).toBe("retained history");
	});

	it("blocks excess requests before storage access and resets daily counters without dropping retained usage", async () => {
		const { stub } = await setup();
		await runInDurableObject(stub, async (_instance, state) => {
			state.storage.sql.exec(
				`UPDATE budget SET requests=${DAILY_REQUEST_QUOTA},events=${DAILY_EVENT_QUOTA},archive_bytes=42 WHERE id=1`,
			);
		});
		await expect(call(stub, "snapshot", null)).rejects.toThrow(
			"Daily ledger request quota reached",
		);
		await runInDurableObject(stub, async (_instance, state) => {
			state.storage.sql.exec("UPDATE budget SET day='2000-01-01' WHERE id=1");
		});
		await call(stub, "snapshot", null);
		await runInDurableObject(stub, async (_instance, state) => {
			expect(
				state.storage.sql
					.exec("SELECT requests,events,archive_bytes FROM budget")
					.one(),
			).toEqual({ requests: 1, events: 0, archive_bytes: 42 });
		});
	});

	it("stops reads at their own quota and keeps publication and admin calls available", async () => {
		const { stub, snapshot } = await setup();
		const reader: Principal = { ...admin, role: "read", execution: "125:1" };
		await runInDurableObject(stub, async (_instance, state) => {
			state.storage.sql.exec(
				`UPDATE budget SET requests=${DAILY_READ_QUOTA} WHERE id=1`,
			);
		});
		await expect(call(stub, "snapshot", null, reader)).rejects.toThrow(
			"Daily ledger request quota reached",
		);
		await call(stub, "snapshot", null, admin);
		await call(
			stub,
			"publish",
			{
				expectedVersion: snapshot.version,
				previousDigest: snapshot.digest,
				ledger: collected(snapshot.ledger),
			},
			publisher,
		);
	});

	it("validates the history cursor before touching the archive", async () => {
		const { stub } = await setup();
		await runInDurableObject(stub, async (_instance, state) => {
			state.storage.sql.exec("INSERT INTO archive_pending VALUES(1)");
		});
		for (const cursor of [-1, Number.NaN]) {
			expect(await stub.request("history", cursor, admin)).toMatchObject({
				ok: false,
				status: 400,
			});
		}
		await runInDurableObject(stub, async (_instance, state) => {
			expect(
				state.storage.sql
					.exec<{ n: number }>("SELECT COUNT(*) n FROM archive_pending")
					.one().n,
			).toBe(1);
		});
	});

	it("refuses a migration chain that repeats a commit", async () => {
		const m = migration();
		m.history.push(
			{ ...m.history[0], commit: "c".repeat(40), parent: m.history[0].commit },
			{ ...m.history[0], commit: "c".repeat(40), parent: "c".repeat(40) },
		);
		const stub = env.LEDGERS.getByName(source);
		await expect(call(stub, "migrate", m)).rejects.toThrow(
			"Migration chain repeats a commit",
		);
	});

	it("refuses a migration longer than the daily event quota", async () => {
		const m = migration();
		for (let i = 1; i <= DAILY_EVENT_QUOTA; i++)
			m.history.push({ ...m.history[0], commit: String(i), parent: null });
		const stub = env.LEDGERS.getByName(source);
		await expect(call(stub, "migrate", m)).rejects.toThrow(
			"Invalid migration history",
		);
	});

	it.each([
		["events", "Daily ledger event quota reached"],
		["archive_bytes", "Ledger archive quota reached"],
	])(
		"rejects a publication at its %s quota without changing history",
		async (column, message) => {
			const { stub, snapshot } = await setup();
			await runInDurableObject(stub, async (_instance, state) => {
				if (column === "events")
					state.storage.sql.exec(
						"UPDATE budget SET events=? WHERE id=1",
						DAILY_EVENT_QUOTA,
					);
				else
					state.storage.sql.exec(
						"UPDATE budget SET archive_bytes=? WHERE id=1",
						ARCHIVE_QUOTA_BYTES - 1,
					);
			});
			await expect(
				call(
					stub,
					"publish",
					{
						expectedVersion: snapshot.version,
						previousDigest: snapshot.digest,
						ledger: collected(snapshot.ledger),
					},
					publisher,
				),
			).rejects.toThrow(message);
			expect(await call(stub, "snapshot", null)).toEqual(snapshot);
			expect((await env.ARCHIVE.list()).objects).toHaveLength(objectsFor(1));
			expect((await stub.history(0)).events).toHaveLength(1);
		},
	);

	it("drains a bounded archive batch on an identical migration retry without reserving storage twice", async () => {
		const m = migration();
		for (let i = 1; i < 12; i++)
			m.history.push({
				...m.history[0],
				commit: i.toString(16).padStart(40, "0"),
				parent: m.history[i - 1].commit,
			});
		const stub = env.LEDGERS.getByName(source);
		await expect(call(stub, "migrate", m)).rejects.toThrow("Archive backlog");
		const snapshot = await call(stub, "migrate", m);
		expect(snapshot.version).toBe(12);
		expect((await env.ARCHIVE.list()).objects).toHaveLength(objectsFor(12));
		let used = 0;
		await runInDurableObject(stub, async (_instance, state) => {
			used = state.storage.sql
				.exec<{ n: number }>("SELECT archive_bytes n FROM budget")
				.one().n;
		});
		const objects = await env.ARCHIVE.list();
		expect(used).toBe(objects.objects.reduce((n, o) => n + o.size, 0));
		await call(stub, "migrate", m);
		await runInDurableObject(stub, async (_instance, state) => {
			expect(
				state.storage.sql
					.exec<{ n: number }>("SELECT archive_bytes n FROM budget")
					.one().n,
			).toBe(used);
		});
	});
});
