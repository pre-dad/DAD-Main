"use client";
import Image from "next/image";
import { useState } from "react";
import { Announcer } from "@/components/donor/Announcer";
import { api, failureMessage, type Schema } from "@/lib/api";
import { resourceMessages, useApiResource } from "@/lib/useApiResource";
export function Rules() {
  const { resource, data, reload } = useApiResource<Schema<"RecognitionRules">>("/donor/staff/content/rules");
  const [busy, setBusy] = useState(false),
    [status, setStatus] = useState(""),
    [error, setError] = useState("");
  const messages = resourceMessages(resource, {
    loading: "Loading earning rules…",
    failure: "Earning rules could not be loaded.",
  });
  return (
    <section>
      <h2>Earning rules and artwork</h2>
      <p>
        Changing thresholds affects future awards. Already earned badges stay earned. Annual streak milestones continue
        every 365 days after the final threshold.
      </p>
      {data && (
        <form
          key={JSON.stringify(data)}
          onSubmit={async (e) => {
            e.preventDefault();
            if (busy) return;
            const form = e.currentTarget,
              f = new FormData(form);
            setBusy(true);
            setError("");
            try {
              const badges = await Promise.all(
                data.badges.map(async (b) => {
                  const file = f.get(`art:${b.code}`);
                  let artwork = b.artwork;
                  if (file instanceof File && file.size) {
                    if (file.size > 256 * 1024) throw new Error("Choose an image under 256 KB.");
                    artwork = await new Promise<string>((resolve, reject) => {
                      const reader = new FileReader();
                      reader.onload = () => resolve(String(reader.result));
                      reader.onerror = () => reject(new Error("Image could not be read."));
                      reader.readAsDataURL(file);
                    });
                  }
                  if (f.get(`clear:${b.code}`)) artwork = "";
                  const population = f.get(`population:${b.code}`);
                  return {
                    ...b,
                    threshold: Number(f.get(`threshold:${b.code}`)),
                    min_population: population === null ? b.min_population : Number(population),
                    artwork,
                  };
                }),
              );
              await api("/donor/staff/content/rules", {
                method: "POST",
                body: JSON.stringify({
                  badges,
                  milestones: String(f.get("milestones"))
                    .split(",")
                    .map((v) => Number(v.trim())),
                }),
              });
              setStatus("Rules and artwork saved.");
              reload();
            } catch (c) {
              setError(failureMessage(c, "Rules could not be saved. Check the thresholds and images."));
            } finally {
              setBusy(false);
            }
          }}
        >
          <fieldset disabled={busy}>
            <legend>Milestone thresholds</legend>
            <label>
              Streak days, increasing and separated by commas
              <input name="milestones" defaultValue={data.milestones.join(", ")} required />
            </label>
          </fieldset>
          {data.badges.map((b) => (
            <fieldset key={b.code} disabled={busy}>
              <legend>
                {b.code} · {b.signal.replaceAll("_", " ")}
              </legend>
              <label>
                Threshold
                <input
                  name={`threshold:${b.code}`}
                  type="number"
                  min={1}
                  max={b.signal === "national_percentile" ? 100 : 1000000}
                  defaultValue={b.threshold}
                  readOnly={
                    ![
                      "streak_days",
                      "referrals",
                      "checkins",
                      "initiatives",
                      "city_rank",
                      "national_percentile",
                    ].includes(b.signal)
                  }
                  required
                />
              </label>
              {(b.signal === "city_rank" || b.signal === "national_percentile") && (
                <label>
                  Fewest ranked donors before this badge is awarded
                  <input
                    name={`population:${b.code}`}
                    type="number"
                    min={1}
                    max={1000000}
                    defaultValue={b.min_population}
                    required
                  />
                </label>
              )}
              {b.artwork && <Image unoptimized src={b.artwork} width={64} height={64} alt={`${b.code} artwork`} />}
              <label>
                Artwork (PNG or JPEG, up to 512 × 512 pixels)
                <input name={`art:${b.code}`} type="file" accept="image/png,image/jpeg" />
              </label>
              <label>
                <input type="checkbox" name={`clear:${b.code}`} />
                Remove artwork
              </label>
            </fieldset>
          ))}
          <button className="button" type="submit" disabled={busy}>
            Save earning rules and artwork
          </button>
        </form>
      )}
      <Announcer status={status || messages.status} alert={error || messages.alert} />
    </section>
  );
}
