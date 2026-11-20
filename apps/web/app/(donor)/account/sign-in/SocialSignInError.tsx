"use client";

import { useEffect, useState } from "react";

const messages: Record<string, string> = {
  "1": "That sign-in didn’t complete. Please try again, or use an email link instead.",
  unavailable: "That sign-in method isn’t available right now. Please use an email link instead.",
};

/** Explains why a social sign-in sent the donor back here. */
export function SocialSignInError() {
  const [message, setMessage] = useState("");
  useEffect(() => {
    setMessage(messages[new URLSearchParams(window.location.search).get("social_error") ?? ""] ?? "");
  }, []);
  return message ? <p role="alert">{message}</p> : null;
}
