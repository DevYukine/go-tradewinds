import { describe, expect, it } from "vitest";
import type { EngineWarning } from "./types";
import { warningText } from "./warnings";

// Warning wire contract shared with store tests: names.basis is "rising" or
// "valuation_24h"; growth_bps facts are basis points, not Crown amounts.
function warning(basis: string): EngineWarning {
  const facts = {
    ours_growth_bps: 50,
    median_growth_bps: 200,
    ranked_players: 4,
    ours_pence: -12345,
    median_pence: 67890,
    rival_count: 3,
  };
  return {
    code: "falling_behind",
    severity: "warning",
    since: "2026-10-21T12:00:00Z",
    reason: "",
    names: { basis },
    facts,
  };
}

describe("rivals overview benchmark basis", () => {
  it("labels the published rising comparison in percentage units, not cash", () => {
    const text = warningText(warning("rising"), "Our fleet");
    expect(text).toMatch(/Our fleet:/);
    expect(text).toMatch(/rising/i);
    expect(text).toContain("0.50%");
    expect(text).toContain("2.00%");
    expect(text).toMatch(/7 (?:real )?days|seven (?:real )?days/i);
    expect(text).not.toContain("Cr");
    expect(text).not.toMatch(/24 h/);
  });

  it("labels the 24 h valuation fallback instead of describing it as rising", () => {
    const text = warningText(warning("valuation_24h"));
    expect(text).toMatch(/24 h.*valu(?:e|ation)/i);
    expect(text).toContain("-123.45 Cr");
    expect(text).toContain("678.90 Cr");
    expect(text).toMatch(/3 rivals/);
    expect(text).not.toMatch(/rising|0\.50%/i);
  });
});
