import { number, record, text } from "./data";
import type { Economics } from "./types";

type EconomicRow = {
  key: string;
  label: string;
  pence: number;
};

export type DecisionView = {
  origin: string;
  destination: string;
  commodity: string;
  basis: "destination" | "legacy";
  basisLabel: string;
  netLabel: string;
  durationLabel: string;
  economicsNotice: string;
  expectedNetPence: number | undefined;
  cashRows: EconomicRow[];
  stressRows: EconomicRow[];
  gameDays: number | undefined;
  dailyPence: number | undefined;
  dailyLabel: string;
  ballastRepeatGameDays: number | undefined;
  ballastRepeatNetPence: number | undefined;
  haircutBps: number | undefined;
  haircutLabel: string;
  stressNotice: string;
};

export function buildDecisionView(details: unknown): DecisionView {
  const plan = record(details);
  const economics = record(plan.economics);
  const basis = economics.basis === "destination" ? "destination" : "legacy";
  const netLabel =
    basis === "destination"
      ? "Modeled delivery contribution"
      : "Modeled expected cash net";
  const expectedNetPence = number(economics.expected_net_pence);
  const cashRows: EconomicRow[] = [];
  const cashFields: [keyof Economics, string][] = [
    ["revenue_pence", "Expected revenue"],
    ["purchase_pence", "Purchase cost"],
    ["customs_pence", "Customs"],
    [
      "provisions_pence",
      basis === "destination" ? "Consumed provisions" : "Provisions",
    ],
    ["berth_dues_pence", "Berth dues"],
    ["upkeep_pence", "Incremental upkeep"],
    ["toll_pence", "Tolls"],
    ["funding_fees_pence", "Regional funding fees"],
    ["expected_net_pence", netLabel],
    ["collateral_pence", "Collateral locked, not an expense"],
  ];
  if (basis === "destination") {
    cashFields.push([
      "store_cash_pence",
      "Initial stores cash, not consumption",
    ]);
  }
  for (const [key, label] of cashFields) {
    const pence = number(economics[key]);
    if (pence !== undefined) {
      cashRows.push({ key, label, pence });
    }
  }
  const stressRows: EconomicRow[] = [];
  for (const [key, label] of [
    ["stress_haircut_pence", "Scenario haircut, not a cost"],
    ["price_uncertainty_stress_pence", "Price uncertainty scenario"],
    ["stress_net_pence", "Stress scenario net"],
  ]) {
    const pence = number(economics[key]);
    if (pence !== undefined) {
      stressRows.push({ key, label, pence });
    }
  }

  const repeatDays = number(economics.ballast_repeat_game_days);
  const repeatNet = number(economics.ballast_repeat_net_pence);
  const hasRepeat =
    basis === "destination" &&
    repeatDays !== undefined &&
    repeatDays > 0 &&
    repeatNet !== undefined;
  return {
    origin: text(plan.origin, ""),
    destination: text(plan.destination, ""),
    commodity: text(plan.commodity, ""),
    basis,
    basisLabel:
      basis === "destination"
        ? "Destination delivery"
        : "Legacy recorded model",
    netLabel,
    durationLabel:
      basis === "destination" ? "Delivery duration" : "Modeled voyage duration",
    economicsNotice:
      basis === "destination"
        ? "Ends at delivery. Initial stores and collateral are funded separately; fixed fleet bills remain reserved."
        : "Recorded before destination-based valuation; original cost and duration semantics are retained.",
    expectedNetPence,
    cashRows,
    stressRows,
    gameDays: number(economics.game_days),
    dailyPence: number(economics.net_pence_per_game_day),
    dailyLabel:
      expectedNetPence === undefined
        ? "Recorded daily diagnostic; cash net unrecorded"
        : basis === "destination"
          ? "Delivery contribution per game day"
          : "Modeled cash per game day",
    ballastRepeatGameDays: hasRepeat ? repeatDays : undefined,
    ballastRepeatNetPence: hasRepeat ? repeatNet : undefined,
    haircutBps: number(economics.stress_haircut_bps),
    haircutLabel: "Scenario haircut rate, not probability",
    stressNotice:
      "Scenarios are not expected costs, loss probabilities, actual losses or acceptance penalties.",
  };
}
