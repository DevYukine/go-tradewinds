import { moneyAxisTicks } from "./money-history";
import type { StandingSeries } from "./types";

export type StandingSelection = "rivals" | "5" | "10" | "20";

export function standingKey(player: StandingSeries) {
  return `${player.season_number}:${player.company_name}`;
}

export function standingsChart(
  players: readonly StandingSeries[],
  selection: StandingSelection,
  hidden: ReadonlySet<string>,
) {
  let charted = players.filter((player) => player.charted);
  if (selection === "rivals") {
    const own = charted
      .filter((player) => player.is_us)
      .sort((a, b) => Date.parse(b.last_at) - Date.parse(a.last_at))[0];
    charted = own
      ? charted.filter(
          (player) =>
            player === own ||
            (player.season_number === own.season_number &&
              player.last_at === own.last_at &&
              Math.abs(player.rank - own.rank) === 1),
        )
      : [];
  }
  const visible = charted.filter((player) => !hidden.has(standingKey(player)));
  return {
    charted,
    visible,
    ticks: moneyAxisTicks(
      visible.flatMap((player) =>
        player.points.map((point) => point.net_worth_pence),
      ),
    ),
  };
}
