import {
	createRemoteJWKSet,
	errors,
	type JWTPayload,
	type JWTVerifyGetKey,
	jwtVerify,
} from "jose";
import { actorName, check, LedgerError, type Principal, sha } from "./ledger";

const githubKeys = createRemoteJWKSet(
	new URL("https://token.actions.githubusercontent.com/.well-known/jwks"),
	{ timeoutDuration: 5000 },
);
const workflowRoles = new Map<unknown, Principal["role"]>([
	["atm-ledger-reader", "read"],
	["atm-ledger-publisher", "publish"],
]);
type Trust = Pick<
	Env,
	"SOURCE" | "REPOSITORY_ID" | "OWNER_ID" | "ADMIN_ACTOR_IDS"
>;

export function authorize(claims: JWTPayload, env: Trust): Principal {
	const repo = env.SOURCE.replace(/^github:/, "");
	check(
		claims.repository === repo &&
			claims.repository_id === env.REPOSITORY_ID &&
			claims.repository_owner_id === env.OWNER_ID &&
			claims.runner_environment === "github-hosted",
		"Identity is not authorized",
		403,
	);
	check(
		typeof claims.workflow_ref === "string" &&
			typeof claims.actor === "string" &&
			actorName.test(claims.actor) &&
			typeof claims.actor_id === "string" &&
			/^\d+$/.test(claims.actor_id),
		"Invalid workflow identity",
		403,
	);
	check(
		typeof claims.run_id === "string" &&
			/^\d+$/.test(claims.run_id) &&
			typeof claims.run_attempt === "string" &&
			/^\d+$/.test(claims.run_attempt) &&
			typeof claims.sha === "string" &&
			sha.test(claims.sha),
		"Invalid execution identity",
		403,
	);
	const workflow = `${repo}/.github/workflows/atm.yml@`;
	const admin = `${workflow}refs/heads/dev`;
	const supportedRef =
		claims.workflow_ref === admin ||
		claims.workflow_ref === `${workflow}refs/heads/main` ||
		new RegExp(
			`^${workflow.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}refs/pull/[0-9]+/merge$`,
		).test(claims.workflow_ref);
	let role: Principal["role"] | undefined;
	if (
		claims.workflow_ref === admin &&
		claims.event_name === "workflow_dispatch" &&
		claims.environment === "atm-ledger-admin" &&
		env.ADMIN_ACTOR_IDS.split(",").includes(claims.actor_id)
	)
		role = "admin";
	else if (
		supportedRef &&
		["push", "pull_request", "workflow_dispatch"].includes(
			String(claims.event_name),
		)
	)
		role = workflowRoles.get(claims.environment);
	check(role, "Identity is not authorized", 403);
	return {
		role,
		actor: claims.actor,
		actorId: claims.actor_id,
		execution: `${claims.run_id}:${claims.run_attempt}`,
		revision: claims.sha,
	};
}

// Key retrieval failures (timeout, bad HTTP status, network) are outages, not
// evidence about the caller; every other failure rejects the credential.
function unavailable(error: unknown) {
	if (error instanceof LedgerError) return false;
	if (error instanceof errors.JWKSTimeout) return true;
	if (error instanceof errors.JOSEError)
		return error.code === "ERR_JOSE_GENERIC";
	return true;
}

export async function authenticate(
	token: string,
	env: Trust,
	keys: JWTVerifyGetKey = githubKeys,
): Promise<Principal> {
	try {
		check(token.length <= 16384, "Invalid credential");
		const { payload } = await jwtVerify(token, keys, {
			issuer: "https://token.actions.githubusercontent.com",
			audience: `atm-ledger:${env.SOURCE}`,
			algorithms: ["RS256"],
			requiredClaims: ["exp", "iat", "nbf"],
			maxTokenAge: "10m",
			clockTolerance: 10,
		});
		return authorize(payload, env);
	} catch (error) {
		if (error instanceof LedgerError && error.status !== 403) throw error;
		if (unavailable(error)) {
			console.error(
				JSON.stringify({
					event: "ledger_identity_keys_unavailable",
					type: error instanceof Error ? error.name : "Unknown",
				}),
			);
			throw new LedgerError("Identity provider unavailable", 503);
		}
		throw new LedgerError("Identity is not authorized", 403);
	}
}
