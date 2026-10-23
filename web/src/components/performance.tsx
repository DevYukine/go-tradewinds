"use client";

import {
  Banknote,
  ChartNoAxesCombined,
  ChevronLeft,
  ChevronRight,
  CircleDollarSign,
  ReceiptText,
  Wallet,
} from "lucide-react";
import { useMemo, useState } from "react";
import { useNow } from "@/app/providers";
import { count, dateTime, money } from "@/lib/format";
import type { Metric } from "@/lib/types";
import { DataTable, EmptyState, Section } from "./ui";
import styles from "./operations.module.css";
import viewStyles from "./world-views.module.css";

const SERIES = {
  liquid_funds_pence: {
    label: "Liquid funds",
    icon: Wallet,
    description:
      "Recorded regional vault cash, excluding escrow. Lines connect recorded balances, not a continuous valuation.",
  },
  realized_profit_pence: {
    label: "Realized settlement net",
    icon: ReceiptText,
    description:
      "Known-cost completed-settlement net. Each bar is one settlement; the subtotal is not complete period profit.",
  },
  cash_outflow_pence: {
    label: "Cash outflow",
    icon: Banknote,
    description:
      "Recorded outbound ledger cash, including purchases and capital movements. It is not trading loss. Repeated records are not added together.",
  },
  expenses_pence: {
    label: "Ledger expenses",
    icon: CircleDollarSign,
    description:
      "Recorded ledger fees and bills, not new charges at each timestamp. Repeated records are not added together.",
  },
} as const;

type SeriesName = keyof typeof SERIES;

