import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type * as Data from "@/lib/data";
import { EngineWarnings } from "@/components/engine-warnings";
import type { EngineWarning, WarningReport } from "./types";
import { warningText } from "./warnings";

const warningQuery = vi.hoisted(() => ({
  data: undefined as WarningReport | undefined,
  isError: false,
}));

vi.mock("@/lib/data", async (original) => ({
  ...(await original<typeof Data>()),
  useWarnings: () => warningQuery,
}));

const since = "2026-10-27T11:20:00Z";

const cases: [string, EngineWarning, string][] = [
  [
    "stalled controller with restart action",
    {
      code: "controller_stalled",
      severity: "critical",
      since,
      facts: { minutes: 14 },
      names: {},
      reason: "",
    },
    "Controller has not completed a check for 14 min. Check the log, fix, then run scripts/restart-bot.ps1.",
  ],
  [
    "fleet idle with longest ship and duration",
    {
      code: "fleet_idle",
      severity: "warning",
      since,
      facts: { idle: 3, total: 4, unknown: 0, minutes: 72 },
      names: {
        ship: "Lapras",
        ship_id: "lapras",
        port: "Hamburg",
        since_source: "berthed_at",
      },
      reason: "",
    },
    "3 of 4 ships idle in port 30+ min; longest Lapras, 1 h 12 min at Hamburg.",
  ],
  [
    "current ship hold",
    {
      code: "hold_stuck",
      severity: "warning",
      since,
      facts: { minutes: 125, other_count: 0 },
      names: {
        ship: "Lapras",
        ship_id: "lapras",
        port: "Hamburg",
        since_source: "berthed_at",
      },
      reason: "waiting for cargo",
    },
    "Lapras idle at Hamburg 2 h 5 min, held: waiting for cargo.",
  ],
  [
    "losses from integer pence",
    {
      code: "loss_streak",
      severity: "warning",
      since,
      facts: { count: 3, net_pence: -421055 },
      names: {},
      reason: "",
    },
    "Last 3 settlements lost 4,210.55 Cr since 11:20.",
  ],
];

describe("warningText acceptance", () => {
  it.each(cases)("formats %s", (_name, warning, expected) => {
    expect(warningText(warning)).toBe(expected);
  });

  it("prefixes each warning with the account label when several accounts are scoped", () => {
    for (const [, warning, expected] of cases) {
      expect(warningText(warning, "Yukine")).toBe(`Yukine: ${expected}`);
    }
  });

  it("formats a one-penny loss without a sign reversal or rounding to zero", () => {
    const warning: EngineWarning = {
      code: "loss_streak",
      severity: "warning",
      since: "2026-10-27T13:20:00+02:00",
      facts: { count: 3, net_pence: -1 },
      names: {},
      reason: "",
    };
    expect(warningText(warning)).toBe(
      "Last 3 settlements lost 0.01 Cr since 11:20.",
    );
  });

  it("renders refresh failures with their recorded datasets and error", () => {
    const text = warningText({
      code: "refresh_failing",
      severity: "critical",
      since,
      facts: { minutes: 14 },
      names: { datasets: "ships, clock" },
      reason: "request timed out",
    });
    expect(text).toContain("ships, clock");
    expect(text).toContain("14 min");
    expect(text).toContain("request timed out");
  });

  it("formats falling behind from structured pence rather than a preformatted reason", () => {
    const text = warningText({
      code: "falling_behind",
      severity: "warning",
      since,
      facts: { ours_pence: -1246694, median_pence: -350565, rival_count: 6 },
      names: {},
      reason: "",
    });
    expect(text).toContain("12,466.94 Cr");
    expect(text).toContain("3,505.65 Cr");
    expect(text).toContain("6 rivals");
    expect(text).toContain("24 h");
  });

  it.each([cases[1][1], cases[2][1]])(
    "discloses the recorded-arrival basis for $code instead of implying a berth clock",
    (warning) => {
      expect(
        warningText({
          ...warning,
          names: { ...warning.names, since_source: "arrival_event" },
        }),
      ).toContain("since recorded arrival");
    },
  );
});

