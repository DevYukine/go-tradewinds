import type { MoneyRange } from "@/lib/money-history";
import styles from "./performance.module.css";

const PRESETS = ["24h", "7d", "30d", "all"] as const;

export function HistoryRange({
  value,
  onChange,
  label,
  allowCustom = false,
}: {
  value: MoneyRange;
  onChange: (range: MoneyRange) => void;
  label: string;
  allowCustom?: boolean;
}) {
  const ranges: readonly MoneyRange[] = allowCustom
    ? [...PRESETS, "custom"]
    : PRESETS;
  return (
    <div className={styles.presets} role="group" aria-label={label}>
      {ranges.map((range) => (
        <button
          key={range}
          type="button"
          aria-pressed={value === range}
          onClick={() => onChange(range)}
        >
          {range === "all" ? "All" : range === "custom" ? "Custom" : range}
        </button>
      ))}
    </div>
  );
}
