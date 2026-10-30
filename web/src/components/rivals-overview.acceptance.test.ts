import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type * as Data from "@/lib/data";
import type { RivalReport, RivalRow } from "@/lib/types";
import type * as UI from "./ui";
import { RivalsView } from "./rivals";

type TableProps = Parameters<typeof UI.DataTable<RivalRow>>[0];
type OverviewRow = RivalRow & {
  company_id: string | null;
  credit: Record<string, number | string | null> | null;
  credit_observed_at: string | null;
  rising_rank: number | null;
  growth_bps: number | null;
  gained_pence: number | null;
  observed_ships_24h: Record<string, number>;
  observed_ships_7d: Record<string, number>;
  expansion_contributions: {
    target_phase: number;
    tons: number;
    share_bps: number;
  }[];
};
type RisingEntry = {
  rank: number;
  company_name: string;
  home_port_id: string;
  growth_bps: number;
  gained_pence: number;
  window_start_at: string;
};
type Overview = Omit<RivalReport, "rivals" | "our"> & {
  rivals: OverviewRow[];
  our: OverviewRow;
  rising: {
    entries: RisingEntry[];
    ranked_players: number;
    window_start_at: string | null;
    you: (RisingEntry & { is_calling_company: boolean }) | null;
  };
};

const state = vi.hoisted(() => ({
  data: undefined as Overview | undefined,
  isError: false,
  isPending: false,
  error: new Error("Service unavailable"),
  tables: [] as TableProps[],
}));
vi.mock("@/lib/data", async (original) => ({
  ...(await original<typeof Data>()),
  useRivals: () => state,
}));
vi.mock("@/app/providers", () => ({
  useNow: () => Date.parse("2026-10-28T12:00:00Z"),
}));
vi.mock("./ui", async (original) => {
  const ui = await original<typeof UI>();
  return {
    ...ui,
    DataTable: (props: TableProps) => {
      state.tables.push(props);
      return createElement(ui.DataTable<RivalRow>, props);
    },
  };
});

function row(name: string, companyID: string | null): OverviewRow {
  return {
    company_name: name,
    company_id: companyID,
    rank: 1,
    net_worth_pence: 436501259,
    change_pence: 9876543,
    baseline_pence: 426624716,
    shared_lanes: 2,
    shared_ports: 1,
    hunch: "most_dangerous",
    fact: { kind: "shared_lanes", count: 2, total: 3 },
    credit: null,
    credit_observed_at: null,
    rising_rank: 1,
    growth_bps: 1250,
    gained_pence: 9876543,
    observed_ships_24h: { hoy: 2, cutter: 1 },
    observed_ships_7d: { hoy: 4, cutter: 2 },
    expansion_contributions: [{ target_phase: 3, tons: 123, share_bps: 4567 }],
  };
}

function fixture(): Overview {
  const ours = row("Ours", "company-one");
  const rival = row("Kraken", "kraken");
  return {
    account_id: "one",
    generated_at: "2026-10-28T12:00:00Z",
    season_number: 1,
    valuation_at: "2026-10-28T11:28:00Z",
    next_refresh_at: "2026-10-28T12:28:00Z",
    baseline_at: "2026-10-27T11:28:00Z",
    from: "2026-10-27T12:00:00Z",
    to: "2026-10-28T12:00:00Z",
    coverage: "partial",
    own_companies_complete: true,
    overlap_known: true,
    events_truncated: false,
    our: ours,
    rivals: [rival],
    growth: [],
    verdicts: {
      most_dangerous: {
        company_name: "Kraken",
        fact: rival.fact,
        reason: "recorded",
      },
      plays_rough: { company_name: null, fact: null, reason: "none_recorded" },
      not_profitable: { company_name: null, fact: null, reason: "no_baseline" },
    },
    rising: {
      entries: [],
      ranked_players: 0,
      window_start_at: null,
      you: null,
    },
  };
}

function render() {
  state.tables = [];
  return renderToStaticMarkup(createElement(RivalsView, { accountID: "one" }));
}

function plain(html: string) {
  return html.replace(/<[^>]*>/g, " ").replace(/\s+/g, " ");
}

