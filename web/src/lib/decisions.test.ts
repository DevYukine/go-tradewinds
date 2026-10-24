import {
  createSortedRowModel,
  rowSortingFeature,
  sortFn_basic,
  tableFeatures,
  useTable,
} from "@tanstack/react-table";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { buildDecisionView, type DecisionView } from "./decisions";
import { humanize, money, percent } from "./format";

const plan = JSON.parse(`{
  "kind": "trade",
  "ship_id": "ship-lapras",
  "origin": "port_royal",
  "destination": "new_haven",
  "commodity": "tea",
  "freight_ids": [],
  "quantity": 10,
  "buy_limit_pence": 202,
  "sell_limit_pence": 1200,
  "stores_to_buy": 1,
  "stores_limit_pence": 404,
  "target_stores_tons": 2.5,
  "economics": {
    "revenue_pence": 12001,
    "purchase_pence": 2020,
    "customs_pence": 303,
    "provisions_pence": 404,
    "berth_dues_pence": 505,
    "upkeep_pence": 606,
    "toll_pence": 707,
    "collateral_pence": 808,
    "expected_net_pence": 7456,
    "stress_haircut_bps": 1250,
    "stress_haircut_pence": 909,
    "price_uncertainty_stress_pence": 1010,
    "stress_net_pence": 5537,
    "game_days": 2.5,
    "net_pence_per_game_day": 2982.4,
    "probability_estimate": null
  },
  "accepted": true,
  "reason": "Observed cash opportunity",
  "price_source": "order_book",
  "expected_arrival": "2026-10-22T12:00:00Z"
}`) as { economics: Record<string, unknown>; [key: string]: unknown };

const features = tableFeatures({
  rowSortingFeature,
  sortedRowModel: createSortedRowModel(),
  sortFns: { basic: sortFn_basic },
});

function SortedCash({
  data,
  descending,
}: {
  data: DecisionView[];
  descending: boolean;
}) {
  const table = useTable({
    features,
    data,
    columns: [
      {
        id: "cash",
        accessorFn: (view) => view.expectedNetPence,
        sortFn: "basic",
        sortUndefined: "last",
      },
    ],
    state: { sorting: [{ id: "cash", desc: descending }] },
  });
  return createElement(
    "ol",
    null,
    table
      .getSortedRowModel()
      .rows.map((row) =>
        createElement(
          "li",
          { key: row.id },
          String(row.getValue("cash") ?? "Not recorded"),
        ),
      ),
  );
}

describe("recorded decision cash", () => {
  it("reads every nested cash field and route from a serialized Go Plan", () => {
    const view = buildDecisionView({ ...plan, expected_net_pence: 999999 });
    expect([view.origin, view.destination, view.commodity]).toEqual([
      "port_royal",
      "new_haven",
      "tea",
    ]);
    expect(humanize(view.origin)).toBe("Port royal");
    expect(humanize(view.destination)).toBe("New haven");
    expect(humanize(view.commodity)).toBe("Tea");
    expect(view.expectedNetPence).toBe(7456);
    expect(view.cashRows).toEqual([
      { key: "revenue_pence", label: "Expected revenue", pence: 12001 },
      { key: "purchase_pence", label: "Purchase cost", pence: 2020 },
      { key: "customs_pence", label: "Customs", pence: 303 },
      { key: "provisions_pence", label: "Provisions", pence: 404 },
      { key: "berth_dues_pence", label: "Berth dues", pence: 505 },
      { key: "upkeep_pence", label: "Incremental upkeep", pence: 606 },
      { key: "toll_pence", label: "Tolls", pence: 707 },
      {
        key: "expected_net_pence",
        label: "Modeled expected cash net",
        pence: 7456,
      },
      {
        key: "collateral_pence",
        label: "Collateral locked, not an expense",
        pence: 808,
      },
    ]);
    expect(view.cashRows.map((row) => money(row.pence))).toEqual([
      "120.01 Cr",
      "20.20 Cr",
      "3.03 Cr",
      "4.04 Cr",
      "5.05 Cr",
      "6.06 Cr",
      "7.07 Cr",
      "74.56 Cr",
      "8.08 Cr",
    ]);
    expect(view.gameDays).toBe(2.5);
    expect(view.dailyPence).toBe(2982.4);
    expect(money(view.dailyPence)).toBe("29.82 Cr");
    expect(view.dailyLabel).toBe("Modeled cash per game day");
  });

  it.each([
    [0, "0.00 Cr"],
    [-100, "-1.00 Cr"],
  ])("preserves a cash net of %s", (net, formatted) => {
    const view = buildDecisionView({ economics: { expected_net_pence: net } });
    expect(view.expectedNetPence).toBe(net);
    expect(view.cashRows[0].pence).toBe(net);
    expect(money(view.expectedNetPence)).toBe(formatted);
  });

  it.each([
    undefined,
    null,
    false,
    42,
    "not a plan",
    [],
    {},
    { economics: null },
    { economics: "unavailable" },
    { economics: [] },
    { ...plan.economics },
  ])("keeps unavailable details unavailable: %j", (details) => {
    const view = buildDecisionView(details);
    expect(view.expectedNetPence).toBeUndefined();
    expect(view.cashRows).toEqual([]);
    expect(view.stressRows).toEqual([]);
    expect(view.gameDays).toBeUndefined();
    expect(view.dailyPence).toBeUndefined();
    expect(view.haircutBps).toBeUndefined();
    expect(money(view.expectedNetPence)).toBe("Not recorded");
  });

  it.each(["123", "", null, undefined, NaN, Infinity, -Infinity])(
    "rejects nonnumeric or non-finite economics: %s",
    (invalid) => {
      const economics = Object.fromEntries(
        Object.keys(plan.economics).map((key) => [key, invalid]),
      );
      const view = buildDecisionView({ economics });
      expect(view.expectedNetPence).toBeUndefined();
      expect(view.cashRows).toEqual([]);
      expect(view.stressRows).toEqual([]);
      expect(view.gameDays).toBeUndefined();
      expect(view.dailyPence).toBeUndefined();
      expect(view.haircutBps).toBeUndefined();
    },
  );
});

