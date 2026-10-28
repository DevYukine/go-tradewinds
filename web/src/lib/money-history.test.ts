import { afterEach, describe, expect, it, vi } from "vitest";
import { money } from "./format";
import {
  customMoneyRange,
  moneyAxisTicks,
  moneyPoints,
  restoreMoneyPreferences,
  settlementTotals,
} from "./money-history";
import type { MoneyHistory, MoneyPeriod } from "./types";

const periods: MoneyPeriod[] = [
  {
    at: "2026-10-24T10:00:00Z",
    to: "2026-10-24T11:00:00Z",
    count: 2,
    known_count: 2,
    gross_pence: 12000,
    costs_pence: 9000,
    net_pence: 3000,
  },
  {
    at: "2026-10-24T11:00:00Z",
    to: "2026-10-24T12:00:00Z",
    count: 1,
    known_count: 1,
    gross_pence: 4000,
    costs_pence: 9000,
    net_pence: -5000,
  },
  {
    at: "2026-10-24T12:00:00Z",
    to: "2026-10-24T13:00:00Z",
    count: 1,
    known_count: 0,
    gross_pence: 5000,
    costs_pence: null,
    net_pence: null,
  },
];
const history: MoneyHistory = {
  account_id: "company",
  generated_at: "2026-10-24T13:00:00Z",
  from: "2026-10-24T00:00:00Z",
  to: "2026-10-25T00:00:00Z",
  bucket_seconds: 3600,
  metrics: [
    {
      id: 1,
      account_id: "company",
      name: "liquid_funds_pence",
      value: 12345,
      recorded_at: "2026-10-24T10:15:00Z",
    },
    {
      id: 2,
      account_id: "company",
      name: "expenses_pence",
      value: 500,
      recorded_at: "2026-10-24T10:16:00Z",
    },
    {
      id: 3,
      account_id: "company",
      name: "expenses_pence",
      value: 500,
      recorded_at: "2026-10-24T11:16:00Z",
    },
  ],
  settlements: periods,
};

describe("recorded money history", () => {
  it("keeps gross, unknown costs, losses and cumulative selected-range net distinct", () => {
    expect(
      moneyPoints(history, "settlement_net").map((point) => point.value),
    ).toEqual([3000, -5000]);
    expect(
      moneyPoints(history, "settlement_gross").map((point) => point.value),
    ).toEqual([12000, 4000, 5000]);
    expect(
      moneyPoints(history, "settlement_costs").map((point) => point.value),
    ).toEqual([-9000, -9000]);
    expect(
      moneyPoints(history, "cumulative_net").map((point) => point.value),
    ).toEqual([3000, -2000]);
    expect(settlementTotals(periods)).toEqual({
      gross: 21000,
      costs: 18000,
      net: -2000,
      known: 3,
      unknown: 1,
      count: 4,
    });
  });

  it("keeps aggregate proceeds unknown and omits unknown gross chart periods", () => {
    const unknown: MoneyPeriod = {
      ...periods[2],
      gross_pence: null,
      unknown_proceeds_count: 1,
    };
    const mixed = [...periods.slice(0, 2), unknown];
    expect(settlementTotals(mixed)).toEqual({
      gross: undefined,
      costs: 18000,
      net: -2000,
      known: 3,
      unknown: 1,
      count: 4,
    });
    expect(settlementTotals([unknown, periods[0]]).gross).toBeUndefined();
    expect(settlementTotals([unknown]).net).toBeUndefined();
    expect(
      moneyPoints({ ...history, settlements: mixed }, "settlement_gross").map(
        (point) => point.value,
      ),
    ).toEqual([12000, 4000]);
    expect(
      moneyPoints(
        { ...history, settlements: [{ ...periods[0], gross_pence: 0 }] },
        "settlement_gross",
      ).map((point) => point.value),
    ).toEqual([0]);
  });

  it("starts cumulative net inside the selected range and never fills missing periods", () => {
    const selected = { ...history, settlements: periods.slice(1) };
    expect(
      moneyPoints(selected, "cumulative_net").map((point) => point.value),
    ).toEqual([-5000]);
    expect(moneyPoints(undefined, "settlement_net")).toEqual([]);
    expect(moneyPoints(history, "net_worth_pence")).toEqual([]);
    expect(settlementTotals([]).net).toBeUndefined();
    expect(settlementTotals(periods.slice(2)).costs).toBeUndefined();
    expect(settlementTotals([{ ...periods[0], net_pence: 0 }]).net).toBe(0);
  });

  it("renders zero-cost periods without a negative-zero amount", () => {
    const point = moneyPoints(
      {
        ...history,
        settlements: [{ ...periods[0], costs_pence: 0 }],
      },
      "settlement_costs",
    )[0];
    expect(point.value).toBe(0);
    expect(money(point.value)).toBe("0.00 Cr");
  });

  it("retains ledger snapshots rather than summing repeated charges", () => {
    expect(
      moneyPoints(history, "expenses_pence").map((point) => point.value),
    ).toEqual([500, 500]);
    const original = moneyPoints(history, "liquid_funds_pence")[0];
    const refreshed = moneyPoints(
      {
        ...history,
        metrics: [
          {
            ...history.metrics[0],
            id: 99,
            recorded_at: "2026-10-24T10:59:00Z",
          },
        ],
      },
      "liquid_funds_pence",
    )[0];
    expect(refreshed.id).toBe(original.id);
  });

  it("uses explicit UTC date-time bounds and rejects missing or reversed ranges", () => {
    expect(customMoneyRange("2026-10-24T10:15", "2026-10-25T11:45")).toEqual({
      from: "2026-10-24T10:15:00.000Z",
      to: "2026-10-25T11:45:00.000Z",
    });
    expect(customMoneyRange("", "2026-10-25T11:45")).toBeUndefined();
    expect(
      customMoneyRange("2026-10-25T11:45", "2026-10-24T10:15"),
    ).toBeUndefined();
    expect(
      customMoneyRange("2026-10-24T10:15", "2026-10-24T10:15"),
    ).toBeUndefined();
  });
});

