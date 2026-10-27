import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type * as Data from "@/lib/data";
import type { DashboardData } from "@/lib/types";
import { Dashboard } from "./dashboard";

const query = vi.hoisted(() => ({
  data: undefined as DashboardData | undefined,
  isPending: true,
  isError: false,
  isSuccess: false,
  isFetching: true,
  error: null as Error | null,
}));

vi.mock("@/lib/data", async (original) => ({
  ...(await original<typeof Data>()),
  useDashboard: () => query,
  useWarnings: () => ({
    isError: false,
    data: {
      generated_at: "2026-10-27T02:14:00Z",
      build: {
        revision: "abc1234def5678",
        modified: false,
        started_at: "2026-10-27T01:58:00Z",
      },
      accounts: [],
      unmonitored: [],
    },
  }),
}));
vi.mock("@/app/providers", () => ({
  useNow: () => Date.parse("2026-10-27T02:14:00Z"),
}));

function recordedEmptyDashboard(): DashboardData {
  return {
    updated_at: "2026-10-27T02:14:00Z",
    accounts: [],
    metrics: [],
    decisions: [],
    events: [],
    requests: [],
    settlements: [],
  };
}

function render() {
  const client = new QueryClient();
  const html = renderToStaticMarkup(
    createElement(QueryClientProvider, { client }, createElement(Dashboard)),
  );
  client.clear();
  return html.replace(/<[^>]*>/g, "").replace(/\s+/g, " ");
}

describe("dashboard warning scope readiness", () => {
  beforeEach(() => {
    query.data = undefined;
    query.isPending = true;
    query.isError = false;
    query.isSuccess = false;
    query.isFetching = true;
    query.error = null;
  });

  it("shows loading, not an empty warning scope, before the first dashboard response", () => {
    const text = render();
    expect(text).toContain("Loading recorded fleet and company data");
    expect(text).not.toContain("no accounts are in scope");
    expect(text).not.toContain("Recording service unavailable");
  });

  it("shows the initial dashboard error without claiming an empty warning scope", () => {
    query.isPending = false;
    query.isError = true;
    query.isFetching = false;
    query.error = new Error("Service unavailable");
    const text = render();
    expect(text).toContain("Updates interrupted.");
    expect(text).toContain("Recording service unavailable");
    expect(text).not.toContain("Loading recorded fleet and company data");
    expect(text).not.toContain("no accounts are in scope");
  });

  it("names a recorded empty scope after a successful dashboard response", () => {
    query.data = recordedEmptyDashboard();
    query.isPending = false;
    query.isSuccess = true;
    query.isFetching = false;
    const text = render();
    expect(text).toContain("No accounts recorded");
    expect(text).toContain("no accounts are in scope");
    expect(text).not.toContain("Updates interrupted.");
  });

  it("keeps a recorded empty scope visible after a failed background dashboard refetch", () => {
    query.data = recordedEmptyDashboard();
    query.isPending = false;
    query.isError = true;
    query.isSuccess = false;
    query.isFetching = false;
    query.error = new Error("Service unavailable");
    const text = render();
    expect(text).toContain("Updates interrupted.");
    expect(text).toContain("No accounts recorded");
    expect(text).toContain("no accounts are in scope");
    expect(text).not.toContain("Recording service unavailable");
  });
});