export function Performance({
  metrics,
  error,
  fetching,
}: {
  metrics: Metric[];
  error?: string;
  fetching: boolean;
}) {
  const [series, setSeries] = useState<SeriesName>("liquid_funds_pence");
  const [range, setRange] = useState("24h");
  const [selectedID, setSelectedID] = useState<number | null>(null);
  const now = useNow();
  const cutoff =
    range === "24h" ? now - 86_400_000 : range === "7d" ? now - 604_800_000 : 0;
  const points = useMemo(
    () =>
      metrics
        .filter(
          (point) =>
            point.name === series &&
            Number.isFinite(point.value) &&
            Date.parse(point.recorded_at) >= cutoff,
        )
        .sort(
          (left, right) =>
            Date.parse(left.recorded_at) - Date.parse(right.recorded_at),
        ),
    [metrics, series, cutoff],
  );
  const first = points[0];
  const last = points.at(-1);
  const selectedIndex =
    selectedID === null
      ? points.length - 1
      : points.findIndex((point) => point.id === selectedID);
  const selected =
    points[selectedIndex < 0 ? points.length - 1 : selectedIndex];
  const settlementSeries = series === "realized_profit_pence";
  const subtotal =
    settlementSeries && points.length
      ? points.reduce((sum, point) => sum + point.value, 0)
      : undefined;
  const headlineValue = settlementSeries ? subtotal : last?.value;
  const width = 800;
  const height = 260;
  const padding = { left: 98, top: 20, right: 22, bottom: 36 };
  const plotWidth = width - padding.left - padding.right;
  const plotHeight = height - padding.top - padding.bottom;
  const minimum = Math.min(0, ...points.map((point) => point.value));
  const maximum = Math.max(1, ...points.map((point) => point.value));
  const span = Math.max(maximum - minimum, 1);
  const start = first ? Date.parse(first.recorded_at) : 0;
  const duration = last ? Math.max(1, Date.parse(last.recorded_at) - start) : 1;
  const coordinates = points.map((point) => ({
    x:
      padding.left +
      (points.length === 1
        ? 0.5
        : (Date.parse(point.recorded_at) - start) / duration) *
        plotWidth,
    y: padding.top + (1 - (point.value - minimum) / span) * plotHeight,
  }));
  const path = coordinates
    .map((point, index) => `${index === 0 ? "M" : "L"}${point.x},${point.y}`)
    .join(" ");
  const zeroY = padding.top + (1 - (0 - minimum) / span) * plotHeight;
  const barWidth = Math.max(
    2,
    Math.min(14, (plotWidth / Math.max(1, points.length)) * 0.65),
  );
  const activeIndex = selectedIndex < 0 ? points.length - 1 : selectedIndex;
  const selectedPosition = coordinates[activeIndex];

  return (
    <Section
      title="Recorded financial history"
      icon={ChartNoAxesCombined}
      className={styles.section}
    >
      <div
        className={styles.tabs}
        role="group"
        aria-label="Recorded metric series"
      >
        {(Object.keys(SERIES) as SeriesName[]).map((name) => {
          const Icon = SERIES[name].icon;
          return (
            <button
              key={name}
              type="button"
              aria-pressed={series === name}
              onClick={() => {
                setSeries(name);
                setSelectedID(null);
              }}
            >
              <Icon size={15} strokeWidth={1.7} aria-hidden="true" />
              {SERIES[name].label}
            </button>
          );
        })}
      </div>
      <div className={styles.toolbar}>
        <label>
          Recorded window
          <select
            value={range}
            onChange={(event) => {
              setRange(event.target.value);
              setSelectedID(null);
            }}
          >
            <option value="24h">Last 24 hours of records</option>
            <option value="7d">Last 7 days of records</option>
            <option value="all">All returned records</option>
          </select>
        </label>
        {fetching && (
          <span className={styles.context}>Updating recorded history</span>
        )}
      </div>
      {error && (
        <div className={styles.warning} role="alert">
          Metric history unavailable: {error} Only returned or retained records
          are shown.
        </div>
      )}
      <dl className={styles.facts}>
        <div>
          <dt>
            {settlementSeries ? "Known-cost subtotal" : "Latest recorded value"}
          </dt>
          <dd
            className={
              headlineValue !== undefined && headlineValue < 0
                ? "negative"
                : settlementSeries &&
                    headlineValue !== undefined &&
                    headlineValue > 0
                  ? "positive"
                  : undefined
            }
          >
            {money(headlineValue)}
          </dd>
        </div>
        <div>
          <dt>Recorded points</dt>
          <dd>{count(points.length)}</dd>
        </div>
      </dl>
      <p className={styles.note}>{SERIES[series].description}</p>
      {points.length === 0 ? (
        <EmptyState
          icon={ChartNoAxesCombined}
          title="No financial history recorded"
          description="No values are recorded for this series and window; this does not establish zero activity."
        />
      ) : (
        <>
          <div className={viewStyles.legend}>
            <span className={viewStyles.inline}>
              <span
                className={
                  settlementSeries
                    ? viewStyles.barSwatch
                    : viewStyles.lineSwatch
                }
              />
              {SERIES[series].label}
            </span>
            <span>
              {settlementSeries
                ? "One bar per settlement"
                : "Recorded points joined by a line"}
            </span>
          </div>
          <div className={`${styles.chart} ${viewStyles.chart}`}>
            <svg
              viewBox={`0 0 ${width} ${height}`}
              role="img"
              aria-label={`${SERIES[series].label}, ${points.length} recorded points from ${dateTime(first.recorded_at)} to ${dateTime(last?.recorded_at)}. ${settlementSeries ? "Bars show individual settlement net." : "Lines join recorded points only."}`}
            >
              {[0, 0.5, 1].map((position) => (
                <g key={position}>
                  <line
                    x1={padding.left}
                    y1={padding.top + plotHeight * position}
                    x2={width - padding.right}
                    y2={padding.top + plotHeight * position}
                    className={styles.gridline}
                  />
                  <text
                    x={padding.left - 10}
                    y={padding.top + plotHeight * position + 4}
                    textAnchor="end"
                    className={styles.axis}
                  >
                    {money(maximum - span * position, true)}
                  </text>
                </g>
              ))}
              <line
                x1={padding.left}
                y1={zeroY}
                x2={width - padding.right}
                y2={zeroY}
                className={viewStyles.zeroLine}
              />
              {selectedPosition && (
                <line
                  x1={selectedPosition.x}
                  y1={padding.top}
                  x2={selectedPosition.x}
                  y2={height - padding.bottom}
                  className={viewStyles.inspectionLine}
                />
              )}
              {!settlementSeries && (
                <path d={path} className={styles.chartLine} />
              )}
              {coordinates.map((point, index) =>
                settlementSeries ? (
                  <rect
                    key={`${points[index].account_id}:${points[index].id}`}
                    x={point.x - barWidth / 2}
                    y={Math.min(point.y, zeroY)}
                    width={barWidth}
                    height={Math.max(1, Math.abs(zeroY - point.y))}
                    className={`${styles.chartBar} ${points[index].value < 0 ? viewStyles.lossBar : points[index].value > 0 ? viewStyles.gainBar : ""} ${index === activeIndex ? viewStyles.selectedBar : ""}`}
                  >
                    <title>
                      {dateTime(points[index].recorded_at)}:{" "}
                      {money(points[index].value)}
                    </title>
                  </rect>
                ) : (
                  <circle
                    key={`${points[index].account_id}:${points[index].id}`}
                    cx={point.x}
                    cy={point.y}
                    r={index === activeIndex ? 5 : 2.5}
                    className={
                      index === activeIndex
                        ? viewStyles.selectedDot
                        : styles.chartDot
                    }
                  >
                    <title>
                      {dateTime(points[index].recorded_at)}:{" "}
                      {money(points[index].value)}
                    </title>
                  </circle>
                ),
              )}
              <text x={padding.left} y={height - 8} className={styles.axis}>
                {dateTime(first.recorded_at)}
              </text>
              <text
                x={width - padding.right}
                y={height - 8}
                textAnchor="end"
                className={styles.axis}
              >
                {dateTime(last?.recorded_at)}
              </text>
            </svg>
          </div>
          <div className={viewStyles.inspector}>
            <div aria-live="polite" className={viewStyles.inspectedValue}>
              <span className={viewStyles.caption}>
                Recorded {dateTime(selected?.recorded_at)}
              </span>
              <strong
                className={
                  selected && selected.value < 0
                    ? "negative"
                    : settlementSeries && selected && selected.value > 0
                      ? "positive"
                      : undefined
                }
              >
                {money(selected?.value)}
              </strong>
              <span className={viewStyles.caption}>
                Point {count(activeIndex + 1)} of {count(points.length)}
              </span>
            </div>
            <div className={viewStyles.sampleControls}>
              <button
                type="button"
                className="icon-button"
                aria-label="Inspect previous recorded point"
                disabled={activeIndex <= 0}
                onClick={() => setSelectedID(points[activeIndex - 1].id)}
              >
                <ChevronLeft size={15} strokeWidth={1.7} aria-hidden="true" />
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
                  aria-valuetext={`${dateTime(selected?.recorded_at)}, ${money(selected?.value)}`}
                />
              </label>
              <button
                type="button"
                className="icon-button"
                aria-label="Inspect next recorded point"
                disabled={activeIndex >= points.length - 1}
                onClick={() => setSelectedID(points[activeIndex + 1].id)}
              >
                <ChevronRight size={15} strokeWidth={1.7} aria-hidden="true" />
              </button>
            </div>
          </div>
        </>
      )}
      <p className={styles.coverage}>
        {count(points.length)} matching points from {count(metrics.length)}{" "}
        returned metric records.{" "}
        {first && (
          <>
            Series coverage: {dateTime(first.recorded_at)} to{" "}
            {dateTime(last?.recorded_at)}.{" "}
          </>
        )}
        Up to 500 records across all four series. The selected window and
        plotted span do not establish complete coverage.
      </p>
      <details className={viewStyles.disclosure}>
        <summary>Inspect recorded values</summary>
        <DataTable
          label={`${SERIES[series].label} recorded value ledger`}
          data={points}
          rowKey={(point) => `${point.account_id}:${point.id}`}
          columns={[
            {
              label: "Recorded",
              value: (point) => point.recorded_at || undefined,
              render: (point) => dateTime(point.recorded_at),
            },
            {
              label: "Recorded value",
              value: (point) => point.value,
              render: (point) => (
                <span
                  className={
                    point.value < 0
                      ? "negative"
                      : settlementSeries && point.value > 0
                        ? "positive"
                        : undefined
                  }
                >
                  {money(point.value)}
                </span>
              ),
              className: styles.numeric,
            },
            {
              label: "Record ID",
              value: (point) => point.id,
              render: (point) => count(point.id),
              className: styles.numeric,
            },
          ]}
        />
      </details>
    </Section>
  );
}
