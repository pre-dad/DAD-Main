import type { Metadata } from "next";
import Link from "next/link";
import { notFound } from "next/navigation";
import { Page } from "@/components/donor/Page";
import { ContentUnavailable, InitiativeFacts } from "@/components/donor/PublicContent";
import { launchDateLabel } from "@/lib/launch";
import { loadInitiative } from "@/lib/publicContent";

export async function generateMetadata({ params }: PageProps<"/initiatives/[slug]">): Promise<Metadata> {
  const found = await loadInitiative((await params).slug);
  return { title: found?.title ?? "Planned initiative", description: found?.summary };
}

export default async function InitiativePage({ params }: PageProps<"/initiatives/[slug]">) {
  const found = await loadInitiative((await params).slug);
  if (found === undefined) notFound();
  return (
    <Page
      eyebrow="Planned initiative"
      back={{ href: "/initiatives", label: "All initiatives" }}
      title={found?.title ?? "Planned initiative"}
    >
      {found ? (
        <>
          <InitiativeFacts initiative={found} />
          <Link className="button" href="/donate">
            Support DAD’s work
          </Link>
          <p className="note">
            Gifts support DAD as a whole and are not designated to this initiative. DAD launches {launchDateLabel()}.
          </p>
        </>
      ) : (
        <ContentUnavailable />
      )}
    </Page>
  );
}