describe("persisted money preferences", () => {
  it("validates enum selections and rejects malformed custom ranges", () => {
    expect(restoreMoneyPreferences(null)).toEqual({
      series: "liquid_funds_pence",
      range: "24h",
      from: "",
      to: "",
    });
    expect(
      restoreMoneyPreferences({
        series: "deleted-series",
        range: "deleted-range",
      }),
    ).toEqual({
      series: "liquid_funds_pence",
      range: "24h",
      from: "",
      to: "",
    });
    expect(
      restoreMoneyPreferences({
        series: "cumulative_net",
        range: "custom",
        from: "2026-10-25T11:45",
        to: "2026-10-24T10:15",
      }),
    ).toEqual({
      series: "cumulative_net",
      range: "24h",
      from: "2026-10-25T11:45",
      to: "2026-10-24T10:15",
    });
  });
});

describe("money chart axis ticks", () => {
  it.each([
    {
      name: "negative-only",
      values: [-599241, -155705],
      expected: [-600000, -400000, -200000, 0],
    },
    {
      name: "positive-only",
      values: [221768, 599241],
      expected: [0, 200000, 400000, 600000],
    },
    {
      name: "mixed",
      values: [-155705, 221768, 599241],
      expected: [-200000, 0, 200000, 400000, 600000],
    },
    {
      name: "flat zero",
      values: [0, 0],
      expected: [0, 100],
    },
    {
      name: "sub-Crown",
      values: [-1, 1],
      expected: [-100, 0, 100],
    },
  ])(
    "covers $name data with round whole-Crown ticks including zero",
    ({ values, expected }) => {
      expect(moneyAxisTicks(values)).toEqual(expected);
    },
  );
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.resetModules();
});

it("persists only UI preferences and restores them after hydration", async () => {
  const storage = new Map<string, string>();
  vi.stubGlobal("window", {
    localStorage: {
      getItem: (key: string) => storage.get(key) ?? null,
      setItem: (key: string, value: string) => storage.set(key, value),
      removeItem: (key: string) => storage.delete(key),
    },
  });
  // Load after storage is installed, then reload the module to exercise a fresh
  // Zustand hydration boundary rather than reusing the in-memory preferences.
  const { useUI } = await import("./ui-store");
  const preferences = {
    series: "settlement_net" as const,
    range: "custom" as const,
    from: "2026-10-24T10:15",
    to: "2026-10-25T11:45",
  };
  useUI.getState().setMoneyHistory(preferences);
  useUI.getState().setAccount("another-company");
  expect(useUI.getState().moneyHistory).toEqual(preferences);
  const saved = storage.get("tradewinds-operations")!;
  expect(JSON.parse(saved).state.moneyHistory).toEqual(preferences);
  expect(JSON.parse(saved).state).not.toHaveProperty("metrics");
  vi.resetModules();
  const { useUI: restored } = await import("./ui-store");
  await restored.persist.rehydrate();
  expect(restored.getState().moneyHistory).toEqual(preferences);
  storage.set(
    "tradewinds-operations",
    JSON.stringify({
      state: { moneyHistory: { series: "removed", range: "removed" } },
      version: 1,
    }),
  );
  await restored.persist.rehydrate();
  expect(restored.getState().moneyHistory).toEqual({
    series: "liquid_funds_pence",
    range: "24h",
    from: "",
    to: "",
  });
});