describe("destination delivery economics", () => {
  it("keeps consumed stores and the full cash purchase distinct", () => {
    const view = buildDecisionView({
      ...plan,
      economics: {
        ...plan.economics,
        basis: "destination",
        provisions_pence: 404,
        store_cash_pence: 82002,
        ballast_repeat_game_days: 4,
        ballast_repeat_net_pence: 7000,
      },
    });
    expect(view.basis).toBe("destination");
    expect(view.expectedNetPence).toBe(7456);
    expect(view.dailyPence).toBe(2982.4);
    expect(
      view.cashRows.find((row) => row.key === "provisions_pence")?.pence,
    ).toBe(404);
    expect(
      view.cashRows.find((row) => row.key === "provisions_pence")?.label,
    ).toMatch(/consum/i);
    expect(
      view.cashRows.find((row) => row.key === "store_cash_pence")?.pence,
    ).toBe(82002);
    expect(
      view.cashRows.find((row) => row.key === "store_cash_pence")?.label,
    ).toMatch(/cash/i);
    expect(view.netLabel).toMatch(/delivery/i);
    expect(view.durationLabel).toMatch(/delivery/i);
    expect(view.ballastRepeatGameDays).toBe(4);
    expect(view.ballastRepeatNetPence).toBe(7000);
    expect(view.cashRows.map((row) => row.key)).not.toContain(
      "ballast_repeat_net_pence",
    );
  });

  it("does not reinterpret historical rows as destination valuations", () => {
    const legacy = buildDecisionView(plan);
    const view = buildDecisionView({
      economics: {
        ...plan.economics,
        store_cash_pence: 5000,
        ballast_repeat_game_days: 4,
        ballast_repeat_net_pence: 7000,
      },
    });
    expect(view.basis).toBe("legacy");
    expect(view.cashRows).toEqual(legacy.cashRows);
    expect(view.dailyLabel).toBe(legacy.dailyLabel);
    expect(view.expectedNetPence).toBe(legacy.expectedNetPence);
    expect(view.ballastRepeatGameDays).toBeUndefined();
    expect(view.ballastRepeatNetPence).toBeUndefined();
  });

  it.each([
    {},
    { ballast_repeat_game_days: 4 },
    { ballast_repeat_net_pence: 7000 },
    { ballast_repeat_game_days: 0, ballast_repeat_net_pence: 7000 },
    { ballast_repeat_game_days: -1, ballast_repeat_net_pence: 7000 },
    { ballast_repeat_game_days: Infinity, ballast_repeat_net_pence: 7000 },
    { ballast_repeat_game_days: 4, ballast_repeat_net_pence: NaN },
  ])(
    "keeps incomplete or unsafe repeat forecasts unavailable: %j",
    (repeat) => {
      const view = buildDecisionView({
        economics: {
          basis: "destination",
          expected_net_pence: 1000,
          ...repeat,
        },
      });
      expect(view.ballastRepeatGameDays).toBeUndefined();
      expect(view.ballastRepeatNetPence).toBeUndefined();
      expect(view.expectedNetPence).toBe(1000);
    },
  );

  it("retains a costed zero or loss repeat without using it as delivery profit", () => {
    for (const repeatNet of [0, -1000]) {
      const view = buildDecisionView({
        economics: {
          basis: "destination",
          expected_net_pence: 1000,
          ballast_repeat_game_days: 4,
          ballast_repeat_net_pence: repeatNet,
          store_cash_pence: 0,
        },
      });
      expect(view.ballastRepeatNetPence).toBe(repeatNet);
      expect(view.expectedNetPence).toBe(1000);
      expect(
        view.cashRows.find((row) => row.key === "store_cash_pence")?.pence,
      ).toBe(0);
    }
  });
});

