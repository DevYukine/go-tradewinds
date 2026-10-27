import { count, money } from "./format";
import type { EngineWarning } from "./types";

export function warningTime(value: string): string {
  const timestamp = Date.parse(value);
  return Number.isFinite(timestamp)
    ? new Date(timestamp).toISOString().slice(11, 16)
    : "Not recorded";
}

function duration(minutes: number): string {
  return minutes < 60
    ? `${count(minutes)} min`
    : `${count(Math.floor(minutes / 60))} h ${count(minutes % 60)} min`;
}

export function warningText(
  warning: EngineWarning,
  accountLabel?: string,
): string {
  const reason = warning.reason.trim().replace(/[.!?]+$/, "");
  let message: string;
  switch (warning.code) {
    case "controller_stalled":
      message = `Controller has not completed a check for ${count(warning.facts.minutes)} min. Check the log, fix, then run scripts/restart-bot.ps1.`;
      break;
    case "refresh_failing":
      message = `Recorded data refresh has failed for ${count(warning.facts.minutes)} min (${warning.names.datasets}).`;
      if (reason) {
        message += ` ${reason}.`;
      }
      message +=
        " Check the log, fix the refresh failure, then run scripts/restart-bot.ps1.";
      break;
    case "fleet_idle":
      message = `${count(warning.facts.idle)} of ${count(warning.facts.total)} ships idle in port 30+ min; longest ${warning.names.ship}, ${duration(warning.facts.minutes)}${warning.names.since_source === "arrival_event" ? " since recorded arrival" : ""} at ${warning.names.port}.`;
      if (warning.facts.unknown > 0) {
        message += ` ${count(warning.facts.unknown)} ${warning.facts.unknown === 1 ? "ship has" : "ships have"} unknown idle time.`;
      }
      break;
    case "hold_stuck":
      message = `${warning.names.ship} idle at ${warning.names.port} ${duration(warning.facts.minutes)}${warning.names.since_source === "arrival_event" ? " since recorded arrival" : ""}, held${reason ? `: ${reason}` : ""}.`;
      if (warning.facts.other_count > 0) {
        message += ` ${count(warning.facts.other_count)} other ${warning.facts.other_count === 1 ? "ship also held" : "ships also held"}.`;
      }
      break;
    case "loss_streak":
      message = `Last ${count(warning.facts.count)} settlements lost ${money(Math.abs(warning.facts.net_pence))} since ${warningTime(warning.since)}.`;
      break;
    case "falling_behind":
      message = `Our 24 h value change is ${money(warning.facts.ours_pence)}; rival median ${money(warning.facts.median_pence)} (${count(warning.facts.rival_count)} rivals).`;
      break;
  }
  return accountLabel ? `${accountLabel}: ${message}` : message;
}
