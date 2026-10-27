import { count, dateTime, humanize, money } from "./format";
import type { RivalFact, RivalHunch } from "./types";

const hunches: Record<RivalHunch, { label: string; rank: number }> = {
  most_dangerous: { label: "Most dangerous", rank: 0 },
  plays_rough: { label: "Plays rough", rank: 1 },
  threat: { label: "Threat", rank: 2 },
  losing_value: { label: "Losing value", rank: 3 },
  growing: { label: "Growing", rank: 4 },
  steady: { label: "Steady", rank: 5 },
  too_new: { label: "Too new to judge", rank: 6 },
  unmeasured: { label: "Not measured", rank: 7 },
  not_ranked: { label: "Not ranked", rank: 8 },
};

export function hunchLabel(hunch: RivalHunch | null | undefined): string {
  return hunch == null ? "Not recorded" : hunches[hunch].label;
}

export function hunchTone(hunch: RivalHunch | null | undefined) {
  return hunch === "most_dangerous"
    ? "danger"
    : hunch === "plays_rough"
      ? "warning"
      : "neutral";
}

export function hunchRank(hunch: RivalHunch | null | undefined): number {
  return hunch == null ? 9 : hunches[hunch].rank;
}

export function rivalFactText(
  fact: RivalFact | null | undefined,
  now?: number,
): string {
  if (fact == null) return "Not recorded";

  switch (fact.kind) {
    case "shared_lanes":
    case "shared_ports":
      if (fact.count == null || fact.total == null) return "Not recorded";
      return fact.kind === "shared_lanes"
        ? `On ${count(fact.count)} of our ${count(fact.total)} lanes`
        : `At ${count(fact.count)} of our ${count(fact.total)} ports`;
    case "larger":
      return fact.net_worth_pence != null &&
        fact.net_worth_pence > 0 &&
        fact.our_net_worth_pence != null &&
        fact.our_net_worth_pence > 0
        ? `${(fact.net_worth_pence / fact.our_net_worth_pence).toFixed(1)}× our net worth`
        : "Largest rival ahead of us";
    case "monopoly_in_force": {
      const expires = Date.parse(fact.expires_at ?? "");
      if (!fact.commodity || !fact.region || !Number.isFinite(expires)) {
        return "Not recorded";
      }
      const expiry = new Date(expires).toISOString();
      const sameDay =
        now === undefined ||
        (now > 0 &&
          expiry.slice(0, 10) === new Date(now).toISOString().slice(0, 10));
      const until = sameDay
        ? `${expiry.slice(11, 16)} UTC`
        : dateTime(fact.expires_at);
      return `Monopoly on ${humanize(fact.commodity)} in ${humanize(fact.region)} until ${until}`;
    }
    case "monopolies_recent":
      if (fact.count == null) return "Not recorded";
      return `${count(fact.count)} ${fact.count === 1 ? "monopoly" : "monopolies"} granted in 7 days`;
    case "loan_walkaways":
    case "loan_defaults":
      if (fact.count == null) return "Not recorded";
      return `${fact.kind === "loan_walkaways" ? "Walked away from" : "Defaulted on"} ${count(fact.count)} ${fact.count === 1 ? "loan" : "loans"}`;
    case "value_change": {
      if (fact.change_pence == null) return "Not recorded";
      const sign = fact.change_pence > 0 ? "+" : "";
      const percentage =
        fact.baseline_pence != null && fact.baseline_pence > 0
          ? ` (${sign}${((fact.change_pence / fact.baseline_pence) * 100).toFixed(1)}%)`
          : "";
      return `${sign}${money(fact.change_pence)} in 24 h${percentage}`;
    }
    case "negative_worth":
      return "Net worth below zero";
    case "short_history":
      return fact.hours == null
        ? "Not recorded"
        : `Only ${count(fact.hours)} h of valuations`;
    case "no_baseline":
      return "No valuation near 24 h ago";
    case "ships_seen":
      if (fact.count == null) return "Not recorded";
      return `${count(fact.count)} ${fact.count === 1 ? "ship" : "ships"} seen in 24 h`;
  }
}
