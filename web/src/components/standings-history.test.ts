import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type * as Data from "@/lib/data";
import { standingKey, standingsChart } from "@/lib/standings-history";
import type { StandingsHistory } from "@/lib/types";
import { HistoryRange } from "./history-range";
import { StandingsHistoryView } from "./standings-history";

const state = vi.hoisted(() => ({
  data: undefined as StandingsHistory | undefined,
  isError: false,
  isPending: false,
  error: new Error("Service unavailable"),
}));
vi.mock("@/lib/data", async (original) => ({
  ...(await original<typeof Data>()),
  useStandingsHistory: () => state,
}));
vi.mock("@/app/providers", () => ({
  useNow: () => Date.parse("2026-10-26T12:30:00Z"),
}));

function fixture(): StandingsHistory {
  return {
    account_id: "one",
    generated_at: "2026-10-26T12:30:00Z",
    from: "2026-10-25T12:30:00Z",
    to: "2026-10-26T12:30:00Z",
    latest_at: "2026-10-26T12:00:00Z",
    next_refresh_at: "2026-10-26T13:00:00Z",
    bucket_seconds: 300,
    top: 5,
    players: [100_000, -50_000, null].map((change, index) => ({
      season_number: 1,
      company_name: ["Leader", "Our fleet", "New player"][index],
      rank: index + 1,
      is_us: index === 1,
      charted: true,
      first_at: "2026-10-26T11:00:00Z",
      last_at: "2026-10-26T12:00:00Z",
      first_net_worth_pence: 200_000,
      last_net_worth_pence: 200_000 + (change ?? 0),
      change_pence: change,
      observations: change === null ? 1 : 2,
      points: [
        {
          at: "2026-10-26T12:00:00Z",
          net_worth_pence: 200_000 + (change ?? 0),
        },
      ],
    })),
  };
}

function render() {
  return renderToStaticMarkup(
    createElement(StandingsHistoryView, { accountID: "one" }),
  );
}

describe("recorded standings history", () => {
  beforeEach(() => {
    state.data = fixture();
    state.isError = false;
    state.isPending = false;
  });

  it("shows signed gains, declines and an unmeasured single observation", () => {
    const html = render();
    expect(html).toContain("+1,000.00 Cr");
    expect(html).toContain("-500.00 Cr");
    expect(html).toContain("Not measured");
    expect(html).toContain("Our fleet (you)");
    expect(html).toContain('role="img"');
    expect(html).toContain("without stable player IDs");
    expect(html).toContain("company names and seasons stay separate");
  });

  it("defaults to nearest rivals with accessible series toggle buttons", () => {
    const html = render();
    expect(html).toContain(
      '<option value="rivals" selected="">You and nearest rivals</option>',
    );
    const legend = html.match(
      /<ul[^>]*aria-label="Charted players"[^>]*>(.*?)<\/ul>/s,
    )?.[1];
    expect(legend).toBeDefined();
    expect(
      legend?.match(/<button type="button" aria-pressed="true"/g),
    ).toHaveLength(3);
  });

  it("selects only our latest company and the adjacent ranks from that snapshot", () => {
    const [above, own, below] = fixture().players;
    above.rank = 2;
    own.rank = 3;
    below.rank = 4;
    const players = [
      { ...above, company_name: "Distant leader", rank: 1 },
      above,
      own,
      below,
      { ...below, company_name: "Distant rival", rank: 5 },
      {
        ...own,
        company_name: "Our previous company",
        last_at: "2026-10-25T12:00:00Z",
      },
      {
        ...above,
        company_name: "Former second place",
        last_at: "2026-10-26T11:00:00Z",
      },
      { ...above, company_name: "Other season", season_number: 2 },
    ];
    expect(standingsChart(players, "rivals", new Set()).charted).toEqual([
      above,
      own,
      below,
    ]);
  });

  it("handles first and last ranks without substituting distant or missing rivals", () => {
    const players = fixture().players;
    players[0].is_us = true;
    players[1].is_us = false;
    expect(standingsChart(players, "rivals", new Set()).charted).toEqual(
      players.slice(0, 2),
    );
    players[0].is_us = false;
    players[2].is_us = true;
    expect(standingsChart(players, "rivals", new Set()).charted).toEqual(
      players.slice(1),
    );
    players[1].charted = false;
    expect(standingsChart(players, "rivals", new Set()).charted).toEqual([
      players[2],
    ]);
    players[2].is_us = false;
    expect(standingsChart(players, "rivals", new Set()).charted).toEqual([]);
  });

  it("removes hidden series from the plot and rescales only to visible values", () => {
    const players = fixture().players;
    players[0].points[0].net_worth_pence = 450_000_000;
    const all = standingsChart(players, "5", new Set());
    const hidden = new Set([standingKey(players[0])]);
    const focused = standingsChart(players, "5", hidden);
    expect(focused.charted).toEqual(all.charted);
    expect(focused.visible).toEqual(players.slice(1));
    expect(Math.max(...focused.ticks)).toBeLessThan(Math.max(...all.ticks));
    expect(Math.max(...focused.ticks)).toBeGreaterThanOrEqual(
      players[2].points[0].net_worth_pence,
    );
    hidden.delete(standingKey(players[0]));
    expect(standingsChart(players, "5", hidden)).toEqual(all);
  });

  it("keeps all legend entries and a valid axis when every line is hidden", () => {
    const players = fixture().players;
    const plot = standingsChart(
      players,
      "rivals",
      new Set(players.map(standingKey)),
    );
    expect(plot.charted).toEqual(players);
    expect(plot.visible).toEqual([]);
    expect(plot.ticks.length).toBeGreaterThan(1);
    expect(plot.ticks.every(Number.isFinite)).toBe(true);
    expect(plot.ticks[0]).toBeLessThan(plot.ticks.at(-1)!);
  });

  it("discloses stale sources and retains data on a failed refresh", () => {
    state.data!.next_refresh_at = "2026-10-26T12:00:00Z";
    state.isError = true;
    const html = render();
    expect(html).toContain("A newer scheduled valuation has not been recorded");
    expect(html).toContain("Retained data remains visible");
    expect(html).toContain("Our fleet (you)");
  });

  it("never renders an outage as successful empty history or zero change", () => {
    state.data = undefined;
    state.isError = true;
    const html = render();
    expect(html).toContain("Standings history unavailable");
    expect(html).toContain("not evidence of an empty range");
    expect(html).not.toContain("+0 Cr");
  });

  it("shares money-history preset semantics and accessible pressed state", () => {
    const html = renderToStaticMarkup(
      createElement(HistoryRange, {
        value: "7d",
        label: "History range",
        onChange: () => {},
      }),
    );
    expect(html).toContain('aria-label="History range"');
    expect(html).toContain('aria-pressed="true">7d</button>');
    expect(html.match(/<button/g)).toHaveLength(4);
    expect(html).not.toContain("Custom");
    const custom = renderToStaticMarkup(
      createElement(HistoryRange, {
        value: "custom",
        label: "Money range",
        allowCustom: true,
        onChange: () => {},
      }),
    );
    expect(custom).toContain('aria-pressed="true">Custom</button>');
  });
});
