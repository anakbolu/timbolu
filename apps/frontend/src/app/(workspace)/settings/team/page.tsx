import type { Metadata } from "next";
import { TeamView } from "@/components/team/team-view";

export const metadata: Metadata = {
  title: "Anggota tim",
  description: "Atur Bolu-mu: persona, gaya bahasa, alat, dan jam istirahat.",
};

export default function TeamPage() {
  return <TeamView />;
}
