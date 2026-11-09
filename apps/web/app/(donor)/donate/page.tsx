import type { Metadata } from "next";
import { launchDateLabel } from "@/lib/launch";
import VerifiedDonation from "./VerifiedDonation";

export const metadata: Metadata = {
  title: "Give",
  description: `Give about a dollar a day — $30/month. DAD launches on ${launchDateLabel()}.`,
};

export default function DonatePage() {
  return <VerifiedDonation />;
}
