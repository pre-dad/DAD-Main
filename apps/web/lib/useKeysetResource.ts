"use client";

import { useEffect, useRef, useState } from "react";
import { useApiResource } from "./useApiResource";

/**
 * Loads a keyset-paged list. `path` builds the request for a cursor, undefined
 * on the first page. After a page turn, focus moves to `heading`: the button
 * just pressed may not exist on the new page, and the reader belongs at the top
 * of the list that replaced the one they were reading.
 */
export function useKeysetResource<T extends { next_cursor: string | null }>(
  path: (cursor: string | undefined) => string,
) {
  const [cursors, setCursors] = useState<string[]>([]);
  const result = useApiResource<T>(path(cursors.at(-1)));
  const { data } = result;
  const heading = useRef<HTMLHeadingElement>(null);
  const turned = useRef<{ from: T | null } | null>(null);
  useEffect(() => {
    // A response to the request before the turn must not consume it.
    if (data && turned.current && data !== turned.current.from) {
      turned.current = null;
      heading.current?.focus();
    }
  }, [data]);
  const turn = (next: string[]) => {
    turned.current = { from: data };
    setCursors(next);
  };
  const nextCursor = data?.next_cursor ?? null;
  return {
    ...result,
    heading,
    newer: cursors.length > 0 ? () => turn(cursors.slice(0, -1)) : undefined,
    older: nextCursor !== null ? () => turn([...cursors, nextCursor]) : undefined,
  };
}
