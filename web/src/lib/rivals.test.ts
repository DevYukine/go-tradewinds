import { describe, expect, it } from "vitest";
import { rivalFactText } from "./rivals";
import type { RivalFact } from "./types";

describe("recorded rival wording", () => {
  it.each<RivalFact>([
    { kind: "shared_lanes" },
    { kind: "shared_lanes", count: 14 },
    { kind: "shared_ports", total: 9 },
    { kind: "monopolies_recent" },
    { kind: "loan_walkaways" },
    { kind: "loan_defaults" },
    { kind: "ships_seen" },
    { kind: "short_history" },
    { kind: "value_change", baseline_pence: 100 },
    { kind: "monopoly_in_force" },
  ])("keeps omitted Go omitempty fields unrecorded in %j", (fact) => {
    expect(rivalFactText(fact)).toBe("Not recorded");
  });

  it("retains measured zero without confusing it with missing evidence", () => {
    expect(rivalFactText({ kind: "shared_lanes", count: 0, total: 16 })).toBe(
      "On 0 of our 16 lanes",
    );
    expect(rivalFactText({ kind: "shared_ports", count: 0, total: 0 })).toBe(
      "At 0 of our 0 ports",
    );
    expect(rivalFactText({ kind: "ships_seen", count: 0 })).toBe(
      "0 ships seen in 24 h",
    );
    expect(rivalFactText({ kind: "short_history", hours: 0 })).toBe(
      "Only 0 h of valuations",
    );
  });

  it("signs positive changes and percentages consistently with the table", () => {
    expect(
      rivalFactText({
        kind: "value_change",
        change_pence: 1234,
        baseline_pence: 10000,
      }),
    ).toBe("+12.34 Cr in 24 h (+12.3%)");
    expect(rivalFactText({ kind: "value_change", change_pence: 100 })).toBe(
      "+1.00 Cr in 24 h",
    );
    expect(rivalFactText({ kind: "larger", net_worth_pence: 100 })).toBe(
      "Largest rival ahead of us",
    );
  });

  it("humanizes both single-word and underscored monopoly identifiers", () => {
    expect(
      rivalFactText({
        kind: "monopoly_in_force",
        commodity: "grain",
        region: "channel",
        expires_at: "2026-10-27T21:28:00Z",
      }),
    ).toBe("Monopoly on Grain in Channel until 21:28 UTC");
    expect(
      rivalFactText({
        kind: "monopoly_in_force",
        commodity: "raw_wool",
        region: "north_sea",
        expires_at: "2026-10-27T21:28:00Z",
      }),
    ).toBe("Monopoly on Raw wool in North sea until 21:28 UTC");
  });

  it("uses the caller's UTC day rather than the expiry's local date", () => {
    expect(
      rivalFactText(
        {
          kind: "monopoly_in_force",
          commodity: "grain",
          region: "channel",
          expires_at: "2026-10-28T01:28:00+04:00",
        },
        Date.parse("2026-10-27T02:00:00Z"),
      ),
    ).toBe("Monopoly on Grain in Channel until 21:28 UTC");
  });

  it("includes the expiry date beyond the caller's UTC day or before its clock is ready", () => {
    const fact: RivalFact = {
      kind: "monopoly_in_force",
      commodity: "grain",
      region: "channel",
      expires_at: "2026-10-28T21:28:00Z",
    };
    expect(rivalFactText(fact, Date.parse("2026-10-27T02:00:00Z"))).toBe(
      "Monopoly on Grain in Channel until 28 Oct, 21:28 UTC",
    );
    expect(rivalFactText(fact, 0)).toBe(
      "Monopoly on Grain in Channel until 28 Oct, 21:28 UTC",
    );
  });

  it("does not manufacture an expiry or monopoly scope from missing fields", () => {
    const fact = {
      kind: "monopoly_in_force" as const,
      commodity: "Raw wool",
      region: "Channel",
      expires_at: "2026-10-27T21:28:00Z",
    };
    expect(rivalFactText({ ...fact, expires_at: "invalid" })).toBe(
      "Not recorded",
    );
    expect(rivalFactText({ ...fact, commodity: "" })).toBe("Not recorded");
    expect(rivalFactText({ ...fact, region: "" })).toBe("Not recorded");
  });
});
