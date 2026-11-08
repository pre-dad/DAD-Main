import { createHash, createHmac, randomBytes, timingSafeEqual } from "node:crypto";
import { cookies } from "next/headers";
import { type NextRequest, NextResponse } from "next/server";
import { donorApiBase } from "@/lib/donorApi";

type Context = { params: Promise<{ provider: string }> };
const equal = (a: string, b: string) => a.length === b.length && timingSafeEqual(Buffer.from(a), Buffer.from(b));
async function handle(request: NextRequest, context: Context) {
  const { provider } = await context.params;
  if (Number(request.headers.get("content-length") ?? 0) > 16384) return new Response(null, { status: 413 });
  if (provider !== "apple" && provider !== "google") return new Response(null, { status: 404 });
  const origin = process.env.DONOR_PUBLIC_ORIGIN;
  if (!origin) return new Response("Sign-in is not configured.", { status: 503 });
  const key = provider.toUpperCase();
  const client = process.env[`DONOR_OAUTH_${key}_CLIENT_ID`];
  const secret = process.env[`DONOR_OAUTH_${key}_CLIENT_SECRET`];
  if (!client || !secret) return new Response("This sign-in method is not configured.", { status: 503 });
  const callback = `${origin}/account/oauth/${provider}`;
  const jar = await cookies();
  const cookie = `dad_oauth_${provider}`;
  const params = request.method === "POST" ? new URLSearchParams(await request.text()) : request.nextUrl.searchParams;
  if (!params.has("code") && !params.has("error") && request.method === "GET") {
    const state = randomBytes(32).toString("base64url");
    const nonce = randomBytes(32).toString("base64url");
    const verifier = randomBytes(32).toString("base64url");
    const value = JSON.stringify({ state, nonce, verifier });
    jar.set(
      cookie,
      `${Buffer.from(value).toString("base64url")}.${createHmac("sha256", secret).update(value).digest("base64url")}`,
      {
        httpOnly: true,
        secure: origin.startsWith("https:"),
        sameSite: provider === "apple" ? "none" : "lax",
        path: `/account/oauth/${provider}`,
        maxAge: 600,
      },
    );
    const url = new URL(
      provider === "apple"
        ? "https://appleid.apple.com/auth/authorize"
        : "https://accounts.google.com/o/oauth2/v2/auth",
    );
    for (const [name, value] of Object.entries({
      client_id: client,
      redirect_uri: callback,
      response_type: "code",
      scope: provider === "apple" ? "name email" : "openid email",
      state,
      nonce,
    }))
      url.searchParams.set(name, value);
    if (provider === "apple") url.searchParams.set("response_mode", "form_post");
    else {
      url.searchParams.set("code_challenge", createHash("sha256").update(verifier).digest("base64url"));
      url.searchParams.set("code_challenge_method", "S256");
    }
    return NextResponse.redirect(url);
  }
  const failed = () => NextResponse.redirect(new URL("/account/sign-in?social_error=1", origin), 303);
  try {
    const [encoded, signature] = (jar.get(cookie)?.value ?? "").split(".");
    jar.set(cookie, "", {
      path: `/account/oauth/${provider}`,
      maxAge: 0,
      httpOnly: true,
      secure: origin.startsWith("https:"),
      sameSite: provider === "apple" ? "none" : "lax",
    });
    if (!encoded || !signature) return failed();
    const value = Buffer.from(encoded, "base64url").toString();
    if (!equal(signature, createHmac("sha256", secret).update(value).digest("base64url"))) return failed();
    const saved = JSON.parse(value);
    if (!saved || !equal(saved.state, params.get("state") ?? "") || !params.get("code")) return failed();
    const body = new URLSearchParams({
      client_id: client,
      client_secret: secret,
      code: params.get("code") ?? "",
      redirect_uri: callback,
      grant_type: "authorization_code",
    });
    if (provider === "google") body.set("code_verifier", saved.verifier);
    const response = await fetch(
      provider === "apple" ? "https://appleid.apple.com/auth/token" : "https://oauth2.googleapis.com/token",
      { method: "POST", body, signal: AbortSignal.timeout(15000), cache: "no-store" },
    );
    if (!response.ok) return failed();
    const token = (await response.json()).id_token;
    if (typeof token !== "string" || token.length > 16000) return failed();
    const claims = JSON.parse(Buffer.from(token.split(".")[1], "base64url").toString());
    if (typeof claims.nonce !== "string" || typeof saved.nonce !== "string" || !equal(claims.nonce, saved.nonce))
      return failed();
    const accepted = await fetch(`${donorApiBase()}/donor/oidc/${provider}/sign-in`, {
      method: "POST",
      headers: { "Content-Type": "application/json", Origin: origin, Cookie: request.headers.get("cookie") ?? "" },
      body: JSON.stringify({ id_token: token, nonce: saved.nonce }),
      signal: AbortSignal.timeout(15000),
      cache: "no-store",
    });
    if (!accepted.ok) return failed();
    const result = NextResponse.redirect(new URL("/account/profile", origin), 303);
    for (const value of accepted.headers.getSetCookie()) result.headers.append("Set-Cookie", value);
    result.headers.set("Cache-Control", "no-store");
    return result;
  } catch {
    return failed();
  }
}
export const GET = handle;
export const POST = handle;