describe("diagnostics-only decision stress", () => {
  it("keeps collateral and stress separate from the expected cash net", () => {
    const view = buildDecisionView(plan);
    expect(view.cashRows.at(-1)?.label).toBe(
      "Collateral locked, not an expense",
    );
    expect(view.stressRows).toEqual([
      {
        key: "stress_haircut_pence",
        label: "Scenario haircut, not a cost",
        pence: 909,
      },
      {
        key: "price_uncertainty_stress_pence",
        label: "Price uncertainty scenario",
        pence: 1010,
      },
      { key: "stress_net_pence", label: "Stress scenario net", pence: 5537 },
    ]);
    expect(view.expectedNetPence).toBe(7456);
    expect(view.stressNotice).toBe(
      "Scenarios are not expected costs, loss probabilities, actual losses or acceptance penalties.",
    );
    expect(view.haircutBps).toBe(1250);
    expect(percent(view.haircutBps)).toBe("12.5%");
    expect(view.haircutLabel).toBe("Scenario haircut rate, not probability");
    expect(view).not.toHaveProperty("probability_estimate");
    expect([...view.cashRows, ...view.stressRows]).not.toContainEqual(
      expect.objectContaining({ key: "probability_estimate" }),
    );
    const changed = buildDecisionView({
      economics: {
        ...plan.economics,
        collateral_pence: 900000,
        stress_haircut_bps: 9999,
        stress_haircut_pence: 800000,
        price_uncertainty_stress_pence: 700000,
        stress_net_pence: -600000,
      },
    });
    expect(changed.expectedNetPence).toBe(view.expectedNetPence);
  });

  it("retains zero-valued cash and stress diagnostics", () => {
    const view = buildDecisionView({
      economics: Object.fromEntries(
        Object.keys(plan.economics).map((key) => [key, 0]),
      ),
    });
    expect(view.cashRows).toHaveLength(9);
    expect(view.cashRows.every((row) => row.pence === 0)).toBe(true);
    expect(view.stressRows).toHaveLength(3);
    expect(view.stressRows.every((row) => row.pence === 0)).toBe(true);
    expect(view.expectedNetPence).toBe(0);
    expect(view.gameDays).toBe(0);
    expect(view.dailyPence).toBe(0);
    expect(percent(view.haircutBps)).toBe("0%");
  });

  it("does not invent stress when only cash is recorded", () => {
    const view = buildDecisionView({ economics: { expected_net_pence: 900 } });
    expect(view.expectedNetPence).toBe(900);
    expect(view.stressRows).toEqual([]);
    expect(view.haircutBps).toBeUndefined();
  });

  it("does not turn stress-only economics into cash profit", () => {
    const view = buildDecisionView({
      economics: {
        stress_haircut_pence: 909,
        price_uncertainty_stress_pence: 1010,
        stress_net_pence: -1919,
        stress_haircut_bps: 1250,
        net_pence_per_game_day: -767.6,
        probability_estimate: null,
      },
    });
    expect(view.expectedNetPence).toBeUndefined();
    expect(view.cashRows).toEqual([]);
    expect(view.stressRows.map((row) => row.pence)).toEqual([909, 1010, -1919]);
    expect(view.dailyPence).toBe(-767.6);
    expect(view.dailyLabel).toBe(
      "Recorded daily diagnostic; cash net unrecorded",
    );
    expect(money(view.dailyPence)).toBe("-7.68 Cr");
  });
});

describe("decision cash sorting", () => {
  const data = [900, undefined, 10000, -100, 0].map((net) =>
    buildDecisionView({ economics: { expected_net_pence: net } }),
  );

  it("supplies numbers or undefined, never formatted money", () => {
    expect(data.map((view) => view.expectedNetPence)).toEqual([
      900,
      undefined,
      10000,
      -100,
      0,
    ]);
    expect(
      data.every(
        (view) =>
          view.expectedNetPence === undefined ||
          typeof view.expectedNetPence === "number",
      ),
    ).toBe(true);
  });

  it.each([
    [false, [-100, 0, 900, 10000, "Not recorded"]],
    [true, [10000, 900, 0, -100, "Not recorded"]],
  ] as const)("sorts numerically with descending=%s", (descending, order) => {
    const rendered = renderToStaticMarkup(
      createElement(SortedCash, { data, descending }),
    );
    expect(rendered).toBe(
      `<ol>${order.map((value) => `<li>${value}</li>`).join("")}</ol>`,
    );
  });
});
