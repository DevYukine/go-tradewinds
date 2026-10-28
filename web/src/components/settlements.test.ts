import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type * as Data from "@/lib/data";
import type { Account, Settlement } from "@/lib/types";
import { RecentSettlements, SettlementLedger } from "./settlements";

const state = vi.hoisted(() => ({ data: [] as Settlement[] }));
vi.mock("@/lib/data", async (original) => ({
  ...(await original<typeof Data>()),
  useHistory: () => ({ data: state.data, isPending: false, error: null }),
}));

const account: Account = {
  id: "account-a",
  label: "Yukine",
  company_id: "company-a",
  enabled: true,
  created_at: "2026-10-28T00:00:00Z",
  snapshot: {},
};
const passage: Settlement = {
  id: "passage-receipt",
  account_id: account.id,
  ship_id: "lapras",
  mission_id: "passage-mission",
  lane: "london|dover",
  action_id: "",
  kind: "passage",
  origin: "london",
  destination: "dover",
  commodity: "",
  quantity_tons: 0,
  game_seconds: 86400,
  costs_known: false,
  acquisition_pence: 0,
  proceeds_pence: null,
  proceeds_unknown: true,
  fees_pence: 100,
  provisions_pence: 200,
  expenses_pence: 300,
  started_at: "2026-10-27T00:00:00Z",
  completed_at: "2026-10-28T00:00:00Z",
  created_at: "2026-10-28T00:00:00Z",
};
function ledger() {
  return renderToStaticMarkup(
    createElement(SettlementLedger, { account, search: "" }),
  );
}
function plain(html: string) {
  return html.replace(/<[^>]*>/g, "").replace(/\s+/g, " ");
}

describe("passage settlement receipts", () => {
  beforeEach(() => {
    state.data = [{ ...passage }];
  });

  it("labels unknown proceeds in rows and both aggregate levels without inventing net", () => {
    const html = plain(ledger());
    expect(html).toContain("Passage");
    expect(html).toContain("Sample proceedsProceeds unknown");
    expect(html.match(/Proceeds unknown/g)?.length).toBeGreaterThanOrEqual(4);
    expect(html).not.toContain("-6.00 Cr");
    const recent = plain(
      renderToStaticMarkup(createElement(RecentSettlements, { account })),
    );
    expect(recent.match(/Proceeds unknown/g)).toHaveLength(2);
    expect(recent).not.toContain("-6.00 Cr");
  });

  it("keeps mixed sample and ship proceeds unknown when another receipt has known revenue", () => {
    state.data.push({
      ...passage,
      id: "paid",
      costs_known: true,
      proceeds_pence: 2000,
      proceeds_unknown: false,
    });
    const html = plain(ledger());
    expect(html).toContain("Sample proceedsProceeds unknown");
    expect(html.match(/Proceeds unknown/g)?.length).toBeGreaterThanOrEqual(4);
    expect(html).toContain("14.00 Cr");
  });

  it("shows the retained fare event ID and preserves a known zero fare", () => {
    state.data = [
      {
        ...passage,
        costs_known: true,
        proceeds_pence: 0,
        proceeds_unknown: false,
        event_id: "fare-event-123",
      },
    ];
    const html = plain(ledger());
    expect(html).toContain("Event IDfare-event-123");
    expect(html).toContain("Action IDNot recorded");
    expect(html).toContain("Sample proceeds0.00 Cr");
    expect(html).toContain("-6.00 Cr");
    expect(html).not.toContain("Proceeds unknown");
  });
});
