import type * as React from "react";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type * as Data from "@/lib/data";
import { buildDecisionView } from "@/lib/decisions";
import type { Account, DashboardData, Decision, JsonRecord } from "@/lib/types";
import type * as UI from "./ui";
import { DecisionPanel } from "./decisions";
import { DiagnosticsView } from "./diagnostics";

type TableProps = Parameters<typeof UI.DataTable<JsonRecord>>[0];
const state = vi.hoisted(() => ({
  kind: "events" as "events" | "requests" | "decisions",
  tables: [] as TableProps[],
}));
vi.mock("react", async (original) => {
  const react = await original<typeof React>();
  return {
    ...react,
    // Choose the existing diagnostics tab for SSR; all other hooks and the
    // real table renderer remain intact. No fake row-key implementation.
    useState: (initial: unknown) =>
      react.useState(initial === "events" ? state.kind : initial),
  };
});
vi.mock("@/app/providers", () => ({
  useNow: () => Date.parse("2026-10-28T12:00:00Z"),
}));
vi.mock("@/lib/data", async (original) => ({
  ...(await original<typeof Data>()),
  useHistory: () => ({
    data: [],
    isPending: false,
    isFetching: false,
    error: null,
    dataUpdatedAt: 0,
  }),
}));
vi.mock("./ui", async (original) => {
  const ui = await original<typeof UI>();
  return {
    ...ui,
    DataTable: (props: TableProps) => {
      state.tables.push(props);
      return createElement(ui.DataTable<JsonRecord>, props);
    },
  };
});

const account: Account = {
  id: "one",
  label: "Our fleet",
  company_id: "company-one",
  enabled: true,
  created_at: "2026-10-21T12:00:00Z",
  snapshot: {},
};

function decision(
  id: number,
  overrides: Partial<Decision> = {},
): Decision & { id: number } {
  return {
    id,
    account_id: "one",
    ship_id: "ship-one",
    kind: "rejected:spot",
    reason: "No destination bid depth",
    created_at: "2026-10-28T11:59:00Z",
    details: {
      kind: "spot",
      accepted: false,
      reason: "No destination bid depth",
      price_source: "",
      economics: zeroEconomics,
    },
    ...overrides,
  };
}

// A serialized, never-priced Go Economics value: zero-value fields do not
// establish a zero-Crown valuation. No new pricing flag is imposed by tests.
const zeroEconomics = {
  basis: "",
  revenue_pence: 0,
  purchase_pence: 0,
  customs_pence: 0,
  provisions_pence: 0,
  berth_dues_pence: 0,
  upkeep_pence: 0,
  toll_pence: 0,
  collateral_pence: 0,
  expected_net_pence: 0,
  stress_haircut_bps: 0,
  stress_haircut_pence: 0,
  price_uncertainty_stress_pence: 0,
  stress_net_pence: 0,
  game_days: 0,
  net_pence_per_game_day: 0,
};

function renderDecisions(decisions: Decision[]) {
  state.tables = [];
  return renderToStaticMarkup(
    createElement(DecisionPanel, { account, decisions, ships: [] }),
  );
}

function renderDiagnostics(dashboard: DashboardData) {
  state.tables = [];
  return renderToStaticMarkup(
    createElement(DiagnosticsView, { account, dashboard }),
  );
}

