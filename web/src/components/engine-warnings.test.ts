import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type * as Data from "@/lib/data";
import type { EngineWarning, WarningReport } from "@/lib/types";
import { EngineWarnings } from "./engine-warnings";

const state = vi.hoisted(() => ({
  data: undefined as WarningReport | undefined,
  isError: false,
}));

vi.mock("@/lib/data", async (original) => ({
  ...(await original<typeof Data>()),
  useWarnings: () => state,
}));

const stalled: EngineWarning = {
  code: "controller_stalled",
  severity: "critical",
  since: "2026-10-27T02:00:00Z",
  facts: { minutes: 14 },
  names: {},
  reason: "",
};
const held: EngineWarning = {
  code: "hold_stuck",
  severity: "warning",
  since: "2026-10-27T00:09:00Z",
  facts: { minutes: 125, other_count: 0 },
  names: { ship: "Lapras", port: "Hamburg" },
  reason: "awaiting safe provisions",
};
const accounts = [
  {
    id: "one",
    label: "Yukine",
    enabled: true,
    company_id: "company-one",
  },
  {
    id: "two",
    label: "Second fleet",
    enabled: true,
    company_id: "company-two",
  },
];
const inactiveAccounts = [
  { ...accounts[0], enabled: false },
  { ...accounts[1], company_id: "" },
];

function fixture(): WarningReport {
  return {
    generated_at: "2026-10-27T02:14:00Z",
    build: {
      revision: "abc1234def5678",
      modified: false,
      started_at: "2026-10-27T01:58:00Z",
    },
    accounts: accounts.map((account) => ({
      account_id: account.id,
      label: account.label,
      available: true,
      error_code: "",
      suppressed: [],
      warnings: [],
    })),
    unmonitored: [],
  };
}

function render(options: Partial<Parameters<typeof EngineWarnings>[0]> = {}) {
  return renderToStaticMarkup(
    createElement(EngineWarnings, {
      accounts: [accounts[0]],
      accountsLoaded: true,
      ...options,
    }),
  );
}

function plain(html: string) {
  return html.replace(/<[^>]*>/g, "").replace(/\s+/g, " ");
}

