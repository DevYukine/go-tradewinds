import type * as React from "react";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it, vi } from "vitest";
import type * as Data from "@/lib/data";
import type { Account, DashboardData, JsonRecord } from "@/lib/types";
import type * as UI from "./ui";
import { DiagnosticsView } from "./diagnostics";

type TableProps = Parameters<typeof UI.DataTable<JsonRecord>>[0];
const state = vi.hoisted(() => ({
  kind: "events" as "events" | "requests" | "decisions",
  history: [] as JsonRecord[],
  table: undefined as TableProps | undefined,
}));
vi.mock("react", async (original) => {
  const react = await original<typeof React>();
  return {
    ...react,
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
    data: state.history,
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
      state.table = props;
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
const common = { account_id: "one", created_at: "2026-10-28T11:59:00Z" };
const dashboard: DashboardData = {
  updated_at: "2026-10-28T12:00:00Z",
  accounts: [account],
  metrics: [],
  settlements: [],
  events: [
    { ...common, event_id: "event-1", event_type: "ship_sailed", payload: {} },
  ],
  requests: [
    {
      ...common,
      id: 1,
      method: "GET",
      path: "/v1/world/clock",
      status: 200,
      duration_ms: 1,
      error: "",
    },
  ],
  decisions: [
    {
      ...common,
      id: 1,
      ship_id: "ship-one",
      kind: "rejected:spot",
      reason: "No bid depth",
      details: {},
    },
  ],
};

describe("diagnostic dashboard and history reconciliation", () => {
  it.each(["events", "requests", "decisions"] as const)(
    "preserves %s entries when the API omits record IDs",
    (kind) => {
      state.kind = kind;
      const first = { ...dashboard[kind][0] } as JsonRecord;
      delete first.id;
      delete first.event_id;
      state.history = [first, { ...first }];
      const html = renderToStaticMarkup(
        createElement(DiagnosticsView, { account, dashboard }),
      );
      const table = state.table!;
      expect(table.data).toHaveLength(3);
      expect(new Set(table.data.map(table.rowKey)).size).toBe(3);
      expect(html).toContain("Record IDs were not provided");
    },
  );
  it.each(["events", "requests", "decisions"] as const)(
    "deduplicates the same stored %s record, not identical payloads or timestamps",
    (kind) => {
      state.kind = kind;
      const row = dashboard[kind][0];
      state.history = [
        { ...row },
        { ...row, id: 2, event_id: "event-2" },
        { ...row, account_id: "other" },
      ];
      renderToStaticMarkup(
        createElement(DiagnosticsView, { account, dashboard }),
      );
      const table = state.table!;
      expect(table.data).toHaveLength(2);
      expect(table.data.every((item) => item.account_id === account.id)).toBe(
        true,
      );
      expect(new Set(table.data.map(table.rowKey)).size).toBe(2);
      expect(table.rowKey(table.data[0], 0)).toBe(
        table.rowKey(table.data[0], 99),
      );
    },
  );
});