describe("rivals money overview", () => {
  beforeEach(() => {
    state.data = fixture();
  });

  it("renders absent credit as Not observed, never a clean zero-credit history", () => {
    const html = render();
    const text = plain(html);
    expect(text).toMatch(/Not observed/);
    expect(text).toMatch(/Credit|Defaults/i);
    expect(text).not.toMatch(/(?:Defaults|Walk.aways|Convictions)\s*0\b/i);
  });

  it("shows credit facts simultaneously, with debt separate from overlapping lifetime losses", () => {
    const rival = state.data!.rivals[0];
    rival.credit_observed_at = "2026-10-28T11:55:00Z";
    rival.credit = {
      company_id: "kraken",
      company_name: "Kraken",
      active_loans: 2,
      active_owed_pence: 1001,
      defaulted_open_loans: 1,
      defaulted_owed_pence: 300,
      loans_repaid: 7,
      loans_defaulted: 4,
      loans_written_off: 2,
      loan_written_off_pence: 700,
      loans_walked_away: 3,
      loan_walked_away_pence: 500,
      open_mortgages: 1,
      futures_claims_open_pence: 41,
      futures_claims_total: 5,
      futures_claims_written_off: 2,
      lent_outstanding_pence: 9009,
      smuggling_convictions: 6,
      last_default_at: null,
      bankrupt_at: null,
      last_conviction_at: null,
    };
    const text = plain(render());
    for (const label of [
      /Defaults/i,
      /Walk.aways/i,
      /Writ(?:e|ten).offs/i,
      /Owed|Loan debt/i,
      /Mortgages/i,
      /Lent|Lending/i,
      /Convictions/i,
    ]) {
      expect(text).toMatch(label);
    }
    expect(text).toContain("7.00 Cr");
    expect(text).toContain("5.00 Cr");
    expect(text).toContain("90.09 Cr");
    // The only current published loan debt is 10.01 + 3.00 Cr; lifetime
    // walk-aways/write-offs are overlapping counters, not extra debt now.
    expect(text).toMatch(/13\.01 Cr|10\.01 Cr.*3\.00 Cr/);
    expect(text).not.toContain("25.01 Cr");
  });

  it("discloses an empty rising board as not yet published", () => {
    expect(plain(render())).toMatch(/rising.*not yet published/is);
  });

  it("renders published growth and gains and visibly identifies our rising row", () => {
    const start = "2026-10-21T11:28:00Z";
    const rival: RisingEntry = {
      rank: 1,
      company_name: "Kraken",
      home_port_id: "london",
      growth_bps: 1250,
      gained_pence: 9876543,
      window_start_at: start,
    };
    const ours: RisingEntry = {
      rank: 2,
      company_name: "Ours",
      home_port_id: "london",
      growth_bps: 500,
      gained_pence: 123456,
      window_start_at: start,
    };
    state.data!.rising = {
      entries: [rival, ours],
      ranked_players: 2,
      window_start_at: start,
      you: { ...ours, is_calling_company: true },
    };
    const html = render();
    const tables = html.match(/<table\b[^>]*>.*?<\/table>/gs) ?? [];
    const rising = tables.find((table) => /rising/i.test(table));
    expect(
      rising,
      "published rising board must have a readable table",
    ).toBeDefined();
    const text = plain(rising ?? "");
    expect(text).toContain("12.50%");
    expect(text).toContain("5.00%");
    expect(text).toContain("98,765.43 Cr");
    expect(text).toContain("1,234.56 Cr");
    const ownRow = (rising?.match(/<tr\b[^>]*>.*?<\/tr>/gs) ?? []).find((tr) =>
      tr.includes("Ours"),
    );
    expect(ownRow, "our row must be present on the rising board").toBeDefined();
    expect(ownRow ?? "").toMatch(
      /aria-current="true"|data-is-us="true"|\b(?:You|Our company)\b/,
    );
  });

  it("describes 24 h and 7 d class counts as observed sightings, not an owned fleet census", () => {
    const text = plain(render());
    expect(text).toMatch(/observed.*sightings|sightings.*observed/i);
    expect(text).toMatch(/24 (?:real )?h/);
    expect(text).toMatch(/7 (?:real )?d/);
    expect(text).toMatch(/Hoy/i);
    expect(text).toMatch(/Cutter/i);
    expect(text).not.toMatch(/(?:owns|total fleet|fleet size)\s*[:=]?\s*3/i);
  });

  it("keeps the company row identity and native details when fresh rows are prepended", () => {
    const html = render();
    const table = state.tables.find((props) =>
      props.data.some((item) => item.company_name === "Kraken"),
    );
    expect(table).toBeDefined();
    expect(html).toContain("<details");
    const before = table!.data.find((item) => item.company_name === "Kraken")!;
    const oldIndex = table!.data.indexOf(before);
    const oldKey = table!.rowKey(before, oldIndex);
    const refreshed = {
      ...state.data!.rivals[0],
      rank: 5,
      net_worth_pence: 440000000,
    };
    state.data!.rivals = [row("New rival", "new-company"), refreshed];
    render();
    const next = state.tables.find((props) =>
      props.data.some((item) => item.company_name === "Kraken"),
    )!;
    const nextRow = next.data.find((item) => item.company_name === "Kraken")!;
    const nextKey = next.rowKey(nextRow, next.data.indexOf(nextRow));
    expect(nextKey).toBe(oldKey);
    expect(nextKey).not.toBe(next.rowKey(next.data[0], 0));
    expect(next.rowKey(nextRow, 99)).toBe(oldKey);
  });
});
