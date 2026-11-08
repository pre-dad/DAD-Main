"use client";

import { useRef, useState } from "react";

/** Mirrors the API's rules for `volunteer_interests` (openapi.json): at most 20, 1-60 characters each, no repeats. */
export const maxInterests = 20;
export const maxInterestLength = 60;

const sameInterest = (a: string, b: string) => a.toLocaleLowerCase() === b.toLocaleLowerCase();

export function VolunteerInterests({
  interests,
  onChange,
  disabled,
}: {
  interests: string[];
  onChange: (interests: string[]) => void;
  disabled?: boolean;
}) {
  const [draft, setDraft] = useState("");
  const [problem, setProblem] = useState("");
  const [announcement, setAnnouncement] = useState("");
  const input = useRef<HTMLInputElement>(null);
  const full = interests.length >= maxInterests;

  const add = () => {
    const interest = draft.trim();
    if (!interest) {
      setProblem("Type an interest to add.");
    } else if (Array.from(interest).length > maxInterestLength) {
      setProblem(`Keep each interest to ${maxInterestLength} characters or fewer.`);
    } else if (interests.some((saved) => sameInterest(saved, interest))) {
      setProblem(`“${interest}” is already on your list.`);
    } else if (full) {
      setProblem(`You can list up to ${maxInterests} interests. Remove one to add another.`);
    } else {
      onChange([...interests, interest]);
      setDraft("");
      setProblem("");
      setAnnouncement(`Added ${interest}. ${interests.length + 1} of ${maxInterests} interests.`);
      input.current?.focus();
      return;
    }
    setAnnouncement("");
  };

  const remove = (interest: string) => {
    onChange(interests.filter((saved) => saved !== interest));
    setProblem("");
    setAnnouncement(`Removed ${interest}. ${interests.length - 1} of ${maxInterests} interests.`);
    input.current?.focus();
  };

  return (
    <fieldset className="interests" disabled={disabled}>
      <legend>What interests you?</legend>
      <p className="note" id="interest-help">
        Add the kinds of volunteering you would enjoy, such as “Mentoring” or “Community clean-ups”. Up to{" "}
        {maxInterests} interests, {maxInterestLength} characters each.
      </p>
      {interests.length === 0 ? (
        <p className="note">No interests added yet.</p>
      ) : (
        <ul className="interestList" aria-label="Your interests">
          {interests.map((interest) => (
            <li key={interest}>
              <span>{interest}</span>
              <button
                type="button"
                className="quiet interestRemove"
                aria-label={`Remove ${interest}`}
                disabled={disabled}
                onClick={() => remove(interest)}
              >
                Remove
              </button>
            </li>
          ))}
        </ul>
      )}
      <div className="interestAdd">
        <label className="interestField">
          Add an interest
          <input
            ref={input}
            value={draft}
            autoComplete="off"
            aria-describedby="interest-help interest-problem"
            aria-invalid={problem ? true : undefined}
            disabled={disabled}
            onChange={(event) => {
              setDraft(event.target.value);
              setProblem("");
            }}
            onKeyDown={(event) => {
              if (event.key !== "Enter") return;
              event.preventDefault();
              add();
            }}
          />
        </label>
        <button type="button" className="secondary" disabled={disabled} onClick={add}>
          Add interest
        </button>
      </div>
      <p id="interest-problem" className="errorMessage" role="alert">
        {problem}
      </p>
      <p role="status" className="visuallyHidden">
        {announcement}
      </p>
    </fieldset>
  );
}