describe("engine warning strip", () => {
  beforeEach(() => {
    state.data = fixture();
    state.isError = false;
  });

  it("keeps successful all-clear results invisible outside Overview", () => {
    const html = render();
    expect(plain(html)).toBe("");
    expect(html).toContain('role="status"');
    expect(html).not.toContain("<div");
  });

  it("shows a quiet recorded-data all-clear and short build identity on Overview", () => {
    const html = render({ overview: true });
    expect(plain(html)).toBe(
      "No engine warnings from recorded data, checked 02:14. Build abc1234 running since 01:58.",
    );
    expect(html).toContain('role="status"');
    expect(html).toMatch(/datetime="2026-10-27T02:14:00Z"/i);
    expect(html).toContain('title="27 Oct, 02:14 UTC"');
    expect(html).not.toContain("notice-danger");
  });

  it("identifies modified and unknown builds honestly", () => {
    state.data!.build.revision = null;
    state.data!.build.modified = true;
    expect(plain(render({ overview: true }))).toContain(
      "Build unknown revision (modified) running since 01:58.",
    );
  });

  it("renders reports from bot builds without an unmonitored field", () => {
    Reflect.deleteProperty(state.data!, "unmonitored");
    expect(plain(render({ overview: true }))).toContain(
      "No engine warnings from recorded data",
    );
    state.data!.accounts[0].warnings = [stalled];
    expect(plain(render())).toContain(
      "Controller has not completed a check for 14 min",
    );
  });

  it("does not manufacture an all-clear while data is pending or unavailable", () => {
    state.data = undefined;
    expect(plain(render())).toBe("");
    expect(render()).toContain('role="status"');
    expect(plain(render({ overview: true }))).toBe(
      "Engine warnings have not been assessed yet.",
    );
    state.isError = true;
    const html = render({ overview: true });
    expect(plain(html)).toBe("Engine warnings unavailable.");
    expect(plain(render())).toBe("Engine warnings unavailable.");
    expect(html).toContain('role="status"');
    expect(html).not.toContain("No engine warnings");
    expect(html).not.toContain("Build");
  });

  it("retains earlier warning lines on error with a stale-results note", () => {
    state.data!.accounts[0].warnings = [stalled];
    state.isError = true;
    const html = render();
    expect(html).toContain("Controller has not completed a check for 14 min");
    expect(plain(html)).toContain(
      "Engine warnings could not be updated. Last recorded results are retained.",
    );
    expect(html).not.toContain("No engine warnings");
  });

  it("retains the last checked all-clear on Overview and explicitly marks the failed refresh", () => {
    state.isError = true;
    const html = render({ overview: true });
    expect(html).toContain("No engine warnings from recorded data");
    expect(html).toContain("could not be updated");
    expect(html).toContain("Last recorded results are retained");
  });

  it("uses shape and screen-reader severity in addition to notice colors", () => {
    state.data!.accounts[0].warnings = [stalled, held];
    const html = render();
    expect(html).toContain("notice-danger");
    expect(html).toContain("lucide-octagon-alert");
    expect(html).toContain("lucide-triangle-alert");
    expect(html).toContain('class="sr-only">Critical: ');
    expect(html).toContain('class="sr-only">Warning: ');
    expect(html.match(/aria-hidden="true"/g)).toHaveLength(2);
    expect(html).toContain('aria-atomic="true"');
    expect(html).not.toContain('role="alert"');
  });

  it("scopes warnings by account and prefixes each line only for multi-account scope", () => {
    state.data!.accounts[0].warnings = [stalled];
    state.data!.accounts[1].warnings = [held];
    const single = plain(render());
    expect(single).toContain("Controller has not completed");
    expect(single).not.toContain("Yukine:");
    expect(single).not.toContain("Lapras");
    const multiple = plain(render({ accounts }));
    expect(multiple).toContain("Yukine: Controller has not completed");
    expect(multiple).toContain("Second fleet: Lapras idle at Hamburg");
  });

  it.each([false, true])(
    "names an empty scope only on Overview (overview=%s)",
    (overview) => {
      const html = render({ accounts: [], overview });
      if (overview) {
        expect(plain(html)).toContain(
          "Engine warnings not assessed: no accounts are in scope.",
        );
      } else {
        expect(plain(html)).toBe("");
        expect(html).not.toContain("<div");
      }
      expect(html).toContain('role="status"');
      expect(html).not.toContain("No engine warnings");
    },
  );

  it("does not name an empty scope before dashboard account data is available", () => {
    const html = render({
      accounts: [],
      accountsLoaded: false,
      overview: true,
    });
    expect(plain(html)).not.toContain("no accounts are in scope");
  });

  it.each([false, true])(
    "identifies missing scoped accounts without hiding other warnings (overview=%s)",
    (overview) => {
      state.data!.accounts = [state.data!.accounts[0]];
      state.data!.accounts[0].warnings = [stalled];
      const html = render({ accounts, overview });
      expect(plain(html)).toContain(
        "Yukine: Controller has not completed a check for 14 min",
      );
      expect(plain(html)).toContain(
        "Second fleet: Engine warnings unavailable. No warning evaluation was returned for this account.",
      );
      expect(html).not.toContain("No engine warnings");
    },
  );

  it.each([false, true])(
    "discloses unavailable account assessments without calling them clear (overview=%s)",
    (overview) => {
      state.data!.accounts[0].warnings = [stalled];
      state.data!.accounts[1].available = false;
      state.data!.accounts[1].error_code = "snapshot_unavailable";
      const html = render({ accounts, overview });
      expect(plain(html)).toContain(
        "Yukine: Controller has not completed a check for 14 min",
      );
      expect(plain(html)).toContain(
        "Second fleet: Engine warnings unavailable. Snapshot unavailable.",
      );
      expect(html).not.toContain("No engine warnings");
    },
  );

  it.each([false, true])(
    "shows suppression notes only on Overview without hiding active warnings (overview=%s)",
    (overview) => {
      state.data!.accounts[0].warnings = [stalled];
      state.data!.accounts[0].suppressed = ["fleet_idle", "hold_stuck"];
      const html = render({ accounts, overview });
      if (overview) {
        expect(plain(html)).toContain(
          "Yukine: Some engine warnings could not be evaluated from recorded data: Fleet idle, Hold stuck.",
        );
      } else {
        expect(html).not.toContain("could not be evaluated");
      }
      expect(plain(html)).toContain(
        "Yukine: Controller has not completed a check for 14 min",
      );
      expect(html).not.toContain("No engine warnings");
    },
  );

  it("keeps suppression-only reports visually quiet outside Overview", () => {
    state.data!.accounts[0].suppressed = ["fleet_idle", "hold_stuck"];
    const html = render();
    expect(plain(html)).toBe("");
    expect(html).toContain('role="status"');
    expect(html).not.toContain("<div");
    expect(plain(render({ overview: true }))).toContain(
      "Some engine warnings could not be evaluated",
    );
  });

  it.each([false, true])(
    "treats disabled and unregistered accounts as not monitored (overview=%s)",
    (overview) => {
      state.data!.accounts = [];
      const html = render({ accounts: inactiveAccounts, overview });
      if (overview) {
        expect(plain(html)).toContain("Yukine: not monitored.");
        expect(plain(html)).toContain("Second fleet: not monitored.");
      } else {
        expect(plain(html)).toBe("");
        expect(html).not.toContain("<div");
      }
      expect(html).not.toContain("unavailable");
      expect(html).not.toContain("No engine warnings");
    },
  );

  it.each([false, true])(
    "ignores recorded warnings from accounts no longer monitored (overview=%s)",
    (overview) => {
      state.data!.accounts[0].warnings = [stalled];
      state.data!.accounts[1].warnings = [held];
      const html = render({ accounts: inactiveAccounts, overview });
      expect(html).not.toContain("Controller has not completed");
      expect(html).not.toContain("Lapras idle");
      expect(html).not.toContain("notice-danger");
      if (!overview) expect(plain(html)).toBe("");
    },
  );

  it.each(["empty", "inactive"])(
    "keeps global API errors quiet without monitored accounts (%s scope)",
    (scope) => {
      state.isError = true;
      const scopedAccounts = scope === "empty" ? [] : inactiveAccounts;
      for (const report of [state.data, undefined]) {
        state.data = report;
        const html = render({ accounts: scopedAccounts });
        expect(plain(html)).toBe("");
        expect(html).toContain('role="status"');
        expect(html).not.toContain("<div");
        expect(html).not.toContain("unavailable");
      }
    },
  );

  it("allows all-clear for fully assessed monitored accounts while naming intentionally unmonitored accounts on Overview", () => {
    state.data!.accounts = [state.data!.accounts[0]];
    const scopedAccounts = [accounts[0], inactiveAccounts[1]];
    const html = render({ accounts: scopedAccounts, overview: true });
    expect(plain(html)).toContain("No engine warnings from recorded data");
    expect(plain(html)).toContain("Second fleet: not monitored.");
    expect(html).not.toContain("unavailable");
    expect(plain(render({ accounts: scopedAccounts }))).toBe("");
  });

  it("keeps ticking warning facts, check times and build details outside the always-mounted announcer", () => {
    state.data!.accounts[0].warnings = [stalled];
    const before = render({ overview: true });
    state.data!.accounts[0].warnings = [{ ...stalled, facts: { minutes: 15 } }];
    state.data!.generated_at = "2026-10-27T02:15:00Z";
    const after = render({ overview: true });
    for (const html of [before, after]) {
      const region = html.match(
        /<span\b[^>]*role="status"[^>]*>(.*?)<\/span>/s,
      );
      expect(region?.[1]).toBe("");
      expect(html.match(/role="status"/g)).toHaveLength(1);
      expect(html).toContain("Build abc1234 running since");
    }
    expect(before).toContain("check for 14 min");
    expect(after).toContain("check for 15 min");
  });

  it("escapes recorded reasons and account labels rather than rendering game HTML", () => {
    state.data!.accounts[0].label = "<b>Yukine</b>";
    state.data!.accounts[0].warnings = [
      { ...held, reason: "<script>unsafe()</script>" },
    ];
    const html = render({ accounts });
    expect(html).toContain("&lt;b&gt;Yukine&lt;/b&gt;");
    expect(html).toContain("&lt;script&gt;unsafe()&lt;/script&gt;");
    expect(html).not.toContain("<script>");
  });
});
