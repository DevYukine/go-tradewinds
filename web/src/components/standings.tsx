"use client";

import { Crown, Medal, Trophy } from "lucide-react";
import { useNow } from "@/app/providers";
import { dataset } from "@/lib/data";
import { count, dateTime, humanize, money } from "@/lib/format";
import type { Account, Leaderboard } from "@/lib/types";
import { StandingsHistoryView } from "./standings-history";
import { Badge, DataTable, Dataset, Section } from "./ui";
import styles from "./world-views.module.css";

export function StandingsView({ account }: { account: Account }) {
  const now = useNow();
  const leaderboard = dataset<Leaderboard>(account, "leaderboard");

  return (
    <Section
      title="Official player standings"
      icon={Trophy}
      actions={
        leaderboard ? (
          <Badge icon={Crown}>Season {leaderboard.season_number}</Badge>
        ) : undefined
      }
    >
      <StandingsHistoryView accountID={account.id} />
      <Dataset
        account={account}
        name="leaderboard"
        emptyTitle="Standings have not been recorded"
        emptyDescription="The official hourly leaderboard appears after the runner records it."
      >
        <dl className="voyage-facts">
          <div>
            <dt>Source refreshed</dt>
            <dd>{dateTime(leaderboard?.refreshed_at)}</dd>
          </div>
          <div>
            <dt>Next hourly refresh</dt>
            <dd>{dateTime(leaderboard?.next_refresh_at)}</dd>
          </div>
          <div>
            <dt>Ranked players</dt>
            <dd>{count(leaderboard?.ranked_players)}</dd>
          </div>
        </dl>
        <p className={styles.caption}>
          Hourly best-company valuation, not cash or realized profit. Do not add
          repeated player records across accounts.
          {leaderboard?.next_refresh_at &&
            Date.parse(leaderboard.next_refresh_at) <= now &&
            " A newer scheduled valuation has not been recorded."}
        </p>
        {leaderboard?.you && (
          <div className={styles.standing}>
            <Trophy size={22} strokeWidth={1.7} aria-hidden="true" />
            <div>
              <strong>
                Your player is ranked {count(leaderboard.you.rank)}.
              </strong>{" "}
              Best company: {leaderboard.you.company_name},{" "}
              {money(leaderboard.you.net_worth_pence)}.{" "}
              {leaderboard.you.is_calling_company
                ? "This is the selected company's official valuation."
                : "The ranked company is another company owned by this player, not the selected company."}
            </div>
          </div>
        )}
        <details className={styles.disclosure}>
          <summary>Latest full standings</summary>
          <DataTable
            label="Official hourly player standings"
            data={leaderboard?.entries ?? []}
            rowKey={(entry) => String(entry.rank)}
            empty="No ranked entries recorded"
            columns={[
              {
                label: "Rank",
                value: (entry) => entry.rank,
                render: (entry) => (
                  <span
                    className={`${styles.rank} ${entry.rank <= 3 ? styles.podium : ""}`}
                  >
                    {entry.rank === 1 ? (
                      <Trophy size={13} strokeWidth={1.7} aria-hidden="true" />
                    ) : entry.rank <= 3 ? (
                      <Medal size={13} strokeWidth={1.7} aria-hidden="true" />
                    ) : null}
                    {count(entry.rank)}
                  </span>
                ),
              },
              {
                label: "Best company",
                value: (entry) => entry.company_name,
                render: (entry) => entry.company_name,
              },
              {
                label: "Home port",
                value: (entry) => entry.home_port_id,
                render: (entry) => humanize(entry.home_port_id),
              },
              {
                label: "Official net worth",
                value: (entry) => entry.net_worth_pence,
                render: (entry) => money(entry.net_worth_pence),
                className: "numeric",
              },
              {
                label: "Titles",
                value: (entry) =>
                  entry.titles.length ? entry.titles.join(", ") : undefined,
                render: (entry) => entry.titles.join(", ") || "None recorded",
              },
            ]}
          />
        </details>
        <details className={styles.disclosure}>
          <summary>What net worth includes</summary>
          <p className={styles.caption}>
            Vaults, escrow, owned goods at catalog base prices and afloat hull
            build values are included in the official valuation.
          </p>
        </details>
      </Dataset>
    </Section>
  );
}
