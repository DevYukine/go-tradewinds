import type { Metadata } from "next";
import { FleetChartPage } from "@/components/fleet-map";

export const metadata: Metadata = {
  title: "Tradewinds | Fleet chart",
  description: "Recorded vessel positions, scheduled voyages and fleet plans.",
};

export default function Page() {
  return <FleetChartPage />;
}
