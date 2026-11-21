/**
 * Passkeys in the browser, for donors and staff. Options and credentials cross the API as WebAuthn JSON,
 * with binary fields in base64url; browsers without the JSON helpers are converted here.
 */

import { useSyncExternalStore } from "react";
import type { Schema } from "./api";

/** The contract types options only as an object; the DOM's WebAuthn JSON types describe them. */
export type Ceremony<Options> = Omit<Schema<"Ceremony">, "options"> & { options: Options };

function passkeysSupported(): boolean {
  return typeof PublicKeyCredential === "function" && typeof navigator.credentials?.create === "function";
}

const unchanging = () => () => {};

/**
 * Whether this browser can use passkeys. The server cannot know, so it renders serverAnswer
 * and the browser corrects it after hydration.
 */
export function usePasskeysSupported(serverAnswer: boolean): boolean {
  return useSyncExternalStore(unchanging, passkeysSupported, () => serverAnswer);
}

function decode(value: string): ArrayBuffer {
  const base64 = value
    .replace(/-/g, "+")
    .replace(/_/g, "/")
    .padEnd(Math.ceil(value.length / 4) * 4, "=");
  return Uint8Array.from(atob(base64), (char) => char.charCodeAt(0)).buffer;
}

function encode(value: ArrayBuffer | null): string | undefined {
  if (!value) return undefined;
  return btoa(Array.from(new Uint8Array(value), (byte) => String.fromCharCode(byte)).join(""))
    .replace(/\+/g, "-")
    .replace(/\//g, "_")
    .replace(/=+$/, "");
}

const descriptors = (list?: PublicKeyCredentialDescriptorJSON[]) =>
  list?.map((descriptor) => ({ ...descriptor, id: decode(descriptor.id) }) as PublicKeyCredentialDescriptor);

function creationOptions(json: PublicKeyCredentialCreationOptionsJSON): PublicKeyCredentialCreationOptions {
  if ("parseCreationOptionsFromJSON" in PublicKeyCredential)
    return PublicKeyCredential.parseCreationOptionsFromJSON(json);
  return {
    ...json,
    challenge: decode(json.challenge),
    user: { ...json.user, id: decode(json.user.id) },
    excludeCredentials: descriptors(json.excludeCredentials),
  } as PublicKeyCredentialCreationOptions;
}

function requestOptions(json: PublicKeyCredentialRequestOptionsJSON): PublicKeyCredentialRequestOptions {
  if ("parseRequestOptionsFromJSON" in PublicKeyCredential)
    return PublicKeyCredential.parseRequestOptionsFromJSON(json);
  return {
    ...json,
    challenge: decode(json.challenge),
    allowCredentials: descriptors(json.allowCredentials),
  } as PublicKeyCredentialRequestOptions;
}

function credentialJSON(credential: PublicKeyCredential): unknown {
  if (typeof credential.toJSON === "function") return credential.toJSON();
  const { response } = credential;
  const fields: Record<string, unknown> = { clientDataJSON: encode(response.clientDataJSON) };
  if (response instanceof AuthenticatorAttestationResponse) {
    fields.attestationObject = encode(response.attestationObject);
    fields.transports = response.getTransports?.() ?? [];
  } else if (response instanceof AuthenticatorAssertionResponse) {
    fields.authenticatorData = encode(response.authenticatorData);
    fields.signature = encode(response.signature);
    fields.userHandle = encode(response.userHandle);
  }
  return {
    id: credential.id,
    rawId: encode(credential.rawId),
    type: credential.type,
    response: fields,
    authenticatorAttachment: credential.authenticatorAttachment ?? undefined,
    clientExtensionResults: credential.getClientExtensionResults(),
  };
}

async function answer(request: Promise<Credential | null>): Promise<unknown> {
  const credential = await request;
  if (!(credential instanceof PublicKeyCredential)) throw new DOMException("No passkey was chosen", "NotAllowedError");
  return credentialJSON(credential);
}

export const createPasskey = (options: PublicKeyCredentialCreationOptionsJSON) =>
  answer(navigator.credentials.create({ publicKey: creationOptions(options) }));

export const choosePasskey = (options: PublicKeyCredentialRequestOptionsJSON) =>
  answer(navigator.credentials.get({ publicKey: requestOptions(options) }));

/**
 * What a WebAuthn rejection means to the person, or null for any other failure. Browsers
 * report a cancelled or timed-out prompt as NotAllowedError, deliberately without saying which.
 */
export function passkeyFailure(cause: unknown): "cancelled" | "unsupported" | "refused" | null {
  if (!(cause instanceof DOMException)) return null;
  switch (cause.name) {
    case "NotAllowedError":
    case "AbortError":
      return "cancelled";
    case "NotSupportedError":
      return "unsupported";
    default:
      return "refused";
  }
}

export const UNSUPPORTED_MESSAGE =
  "This browser can’t use passkeys. Use a current version of Chrome, Edge, Firefox, or Safari.";

/** For NotSupportedError: the browser has WebAuthn, but nothing here can make or use a suitable passkey. */
export const NO_SUITABLE_PASSKEY_MESSAGE =
  "This device can’t use a passkey here. Try another browser, your phone, or a security key.";

/** Read out while a passkey prompt is open; the button keeps its own name. */
export const WAITING_MESSAGE = "Waiting for your passkey. Follow the prompt from your device or password manager.";
