import type { Metadata } from "next";
import StaffInsights from "./Insights";

export const metadata: Metadata = { title: "Dashboard insights" };

export default function Page() {
  return <StaffInsights />;
}
