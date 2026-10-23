import { number, record, text } from "./data";

type EconomicRow = {
  key: string;
  label: string;
  pence: number;
};

export type DecisionView = {
  origin: string;
  destination: string;
  commodity: string;
  expectedNetPence: number | undefined;
  cashRows: EconomicRow[];
  stressRows: EconomicRow[];
  gameDays: number | undefined;
  dailyPence: number | undefined;
  dailyLabel: string;
  haircutBps: number | undefined;
  haircutLabel: string;
  stressNotice: string;
};

export function buildDecisionView(details: unknown): DecisionView {
  const plan = record(details);
  const economics = record(plan.economics);
  const expectedNetPence = number(economics.expected_net_pence);
  const cashRows: EconomicRow[] = [];
  for (const [key, label] of [
    ["revenue_pence", "Expected revenue"],
    ["purchase_pence", "Purchase cost"],
    ["customs_pence", "Customs"],
    ["provisions_pence", "Provisions"],
    ["berth_dues_pence", "Berth dues"],
    ["upkeep_pence", "Incremental upkeep"],
    ["toll_pence", "Tolls"],
    ["expected_net_pence", "Modeled expected cash net"],
    ["collateral_pence", "Collateral locked, not an expense"],
  ]) {
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

  return {
    origin: text(plan.origin, ""),
    destination: text(plan.destination, ""),
    commodity: text(plan.commodity, ""),
    expectedNetPence,
    cashRows,
    stressRows,
    gameDays: number(economics.game_days),
    dailyPence: number(economics.net_pence_per_game_day),
    dailyLabel:
      expectedNetPence === undefined
        ? "Recorded daily diagnostic; cash net unrecorded"
        : "Modeled cash per game day",
    haircutBps: number(economics.stress_haircut_bps),
    haircutLabel: "Scenario haircut rate, not probability",
    stressNotice:
      "Scenarios are not expected costs, loss probabilities, actual losses or acceptance penalties.",
  };
}
