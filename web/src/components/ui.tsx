"use client";

import type { LucideIcon } from "lucide-react";
import {
  AlertTriangle,
  Anchor,
  ArrowUpRight,
  CheckCircle2,
  Circle,
  Clock3,
  Database,
  Search,
  Ship,
  Skull,
} from "lucide-react";
import { useState, type ReactNode } from "react";
import { useNow } from "@/app/providers";
import { dataset, metadata } from "@/lib/data";
import { age, dateTime, humanize } from "@/lib/format";
import type { Account } from "@/lib/types";
import { pokemonSprite } from "@/lib/sprites";
export { DataTable, type Column } from "./data-table";

export function Badge({
  children,
  tone = "neutral",
  icon: Icon,
}: {
  children: ReactNode;
  tone?: "neutral" | "good" | "warning" | "danger" | "blue";
  icon?: LucideIcon;
}) {
  return (
    <span className={`badge badge-${tone}`}>
      {Icon && <Icon size={13} strokeWidth={1.7} aria-hidden="true" />}
      {children}
    </span>
  );
}

export function StatusBadge({ status }: { status: string }) {
  const tone = [
    "sailing",
    "active",
    "completed",
    "settled_safe",
    "selected",
    "executed",
    "good",
  ].includes(status)
    ? "good"
    : ["sunk", "defaulted", "claimed_loss", "error", "rejected"].includes(
          status,
        )
      ? "danger"
      : [
            "anchored_roadstead",
            "open",
            "waiting",
            "pending",
            "held",
            "warning",
          ].includes(status)
        ? "warning"
        : "neutral";
  const Icon =
    status === "sailing"
      ? Ship
      : ["berthed", "anchored_roadstead"].includes(status)
        ? Anchor
        : status === "sunk"
          ? Skull
          : tone === "danger" || status === "warning"
            ? AlertTriangle
            : ["waiting", "pending", "held"].includes(status)
              ? Clock3
              : tone === "good"
                ? CheckCircle2
                : Circle;
  return (
    <Badge tone={tone} icon={Icon}>
      {humanize(status)}
    </Badge>
  );
}

export function Section({
  title,
  icon: Icon,
  actions,
  children,
  className = "",
  id,
}: {
  title: string;
  icon: LucideIcon;
  actions?: ReactNode;
  children: ReactNode;
  className?: string;
  id?: string;
}) {
  return (
    <section className={`card ${className}`} id={id} aria-label={title}>
      <div className="card-heading">
        <div className="card-title-wrap">
          <span className="card-icon">
            <Icon size={17} strokeWidth={1.7} aria-hidden="true" />
          </span>
          <div>
            <h2>{title}</h2>
          </div>
        </div>
        {actions && <div className="card-actions">{actions}</div>}
      </div>
      {children}
    </section>
  );
}

export function EmptyState({
  title,
  description,
  icon: Icon = Database,
}: {
  title: string;
  description: string;
  icon?: LucideIcon;
}) {
  return (
    <div className="empty-state">
      <span className="empty-icon">
        <Icon size={24} strokeWidth={1.7} aria-hidden="true" />
      </span>
      <h3>{title}</h3>
      <p>{description}</p>
    </div>
  );
}