describe("durable decision and diagnostic row identity", () => {
  beforeEach(() => {
    state.kind = "events";
  });

  it("keys equal-timestamp decision rows by their stored id across a prepend", () => {
    const first = decision(101);
    const second = decision(102);
    const html = renderDecisions([first, second]);
    expect(html).toContain("<details");
    const before = state.tables[0];
    expect(before.data).toHaveLength(2);
    const keys = before.data.map((row, i) => before.rowKey(row, i));
    expect(new Set(keys).size).toBe(2);
    renderDecisions([
      decision(103, { created_at: "2026-10-28T12:00:00Z" }),
      first,
      second,
    ]);
    const after = state.tables[0];
    for (let i = 0; i < before.data.length; i++) {
      const old = before.data[i];
      const next = after.data.find((row) => row.id === old.id)!;
      expect(after.rowKey(next, after.data.indexOf(next))).toBe(keys[i]);
      expect(after.rowKey(next, 99)).toBe(keys[i]);
    }
  });

  it.each(["events", "requests", "decisions"] as const)(
    "keeps distinct %s records and details row identity across a prepend",
    (kind) => {
      state.kind = kind;
      const dashboard: DashboardData = {
        updated_at: "2026-10-28T12:00:00Z",
        accounts: [account],
        metrics: [],
        settlements: [],
        decisions: [decision(201), decision(202)],
        events: [201, 202].map((id) => ({
          account_id: "one",
          event_id: `event-${id}`,
          event_type: "ship_sailed",
          payload: {},
          created_at: "2026-10-28T11:59:00Z",
        })),
        requests: [201, 202].map((id) => ({
          id,
          account_id: "one",
          method: "GET",
          path: "/v1/world/clock",
          status: 200,
          duration_ms: 1,
          error: "",
          created_at: "2026-10-28T11:59:00Z",
        })),
      };
      const html = renderDiagnostics(dashboard);
      expect(html).toContain("<details");
      const before = state.tables[0];
      expect(
        before.data,
        "different stored ids must not collapse merely because their payloads and timestamps match",
      ).toHaveLength(2);
      const keys = before.data.map((row, i) => before.rowKey(row, i));
      expect(new Set(keys).size).toBe(2);
      const latest = "2026-10-28T12:00:00Z";
      const newRequest = {
        ...dashboard.requests[0],
        id: 203,
        created_at: latest,
      };
      const next: DashboardData = {
        ...dashboard,
        decisions: [
          decision(203, { created_at: latest }),
          ...dashboard.decisions,
        ],
        events: [
          { ...dashboard.events[0], event_id: "event-203", created_at: latest },
          ...dashboard.events,
        ],
        requests: [newRequest, ...dashboard.requests],
      };
      renderDiagnostics(next);
      const after = state.tables[0];
      for (let i = 0; i < before.data.length; i++) {
        const old = before.data[i];
        const row = after.data.find((item) =>
          kind === "events"
            ? item.event_id === old.event_id
            : item.id === old.id,
        )!;
        expect(after.rowKey(row, after.data.indexOf(row))).toBe(keys[i]);
        expect(after.rowKey(row, 99)).toBe(keys[i]);
      }
    },
  );
});

describe("unpriced rejected decisions", () => {
  it("does not turn a zero-value Go Economics record into money rows", () => {
    const view = buildDecisionView(decision(301).details);
    expect(view.expectedNetPence).toBeUndefined();
    expect(view.dailyPence).toBeUndefined();
    expect(view.cashRows).toEqual([]);
    expect(view.stressRows).toEqual([]);
  });

  it("renders Not priced and the rejection reason without Crown figures", () => {
    const html = renderDecisions([decision(301)]);
    const text = html.replace(/<[^>]*>/g, " ").replace(/\s+/g, " ");
    expect(text).toContain("Not priced");
    expect(text).toContain("No destination bid depth");
    expect(text).not.toContain("Cr");
    expect(text).not.toMatch(
      /Expected revenue|Purchase cost|Stress scenario net/,
    );
  });

  it("retains a genuinely priced rejection and measured zero rather than hiding all rejected plans", () => {
    const details = {
      accepted: false,
      reason: "Below desired margin",
      price_source: "order_book",
      economics: {
        basis: "destination",
        revenue_pence: 100,
        purchase_pence: 100,
        expected_net_pence: 0,
        game_days: 1,
      },
    };
    const view = buildDecisionView(details);
    expect(view.expectedNetPence).toBe(0);
    expect(
      view.cashRows.some(
        (row) => row.key === "revenue_pence" && row.pence === 100,
      ),
    ).toBe(true);
    const html = renderDecisions([
      decision(302, { details, reason: "Below desired margin" }),
    ]);
    expect(html).toContain("0.00 Cr");
    expect(html).not.toContain("Not priced");
  });
});
