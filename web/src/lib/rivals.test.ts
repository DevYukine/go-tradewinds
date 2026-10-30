import { describe, expect, it } from "vitest";
import {
  observedShipsText,
  rivalCreditFacts,
  rivalExpansionText,
  rivalFactText,
  rivalLoanDebt,
  rivalPercent,
} from "./rivals";
import type { RivalCredit, RivalFact } from "./types";

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

describe("rival overview facts", () => {
  const credit: RivalCredit = {
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
    last_default_at: "2026-10-20T01:28:00Z",
    bankrupt_at: null,
    last_conviction_at: null,
  };

  it.each([
    [1250, "12.50%"],
    [50, "0.50%"],
    [-50, "-0.50%"],
    [0, "0.00%"],
    [null, "Not observed"],
    [undefined, "Not observed"],
  ])("formats %s growth basis points as %s", (bps, expected) => {
    expect(rivalPercent(bps)).toBe(expected);
  });

  it("adds only active and open-default loan debt, not overlapping lifetime losses", () => {
    expect(rivalLoanDebt(credit)).toBe(1301);
    expect(rivalLoanDebt(null)).toBeUndefined();
    expect(
      rivalLoanDebt({
        ...credit,
        active_owed_pence: 0,
        defaulted_owed_pence: 0,
      }),
    ).toBe(0);
  });

  it("retains every published credit counter and the observed timestamps", () => {
    const facts = Object.fromEntries(rivalCreditFacts(credit));
    expect(facts["Active loan debt"]).toBe("10.01 Cr");
    expect(facts["Defaulted loan debt"]).toBe("3.00 Cr");
    expect(facts.Defaults).toBe("4");
    expect(facts["Write-offs"]).toBe("2; 7.00 Cr lifetime");
    expect(facts["Walk-aways"]).toBe("3; 5.00 Cr lifetime");
    expect(facts["Open mortgages"]).toBe("1");
    expect(facts["Open futures claims"]).toBe("0.41 Cr");
    expect(facts["Futures claims"]).toBe("5 total; 2 written off");
    expect(facts["Outstanding lending"]).toBe("90.09 Cr");
    expect(facts["Smuggling convictions"]).toBe("6");
    expect(facts["Last default"]).toBe("20 Oct, 01:28 UTC");
    expect(facts.Bankruptcy).toBe("Not observed");
    expect(facts["Last conviction"]).toBe("Not observed");
  });

  it("labels class observations without inferring owned fleet size", () => {
    expect(observedShipsText({ hoy: 2, cutter: 1 })).toBe("Cutter 1; Hoy 2");
    expect(observedShipsText({})).toBe("Not observed");
  });

  it("shows published expansion tons and shares without treating a bonus as income", () => {
    expect(
      rivalExpansionText([{ target_phase: 3, tons: 123, share_bps: 4567 }]),
    ).toBe("Phase 3: 123 t (45.67% share)");
    expect(rivalExpansionText([])).toBe("Not observed");
  });
});
