import type { MoneyHistory, MoneyPeriod } from "./types";

const MONEY_RANGES = ["24h", "7d", "30d", "all", "custom"] as const;
export type MoneyRange = (typeof MONEY_RANGES)[number];
const MONEY_SERIES = [
  "liquid_funds_pence",
  "net_worth_pence",
  "settlement_net",
  "settlement_gross",
  "settlement_costs",
  "cumulative_net",
  "cash_outflow_pence",
  "expenses_pence",
] as const;
export type MoneySeries = (typeof MONEY_SERIES)[number];

export interface MoneyPreferences {
  series: MoneySeries;
  range: MoneyRange;
  from: string;
  to: string;
}

export const DEFAULT_MONEY_PREFERENCES: MoneyPreferences = {
  series: "liquid_funds_pence",
  range: "24h",
  from: "",
  to: "",
};

export function restoreMoneyPreferences(value: unknown): MoneyPreferences {
  if (value === null || typeof value !== "object") {
    return { ...DEFAULT_MONEY_PREFERENCES };
  }
  const preferences = value as Record<string, unknown>;
  const series =
    MONEY_SERIES.find((item) => item === preferences.series) ??
    DEFAULT_MONEY_PREFERENCES.series;
  let range =
    MONEY_RANGES.find((item) => item === preferences.range) ??
    DEFAULT_MONEY_PREFERENCES.range;
  const from = typeof preferences.from === "string" ? preferences.from : "";
  const to = typeof preferences.to === "string" ? preferences.to : "";
  if (range === "custom" && !customMoneyRange(from, to)) {
    range = DEFAULT_MONEY_PREFERENCES.range;
  }
  return { series, range, from, to };
}

export function customMoneyRange(from: string, to: string) {
  // Native datetime controls have no timezone. The UI explicitly labels UTC.
  const start = new Date(`${from}Z`);
  const end = new Date(`${to}Z`);
  if (
    !Number.isFinite(start.getTime()) ||
    !Number.isFinite(end.getTime()) ||
    start >= end
  ) {
    return undefined;
  }
  return { from: start.toISOString(), to: end.toISOString() };
}

export interface MoneyPoint {
  id: string;
  at: string;
  value: number;
  period?: MoneyPeriod;
}

export function moneyPoints(
  history: MoneyHistory | undefined,
  series: MoneySeries,
): MoneyPoint[] {
  if (!history) return [];
  if (!series.startsWith("settlement_") && series !== "cumulative_net") {
    return history.metrics
      .filter((item) => item.name === series && Number.isFinite(item.value))
      .map((item) => ({
        id: `${series}:${Math.floor(Date.parse(item.recorded_at) / (history.bucket_seconds * 1000))}`,
        at: item.recorded_at,
        value: item.value,
      }));
  }
  let cumulative = 0;
  return history.settlements.flatMap((period) => {
    let value: number | null;
    switch (series) {
      case "settlement_gross":
        value = period.gross_pence;
        break;
      case "settlement_costs":
        value = period.costs_pence === null ? null : 0 - period.costs_pence;
        break;
      case "cumulative_net":
        if (period.net_pence === null) return [];
        cumulative += period.net_pence;
        value = cumulative;
        break;
      default:
        value = period.net_pence;
    }
    return value === null
      ? []
      : [{ id: period.at, at: period.at, value, period }];
  });
}
export function moneyAxisTicks(values: readonly number[]): number[] {
  const minimum = Math.min(0, ...values);
  const maximum = Math.max(0, ...values);
  // Aim for four intervals, with 1/2/5 steps and at least one whole Crown.
  const targetStep = Math.max(100, (maximum - minimum) / 4);
  const magnitude = 10 ** Math.floor(Math.log10(targetStep));
  const fraction = targetStep / magnitude;
  const step =
    magnitude *
    (fraction <= 1 ? 1 : fraction <= 2 ? 2 : fraction <= 5 ? 5 : 10);
  const lower = Math.floor(minimum / step) * step;
  const upper = Math.max(lower + step, Math.ceil(maximum / step) * step);
  return Array.from(
    { length: Math.round((upper - lower) / step) + 1 },
    (_, index) => lower + index * step,
  );
}

export function settlementTotals(periods: MoneyPeriod[]) {
  let gross = 0;
  let costs = 0;
  let net = 0;
  let known = 0;
  let count = 0;
  for (const period of periods) {
    gross += period.gross_pence;
    costs += period.costs_pence ?? 0;
    net += period.net_pence ?? 0;
    known += period.known_count;
    count += period.count;
  }
  return {
    gross: count ? gross : undefined,
    costs: known ? costs : undefined,
    net: known ? net : undefined,
    known,
    unknown: count - known,
    count,
  };
}
