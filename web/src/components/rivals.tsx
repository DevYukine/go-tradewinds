"use client";

import { useMemo } from "react";
import { useNow } from "@/app/providers";
import { useRivals } from "@/lib/data";
import { count, dateTime, humanize, money } from "@/lib/format";
import {
  hunchLabel,
  hunchRank,
  hunchTone,
  observedShipsText,
  rivalCreditFacts,
  rivalExpansionText,
  rivalFactText,
  rivalLoanDebt,
  rivalPercent,
} from "@/lib/rivals";
import type { RivalRow } from "@/lib/types";
import { Badge, DataTable, type Column } from "./ui";
import chartStyles from "./performance.module.css";
import styles from "./rivals.module.css";

const growthReasons: Record<string, string> = {
  no_publication: "No official standings publication recorded",
  no_baseline: "No matching publication within 60 min of the window start",
  our_valuation_missing: "Our valuation is missing at one or both publications",
  company_changed: "Our best company changed between publications",
  change_out_of_range: "The valuation change is outside the supported range",
};

export function RivalsView({ accountID }: { accountID: string }) {
  const query = useRivals(accountID);
  const now = useNow();
  const columns = useMemo<Column<RivalRow>[]>(
    () => [
      {
        label: "Company",
        value: (row) => row.company_name,
        render: (row) => (
          <>
            <strong>{row.company_name}</strong>
            <small className="cell-subtitle">
              {row.rank === null ? "Not ranked" : `Rank ${count(row.rank)}`}
            </small>
          </>
        ),
      },
      {
        label: "Hunch",
        value: (row) => hunchRank(row.hunch),
        filter: false,
        render: (row) => (
          <Badge tone={hunchTone(row.hunch)}>{hunchLabel(row.hunch)}</Badge>
        ),
      },
      {
        label: "Key fact",
        value: (row) => rivalFactText(row.fact, now),
        render: (row) => rivalFactText(row.fact, now),
      },
      {
        label: "Net worth",
        value: (row) => row.net_worth_pence ?? undefined,
        className: "numeric",
        render: (row) =>
          row.net_worth_pence == null
            ? "Not observed"
            : money(row.net_worth_pence),
      },
      {
        label: "24 h change",
        value: (row) => row.change_pence ?? undefined,
        className: "numeric",
        render: (row) => (
          <span
            className={
              row.change_pence == null
                ? undefined
                : row.change_pence < 0
                  ? chartStyles.loss
                  : row.change_pence > 0
                    ? chartStyles.gain
                    : undefined
            }
          >
            {row.change_pence != null && row.change_pence > 0 ? "+" : ""}
            {row.change_pence == null
              ? "Not observed"
              : money(row.change_pence)}
          </span>
        ),
      },
      {
        label: "Rising rank",
        value: (row) => row.rising_rank ?? undefined,
        className: "numeric",
        render: (row) =>
          row.rising_rank == null ? "Not observed" : count(row.rising_rank),
      },
      {
        label: "7 d growth",
        value: (row) => row.growth_bps ?? undefined,
        className: "numeric",
        render: (row) => rivalPercent(row.growth_bps),
      },
      {
        label: "7 d gained",
        value: (row) => row.gained_pence ?? undefined,
        className: "numeric",
        render: (row) =>
          row.gained_pence == null ? "Not observed" : money(row.gained_pence),
      },
      {
        label: "Loan debt",
        value: (row) => rivalLoanDebt(row.credit),
        className: "numeric",
        render: (row) =>
          row.credit == null
            ? "Not observed"
            : money(rivalLoanDebt(row.credit)),
      },
      {
        label: "Credit history",
        value: (row) =>
          row.credit == null
            ? "Not observed"
            : rivalCreditFacts(row.credit)
                .map(([label, value]) => `${label} ${value}`)
                .join("; "),
        render: (row) => (
          <details className={styles.details}>
            <summary>
              {row.credit == null ? "Not observed" : "Credit history"}
            </summary>
            {row.credit != null && (
              <>
                <p className={styles.caption}>
                  Credit observed:{" "}
                  {row.credit_observed_at
                    ? dateTime(row.credit_observed_at)
                    : "Not observed"}
                  . Lifetime defaults, walk-aways and write-offs may overlap;
                  they are not added to current loan debt.
                </p>
                <dl className={styles.facts}>
                  {rivalCreditFacts(row.credit).map(([label, value]) => (
                    <div key={label}>
                      <dt>{label}</dt>
                      <dd>{value}</dd>
                    </div>
                  ))}
                </dl>
              </>
            )}
          </details>
        ),
      },
      {
        label: "Observed ships",
        value: (row) =>
          `24 h: ${observedShipsText(row.observed_ships_24h)}; 7 d: ${observedShipsText(row.observed_ships_7d)}`,
        render: (row) => (
          <>
            <span>24 real h: {observedShipsText(row.observed_ships_24h)}</span>
            <small className="cell-subtitle">
              7 real d: {observedShipsText(row.observed_ships_7d)}
            </small>
          </>
        ),
      },
      {
        label: "Expansion contributions",
        value: (row) => rivalExpansionText(row.expansion_contributions),
        render: (row) => rivalExpansionText(row.expansion_contributions),
      },
    ],
    [now],
  );
  const report = query.data;
  const growth = report?.growth ?? [];
  const rising = report?.rising;
  const risingRows = useMemo(() => {
    const entries = rising?.entries ?? [];
    const you = rising?.you;
    return you &&
      !entries.some((entry) => entry.company_name === you.company_name)
      ? [...entries, you].sort((left, right) => left.rank - right.rank)
      : entries;
  }, [rising]);
  const ourChange = report?.our?.change_pence;
  const overdue =
    report?.next_refresh_at && now > Date.parse(report.next_refresh_at);
  const roughRow = report?.rivals.find(
    (row) => row.company_name === report?.verdicts.plays_rough.company_name,
  );
  const verdicts = report
    ? [
        {
          label: "Most dangerous",
          verdict: report.verdicts.most_dangerous,
          gap:
            report.verdicts.most_dangerous.reason === "our_valuation_missing"
              ? "Our valuation has not been recorded"
              : "No larger rival seen on our lanes",
        },
        {
          label: "Plays rough",
          verdict: report.verdicts.plays_rough,
          gap:
            report.verdicts.plays_rough.reason === "unlisted_only"
              ? "Rough-play evidence was recorded only for unlisted companies; marque sponsors are anonymous"
              : "No attributable rough play in 7 days; marque sponsors are anonymous",
        },
        {
          label: "Not making profit",
          verdict: report.verdicts.not_profitable,
          gap: !report.valuation_at
            ? "Rival valuations have not been recorded"
            : report.verdicts.not_profitable.reason === "no_baseline"
              ? "No valuation near 24 h ago"
              : "No rival lost value in 24 h",
        },
      ]
    : [];
  const coverageCaveats = report
    ? [
        !report.own_companies_complete
          ? "our configured company names are incomplete; the observed list may include our own companies and verdicts are withheld"
          : "",
        !report.overlap_known ? "lane and port overlap is unknown" : "",
        report.events_truncated
          ? "the event limit was reached, so observations are a lower bound"
          : "",
        report.observed_ships_7d_truncated
          ? "seven-day ship sightings have partial coverage because the event limit was reached; counts are a lower bound"
          : "",
        roughRow?.hunch === "most_dangerous"
          ? "the rough-play headline uses the strongest evidence; its Most dangerous chip takes precedence, while other Plays rough chips flag their own evidence"
          : "",
      ]
        .filter(Boolean)
        .join("; ")
    : "";

  return (
    <section className={styles.rivals} aria-label="Rivals">
      <h3>Rivals</h3>
      {query.isError && (
        <p className="notice notice-danger" role="alert">
          Rival observations could not be {report ? "updated" : "loaded"}.{" "}
          {query.error.message}
          {report && " The last recorded observations are retained below."}
        </p>
      )}
      {!report ? (
        !query.isError && (
          <p className="notice" role="status">
            {query.isPending
              ? "Loading recorded rival observations"
              : "Rival observations have not been recorded"}
          </p>
        )
      ) : (
        <>
          {!report.valuation_at && (
            <p className="notice" role="status">
              Rival observations have not been recorded. Official standings are
              unavailable; any recorded port notices remain below.
            </p>
          )}
          <table className={styles.growth}>
            <caption>
              <strong>Growth vs rivals</strong>
              <span className={styles.caption}>
                Official net-worth changes, not realized profit. Matched
                publications:
                {growth.map((window) => (
                  <span key={window.window_hours}>
                    {" "}
                    {count(window.window_hours)} h:{" "}
                    {window.from ? dateTime(window.from) : "Not measured"} to{" "}
                    {window.to ? dateTime(window.to) : "Not measured"}.
                  </span>
                ))}
              </span>
            </caption>
            <thead>
              <tr>
                <th scope="col">Window</th>
                <th scope="col">Our change</th>
                <th scope="col">Rival median (n rivals)</th>
                <th scope="col">Best rival</th>
                <th scope="col">Our place</th>
              </tr>
            </thead>
            <tbody>
              {growth.length === 0 && (
                <tr>
                  <td colSpan={5}>Growth comparisons are not available.</td>
                </tr>
              )}
              {growth.map((window) => {
                const ourReason =
                  growthReasons[window.ours_reason] ??
                  (window.ours_reason
                    ? humanize(window.ours_reason)
                    : "Our valuation is not recorded at both publications");
                const rivalReason =
                  window.from && window.to
                    ? "No rivals measured at both publications"
                    : "No matched publications for this window";
                return (
                  <tr key={window.window_hours}>
                    <th scope="row">{count(window.window_hours)} h</th>
                    <td>
                      {window.ours_pence === null ? (
                        <>
                          Not measured
                          <small>{ourReason}</small>
                        </>
                      ) : (
                        <span
                          className={
                            window.ours_pence < 0
                              ? chartStyles.loss
                              : window.ours_pence > 0
                                ? chartStyles.gain
                                : undefined
                          }
                        >
                          {window.ours_pence > 0 ? "+" : ""}
                          {money(window.ours_pence)}
                        </span>
                      )}
                    </td>
                    <td>
                      {window.rival_median_pence === null ? (
                        <>
                          Not measured
                          <small>{rivalReason}</small>
                        </>
                      ) : (
                        <>
                          {window.rival_median_pence > 0 ? "+" : ""}
                          {money(window.rival_median_pence)}
                        </>
                      )}
                      <small>
                        {count(window.rivals_measured)}{" "}
                        {window.rivals_measured === 1 ? "rival" : "rivals"}
                        {window.rivals_unmeasured > 0 &&
                          `; ${count(window.rivals_unmeasured)} not measured`}
                      </small>
                    </td>
                    <td>
                      {window.top_rival === null ||
                      window.top_rival_pence === null ? (
                        <>
                          Not measured
                          <small>{rivalReason}</small>
                        </>
                      ) : (
                        <>
                          <strong>{window.top_rival}</strong>
                          <small>
                            {window.top_rival_pence > 0 ? "+" : ""}
                            {money(window.top_rival_pence)}
                          </small>
                        </>
                      )}
                    </td>
                    <td>
                      {window.our_place === null || window.place_of === null ? (
                        <>
                          Not measured
                          <small>{ourReason}</small>
                        </>
                      ) : (
                        `${count(window.our_place)} of ${count(window.place_of)}`
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
          <table className={`${styles.growth} ${styles.rising}`}>
            <caption>
              <strong>Rising board</strong>
              <span className={styles.caption}>
                Publication:{" "}
                {report.rising_refreshed_at
                  ? dateTime(report.rising_refreshed_at)
                  : "Not observed"}
                {report.rising_stale !== false && " (stale)"}. Published growth
                over 7 real days, not realized profit. Window start:{" "}
                {rising?.window_start_at
                  ? dateTime(rising.window_start_at)
                  : "Not observed"}
                . Ranked players:{" "}
                {rising ? count(rising.ranked_players) : "Not observed"}.
              </span>
            </caption>
            <thead>
              <tr>
                <th scope="col">Rank</th>
                <th scope="col">Company</th>
                <th scope="col">Growth</th>
                <th scope="col">Gained value</th>
              </tr>
            </thead>
            <tbody>
              {risingRows.length === 0 && (
                <tr>
                  <td colSpan={4}>Rising board not yet published.</td>
                </tr>
              )}
              {risingRows.map((entry) => {
                const isUs = entry.company_name === rising?.you?.company_name;
                return (
                  <tr key={entry.company_name} data-is-us={isUs || undefined}>
                    <th scope="row">{count(entry.rank)}</th>
                    <td>
                      <strong>{entry.company_name}</strong>
                      {isUs && (
                        <small className="cell-subtitle">Our company</small>
                      )}
                    </td>
                    <td className="numeric">
                      {rivalPercent(entry.growth_bps)}
                    </td>
                    <td className="numeric">{money(entry.gained_pence)}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
          <dl className={styles.hunches}>
            {verdicts.map(({ label, verdict, gap }) => (
              <div key={label}>
                <dt>{label}</dt>
                <dd>
                  {verdict.company_name ? (
                    <>
                      <strong>{verdict.company_name}</strong>:{" "}
                      {rivalFactText(verdict.fact, now)}
                    </>
                  ) : verdict.reason === "own_companies_incomplete" ? (
                    "Verdict withheld until our company names are complete"
                  ) : (
                    gap
                  )}
                </dd>
              </div>
            ))}
          </dl>
          <p className={styles.caption}>
            Official valuation: {dateTime(report.valuation_at)}
            {overdue && " (overdue)"}. Evidence: last 24 real hours of port
            notices,{" "}
            {report.coverage === "partial" ? "partial" : "not observed"}; our 24
            h change: {ourChange != null && ourChange > 0 ? "+" : ""}
            {money(ourChange)}; hunches from recorded evidence, not certainties.{" "}
            Ship classes are observed sightings over 24 real h and 7 real d, not
            a fleet census. Expansion contributions are published tons and
            shares, not bonus income.
            {coverageCaveats && <> Coverage: {coverageCaveats}.</>}
          </p>
          {(report.valuation_at || report.rivals.length > 0) && (
            <DataTable
              label="Rival companies"
              data={report.rivals}
              rowKey={(row) => row.company_id ?? `name:${row.company_name}`}
              columns={columns}
              empty="No rival companies recorded in standings or port notices"
            />
          )}
        </>
      )}
    </section>
  );
}
