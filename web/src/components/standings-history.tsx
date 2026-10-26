"use client";

import { ChartNoAxesCombined } from "lucide-react";
import { useEffect, useId, useMemo, useRef, useState } from "react";
import { useNow } from "@/app/providers";
import { useStandingsHistory } from "@/lib/data";
import { count, dateTime, money } from "@/lib/format";
import type { MoneyRange } from "@/lib/money-history";
import {
  standingKey,
  standingsChart,
  type StandingSelection,
} from "@/lib/standings-history";
import { HistoryRange } from "./history-range";
import { DataTable, EmptyState } from "./ui";
import chartStyles from "./performance.module.css";
import styles from "./standings-history.module.css";

export function StandingsHistoryView({ accountID }: { accountID: string }) {
  const [range, setRange] = useState<Exclude<MoneyRange, "custom">>("24h");
  const [selection, setSelection] = useState<StandingSelection>("rivals");
  const [hidden, setHidden] = useState<ReadonlySet<string>>(() => new Set());
  const history = useStandingsHistory(
    accountID,
    range,
    selection === "rivals" ? 20 : Number(selection),
  );
  const now = useNow();
  const descriptionID = useId();
  const plot = useRef<HTMLDivElement>(null);
  const [width, setWidth] = useState(800);
  const players = history.data?.players;
  const { charted, visible, ticks } = useMemo(
    () => standingsChart(players ?? [], selection, hidden),
    [players, selection, hidden],
  );

  useEffect(() => {
    const element = plot.current;
    if (!element) return;
    const observer = new ResizeObserver(([entry]) => {
      if (entry.contentRect.width > 0) {
        setWidth(Math.round(entry.contentRect.width));
      }
    });
    observer.observe(element);
    return () => observer.disconnect();
  }, []);

  const height = 250;
  const padding = { left: 90, top: 16, right: 14, bottom: 34 };
  const plotWidth = Math.max(1, width - padding.left - padding.right);
  const plotHeight = height - padding.top - padding.bottom;
  const start = history.data ? Date.parse(history.data.from) : 0;
  const end = history.data ? Date.parse(history.data.to) : 1;
  const x = (at: string) =>
    padding.left +
    ((Date.parse(at) - start) / Math.max(1, end - start)) * plotWidth;
  const minimum = ticks[0];
  const maximum = ticks[ticks.length - 1];
  const y = (value: number) =>
    padding.top + (1 - (value - minimum) / (maximum - minimum)) * plotHeight;
  const stale =
    history.data?.latest_at &&
    (history.data.next_refresh_at
      ? now > Date.parse(history.data.next_refresh_at)
      : now - Date.parse(history.data.latest_at) > 3_600_000);

  return (
    <div className={styles.history}>
      <h3>Net worth over time</h3>
      <div className={chartStyles.controls}>
        <label className={styles.top}>
          Chart players
          <select
            value={selection}
            onChange={(event) =>
              setSelection(event.target.value as StandingSelection)
            }
          >
            <option value="rivals">You and nearest rivals</option>
            {[5, 10, 20].map((value) => (
              <option key={value} value={value}>
                Top {value} + your player
              </option>
            ))}
          </select>
        </label>
        <HistoryRange
          value={range}
          label="Standings history date range"
          onChange={(value) => {
            if (value !== "custom") setRange(value);
          }}
        />
      </div>
      <p id={descriptionID} className={chartStyles.description}>
        Hourly official valuations, not profit; toggle players to rescale the
        chart.
      </p>
      {history.isError && (
        <p className={chartStyles.warning} role="alert">
          Standings history unavailable: {history.error.message}{" "}
          {history.data
            ? "Retained data remains visible."
            : "This is not evidence of an empty range."}
        </p>
      )}
      {stale && (
        <p className={chartStyles.warning} role="status">
          A newer scheduled valuation has not been recorded. Latest source
          refresh: {dateTime(history.data?.latest_at)}.
        </p>
      )}
      <div ref={plot} className={chartStyles.chart}>
        {charted.length === 0 ? (
          <EmptyState
            icon={ChartNoAxesCombined}
            title={
              history.isPending
                ? "Retrieving standings history"
                : history.isError
                  ? "Standings history unavailable"
                  : selection === "rivals" && players?.length
                    ? "Your player has no recorded valuation in this range"
                    : "No recorded valuations in this range"
            }
            description="History starts with retained official observations. Missing valuations are not reconstructed or treated as zero."
          />
        ) : (
          <>
            <ul className={styles.legend} aria-label="Charted players">
              {charted.map((player, index) => (
                <li key={`${player.season_number}:${player.company_name}`}>
                  <button
                    type="button"
                    aria-pressed={!hidden.has(standingKey(player))}
                    onClick={() =>
                      setHidden((current) => {
                        const next = new Set(current);
                        const key = standingKey(player);
                        if (next.has(key)) next.delete(key);
                        else next.add(key);
                        return next;
                      })
                    }
                  >
                    <span
                      className={
                        player.is_us
                          ? styles.ours
                          : styles[`series${index % 5}`]
                      }
                      aria-hidden="true"
                    >
                      ━
                    </span>
                    <span
                      className={player.is_us ? styles.ownLabel : undefined}
                    >
                      {player.company_name}
                      {player.is_us ? " (you)" : ""}
                      {` · S${player.season_number}`}
                    </span>
                  </button>
                </li>
              ))}
            </ul>
            {visible.length === 0 && (
              <p className={chartStyles.coverage} role="status">
                All players are hidden; select a legend button to show a line.
              </p>
            )}
            <svg
              viewBox={`0 0 ${width} ${height}`}
              role="img"
              aria-label="Official player net worth over time. Your player has a heavier solid line. Exact first and last values and signed changes are in the table below."
              aria-describedby={descriptionID}
            >
              {ticks.map((tick) => (
                <g key={tick}>
                  <line
                    x1={padding.left}
                    y1={y(tick)}
                    x2={width - padding.right}
                    y2={y(tick)}
                    className={chartStyles.gridline}
                  />
                  <text
                    x={padding.left - 10}
                    y={y(tick) + 4}
                    textAnchor="end"
                    className={chartStyles.axis}
                  >
                    {count(tick / 100)} Cr
                  </text>
                </g>
              ))}
              {charted.map((player, index) =>
                hidden.has(standingKey(player)) ? null : (
                  <g
                    key={`${player.season_number}:${player.company_name}`}
                    className={
                      player.is_us ? styles.ours : styles[`series${index % 5}`]
                    }
                  >
                    <path
                      d={player.points
                        .map(
                          (point, index) =>
                            `${index === 0 ? "M" : "L"}${x(point.at)},${y(point.net_worth_pence)}`,
                        )
                        .join(" ")}
                      className={player.is_us ? styles.ownLine : styles.line}
                      strokeDasharray={
                        player.is_us ? undefined : `${4 + (index % 3) * 3} 3`
                      }
                    />
                    {player.points.map((point) => (
                      <circle
                        key={point.at}
                        cx={x(point.at)}
                        cy={y(point.net_worth_pence)}
                        r={player.is_us ? 3 : 1.8}
                        className={styles.dot}
                      >
                        <title>
                          {player.company_name}, {dateTime(point.at)}:{" "}
                          {money(point.net_worth_pence)}
                        </title>
                      </circle>
                    ))}
                  </g>
                ),
              )}
              <text
                x={padding.left}
                y={height - 8}
                className={chartStyles.axis}
              >
                {dateTime(history.data?.from)}
              </text>
              <text
                x={width - padding.right}
                y={height - 8}
                textAnchor="end"
                className={chartStyles.axis}
              >
                {dateTime(history.data?.to)}
              </text>
            </svg>
          </>
        )}
      </div>
      {players && players.length > 0 && (
        <>
          <p className={chartStyles.coverage}>
            Change is last minus first in range; without stable player IDs,
            company names and seasons stay separate.
          </p>
          <DataTable
            label="Standings net worth changes"
            data={players}
            rowKey={(player) =>
              `${player.season_number}:${player.company_name}`
            }
            columns={[
              {
                label: "Player's best company",
                value: (player) => player.company_name,
                render: (player) => (
                  <span className={player.is_us ? styles.ownLabel : undefined}>
                    {player.company_name}
                    {player.is_us ? " (you)" : ""}
                    <small className={styles.season}>
                      Season {player.season_number} · last rank{" "}
                      {count(player.rank)}
                    </small>
                  </span>
                ),
              },
              {
                label: "Change",
                value: (player) => player.change_pence ?? undefined,
                className: "numeric",
                render: (player) =>
                  player.change_pence === null ? (
                    <span title="At least two observations are needed">
                      Not measured
                    </span>
                  ) : (
                    <span
                      className={
                        player.change_pence < 0
                          ? chartStyles.loss
                          : player.change_pence > 0
                            ? chartStyles.gain
                            : undefined
                      }
                    >
                      {player.change_pence > 0 ? "+" : ""}
                      {money(player.change_pence)}
                    </span>
                  ),
              },
              {
                label: "First observed",
                value: (player) => player.first_net_worth_pence,
                className: "numeric",
                render: (player) => (
                  <span title={dateTime(player.first_at)}>
                    {money(player.first_net_worth_pence)}
                    <small className={styles.season}>
                      {dateTime(player.first_at)}
                    </small>
                  </span>
                ),
              },
              {
                label: "Last observed",
                value: (player) => player.last_net_worth_pence,
                className: "numeric",
                render: (player) => (
                  <span title={dateTime(player.last_at)}>
                    {money(player.last_net_worth_pence)}
                    <small className={styles.season}>
                      {dateTime(player.last_at)}
                    </small>
                  </span>
                ),
              },
              {
                label: "Valuations",
                value: (player) => player.observations,
                className: "numeric",
                render: (player) => count(player.observations),
              },
            ]}
          />
        </>
      )}
    </div>
  );
}
