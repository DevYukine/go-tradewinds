import { useQuery } from "@tanstack/react-query";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it, vi } from "vitest";
import { useWarnings } from "./data";
import { warningText, warningTime } from "./warnings";

vi.mock("@tanstack/react-query", () => ({ useQuery: vi.fn() }));

const common = {
  severity: "warning" as const,
  since: "2026-10-27T11:20:00Z",
  names: {},
  reason: "",
};

function WarningQuery() {
  useWarnings();
  return null;
}

describe("warning presentation", () => {
  it("keeps unknown idle times separate from measured idle ships", () => {
    expect(
      warningText({
        ...common,
        code: "fleet_idle",
        facts: { idle: 2, total: 4, unknown: 1, minutes: 45 },
        names: { ship: "Lapras", port: "Hamburg" },
      }),
    ).toBe(
      "2 of 4 ships idle in port 30+ min; longest Lapras, 45 min at Hamburg. 1 ship has unknown idle time.",
    );
  });

  it("reports other held ships without losing the longest ship or its reason", () => {
    expect(
      warningText({
        ...common,
        code: "hold_stuck",
        facts: { minutes: 125, other_count: 2 },
        names: { ship: "Lapras", port: "Hamburg" },
        reason: "waiting for safe provisions",
      }),
    ).toBe(
      "Lapras idle at Hamburg 2 h 5 min, held: waiting for safe provisions. 2 other ships also held.",
    );
  });

  it.each(["arrival_event", "berthed_at", "dock_queued_at"])(
    "qualifies idle and hold durations only for recorded arrival receipt time (%s)",
    (since_source) => {
      const names = { ship: "Lapras", port: "Hamburg", since_source };
      const idle = warningText({
        ...common,
        code: "fleet_idle",
        facts: { idle: 3, total: 4, unknown: 0, minutes: 72 },
        names,
      });
      const held = warningText({
        ...common,
        code: "hold_stuck",
        facts: { minutes: 125, other_count: 0 },
        names,
        reason: "waiting for cargo",
      });
      for (const text of [idle, held]) {
        if (since_source === "arrival_event") {
          expect(text).toContain("since recorded arrival");
        } else {
          expect(text).not.toContain("since recorded arrival");
        }
      }
    },
  );

  it("includes the affected refresh datasets and recorded first error", () => {
    const text = warningText({
      ...common,
      code: "refresh_failing",
      facts: { minutes: 12 },
      names: { datasets: "ships,clock" },
      reason: "upstream returned HTTP 503",
    });
    expect(text).toContain("12 min (ships,clock)");
    expect(text).toContain("upstream returned HTTP 503");
    expect(text).toContain(
      "Check the log, fix the refresh failure, then run scripts/restart-bot.ps1.",
    );
  });

  it.each(["", "   "])(
    "keeps empty reasons out of refresh and hold messages (%j)",
    (reason) => {
      expect(
        warningText({
          ...common,
          code: "refresh_failing",
          facts: { minutes: 12 },
          names: { datasets: "ships" },
          reason,
        }),
      ).toBe(
        "Recorded data refresh has failed for 12 min (ships). Check the log, fix the refresh failure, then run scripts/restart-bot.ps1.",
      );
      expect(
        warningText({
          ...common,
          code: "hold_stuck",
          facts: { minutes: 125, other_count: 0 },
          names: { ship: "Lapras", port: "Hamburg" },
          reason,
        }),
      ).toBe("Lapras idle at Hamburg 2 h 5 min, held.");
    },
  );

  it("formats signed valuation changes from pence without implying rival profit", () => {
    expect(
      warningText({
        ...common,
        code: "falling_behind",
        facts: {
          ours_pence: -1246694,
          median_pence: 3505653,
          rival_count: 6,
        },
      }),
    ).toBe(
      "Our 24 h value change is -12,466.94 Cr; rival median 35,056.53 Cr (6 rivals).",
    );
  });

  it("uses UTC for settlement times and keeps unavailable times explicit", () => {
    expect(warningTime("2026-10-27T13:20:00+02:00")).toBe("11:20");
    expect(warningTime("not a timestamp")).toBe("Not recorded");
  });

  it("shares a global warnings query that polls only visible tabs every 30 seconds", () => {
    renderToStaticMarkup(createElement(WarningQuery));
    expect(vi.mocked(useQuery)).toHaveBeenLastCalledWith(
      expect.objectContaining({
        queryKey: ["warnings"],
        refetchInterval: 30_000,
        refetchIntervalInBackground: false,
        retry: 1,
      }),
    );
  });
});
