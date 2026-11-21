import { afterEach, expect, it, vi } from "vitest";
import { choosePasskey, createPasskey, passkeyFailure } from "./passkeys";

const bytes = (...values: number[]) => new Uint8Array(values).buffer;

// A browser that has WebAuthn but neither JSON helper, as older Safari and Firefox do.
function olderBrowser() {
  class AuthenticatorAttestationResponse {
    clientDataJSON = bytes(1);
    attestationObject = bytes(251, 255);
    getTransports = () => ["internal"];
  }
  class AuthenticatorAssertionResponse {
    clientDataJSON = bytes(2);
    authenticatorData = bytes(3);
    signature = bytes(4);
    userHandle = bytes(5, 6);
  }
  class PublicKeyCredential {
    id = "AQI";
    rawId = bytes(1, 2);
    type = "public-key";
    authenticatorAttachment = "platform";
    constructor(readonly response: object) {}
    getClientExtensionResults = () => ({});
  }
  const credentials = {
    create: vi.fn(
      async (_options: CredentialCreationOptions) => new PublicKeyCredential(new AuthenticatorAttestationResponse()),
    ),
    get: vi.fn(
      async (_options: CredentialRequestOptions) => new PublicKeyCredential(new AuthenticatorAssertionResponse()),
    ),
  };
  vi.stubGlobal("PublicKeyCredential", PublicKeyCredential);
  vi.stubGlobal("AuthenticatorAttestationResponse", AuthenticatorAttestationResponse);
  vi.stubGlobal("AuthenticatorAssertionResponse", AuthenticatorAssertionResponse);
  Object.defineProperty(navigator, "credentials", { value: credentials, configurable: true });
  return credentials;
}

afterEach(() => {
  vi.unstubAllGlobals();
  Reflect.deleteProperty(navigator, "credentials");
});

it("converts creation options and the new passkey without the browser's JSON helpers", async () => {
  const credentials = olderBrowser();
  const created = await createPasskey({
    challenge: "-_8",
    rp: { id: "staff.example.org", name: "DAD staff" },
    user: { id: "AAE", name: "a@example.org", displayName: "A" },
    pubKeyCredParams: [{ type: "public-key", alg: -7 }],
    excludeCredentials: [{ type: "public-key", id: "Bw" }],
  });
  const publicKey = credentials.create.mock.calls[0][0].publicKey as PublicKeyCredentialCreationOptions;
  expect(new Uint8Array(publicKey.challenge as ArrayBuffer)).toEqual(new Uint8Array([251, 255]));
  expect(new Uint8Array(publicKey.user.id as ArrayBuffer)).toEqual(new Uint8Array([0, 1]));
  expect(new Uint8Array(publicKey.excludeCredentials?.[0].id as ArrayBuffer)).toEqual(new Uint8Array([7]));
  expect(created).toEqual({
    id: "AQI",
    rawId: "AQI",
    type: "public-key",
    authenticatorAttachment: "platform",
    clientExtensionResults: {},
    response: { clientDataJSON: "AQ", attestationObject: "-_8", transports: ["internal"] },
  });
});

it("signs a request without the browser's JSON helpers", async () => {
  const credentials = olderBrowser();
  const assertion = await choosePasskey({ challenge: "AQID", rpId: "staff.example.org", userVerification: "required" });
  const publicKey = credentials.get.mock.calls[0][0].publicKey as PublicKeyCredentialRequestOptions;
  expect(new Uint8Array(publicKey.challenge as ArrayBuffer)).toEqual(new Uint8Array([1, 2, 3]));
  expect(publicKey.userVerification).toBe("required");
  expect(assertion).toMatchObject({
    response: { clientDataJSON: "Ag", authenticatorData: "Aw", signature: "BA", userHandle: "BQY" },
  });
});

it("names WebAuthn rejections and leaves other failures alone", () => {
  expect(passkeyFailure(new DOMException("", "NotAllowedError"))).toBe("cancelled");
  expect(passkeyFailure(new DOMException("", "InvalidStateError"))).toBe("refused");
  expect(passkeyFailure(new DOMException("", "NotSupportedError"))).toBe("unsupported");
  expect(passkeyFailure(new DOMException("", "SecurityError"))).toBe("refused");
  expect(passkeyFailure(new Error("network"))).toBeNull();
});

it("uses the browser's own JSON helpers and credential serialisation when present", async () => {
  const toJSON = () => ({ id: "modern", type: "public-key" });
  class PublicKeyCredential {
    toJSON = toJSON;
    static parseRequestOptionsFromJSON = vi.fn((json: { challenge: string }) => ({ parsed: json.challenge }));
  }
  vi.stubGlobal("PublicKeyCredential", PublicKeyCredential);
  const get = vi.fn(async () => new PublicKeyCredential());
  Object.defineProperty(navigator, "credentials", { value: { create: vi.fn(), get }, configurable: true });
  await expect(choosePasskey({ challenge: "abc" })).resolves.toEqual({ id: "modern", type: "public-key" });
  expect(get).toHaveBeenCalledWith({ publicKey: { parsed: "abc" } });
});

it("reports a dismissed prompt as not allowed rather than a credential", async () => {
  olderBrowser().get.mockResolvedValueOnce(null as never);
  await expect(choosePasskey({ challenge: "AQID" })).rejects.toMatchObject({ name: "NotAllowedError" });
});

it("encodes a large attestation without exceeding the argument limit", async () => {
  const credentials = olderBrowser();
  const large = new Uint8Array(200_000).fill(65).buffer;
  const credential = await credentials.create.getMockImplementation()?.({ publicKey: {} as never });
  Object.assign(credential?.response as object, { attestationObject: large });
  credentials.create.mockResolvedValueOnce(credential as never);
  const created = (await createPasskey({
    challenge: "AQID",
    rp: { name: "DAD" },
    user: { id: "AAE", name: "a", displayName: "A" },
    pubKeyCredParams: [],
  })) as { response: { attestationObject: string } };
  expect(created.response.attestationObject).toHaveLength(Math.ceil((200_000 * 4) / 3));
});
