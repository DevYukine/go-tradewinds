"use client";

import { useMemo } from "react";
import { useNow } from "@/app/providers";
import { useRivals } from "@/lib/data";
import { count, dateTime, money } from "@/lib/format";
import { hunchLabel, hunchRank, hunchTone, rivalFactText } from "@/lib/rivals";
import type { RivalRow } from "@/lib/types";
import { Badge, DataTable, type Column } from "./ui";
import chartStyles from "./performance.module.css";
import styles from "./rivals.module.css";

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
        render: (row) => money(row.net_worth_pence),
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
            {money(row.change_pence)}
          </span>
        ),
      },
    ],
    [now],
  );
  const report = query.data;
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
            {money(ourChange)}; hunches from recorded evidence, not certainties.
            {coverageCaveats && <> Coverage: {coverageCaveats}.</>}
          </p>
          {(report.valuation_at || report.rivals.length > 0) && (
            <DataTable
              label="Rival companies"
              data={report.rivals}
              rowKey={(row) => row.company_name}
              columns={columns}
              empty="No rival companies recorded in standings or port notices"
            />
          )}
        </>
      )}
    </section>
  );
}
