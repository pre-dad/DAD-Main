import Link from "next/link";
import { Page } from "@/components/donor/Page";

export function RouteError({
  error,
  retry,
  lead,
  home,
  quoteTo = " to DAD",
}: {
  error: Error & { digest?: string };
  retry: () => void;
  lead: string;
  home: { href: string; label: string };
  quoteTo?: string;
}) {
  return (
    <Page eyebrow="Something went wrong" title="This page didn’t load">
      <p className="lead">{lead}</p>
      {error.digest && (
        <p className="note">
          If this keeps happening, quote reference <span className="hash">{error.digest}</span>
          {quoteTo}.
        </p>
      )}
      <div className="actions">
        <button type="button" className="button" onClick={retry}>
          Try again
        </button>
        <Link className="secondary" href={home.href}>
          {home.label}
        </Link>
      </div>
    </Page>
  );
}
