"use client";

import { ChartNoAxesCombined, ChevronLeft, ChevronRight } from "lucide-react";
import { useEffect, useId, useMemo, useRef, useState } from "react";
import { useNow } from "@/app/providers";
import { useMoneyHistory } from "@/lib/data";
import { count, dateTime, money } from "@/lib/format";
import {
  customMoneyRange,
  moneyAxisTicks,
  moneyPoints,
  settlementTotals,
  type MoneyPreferences,
  type MoneySeries,
} from "@/lib/money-history";
import { useUI } from "@/lib/ui-store";
import { HistoryRange } from "./history-range";
import { DataTable, EmptyState, Section } from "./ui";
import styles from "./performance.module.css";
import viewStyles from "./world-views.module.css";

const SERIES: Record<MoneySeries, { label: string; description: string }> = {
  liquid_funds_pence: {
    label: "Cash balance",
    description:
      "Regional vault cash, excluding escrow. Balance changes include purchases and transfers; they are not profit.",
  },
  net_worth_pence: {
    label: "Net worth",
    description:
      "Recorded official valuations, not earnings. A current standing cannot reconstruct historical net worth.",
  },
  settlement_net: {
    label: "Realized settlement net",
    description:
      "Revenue less acquisition, fees, provisions and expenses for known-cost completed settlements. Bars group completions by recorded UTC time.",
  },
  settlement_gross: {
    label: "Gross settlement revenue",
    description:
      "Recorded settlement proceeds before costs. Periods with unknown proceeds are omitted, not treated as zero. Seed capital, transfer principal and collateral refunds are not revenue.",
  },
  settlement_costs: {
    label: "Settlement costs",
    description:
      "Acquisition, fees, provisions and expenses of known-cost settlements, shown below zero. Unknown acquisition costs are excluded, not treated as free.",
  },
  cumulative_net: {
    label: "Cumulative settlement net",
    description:
      "Running known-cost settlement net within the selected range, not lifetime profit or a cash balance. Unknown-cost settlements are excluded.",
  },
  cash_outflow_pence: {
    label: "Ledger cash outflow",
    description:
      "Outbound cash in each recorded ledger snapshot, including purchases and capital movements. Snapshots overlap; do not add them together or interpret them as trading losses.",
  },
  expenses_pence: {
    label: "Ledger fees & bills",
    description:
      "Fees and bills in each recorded ledger snapshot. Snapshots overlap; these are not new charges at every observation.",
  },
};
const signedMoney = (value: number | undefined) =>
  value !== undefined && value > 0 ? `+${money(value)}` : money(value);
const tone = (value: number | undefined) =>
  value === undefined || value === 0
    ? undefined
    : value < 0
      ? styles.loss
      : styles.gain;

