"use client";

import { useEffect, useRef } from "react";

/**
 * After an offset page turn the button just pressed may be gone, so focus moves to
 * `heading` once the new page's data replaces the old. Call `turned` as the page is turned.
 */
export function usePageTurnFocus<T>(data: T | null) {
  const heading = useRef<HTMLHeadingElement>(null);
  const pending = useRef<{ from: T | null } | null>(null);
  useEffect(() => {
    if (data && pending.current && data !== pending.current.from) {
      pending.current = null;
      heading.current?.focus();
    }
  }, [data]);
  return {
    heading,
    turned: () => {
      pending.current = { from: data };
    },
  };
}
