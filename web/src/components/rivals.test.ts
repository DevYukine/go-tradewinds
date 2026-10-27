import type * as ReactTable from "@tanstack/react-table";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type * as Data from "@/lib/data";
import type { RivalReport, RivalRow } from "@/lib/types";
import type * as UI from "./ui";
import { RivalsView } from "./rivals";

type TableProps = Parameters<typeof UI.DataTable<RivalRow>>[0];

const state = vi.hoisted(() => ({
  data: undefined as RivalReport | undefined,
  isError: false,
  isPending: false,
  error: new Error("Service unavailable"),
  now: Date.parse("2026-10-27T02:00:00Z"),
}));
const tableState = vi.hoisted(() => ({
  props: undefined as TableProps | undefined,
  sorting: undefined as ReactTable.SortingState | undefined,
}));

vi.mock("@tanstack/react-table", async (original) => {
  const reactTable = await original<typeof ReactTable>();
  const useControlledTable: typeof reactTable.useTable = (options, selector) =>
    reactTable.useTable(
      tableState.sorting === undefined
        ? options
        : {
            ...options,
            state: Object.assign({}, options.state, {
              sorting: tableState.sorting,
            }),
          },
      selector,
    );
  return { ...reactTable, useTable: useControlledTable };
});

vi.mock("@/lib/data", async (original) => ({
  ...(await original<typeof Data>()),
  useRivals: () => state,
}));
vi.mock("@/app/providers", () => ({
  useNow: () => state.now,
}));
vi.mock("./ui", async (original) => {
  const ui = await original<typeof UI>();
  return {
    ...ui,
    DataTable: (props: TableProps) => {
      tableState.props = props;
      return createElement(ui.DataTable<RivalRow>, props);
    },
  };
});

function row(overrides: Partial<RivalRow> = {}): RivalRow {
  return {
    company_name: "Kraken",
    rank: 1,
    net_worth_pence: 436501259,
    change_pence: -11764585,
    baseline_pence: 448265844,
    shared_lanes: 14,
    shared_ports: 5,
    hunch: "most_dangerous",
    fact: { kind: "shared_lanes", count: 14, total: 16 },
    ...overrides,
  };
}

function fixture(): RivalReport {
  const rivals = [
    row(),
    row({
      company_name: "Rough Trader",
      rank: 2,
      hunch: "plays_rough",
      fact: {
        kind: "monopoly_in_force",
        commodity: "raw_wool",
        region: "channel",
        expires_at: "2026-10-27T21:28:00Z",
      },
    }),
    row({
      company_name: "Meridian",
      rank: 20,
      hunch: "losing_value",
      net_worth_pence: -1245586,
      change_pence: -4217923,
      fact: { kind: "negative_worth" },
    }),
    row({
      company_name: "TescoValue",
      rank: 8,
      hunch: "growing",
      change_pence: 93888,
      fact: {
        kind: "value_change",
        change_pence: 93888,
        baseline_pence: 1000000,
      },
    }),
    row({
      company_name: "TopStats AI Logistics",
      rank: null,
      hunch: "not_ranked",
      net_worth_pence: null,
      change_pence: null,
      baseline_pence: null,
      fact: { kind: "ships_seen", count: 2 },
    }),
  ];
  return {
    account_id: "one",
    generated_at: "2026-10-27T02:00:00Z",
    season_number: 1,
    valuation_at: "2026-10-27T01:28:00Z",
    next_refresh_at: "2026-10-27T02:28:00Z",
    baseline_at: "2026-10-26T01:28:00Z",
    from: "2026-10-26T02:00:00Z",
    to: "2026-10-27T02:00:00Z",
    coverage: "partial",
    own_companies_complete: true,
    overlap_known: true,
    events_truncated: false,
    our: row({
      company_name: "The Pokemon Pirates",
      rank: 5,
      hunch: "losing_value",
      net_worth_pence: 11613290,
      change_pence: -1246694,
      baseline_pence: 12859984,
      fact: {
        kind: "value_change",
        change_pence: -1246694,
        baseline_pence: 12859984,
      },
    }),
    rivals,
    verdicts: {
      most_dangerous: {
        company_name: rivals[0].company_name,
        fact: rivals[0].fact,
        reason: "recorded",
      },
      plays_rough: {
        company_name: rivals[1].company_name,
        fact: rivals[1].fact,
        reason: "recorded",
      },
      not_profitable: {
        company_name: rivals[2].company_name,
        fact: rivals[2].fact,
        reason: "recorded",
      },
    },
  };
}