export function Performance({ accountID }: { accountID: string }) {
  const preferences = useUI((state) => state.moneyHistory);
  const setPreferences = useUI((state) => state.setMoneyHistory);
  const { series, range } = preferences;
  const history = useMoneyHistory(accountID, preferences);
  const [selectedID, setSelectedID] = useState<string | null>(null);
  const [customDraft, setCustomDraft] = useState<Pick<
    MoneyPreferences,
    "from" | "to"
  > | null>(null);
  const [width, setWidth] = useState(800);
  const plot = useRef<HTMLDivElement>(null);
  const chartID = useId();
  const now = useNow();
  const points = useMemo(
    () => moneyPoints(history.data, series),
    [history.data, series],
  );
  const totals = useMemo(
    () => settlementTotals(history.data?.settlements ?? []),
    [history.data],
  );
  const hasPoints = points.length > 0;
  const settlementSeries =
    series.startsWith("settlement_") || series === "cumulative_net";
  const bars = series.startsWith("settlement_");
  const signed =
    series === "settlement_net" ||
    series === "settlement_costs" ||
    series === "cumulative_net";
  const customFrom = customDraft?.from ?? preferences.from;
  const customTo = customDraft?.to ?? preferences.to;
  const invalidCustom =
    range === "custom" && !customMoneyRange(customFrom, customTo);
  const customChanged =
    customFrom !== preferences.from || customTo !== preferences.to;
  const summaryPlaceholder = history.data
    ? undefined
    : invalidCustom
      ? "Choose a range"
      : history.isError
        ? "Unavailable"
        : "Loading…";
  const last = points.at(-1);
  const first = points[0];
  const index =
    selectedID === null
      ? -1
      : points.findIndex((point) => point.id === selectedID);
  const activeIndex = index < 0 ? points.length - 1 : index;
  const selected = points[activeIndex];
  const stale =
    !settlementSeries &&
    history.data &&
    last &&
    Math.abs(
      Date.parse(history.data.to) - Date.parse(history.data.generated_at),
    ) <= 120_000 &&
    now - Date.parse(last.at) > 120_000;
  const pointDate = (point: (typeof points)[number]) =>
    bars && point.period
      ? `${dateTime(point.at)} – ${dateTime(point.period.to)}`
      : dateTime(point.at);

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
  }, [hasPoints]);

  const height = 250;
  const padding = { left: 90, top: 16, right: 14, bottom: 34 };
  const plotWidth = Math.max(1, width - padding.left - padding.right);
  const plotHeight = height - padding.top - padding.bottom;
  const ticks = useMemo(
    () => moneyAxisTicks(points.map((point) => point.value)),
    [points],
  );
  const minimum = ticks[0];
  const maximum = ticks[ticks.length - 1];
  const span = maximum - minimum;
  const start = history.data ? Date.parse(history.data.from) : 0;
  const end = history.data ? Date.parse(history.data.to) : 1;
  const duration = Math.max(1, end - start);
  const y = (value: number) =>
    padding.top + (1 - (value - minimum) / span) * plotHeight;
  const zeroY = y(0);
  const coordinates = points.map((point) => ({
    x:
      padding.left +
      Math.max(
        0,
        Math.min(
          1,
          (Date.parse(point.at) +
            (bars && point.period
              ? (Date.parse(point.period.to) - Date.parse(point.at)) / 2
              : 0) -
            start) /
            duration,
        ),
      ) *
        plotWidth,
    y: y(point.value),
  }));
  const path = coordinates
    .map((point, i) => `${i === 0 ? "M" : "L"}${point.x},${point.y}`)
    .join(" ");
  const barWidth = Math.max(
    2,
    Math.min(
      24,
      plotWidth *
        (((history.data?.bucket_seconds ?? 1) * 1000) / duration) *
        0.75,
    ),
  );
  const selectedPosition = coordinates[activeIndex];
  const periodLabel = history.data
    ? history.data.bucket_seconds < 3600
      ? `${history.data.bucket_seconds / 60}-minute`
      : history.data.bucket_seconds < 86400
        ? `${history.data.bucket_seconds / 3600}-hour`
        : `${history.data.bucket_seconds / 86400}-day`
    : "";

  return (
    <Section
      title="Money history"
      icon={ChartNoAxesCombined}
      className={styles.section}
    >
      <div className={styles.controls}>
        <label className={styles.metric}>
          Metric
          <select
            value={series}
            onChange={(event) => {
              setPreferences({ series: event.target.value as MoneySeries });
              setSelectedID(null);
            }}
          >
            <optgroup label="Balances">
              <option value="liquid_funds_pence">Cash balance</option>
              <option value="net_worth_pence">Net worth</option>
            </optgroup>
            <optgroup label="Completed settlements">
              <option value="settlement_net">Realized settlement net</option>
              <option value="settlement_gross">Gross settlement revenue</option>
              <option value="settlement_costs">Settlement costs</option>
              <option value="cumulative_net">Cumulative settlement net</option>
            </optgroup>
            <optgroup label="Overlapping ledger snapshots">
              <option value="cash_outflow_pence">Ledger cash outflow</option>
              <option value="expenses_pence">Ledger fees &amp; bills</option>
            </optgroup>
          </select>
        </label>
        <HistoryRange
          value={range}
          label="Money history date range"
          allowCustom
          onChange={(range) => {
            setPreferences({
              range,
              ...(range === "custom" &&
              !customMoneyRange(preferences.from, preferences.to)
                ? {
                    from: new Date(now - 86_400_000).toISOString().slice(0, 16),
                    to: new Date(now).toISOString().slice(0, 16),
                  }
                : {}),
            });
            setSelectedID(null);
            setCustomDraft(null);
          }}
        />
      </div>
      {range === "custom" && (
        <form
          className={styles.custom}
          onSubmit={(event) => {
            event.preventDefault();
            if (invalidCustom) return;
            setPreferences({ from: customFrom, to: customTo });
            setCustomDraft(null);
            setSelectedID(null);
          }}
        >
          <label>
            From (UTC)
            <input
              type="datetime-local"
              value={customFrom}
              onChange={(event) =>
                setCustomDraft({ from: event.target.value, to: customTo })
              }
              aria-invalid={Boolean(invalidCustom)}
              aria-describedby={
                invalidCustom ? `${chartID}-range-error` : undefined
              }
            />
          </label>
          <label>
            To (UTC)
            <input
              type="datetime-local"
              value={customTo}
              onChange={(event) =>
                setCustomDraft({ from: customFrom, to: event.target.value })
              }
              aria-invalid={Boolean(invalidCustom)}
              aria-describedby={
                invalidCustom ? `${chartID}-range-error` : undefined
              }
            />
          </label>
          <button
            className="button"
            type="submit"
            disabled={Boolean(invalidCustom) || !customChanged}
          >
            Apply range
          </button>
        </form>
      )}
      {invalidCustom && (
        <p id={`${chartID}-range-error`} className={styles.loss} role="alert">
          Choose both dates and times, with To after From. Times are UTC.
        </p>
      )}
      {history.isError && (
        <p className={styles.warning} role="alert">
          Money history unavailable: {history.error.message}{" "}
          {history.data
            ? "Retained data remains visible."
            : "No history could be retrieved."}
        </p>
      )}
      {settlementSeries ? (
        <dl className={styles.summary}>
          <div>
            <dt>Gross revenue</dt>
            <dd>
              {summaryPlaceholder ??
                (totals.count > 0 && totals.gross === undefined
                  ? "Proceeds unknown"
                  : money(totals.gross))}
            </dd>
          </div>
          <div>
            <dt>Known costs</dt>
            <dd>{summaryPlaceholder ?? money(totals.costs)}</dd>
          </div>
          <div>
            <dt>Known-cost net</dt>
            <dd className={tone(totals.net)}>
              {summaryPlaceholder ?? signedMoney(totals.net)}
            </dd>
          </div>
        </dl>
      ) : (
        <dl className={styles.summary}>
          <div>
            <dt>Latest recorded</dt>
            <dd>{summaryPlaceholder ?? money(last?.value)}</dd>
          </div>
          {series === "liquid_funds_pence" && points.length > 1 && (
            <div>
              <dt>Sampled cash change, not profit</dt>
              <dd className={tone(last!.value - first.value)}>
                {signedMoney(last!.value - first.value)}
              </dd>
            </div>
          )}
        </dl>
      )}
      <div className={styles.status}>
        {history.isPending && !invalidCustom && (
          <span role="status">Retrieving recorded history. </span>
        )}
        {stale
          ? `Balance observations are stale; latest recorded ${dateTime(last.at)}. `
          : ""}
        {settlementSeries && history.data && (
          <>
            {count(totals.known)} known-cost settlements;{" "}
            {count(totals.unknown)} with unknown costs or proceeds. Not complete
            company profit.
          </>
        )}
      </div>
      {points.length === 0 ? (
        <EmptyState
          icon={ChartNoAxesCombined}
          title={
            history.isError && !history.data
              ? "Money history unavailable"
              : history.isPending && !invalidCustom
                ? "Retrieving money history"
                : series === "net_worth_pence"
                  ? "Net-worth history not recorded"
                  : "No recorded values in this range"
          }
          description={
            history.isError && !history.data
              ? "The recording service could not retrieve this history. This is not evidence of an empty range."
              : invalidCustom
                ? "Enter a valid UTC range and choose Apply range to show recorded values."
                : history.isPending
                  ? "Waiting for recorded observations from the service."
                  : series === "net_worth_pence"
                    ? "The current leaderboard is a snapshot, not a historical series. No past valuations have been invented."
                    : "Missing observations do not establish zero activity. Try a wider date range."
          }
        />
      ) : (
        <>
          <div className={styles.legend}>
            <span>
              {SERIES[series].label}
              {bars ? ` · ${periodLabel} periods` : ""}
            </span>
            <span>
              {signed ? (
                <>
                  <span className={styles.gain}>+ Gain</span>
                  <span className={styles.loss}>− Cost / loss</span>
                </>
              ) : (
                "Recorded observations only"
              )}
            </span>
          </div>
          <div ref={plot} className={styles.chart}>
            <svg
              viewBox={`0 0 ${width} ${height}`}
              role="img"
              aria-label={`${SERIES[series].label}, ${points.length} recorded ${bars ? "periods" : "points"}. ${signed ? "Gains above zero; costs and losses below zero." : bars ? "Bars show recorded revenue per period." : "Lines connect recorded observations, not continuous measurements."} Exact values are available below.`}
              onPointerMove={(event) => {
                const bounds = event.currentTarget.getBoundingClientRect();
                const x =
                  ((event.clientX - bounds.left) * width) / bounds.width;
                let nearest = 0;
                for (let i = 1; i < coordinates.length; i++)
                  if (
                    Math.abs(coordinates[i].x - x) <
                    Math.abs(coordinates[nearest].x - x)
                  )
                    nearest = i;
                setSelectedID(points[nearest].id);
              }}
            >
              <defs>
                <clipPath id={`${chartID}-positive`}>
                  <rect x="0" y="0" width={width} height={zeroY} />
                </clipPath>
                <clipPath id={`${chartID}-negative`}>
                  <rect x="0" y={zeroY} width={width} height={height - zeroY} />
                </clipPath>
              </defs>
              {ticks.map((tick) => (
                <g key={tick}>
                  <line
                    x1={padding.left}
                    y1={y(tick)}
                    x2={width - padding.right}
                    y2={y(tick)}
                    className={styles.gridline}
                  />
                  <text
                    x={padding.left - 10}
                    y={y(tick) + 4}
                    textAnchor="end"
                    className={styles.axis}
                  >
                    {count(tick / 100)} Cr
                  </text>
                </g>
              ))}
              <line
                x1={padding.left}
                y1={zeroY}
                x2={width - padding.right}
                y2={zeroY}
                className={styles.zeroLine}
              />
              {selectedPosition && (
                <line
                  x1={selectedPosition.x}
                  y1={padding.top}
                  x2={selectedPosition.x}
                  y2={height - padding.bottom}
                  className={styles.inspectionLine}
                />
              )}
              {!bars &&
                (signed ? (
                  <>
                    <path
                      d={path}
                      className={`${styles.line} ${styles.gainLine}`}
                      clipPath={`url(#${chartID}-positive)`}
                    />
                    <path
                      d={path}
                      className={`${styles.line} ${styles.lossLine}`}
                      clipPath={`url(#${chartID}-negative)`}
                    />
                  </>
                ) : (
                  <path d={path} className={styles.line} />
                ))}
              {coordinates.map((point, i) =>
                bars ? (
                  <rect
                    key={points[i].id}
                    x={Math.min(
                      width - padding.right - barWidth,
                      Math.max(padding.left, point.x - barWidth / 2),
                    )}
                    y={Math.min(point.y, zeroY)}
                    width={barWidth}
                    height={Math.max(1, Math.abs(zeroY - point.y))}
                    className={`${styles.bar} ${signed ? (points[i].value < 0 ? styles.lossBar : points[i].value > 0 ? styles.gainBar : "") : ""} ${i === activeIndex ? styles.selectedBar : ""}`}
                  >
                    <title>
                      {pointDate(points[i])}: {signedMoney(points[i].value)}
                    </title>
                  </rect>
                ) : (
                  <circle
                    key={points[i].id}
                    cx={point.x}
                    cy={point.y}
                    r={i === activeIndex ? 4.5 : 2}
                    className={`${styles.dot} ${signed ? (points[i].value < 0 ? styles.lossBar : styles.gainBar) : ""}`}
                  >
                    <title>
                      {pointDate(points[i])}: {signedMoney(points[i].value)}
                    </title>
                  </circle>
                ),
              )}
              <text x={padding.left} y={height - 8} className={styles.axis}>
                {dateTime(history.data?.from)}
              </text>
              <text
                x={width - padding.right}
                y={height - 8}
                textAnchor="end"
                className={styles.axis}
              >
                {dateTime(history.data?.to)}
              </text>
            </svg>
          </div>
          <div className={styles.inspector}>
            <div>
              <span>{pointDate(selected)}</span>
              <strong className={signed ? tone(selected.value) : undefined}>
                {signed ? signedMoney(selected.value) : money(selected.value)}
              </strong>
              <span>
                {count(activeIndex + 1)} of {count(points.length)}
              </span>
            </div>
            <div className={styles.sampleControls}>
              <button
                type="button"
                className="icon-button"
                aria-label="Inspect previous recorded point"
                disabled={activeIndex <= 0}
                onClick={() => setSelectedID(points[activeIndex - 1].id)}
              >
                <ChevronLeft size={15} aria-hidden="true" />
              </button>
              <label>
                <span className="sr-only">Inspect recorded point</span>
                <input
                  type="range"
                  min={0}
                  max={points.length - 1}
                  value={activeIndex}
                  disabled={points.length < 2}
                  onChange={(event) =>
                    setSelectedID(points[Number(event.target.value)].id)
                  }
                  aria-valuetext={`${pointDate(selected)}, ${signed ? signedMoney(selected.value) : money(selected.value)}`}
                />
              </label>
              <button
                type="button"
                className="icon-button"
                aria-label="Inspect next recorded point"
                disabled={activeIndex >= points.length - 1}
                onClick={() => setSelectedID(points[activeIndex + 1].id)}
              >
                <ChevronRight size={15} aria-hidden="true" />
              </button>
            </div>
          </div>
        </>
      )}
      <p className={styles.description}>{SERIES[series].description}</p>
      {history.data && (
        <p className={styles.coverage}>
          {settlementSeries
            ? `${count(points.length)} populated ${periodLabel} periods from ${count(totals.count)} completed settlements.`
            : `${count(points.length)} plotted samples. Last observation per ${periodLabel} bucket; never summed.`}{" "}
          {first && (
            <>
              Plotted span: {dateTime(first.at)} to {dateTime(last?.at)}.{" "}
            </>
          )}
          Missing intervals are not filled. Recorded settlements exclude
          uncompleted voyages and unallocated company costs.
          {settlementSeries &&
            totals.unknown > 0 &&
            " Known proceeds from unknown-cost settlements count toward gross only. Unknown proceeds keep total gross unknown; affected gross chart periods are omitted. Costs and net include only settlements with known proceeds and costs."}
        </p>
      )}
      {points.length > 0 && (
        <details className={viewStyles.disclosure}>
          <summary>Inspect recorded values</summary>
          <DataTable
            label={`${SERIES[series].label} recorded values`}
            data={points}
            rowKey={(point) => point.id}
            columns={[
              {
                label: bars ? "Period (UTC)" : "Recorded (UTC)",
                value: (point) => point.at,
                render: (point) => pointDate(point),
              },
              {
                label: SERIES[series].label,
                value: (point) => point.value,
                render: (point) => (
                  <span className={signed ? tone(point.value) : undefined}>
                    {signed ? signedMoney(point.value) : money(point.value)}
                  </span>
                ),
                className: "numeric",
              },
              ...(settlementSeries
                ? [
                    {
                      label: "Settlements / known costs",
                      value: (point: (typeof points)[number]) =>
                        point.period?.count,
                      render: (point: (typeof points)[number]) =>
                        `${count(point.period?.count)} / ${count(point.period?.known_count)}`,
                    },
                  ]
                : []),
            ]}
          />
        </details>
      )}
    </Section>
  );
}
