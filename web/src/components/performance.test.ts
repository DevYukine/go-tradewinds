import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it, vi } from "vitest";
import type * as Data from "@/lib/data";
import type { MoneyHistory, MoneyPeriod } from "@/lib/types";
import type { MoneyPreferences } from "@/lib/money-history";
import { Performance } from "./performance";

const state = vi.hoisted(() => ({
  data: undefined as MoneyHistory | undefined,
}));
vi.mock("@/lib/data", async (original) => ({
  ...(await original<typeof Data>()),
  useMoneyHistory: () => ({
    data: state.data,
    isPending: false,
    isError: false,
  }),
}));
vi.mock("@/lib/ui-store", () => ({
  useUI: (
    select: (value: {
      moneyHistory: MoneyPreferences;
      setMoneyHistory: (preferences: Partial<MoneyPreferences>) => void;
    }) => unknown,
  ) =>
    select({
      moneyHistory: {
        series: "settlement_gross",
        range: "24h",
        from: "",
        to: "",
      },
      setMoneyHistory: vi.fn(),
    }),
}));
const known: MoneyPeriod = {
  at: "2026-10-28T10:00:00Z",
  to: "2026-10-28T11:00:00Z",
  count: 1,
  known_count: 1,
  gross_pence: 2000,
  costs_pence: 600,
  net_pence: 1400,
};

function render(periods: MoneyPeriod[]) {
  state.data = {
    account_id: "account-a",
    generated_at: "2026-10-28T12:00:00Z",
    from: "2026-10-28T00:00:00Z",
    to: "2026-10-29T00:00:00Z",
    bucket_seconds: 3600,
    metrics: [],
    settlements: periods,
  };
  return renderToStaticMarkup(
    createElement(Performance, { accountID: "account-a" }),
  )
    .replace(/<[^>]*>/g, "")
    .replace(/\s+/g, " ");
}

describe("settlement performance evidence", () => {
  it("shows unknown total gross and explains omitted gross periods without erasing known net", () => {
    const html = render([
      known,
      {
        ...known,
        at: "2026-10-28T11:00:00Z",
        known_count: 0,
        gross_pence: null,
        unknown_proceeds_count: 1,
        costs_pence: null,
        net_pence: null,
      },
    ]);
    expect(html).toContain("Gross revenueProceeds unknown");
    expect(html).toContain("Known-cost net+14.00 Cr");
    expect(html).toContain(
      "1 populated 1-hour periods from 2 completed settlements.",
    );
    expect(html).toContain(
      "Unknown proceeds keep total gross unknown; affected gross chart periods are omitted.",
    );
    expect(html).toContain(
      "Costs and net include only settlements with known proceeds and costs.",
    );
    expect(html).not.toContain(
      "Gross includes unknown-cost proceeds; costs and net exclude them.",
    );
  });

  it("includes known proceeds with unknown costs in gross, but not in costs or net", () => {
    const html = render([
      known,
      {
        ...known,
        at: "2026-10-28T11:00:00Z",
        known_count: 0,
        gross_pence: 3000,
        costs_pence: null,
        net_pence: null,
      },
    ]);
    expect(html).toContain("Gross revenue50.00 Cr");
    expect(html).toContain("Known costs6.00 Cr");
    expect(html).toContain("Known-cost net+14.00 Cr");
    expect(html).toContain(
      "Known proceeds from unknown-cost settlements count toward gross only.",
    );
    expect(html).not.toContain("Gross revenueProceeds unknown");
  });
});
