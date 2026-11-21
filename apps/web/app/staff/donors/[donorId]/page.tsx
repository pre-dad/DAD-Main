import type { Metadata } from "next";
import DonorDetail from "./DonorDetail";

export const metadata: Metadata = { title: "Donor record" };

export default async function Page({ params }: PageProps<"/staff/donors/[donorId]">) {
  const { donorId } = await params;
  return <DonorDetail key={donorId} donor={donorId} />;
}
