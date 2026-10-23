const countFormat = new Intl.NumberFormat("en-GB", {
  maximumFractionDigits: 1,
});
const crownFormat = new Intl.NumberFormat("en-GB", {
  minimumFractionDigits: 2,
  maximumFractionDigits: 2,
});

export function count(value: number | null | undefined): string {
  return value === null || value === undefined || !Number.isFinite(value)
    ? "Not recorded"
    : countFormat.format(value);
}

export function money(
  pence: number | null | undefined,
  compact = false,
): string {
  if (pence === null || pence === undefined || !Number.isFinite(pence)) {
    return "Not recorded";
  }
  const crowns = pence / 100;
  if (compact && Math.abs(crowns) >= 100_000) {
    const divisor = Math.abs(crowns) >= 1_000_000 ? 1_000_000 : 1_000;
    return `${countFormat.format(crowns / divisor)}${divisor === 1_000_000 ? "m" : "k"} Cr`;
  }
  return `${crownFormat.format(crowns)} Cr`;
}

export function humanize(value: string | null | undefined): string {
  if (!value) {
    return "Not recorded";
  }
  const words = value
    .replaceAll("_", " ")
    .replace(/[\u2013\u2014]/g, " ")
    .toLowerCase();
  return words.charAt(0).toUpperCase() + words.slice(1);
}

export function dateTime(value: string | null | undefined): string {
  if (!value || !Number.isFinite(Date.parse(value))) {
    return "Not recorded";
  }
  return (
    new Intl.DateTimeFormat("en-GB", {
      day: "2-digit",
      month: "short",
      hour: "2-digit",
      minute: "2-digit",
      timeZone: "UTC",
    }).format(new Date(value)) + " UTC"
  );
}

export function age(value: string | null | undefined, now: number): string {
  if (!value || !Number.isFinite(Date.parse(value))) {
    return "Not recorded";
  }
  if (now === 0) {
    return dateTime(value);
  }
  const seconds = Math.max(0, (now - Date.parse(value)) / 1000);
  if (seconds < 60) {
    return `${Math.floor(seconds)}s ago`;
  }
  if (seconds < 3600) {
    return `${Math.floor(seconds / 60)}m ago`;
  }
  if (seconds < 86400) {
    return `${Math.floor(seconds / 3600)}h ago`;
  }
  return `${Math.floor(seconds / 86400)}d ago`;
}

export function eta(value: string | null | undefined, now: number): string {
  if (!now || !value || !Number.isFinite(Date.parse(value))) {
    return "Not recorded";
  }
  const minutes = Math.ceil((Date.parse(value) - now) / 60_000);
  if (minutes <= 0) {
    return "Awaiting arrival";
  }
  if (minutes < 60) {
    return `${minutes}m`;
  }
  return `${Math.floor(minutes / 60)}h ${minutes % 60}m`;
}

export function percent(bps: number | null | undefined): string {
  return bps === null || bps === undefined || !Number.isFinite(bps)
    ? "Not recorded"
    : `${count(bps / 100)}%`;
}
