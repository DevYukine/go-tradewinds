// Assumed API: hunchLabel(hunch: RivalHunch | null | undefined): string;
// hunchTone(hunch: RivalHunch | null | undefined): Badge tone;
// hunchRank(hunch: RivalHunch | null | undefined): number (lower is higher attention);
// rivalFactText(fact: RivalFact | null | undefined): string.
// RivalHunch is the nine snake_case literals below. RivalFact is a flat
// kind-tagged union with the relevant count/total, integer-pence, hours, or
// commodity/region/expires_at fields used in these cases.
import { describe, expect, it } from "vitest";
import { hunchLabel, hunchRank, hunchTone, rivalFactText } from "./rivals";
import type { RivalFact, RivalHunch } from "./types";

const hunches: [RivalHunch, string][] = [
  ["most_dangerous", "Most dangerous"],
  ["plays_rough", "Plays rough"],
  ["threat", "Threat"],
  ["losing_value", "Losing value"],
  ["growing", "Growing"],
  ["steady", "Steady"],
  ["too_new", "Too new to judge"],
  ["unmeasured", "Not measured"],
  ["not_ranked", "Not ranked"],
];

describe("rival hunch chips", () => {
  it.each(hunches)(
    "labels %s without strengthening the hunch",
    (hunch, label) => {
      expect(hunchLabel(hunch)).toBe(label);
    },
  );

  it("uses danger only for most dangerous and warning only for plays rough", () => {
    for (const [hunch] of hunches) {
      const tone = hunchTone(hunch);
      if (hunch === "most_dangerous") expect(tone).toBe("danger");
      else if (hunch === "plays_rough") expect(tone).toBe("warning");
      else {
        expect(tone).not.toBe("danger");
        expect(tone).not.toBe("warning");
      }
    }
  });

  it("sorts all nine chips in the disclosed precedence, not alphabetically", () => {
    const reversed = hunches.map(([hunch]) => hunch).reverse();
    expect(reversed.sort((a, b) => hunchRank(a) - hunchRank(b))).toEqual(
      hunches.map(([hunch]) => hunch),
    );
    expect(new Set(hunches.map(([hunch]) => hunchRank(hunch))).size).toBe(9);
  });

  it.each([null, undefined])(
    "keeps absent chip evidence unrecorded (%s)",
    (value) => {
      expect(hunchLabel(value)).toBe("Not recorded");
      expect(hunchTone(value)).toBe("neutral");
      expect(hunchRank(value)).toBeGreaterThan(hunchRank("not_ranked"));
    },
  );
});

describe("rival structured facts", () => {
  const cases: [string, RivalFact, string][] = [
    [
      "reported lanes",
      { kind: "shared_lanes", count: 14, total: 16 },
      "On 14 of our 16 lanes",
    ],
    [
      "direct ports",
      { kind: "shared_ports", count: 5, total: 9 },
      "At 5 of our 9 ports",
    ],
    [
      "size ratio",
      {
        kind: "larger",
        net_worth_pence: 436501259,
        our_net_worth_pence: 11613290,
      },
      "37.6× our net worth",
    ],
    [
      "current monopoly",
      {
        kind: "monopoly_in_force",
        commodity: "Raw wool",
        region: "Channel",
        expires_at: "2026-10-27T23:28:00+02:00",
      },
      "Monopoly on Raw wool in Channel until 21:28 UTC",
    ],
    [
      "retained grants",
      { kind: "monopolies_recent", count: 4 },
      "4 monopolies granted in 7 days",
    ],
    [
      "walkaways",
      { kind: "loan_walkaways", count: 2 },
      "Walked away from 2 loans",
    ],
    ["one default", { kind: "loan_defaults", count: 1 }, "Defaulted on 1 loan"],
    [
      "one walkaway",
      { kind: "loan_walkaways", count: 1 },
      "Walked away from 1 loan",
    ],
    [
      "multiple defaults",
      { kind: "loan_defaults", count: 2 },
      "Defaulted on 2 loans",
    ],
    [
      "one grant",
      { kind: "monopolies_recent", count: 1 },
      "1 monopoly granted in 7 days",
    ],
    [
      "24-hour decline",
      { kind: "value_change", change_pence: -2793384, baseline_pence: 2887449 },
      "-27,933.84 Cr in 24 h (-96.7%)",
    ],
    ["negative worth", { kind: "negative_worth" }, "Net worth below zero"],
    [
      "short history",
      { kind: "short_history", hours: 5 },
      "Only 5 h of valuations",
    ],
    ["missing baseline", { kind: "no_baseline" }, "No valuation near 24 h ago"],
    ["seen ships", { kind: "ships_seen", count: 2 }, "2 ships seen in 24 h"],
    ["one seen ship", { kind: "ships_seen", count: 1 }, "1 ship seen in 24 h"],
  ];

  it.each(cases)("formats %s from recorded fields", (_name, fact, text) => {
    expect(rivalFactText(fact)).toBe(text);
  });

  it.each([
    [0, 11613290],
    [-1, 11613290],
    [436501259, 0],
    [436501259, -1],
    [null, 11613290],
    [436501259, null],
  ])("suppresses an invalid financial ratio (%s / %s)", (rival, ours) => {
    expect(
      rivalFactText({
        kind: "larger",
        net_worth_pence: rival,
        our_net_worth_pence: ours,
      }),
    ).toBe("Largest rival ahead of us");
  });

  it.each([0, -100, null])(
    "does not calculate a percentage from baseline %s",
    (baseline) => {
      expect(
        rivalFactText({
          kind: "value_change",
          change_pence: -100,
          baseline_pence: baseline,
        }),
      ).toBe("-1.00 Cr in 24 h");
    },
  );

  it("keeps measured zero distinct from absent change", () => {
    expect(
      rivalFactText({
        kind: "value_change",
        change_pence: 0,
        baseline_pence: 100,
      }),
    ).toBe("0.00 Cr in 24 h (0.0%)");
    expect(
      rivalFactText({
        kind: "value_change",
        change_pence: null,
        baseline_pence: 100,
      }),
    ).toBe("Not recorded");
  });

  it.each([null, undefined])(
    "never turns an absent fact into zero or a clean record (%s)",
    (fact) => {
      expect(rivalFactText(fact)).toBe("Not recorded");
    },
  );

  it("keeps missing overlap counts unrecorded rather than inventing zero lanes", () => {
    expect(
      rivalFactText({ kind: "shared_lanes", count: null, total: 16 }),
    ).toBe("Not recorded");
    expect(rivalFactText({ kind: "shared_ports", count: 5, total: null })).toBe(
      "Not recorded",
    );
  });
});
