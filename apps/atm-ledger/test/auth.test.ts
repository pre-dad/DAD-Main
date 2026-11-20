import { env } from "cloudflare:workers";
import { createLocalJWKSet, exportJWK, generateKeyPair, SignJWT } from "jose";
import { describe, expect, it } from "vitest";
import { authenticate, authorize } from "../src/auth";

const claims = {
	repository: "pre-dad/DAD-Main",
	repository_id: "1378692308",
	repository_owner_id: "353088835",
	runner_environment: "github-hosted",
	actor: "CoachCoe",
	actor_id: "17258192",
	run_id: "123",
	run_attempt: "1",
	sha: "a".repeat(40),
	event_name: "push",
	environment: "atm-ledger-reader",
	workflow_ref: "pre-dad/DAD-Main/.github/workflows/atm.yml@refs/heads/dev",
};

describe("GitHub OIDC authorization", () => {
	it.each([
		{ repository_id: "1" },
		{ repository_owner_id: "1" },
		{ repository: "attacker/DAD-Main" },
		{ runner_environment: "self-hosted" },
		{ environment: "production" },
		{ event_name: "pull_request_target" },
		{
			workflow_ref:
				"pre-dad/DAD-Main/.github/workflows/other.yml@refs/heads/dev",
		},
		{
			workflow_ref:
				"pre-dad/DAD-Main/.github/workflows/atm.yml@refs/heads/untrusted",
		},
	])(
		"rejects identities outside the configured trust boundary: %j",
		(override) => {
			expect(() => authorize({ ...claims, ...override }, env)).toThrow();
		},
	);

	it.each(["refs/pull/7/merge", "refs/pull/123456/merge"])(
		"accepts pull request merge refs: %s",
		(ref) => {
			const workflow_ref = `pre-dad/DAD-Main/.github/workflows/atm.yml@${ref}`;
			expect(
				authorize({ ...claims, event_name: "pull_request", workflow_ref }, env)
					.role,
			).toBe("read");
		},
	);

	it.each([
		"refs/pull/merge",
		"refs/pull/x/merge",
		"refs/pull/7/head",
		"refs/pull/7/merge/extra",
		"refs/tags/v1",
	])("rejects unsupported workflow refs: %s", (ref) => {
		const workflow_ref = `pre-dad/DAD-Main/.github/workflows/atm.yml@${ref}`;
		expect(() =>
			authorize({ ...claims, event_name: "pull_request", workflow_ref }, env),
		).toThrow();
	});

	it.each(["refs/heads/main", "refs/pull/7/merge"])(
		"never grants admin from %s",
		(ref) => {
			expect(() =>
				authorize(
					{
						...claims,
						environment: "atm-ledger-admin",
						event_name: "workflow_dispatch",
						workflow_ref: `pre-dad/DAD-Main/.github/workflows/atm.yml@${ref}`,
					},
					env,
				),
			).toThrow();
		},
	);

	it("separates readers, publishers and owner-only triage", () => {
		expect(authorize(claims, env).role).toBe("read");
		expect(
			authorize({ ...claims, environment: "atm-ledger-publisher" }, env).role,
		).toBe("publish");
		const admin = {
			...claims,
			environment: "atm-ledger-admin",
			event_name: "workflow_dispatch",
		};
		expect(authorize(admin, env).role).toBe("admin");
		expect(() =>
			authorize({ ...admin, actor_id: "9", actor: "attacker" }, env),
		).toThrow();
		expect(() => authorize({ ...admin, event_name: "push" }, env)).toThrow();
	});

	it("requires valid signatures, issuer, audience and expiry", async () => {
		const key = await generateKeyPair("RS256", {
			modulusLength: 2048,
			extractable: true,
		});
		const jwk = {
			...(await exportJWK(key.publicKey)),
			kid: "test",
			alg: "RS256",
		};
		const verify = createLocalJWKSet({ keys: [jwk] });
		const sign = (
			issuer = "https://token.actions.githubusercontent.com",
			audience = "atm-ledger:github:pre-dad/DAD-Main",
			expires = "2m",
		) =>
			new SignJWT(claims)
				.setProtectedHeader({ alg: "RS256", kid: "test" })
				.setIssuer(issuer)
				.setAudience(audience)
				.setIssuedAt()
				.setNotBefore("0s")
				.setExpirationTime(expires)
				.sign(key.privateKey);
		expect((await authenticate(await sign(), env, verify)).execution).toBe(
			"123:1",
		);
		await expect(
			authenticate(await sign("https://attacker.example"), env, verify),
		).rejects.toThrow();
		await expect(
			authenticate(await sign(undefined, "other-service"), env, verify),
		).rejects.toThrow();
		await expect(
			authenticate(await sign(undefined, undefined, "-1h"), env, verify),
		).rejects.toThrow();
		const valid = await sign();
		const parts = valid.split(".");
		parts[1] = btoa(JSON.stringify({ ...claims, actor_id: "1" }));
		await expect(authenticate(parts.join("."), env, verify)).rejects.toThrow();
	});
});
