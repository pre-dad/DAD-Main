import type { Metadata } from "next";
import Signup from "./Signup";

export const metadata: Metadata = { title: "Create your donor account" };

export default function SignupPage() {
  return <Signup />;
}
