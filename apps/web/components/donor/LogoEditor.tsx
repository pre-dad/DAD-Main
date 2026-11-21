"use client";
import Image from "next/image";
import { useState } from "react";
import { Announcer } from "@/components/donor/Announcer";
import { api, failureMessage, type RequestBody } from "@/lib/api";

type LogoPath = "/donor/partner/logo" | "/donor/staff/partners/{partner_id}/logo";

/**
 * Uploads, shows and removes a partner organization's logo at `path`, which serves it on GET.
 * A page with its own live region passes `announce` so outcomes are read from one place.
 */
export function LogoEditor({
  path,
  hasLogo,
  alt,
  onSaved,
  level = 3,
  announce,
}: {
  path: string;
  hasLogo: boolean;
  alt: string;
  onSaved: () => void;
  level?: 2 | 3;
  announce?: { status: (message: string) => void; alert: (message: string) => void };
}) {
  const Heading = level === 2 ? "h2" : "h3";
  const [busy, setBusy] = useState(false);
  const [ownStatus, setOwnStatus] = useState("");
  const [ownError, setOwnError] = useState("");
  const setStatus = announce?.status ?? setOwnStatus;
  const setError = announce?.alert ?? setOwnError;
  const [version, setVersion] = useState(0);
  async function save(dataURL: string) {
    setBusy(true);
    setStatus("");
    setError("");
    try {
      await api(path, {
        method: "POST",
        body: JSON.stringify({ data_url: dataURL } satisfies RequestBody<LogoPath, "post">),
      });
      setVersion((v) => v + 1);
      setStatus(dataURL ? "Organization logo saved." : "Organization logo removed.");
      onSaved();
    } catch (cause) {
      setError(failureMessage(cause, "The logo could not be saved. Please retry."));
    } finally {
      setBusy(false);
    }
  }
  async function upload(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setStatus("");
    setError("");
    const input = event.currentTarget.elements.namedItem("logo");
    const file = input instanceof HTMLInputElement ? input.files?.[0] : undefined;
    if (
      !(file instanceof File) ||
      file.size === 0 ||
      file.size > 256 * 1024 ||
      !["image/png", "image/jpeg"].includes(file.type)
    ) {
      setError("Choose a PNG or JPEG up to 256 KiB.");
      return;
    }
    setBusy(true);
    setError("");
    try {
      const dataURL = await new Promise<string>((resolve, reject) => {
        const reader = new FileReader();
        reader.onload = () => resolve(String(reader.result));
        reader.onerror = () => reject(new Error("File could not be read"));
        reader.readAsDataURL(file);
      });
      await save(dataURL);
    } catch {
      setError("The image could not be read. Please choose it again.");
      setBusy(false);
    }
  }
  return (
    <div>
      <Heading>Organization logo</Heading>
      {hasLogo && (
        <Image
          src={`/api${path}?v=${version}`}
          alt={alt}
          width={160}
          height={80}
          unoptimized
          style={{ objectFit: "contain" }}
        />
      )}
      <form onSubmit={upload}>
        <fieldset disabled={busy}>
          <label>
            Logo image
            <input type="file" name="logo" accept="image/png,image/jpeg" required aria-describedby="logo-help" />
          </label>
          <p className="note" id="logo-help">
            PNG or JPEG, up to 512 by 512 pixels and 256 KiB. Included in new PDF reports.
          </p>
          <button type="submit" className="button">
            {busy ? "Saving logo…" : "Upload logo"}
          </button>
          {hasLogo && (
            <button type="button" className="secondary" onClick={() => save("")}>
              Remove logo
            </button>
          )}
        </fieldset>
      </form>
      {!announce && <Announcer status={ownStatus} alert={ownError} />}
    </div>
  );
}