describe("warning scope acceptance", () => {
  const scopedAccounts = [
    { id: "one", label: "Yukine", enabled: true, company_id: "company-one" },
    {
      id: "two",
      label: "Second fleet",
      enabled: true,
      company_id: "company-two",
    },
  ];

  beforeEach(() => {
    warningQuery.isError = false;
    warningQuery.data = {
      generated_at: "2026-10-27T02:14:00Z",
      unmonitored: [],
      build: {
        revision: "abc1234def5678",
        modified: false,
        started_at: "2026-10-27T01:58:00Z",
      },
      accounts: scopedAccounts.map((account) => ({
        account_id: account.id,
        label: account.label,
        available: true,
        error_code: "",
        suppressed: [],
        warnings: [],
      })),
    };
  });

  function render(
    overview: boolean,
    accounts = scopedAccounts,
    accountsLoaded = true,
  ) {
    return renderToStaticMarkup(
      createElement(EngineWarnings, {
        accounts,
        accountsLoaded,
        overview,
      }),
    )
      .replace(/<[^>]*>/g, "")
      .replace(/\s+/g, " ");
  }

  it.each([false, true])(
    "does not claim all-clear when a scoped account is absent (overview=%s)",
    (overview) => {
      warningQuery.data!.accounts = warningQuery.data!.accounts.slice(0, 1);
      const text = render(overview);
      expect(text).not.toBe("");
      expect(text).not.toContain("No engine warnings");
      expect(text).toContain("Second fleet");
      expect(text).toContain(
        "No warning evaluation was returned for this account",
      );
    },
  );

  it.each([false, true])(
    "keeps healthy warnings and discloses unavailable accounts (overview=%s)",
    (overview) => {
      warningQuery.data!.accounts[0].warnings = [cases[0][1]];
      warningQuery.data!.accounts[1].available = false;
      warningQuery.data!.accounts[1].error_code = "snapshot_invalid";
      const text = render(overview);
      expect(text).toContain(
        "Yukine: Controller has not completed a check for 14 min",
      );
      expect(text).toContain("Second fleet");
      expect(text.toLowerCase()).toContain("unavailable");
      expect(text).not.toContain("No engine warnings");
    },
  );

  it.each([false, true])(
    "shows suppressed assessment only on Overview (overview=%s)",
    (overview) => {
      warningQuery.data!.accounts[0].suppressed = ["fleet_idle", "hold_stuck"];
      const text = render(overview);
      if (overview) {
        expect(text).toContain("Yukine");
        expect(text).not.toContain("No engine warnings");
        expect(text).toContain("could not be evaluated");
        expect(text).toContain("Fleet idle");
        expect(text).toContain("Hold stuck");
      } else {
        expect(text).toBe("");
      }
    },
  );

  it.each([false, true])(
    "does not turn an unavailable account's empty warnings into all-clear (overview=%s)",
    (overview) => {
      warningQuery.data!.accounts[1].available = false;
      warningQuery.data!.accounts[1].error_code = "credentials_unavailable";
      const text = render(overview);
      expect(text).toContain("Second fleet");
      expect(text.toLowerCase()).toContain("unavailable");
      expect(text).not.toContain("No engine warnings");
    },
  );

  it.each([false, true])(
    "keeps an empty scope quiet outside Overview (overview=%s)",
    (overview) => {
      const text = render(overview, []);
      if (overview) {
        expect(text).toContain("no accounts are in scope");
        expect(text).not.toContain("No engine warnings");
      } else {
        expect(text).toBe("");
      }
    },
  );

  it.each([
    ["disabled", false, false, "company-two"],
    ["disabled", true, false, "company-two"],
    ["unregistered", false, true, ""],
    ["unregistered", true, true, ""],
  ] as const)(
    "does not treat a %s account as unavailable (overview=%s)",
    (_kind, overview, enabled, company_id) => {
      warningQuery.data!.accounts = warningQuery.data!.accounts.slice(0, 1);
      const inactive = { ...scopedAccounts[1], enabled, company_id };
      const text = render(overview, [scopedAccounts[0], inactive]);
      expect(text.toLowerCase()).not.toContain("unavailable");
      expect(text).not.toContain("No warning evaluation was returned");
      if (overview) {
        expect(text).toContain("Second fleet");
        expect(text.toLowerCase()).toContain("not monitored");
      } else {
        expect(text).toBe("");
      }
    },
  );

  it.each([
    ["disabled", false, "company-two"],
    ["unregistered", true, ""],
  ] as const)(
    "ignores stale warnings for a now %s account",
    (_kind, enabled, company_id) => {
      warningQuery.data!.accounts[1].warnings = [cases[0][1]];
      const inactive = { ...scopedAccounts[1], enabled, company_id };
      const text = render(false, [scopedAccounts[0], inactive]);
      expect(text).toBe("");
    },
  );

  it.each([
    { kind: "empty", accounts: [] },
    { kind: "disabled", accounts: [{ ...scopedAccounts[0], enabled: false }] },
    {
      kind: "unregistered",
      accounts: [{ ...scopedAccounts[0], company_id: "" }],
    },
  ])(
    "keeps global failure quiet outside Overview for $kind scope",
    ({ accounts }) => {
      warningQuery.data = undefined;
      warningQuery.isError = true;
      expect(render(false, accounts)).toBe("");
    },
  );

  it("keeps actual warnings outside Overview without adding suppression notes", () => {
    warningQuery.data!.accounts[0].warnings = [cases[0][1]];
    warningQuery.data!.accounts[1].suppressed = ["fleet_idle", "hold_stuck"];
    const text = render(false);
    expect(text).toContain(
      "Yukine: Controller has not completed a check for 14 min",
    );
    expect(text).not.toContain("could not be evaluated");
    expect(text).not.toContain("Fleet idle");
    expect(text).not.toContain("Hold stuck");
  });

  it.each([false, true])(
    "does not claim an empty or all-clear scope before accounts load (warnings query failed=%s)",
    (queryFailed) => {
      warningQuery.isError = queryFailed;
      for (const overview of [false, true]) {
        const text = render(overview, [], false);
        expect(text).not.toContain("no accounts are in scope");
        expect(text).not.toContain("No engine warnings");
      }
    },
  );

  it.each([false, true])(
    "treats an explicit no-API-key account as not monitored (overview=%s)",
    (overview) => {
      warningQuery.data!.accounts = [];
      warningQuery.data!.unmonitored = [
        { account_id: "one", reason: "no_api_key" },
      ];
      const text = render(overview, [scopedAccounts[0]]);
      expect(text.toLowerCase()).not.toContain("unavailable");
      expect(text).not.toContain("No warning evaluation was returned");
      expect(text).not.toContain("No engine warnings");
      if (overview) {
        expect(text).toContain("Yukine");
        expect(text.toLowerCase()).toContain("not monitored");
      } else {
        expect(text).toBe("");
      }
    },
  );
});