export function DatasetStatus({
  account,
  name,
}: {
  account?: Account;
  name: string;
}) {
  const now = useNow();
  const meta = metadata(account, name);
  const present = dataset(account, name) !== undefined;
  const timestamp = meta.updated_at;
  const elapsed = timestamp ? now - Date.parse(timestamp) : undefined;
  const dynamic =
    name === "ships" ||
    name.startsWith("ship_details/") ||
    name === "clock" ||
    name === "vaults";
  const stale =
    meta.stale ||
    meta.complete === false ||
    (dynamic && elapsed !== undefined && elapsed > 120_000);
  const delayed = dynamic && elapsed !== undefined && elapsed > 30_000;

  return (
    <div className={`dataset-status ${meta.error ? "has-error" : ""}`}>
      {meta.error || stale ? (
        <AlertTriangle size={13} strokeWidth={1.7} aria-hidden="true" />
      ) : (
        <Clock3 size={13} strokeWidth={1.7} aria-hidden="true" />
      )}
      <span>
        {!present
          ? "Not recorded"
          : meta.complete === false
            ? "Incomplete recorded data"
            : stale
              ? "Stale recorded data"
              : delayed
                ? "Delayed recorded data"
                : timestamp
                  ? "Recorded"
                  : "Freshness unknown"}
        {timestamp && ` ${age(timestamp, now)}`}
      </span>
      {meta.error && (
        <details className="dataset-error">
          <summary>Fetch error</summary>
          <div role="note">
            <p>{meta.error}</p>
            <small>
              Last attempted {dateTime(meta.attempted_at)}.{" "}
              {present
                ? "Last successful data is retained."
                : "No successful data has been recorded."}
            </small>
          </div>
        </details>
      )}
      {meta.generation && (
        <details className="dataset-generation">
          <summary>Source</summary>
          <div>
            <span>Generation</span>
            <code>{meta.generation}</code>
            <span>
              Complete:{" "}
              {meta.complete === undefined ? "unknown" : String(meta.complete)}
            </span>
            {meta.upstream_as_of && (
              <span>Upstream source: {dateTime(meta.upstream_as_of)}</span>
            )}
          </div>
        </details>
      )}
    </div>
  );
}

export function Dataset({
  account,
  name,
  children,
  emptyTitle = "Awaiting recorded data",
  emptyDescription = "The runner has not recorded this dataset. Values are never inferred from another market or account.",
}: {
  account?: Account;
  name: string;
  children: ReactNode;
  emptyTitle?: string;
  emptyDescription?: string;
}) {
  return (
    <>
      <DatasetStatus account={account} name={name} />
      {dataset(account, name) === undefined ? (
        <EmptyState title={emptyTitle} description={emptyDescription} />
      ) : (
        children
      )}
    </>
  );
}

export function SearchField({
  value,
  onChange,
  placeholder,
  label,
}: {
  value: string;
  onChange: (value: string) => void;
  placeholder: string;
  label: string;
}) {
  return (
    <label className="search-field">
      <Search size={15} strokeWidth={1.7} aria-hidden="true" />
      <span className="sr-only">{label}</span>
      <input
        type="search"
        value={value}
        onChange={(event) => onChange(event.target.value)}
        placeholder={placeholder}
      />
    </label>
  );
}

export function StructuredDetails({
  value,
  label = "Inspect record",
}: {
  value: unknown;
  label?: string;
}) {
  return (
    <details className="structured-details">
      <summary>
        {label}
        <ArrowUpRight size={13} strokeWidth={1.7} aria-hidden="true" />
      </summary>
      <pre>
        {JSON.stringify(value, null, 2) ?? "No structured detail recorded."}
      </pre>
    </details>
  );
}

export function Progress({ value, label }: { value: number; label: string }) {
  const bounded = Math.max(
    0,
    Math.min(100, Number.isFinite(value) ? value : 0),
  );
  return (
    <span
      className="progress-track"
      role="progressbar"
      aria-label={label}
      aria-valuenow={bounded}
      aria-valuemin={0}
      aria-valuemax={100}
    >
      <span style={{ width: `${bounded}%` }} />
    </span>
  );
}

export function SpriteImage({ name }: { name: string }) {
  const source = pokemonSprite(name);
  const [failedSource, setFailedSource] = useState("");
  return (
    <span className="sprite-image">
      {!source || failedSource === source ? (
        <Ship
          size={24}
          strokeWidth={1.7}
          role="img"
          aria-label={`${name} vessel`}
        />
      ) : (
        // Species sprites are small, fixed-size assets and do not need an image optimizer.
        // eslint-disable-next-line @next/next/no-img-element
        <img
          src={source}
          width={40}
          height={40}
          alt={`${name} sprite`}
          loading="lazy"
          onError={() => setFailedSource(source)}
        />
      )}
    </span>
  );
}