function render(sorting?: ReactTable.SortingState) {
  tableState.sorting = sorting;
  return renderToStaticMarkup(createElement(RivalsView, { accountID: "one" }));
}

function plain(html: string) {
  return html.replace(/<[^>]*>/g, "").replace(/\s+/g, " ");
}

describe("rival company ledger", () => {
  beforeEach(() => {
    state.data = fixture();
    state.isError = false;
    state.isPending = false;
    tableState.props = undefined;
    state.now = Date.parse("2026-10-27T02:00:00Z");
  });

  it("renders three textual hunch lines with the selected company's recorded fact", () => {
    const html = render();
    const hunches = html.match(/<dl[^>]*>(.*?)<\/dl>/s)?.[1] ?? "";
    expect(hunches.match(/<dt>/g)).toHaveLength(3);
    expect(plain(hunches)).toContain(
      "Most dangerousKraken: On 14 of our 16 lanes",
    );
    expect(plain(hunches)).toContain(
      "Plays roughRough Trader: Monopoly on Raw wool in Channel until 21:28 UTC",
    );
    expect(plain(hunches)).toContain(
      "Not making profitMeridian: Net worth below zero",
    );
    expect(html).not.toContain("<details");
  });

  it("shows five columns with exactly one text chip and one key fact per row", () => {
    const html = render();
    const body = html.match(/<tbody>(.*?)<\/tbody>/s)?.[1] ?? "";
    const rows = [...body.matchAll(/<tr>(.*?)<\/tr>/gs)].map(
      (match) => match[1],
    );
    expect(rows).toHaveLength(5);
    for (const entry of rows) {
      expect(entry.match(/<td(?:\s[^>]*)?>/g)).toHaveLength(5);
      expect(entry.match(/class="badge badge-/g)).toHaveLength(1);
    }
    const headings = html.match(/<thead>(.*?)<\/thead>/s)?.[1] ?? "";
    expect(
      [...headings.matchAll(/<th(?:\s[^>]*)?>(.*?)<\/th>/gs)].map((match) =>
        plain(match[1]),
      ),
    ).toEqual(["Company", "Hunch", "Key fact", "Net worth", "24 h change"]);
    expect(
      [...rows[0].matchAll(/<td(?:\s[^>]*)?>(.*?)<\/td>/gs)][2][1],
    ).toContain("On 14 of our 16 lanes");
    expect(rows[0]).toContain("Rank 1");
    expect(rows[0]).toContain("Most dangerous");
    expect(rows[0]).toContain("4,365,012.59 Cr");
    expect(rows[0]).toContain("-117,645.85 Cr");
    expect(rows[0].match(/On 14 of our 16 lanes/g)).toHaveLength(1);
    expect(rows[3]).toContain("+938.88 Cr");
    expect(rows[4]).toContain("Not ranked");
    expect(rows[4].match(/Not recorded/g)).toHaveLength(2);
    expect(rows[4]).toContain("2 ships seen in 24 h");
    expect(html).toContain('aria-label="Rival companies table"');
    expect(html).toContain('tabindex="0"');
  });

  it("preserves the server's input attention order by default", () => {
    state.data!.rivals = [
      row({ company_name: "Zulu", hunch: "too_new" }),
      row({ company_name: "Alpha", hunch: "threat" }),
    ];
    const body = render().match(/<tbody>(.*?)<\/tbody>/s)?.[1] ?? "";
    expect(body.indexOf("Zulu")).toBeLessThan(body.indexOf("Alpha"));
  });

  it("discloses valuation time, partial traffic, our change and uncertainty in one caption", () => {
    const text = plain(render());
    expect(text).toContain("Official valuation: 27 Oct, 01:28 UTC");
    expect(text).toContain("last 24 real hours of port notices, partial");
    expect(text).toContain("our 24 h change: -12,466.94 Cr");
    expect(text).toContain("hunches from recorded evidence, not certainties");
    expect(text).not.toContain("overdue");
    expect(text).not.toContain("lane and port overlap is unknown");
  });

  it("flags overdue valuations and bounded event evidence", () => {
    state.data!.next_refresh_at = "2026-10-27T01:59:00Z";
    state.data!.events_truncated = true;
    const text = plain(render());
    expect(text).toContain("(overdue)");
    expect(text).toContain(
      "the event limit was reached, so observations are a lower bound.",
    );
  });

  it("keeps unobserved traffic and missing own change distinct from measured zero", () => {
    state.data!.coverage = "not_observed";
    state.data!.our = null;
    expect(plain(render())).toContain(
      "port notices, not observed; our 24 h change: Not recorded",
    );
    state.data!.our = row({ change_pence: 0 });
    expect(plain(render())).toContain("our 24 h change: 0.00 Cr");
  });

  it("states the three exact honest gaps rather than inventing a winning company", () => {
    const gap = { company_name: null, fact: null, reason: "none_recorded" };
    state.data!.verdicts = {
      most_dangerous: gap,
      plays_rough: gap,
      not_profitable: gap,
    };
    const text = plain(render());
    expect(text).toContain("No larger rival seen on our lanes");
    expect(text).toContain(
      "No attributable rough play in 7 days; marque sponsors are anonymous",
    );
    expect(text).toContain("No rival lost value in 24 h");
  });

  it("does not report no threat or no decline when the required valuation is missing", () => {
    state.data!.our = null;
    state.data!.verdicts.most_dangerous = {
      company_name: null,
      fact: null,
      reason: "our_valuation_missing",
    };
    state.data!.verdicts.not_profitable = {
      company_name: null,
      fact: null,
      reason: "no_baseline",
    };
    const text = plain(render());
    expect(text).toContain("Our valuation has not been recorded");
    expect(text).toContain("No valuation near 24 h ago");
    expect(text).not.toContain("No larger rival seen on our lanes");
    expect(text).not.toContain("No rival lost value in 24 h");
  });

  it("warns that incomplete ownership may include our companies and withholds verdicts", () => {
    state.data!.own_companies_complete = false;
    state.data!.overlap_known = false;
    state.data!.rivals[0].hunch = "threat";
    const withheld = {
      company_name: null,
      fact: null,
      reason: "own_companies_incomplete",
    };
    state.data!.verdicts = {
      most_dangerous: withheld,
      plays_rough: withheld,
      not_profitable: withheld,
    };
    const html = render();
    const text = plain(html);
    expect(text).toContain(
      "the observed list may include our own companies and verdicts are withheld",
    );
    expect(text).toContain("lane and port overlap is unknown");
    expect(text).toContain("last 24 real hours of port notices, partial");
    expect(
      text.match(/Verdict withheld until our company names are complete/g),
    ).toHaveLength(3);
    expect(text).not.toContain("No larger rival seen on our lanes");
    expect(text).not.toContain("No attributable rough play");
    expect(text).not.toContain("No rival lost value in 24 h");
    expect(html).toContain("<table");
  });

  it("discloses unknown overlap when ownership is complete but no usable own notices exist", () => {
    state.data!.overlap_known = false;
    state.data!.rivals = state.data!.rivals.map((item) => ({
      ...item,
      shared_lanes: null,
      shared_ports: null,
    }));
    const larger = {
      kind: "larger" as const,
      net_worth_pence: 436501259,
      our_net_worth_pence: 11613290,
    };
    state.data!.rivals[0].fact = larger;
    state.data!.verdicts.most_dangerous.fact = larger;
    const text = plain(render());
    expect(text).toContain("lane and port overlap is unknown");
    expect(text).toContain("37.6× our net worth");
    expect(text).not.toContain("our configured company names are incomplete");
    expect(text).not.toContain("verdicts are withheld");
  });

  it("does not claim no rough play when the recorded evidence belongs to unlisted companies", () => {
    state.data!.rivals = state.data!.rivals.filter(
      (item) => item.company_name !== "Rough Trader",
    );
    state.data!.verdicts.plays_rough = {
      company_name: null,
      fact: null,
      reason: "unlisted_only",
    };
    const text = plain(render());
    expect(text).toContain(
      "Rough-play evidence was recorded only for unlisted companies",
    );
    expect(text).toContain("marque sponsors are anonymous");
    expect(text).not.toContain("No attributable rough play");
  });

  it("keeps negative worth truthful when the company's measured value is growing", () => {
    state.data!.rivals[2] = {
      ...state.data!.rivals[2],
      hunch: "growing",
      change_pence: 10000,
      fact: { kind: "negative_worth" },
    };
    const body = render().match(/<tbody>(.*?)<\/tbody>/s)?.[1] ?? "";
    const meridian =
      [...body.matchAll(/<tr>(.*?)<\/tr>/gs)].find((match) =>
        match[1].includes("Meridian"),
      )?.[1] ?? "";
    expect(meridian).toContain("Growing");
    expect(meridian).toContain("Net worth below zero");
    expect(meridian).toContain("+100.00 Cr");
    expect(meridian).not.toContain("Losing value");
  });

  it("explains when the strongest rough-play headline has a higher-precedence row chip", () => {
    state.data!.verdicts.plays_rough = {
      company_name: "Kraken",
      fact: {
        kind: "monopoly_in_force",
        commodity: "grain",
        region: "channel",
        expires_at: "2026-10-27T21:28:00Z",
      },
      reason: "recorded",
    };
    const html = render();
    const hunches = plain(html.match(/<dl[^>]*>(.*?)<\/dl>/s)?.[1] ?? "");
    expect(hunches).toContain(
      "Plays roughKraken: Monopoly on Grain in Channel until 21:28 UTC",
    );
    expect(plain(html)).toContain(
      "its Most dangerous chip takes precedence, while other Plays rough chips flag their own evidence",
    );
    const body = html.match(/<tbody>(.*?)<\/tbody>/s)?.[1] ?? "";
    expect(body).toContain("Most dangerous");
    expect(body).toContain("Rough Trader");
    expect(body).toContain("Plays rough");
  });

  it("joins incomplete ownership, unknown overlap, truncation and precedence into one coverage sentence", () => {
    state.data!.own_companies_complete = false;
    state.data!.overlap_known = false;
    state.data!.events_truncated = true;
    state.data!.verdicts.plays_rough = {
      company_name: "Kraken",
      fact: { kind: "loan_walkaways", count: 2 },
      reason: "recorded",
    };
    const html = render();
    const caption =
      [...html.matchAll(/<p[^>]*>(.*?)<\/p>/gs)]
        .map((match) => plain(match[1]))
        .find((text) => text.includes("Official valuation:")) ?? "";
    const coverage = caption.slice(caption.indexOf("Coverage:"));
    expect(coverage).toContain(
      "our configured company names are incomplete; the observed list may include our own companies and verdicts are withheld; lane and port overlap is unknown; the event limit was reached, so observations are a lower bound; the rough-play headline uses the strongest evidence; its Most dangerous chip takes precedence, while other Plays rough chips flag their own evidence",
    );
    expect(coverage.match(/Coverage:/g)).toHaveLength(1);
    expect(coverage.match(/\./g)).toHaveLength(1);
    expect(coverage.endsWith(".")).toBe(true);
  });

  it("uses the shared current clock for expiry dates in both headlines and row facts", () => {
    const fact = {
      kind: "monopoly_in_force" as const,
      commodity: "grain",
      region: "channel",
      expires_at: "2026-10-29T21:28:00Z",
    };
    state.data!.rivals[1].fact = fact;
    state.data!.verdicts.plays_rough.fact = fact;
    const future = plain(render());
    expect(
      future.match(/Monopoly on Grain in Channel until 29 Oct, 21:28 UTC/g),
    ).toHaveLength(2);
    state.now = Date.parse("2026-10-29T02:00:00Z");
    const today = plain(render());
    expect(
      today.match(/Monopoly on Grain in Channel until 21:28 UTC/g),
    ).toHaveLength(2);
    expect(today).not.toContain("until 29 Oct");
  });

  it("names a pending read without rendering a successful empty table", () => {
    state.data = undefined;
    state.isPending = true;
    const html = render();
    expect(html).toContain("Loading recorded rival observations");
    expect(html).toContain('role="status"');
    expect(html).not.toContain("<table");
    expect(html).not.toContain("No rival lost value");
  });

  it("names an initial failure and does not replace it with an empty result", () => {
    state.data = undefined;
    state.isError = true;
    const html = render();
    expect(html).toContain("Rival observations could not be loaded");
    expect(html).toContain("Service unavailable");
    expect(html).toContain('role="alert"');
    expect(html).not.toContain("<table");
    expect(html).not.toContain("Loading recorded rival observations");
    expect(html).not.toContain("Rival observations have not been recorded");
  });

  it("keeps retained rows and hunches visible on a failed refresh", () => {
    state.isError = true;
    const html = render();
    expect(html).toContain("Rival observations could not be updated");
    expect(html).toContain("Service unavailable");
    expect(html).toContain("The last recorded observations are retained below");
    expect(html).toContain("Kraken");
    expect(html).toContain("<table");
    expect(html).toContain("On 14 of our 16 lanes");
  });

  it("names missing standings without manufacturing a zero-valued table", () => {
    state.data!.valuation_at = null;
    state.data!.next_refresh_at = null;
    state.data!.our = null;
    state.data!.rivals = [];
    state.data!.verdicts = {
      most_dangerous: {
        company_name: null,
        fact: null,
        reason: "our_valuation_missing",
      },
      plays_rough: { company_name: null, fact: null, reason: "none_recorded" },
      not_profitable: {
        company_name: null,
        fact: null,
        reason: "none_recorded",
      },
    };
    const html = render();
    expect(html).toContain("Rival observations have not been recorded");
    expect(html).not.toContain("<table");
    expect(html).not.toContain("0.00 Cr");
    expect(html).not.toContain("No rival lost value in 24 h");
  });

  it("retains traffic-only companies when official standings are not recorded", () => {
    state.data!.valuation_at = null;
    state.data!.our = null;
    state.data!.rivals = [state.data!.rivals[4]];
    const html = render();
    expect(html).toContain("Rival observations have not been recorded");
    expect(html).toContain("TopStats AI Logistics");
    expect(html).toContain("2 ships seen in 24 h");
  });

  it("keys a company by its exact name across changed ranks and report times", () => {
    render();
    const first = tableState.props!;
    const key = first.rowKey(first.data[0], 0);
    state.data!.generated_at = "2026-10-27T02:00:30Z";
    state.data!.rivals[0] = { ...state.data!.rivals[0], rank: 7 };
    render();
    expect(key).toBe("Kraken");
    expect(tableState.props!.rowKey(tableState.props!.data[0], 4)).toBe(key);
  });

  it.each([
    [
      "Net worth",
      false,
      ["Negative", "Zero", "Nine", "Ten thousand", "Traffic only"],
    ],
    [
      "Net worth",
      true,
      ["Ten thousand", "Nine", "Zero", "Negative", "Traffic only"],
    ],
    [
      "24 h change",
      false,
      ["Nine", "Ten thousand", "Zero", "Negative", "Traffic only"],
    ],
    [
      "24 h change",
      true,
      ["Negative", "Zero", "Ten thousand", "Nine", "Traffic only"],
    ],
  ] as const)(
    "sorts the real DataTable %s column with descending=%s and unknowns last",
    (column, descending, expected) => {
      state.data!.rivals = [
        row({ company_name: "Nine", net_worth_pence: 900, change_pence: -100 }),
        row({
          company_name: "Traffic only",
          net_worth_pence: null,
          change_pence: null,
        }),
        row({
          company_name: "Ten thousand",
          net_worth_pence: 10000,
          change_pence: 0,
        }),
        row({
          company_name: "Negative",
          net_worth_pence: -100,
          change_pence: 10000,
        }),
        row({ company_name: "Zero", net_worth_pence: 0, change_pence: 900 }),
      ];
      const html = render([{ id: column, desc: descending }]);
      const body = html.match(/<tbody>(.*?)<\/tbody>/s)?.[1] ?? "";
      const names = [
        ...body.matchAll(/<tr><td[^>]*><strong>(.*?)<\/strong>/gs),
      ].map((match) => plain(match[1]));
      expect(names).toEqual(expected);
    },
  );

  it.each([false, true])(
    "sorts all nine hunches through the real DataTable with descending=%s",
    (descending) => {
      state.data!.rivals = [
        row({ company_name: "Copper Lantern", hunch: "steady" }),
        row({ company_name: "Blue Sail", hunch: "growing" }),
        row({ company_name: "Aster", hunch: "not_ranked" }),
        row({ company_name: "Salt Ledger", hunch: "plays_rough" }),
        row({ company_name: "New Quay", hunch: "too_new" }),
        row({ company_name: "Harbor Crown", hunch: "most_dangerous" }),
        row({ company_name: "Silent Hold", hunch: "unmeasured" }),
        row({ company_name: "Tidefall", hunch: "losing_value" }),
        row({ company_name: "Anchor Line", hunch: "threat" }),
      ];
      const attention = [
        "Harbor Crown",
        "Salt Ledger",
        "Anchor Line",
        "Tidefall",
        "Blue Sail",
        "Copper Lantern",
        "New Quay",
        "Silent Hold",
        "Aster",
      ];
      const html = render([{ id: "Hunch", desc: descending }]);
      const body = html.match(/<tbody>(.*?)<\/tbody>/s)?.[1] ?? "";
      const names = [
        ...body.matchAll(/<tr><td[^>]*><strong>(.*?)<\/strong>/gs),
      ].map((match) => plain(match[1]));
      expect(names).toEqual(descending ? [...attention].reverse() : attention);
    },
  );
});
