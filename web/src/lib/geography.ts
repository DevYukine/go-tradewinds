import type { Clock, DatasetMeta, ShipDetail } from "./types";

export interface Coordinate {
  latitude: number;
  longitude: number;
}

// WGS84 harbour/city coordinates, not catalog data. Historical names resolve to
// their modern ports: Danzig = Gdańsk, Königsberg = Kaliningrad, Batavia = Jakarta,
// Whampoa = Huangpu. The combined Calicut / Madras entry is placed at Calicut.
export const PORT_COORDINATES: Record<string, Coordinate> = {
  dover: { latitude: 51.125, longitude: 1.328 },
  calais: { latitude: 50.966, longitude: 1.855 },
  portsmouth: { latitude: 50.797, longitude: -1.107 },
  london: { latitude: 51.507, longitude: -0.078 },
  dunkirk: { latitude: 51.052, longitude: 2.376 },
  le_havre: { latitude: 49.493, longitude: 0.107 },
  ostend: { latitude: 51.232, longitude: 2.928 },
  amsterdam: { latitude: 52.378, longitude: 4.9 },
  newcastle: { latitude: 54.97, longitude: -1.601 },
  hamburg: { latitude: 53.543, longitude: 9.967 },
  bergen: { latitude: 60.394, longitude: 5.322 },
  copenhagen: { latitude: 55.687, longitude: 12.599 },
  stockholm: { latitude: 59.323, longitude: 18.084 },
  danzig: { latitude: 54.352, longitude: 18.657 },
  konigsberg: { latitude: 54.706, longitude: 20.487 },
  riga: { latitude: 56.951, longitude: 24.099 },
  nantes: { latitude: 47.207, longitude: -1.564 },
  bordeaux: { latitude: 44.842, longitude: -0.57 },
  lisbon: { latitude: 38.709, longitude: -9.135 },
  cadiz: { latitude: 36.53, longitude: -6.289 },
  madeira: { latitude: 32.645, longitude: -16.909 },
  tenerife: { latitude: 28.466, longitude: -16.246 },
  cape_verde: { latitude: 14.917, longitude: -23.508 },
  elmina: { latitude: 5.083, longitude: -1.35 },
  bridgetown: { latitude: 13.097, longitude: -59.62 },
  port_royal: { latitude: 17.938, longitude: -76.841 },
  santo_domingo: { latitude: 18.473, longitude: -69.882 },
  havana: { latitude: 23.139, longitude: -82.351 },
  cartagena: { latitude: 10.419, longitude: -75.55 },
  veracruz: { latitude: 19.201, longitude: -96.127 },
  st_helena: { latitude: -15.925, longitude: -5.717 },
  cape_town: { latitude: -33.906, longitude: 18.421 },
  zanzibar: { latitude: -6.157, longitude: 39.19 },
  muscat: { latitude: 23.621, longitude: 58.566 },
  goa: { latitude: 15.407, longitude: 73.799 },
  calicut: { latitude: 11.245, longitude: 75.771 },
  colombo: { latitude: 6.951, longitude: 79.844 },
  aceh: { latitude: 5.598, longitude: 95.319 },
  malacca: { latitude: 2.19, longitude: 102.247 },
  batavia: { latitude: -6.121, longitude: 106.809 },
  ternate: { latitude: 0.789, longitude: 127.389 },
  canton: { latitude: 23.095, longitude: 113.448 },
};

// Sea-zone centres are approximate route waypoints, never GPS ship positions.
const SEA_WAYPOINTS: Record<string, Coordinate> = {
  dover_strait: { latitude: 51.02, longitude: 1.59 },
  english_channel: { latitude: 50.1, longitude: -1.8 },
  thames_estuary: { latitude: 51.55, longitude: 1.2 },
  flemish_banks: { latitude: 51.65, longitude: 2.6 },
  zuiderzee_texel: { latitude: 53.1, longitude: 4.5 },
  dogger_north_sea: { latitude: 55.1, longitude: 3.3 },
  heligoland_bight: { latitude: 54.1, longitude: 7.8 },
  norwegian_trench: { latitude: 58.3, longitude: 4.2 },
  the_sound: { latitude: 55.9, longitude: 12.7 },
  gdansk_bay: { latitude: 54.7, longitude: 19.2 },
  central_baltic: { latitude: 57.1, longitude: 19.2 },
  gulf_of_riga: { latitude: 57.5, longitude: 23.6 },
  bay_of_biscay: { latitude: 46, longitude: -5.8 },
  iberian_coast: { latitude: 39.7, longitude: -10.4 },
  gulf_of_cadiz: { latitude: 36, longitude: -7.4 },
  macaronesia: { latitude: 28, longitude: -19 },
  guinea_current: { latitude: 7, longitude: -18 },
  lesser_antilles: { latitude: 16, longitude: -59.3 },
  caribbean_sea: { latitude: 16, longitude: -73 },
  spanish_main: { latitude: 14, longitude: -82 },
  florida_straits: { latitude: 24, longitude: -81.5 },
  south_atlantic: { latitude: -18, longitude: -10 },
  cape_of_good_hope: { latitude: -35.5, longitude: 18.5 },
  mozambique_channel: { latitude: -17, longitude: 42 },
  arabian_sea: { latitude: 16, longitude: 65 },
  bay_of_bengal: { latitude: 10, longitude: 84 },
  malacca_strait: { latitude: 4, longitude: 99 },
  java_sea: { latitude: -4.5, longitude: 115 },
  south_china_sea: { latitude: 15.5, longitude: 114 },
};

export function nodeCoordinate(node: string): Coordinate | undefined {
  return PORT_COORDINATES[node] ?? SEA_WAYPOINTS[node];
}

export function routeSegments(detail: ShipDetail) {
  return (detail.route_legs ?? []).flatMap((leg, index) => {
    const from = nodeCoordinate(leg.from_node);
    const to = nodeCoordinate(leg.to_node);
    return from && to ? [{ from, to, index }] : [];
  });
}

export function travelBearing(from: Coordinate, to: Coordinate): number {
  const radians = Math.PI / 180;
  const latitudeFrom = from.latitude * radians;
  const latitudeTo = to.latitude * radians;
  const longitude = (to.longitude - from.longitude) * radians;
  const north = Math.log(
    Math.tan(Math.PI / 4 + latitudeTo / 2) /
      Math.tan(Math.PI / 4 + latitudeFrom / 2),
  );
  return (Math.atan2(longitude, north) / radians + 360) % 360;
}

export function shipCoordinate(
  detail: ShipDetail,
  estimatedCurrentUTC: number,
): Coordinate | undefined {
  if (detail.ship.status === "sunk") {
    return undefined;
  }
  if (detail.ship.status !== "sailing") {
    return PORT_COORDINATES[detail.ship.current_port_id ?? ""];
  }
  const leg = detail.route_legs?.[detail.ship.current_leg_idx];
  if (!leg) {
    return undefined;
  }
  const from = nodeCoordinate(leg.from_node);
  const to = nodeCoordinate(leg.to_node);
  const departure = Date.parse(detail.ship.leg_departs_at ?? "");
  const arrival = Date.parse(detail.ship.leg_arrives_at ?? "");
  if (
    !from ||
    !to ||
    !Number.isFinite(estimatedCurrentUTC) ||
    !Number.isFinite(departure) ||
    !Number.isFinite(arrival) ||
    arrival <= departure
  ) {
    return undefined;
  }
  // The recorded leg timestamps are real UTC, already adjusted by the game.
  const progress = Math.max(
    0,
    Math.min(1, (estimatedCurrentUTC - departure) / (arrival - departure)),
  );
  return {
    latitude: from.latitude + (to.latitude - from.latitude) * progress,
    longitude: from.longitude + (to.longitude - from.longitude) * progress,
  };
}

export interface ChartPosition {
  coordinate: Coordinate;
  voyageKey: string;
  kind: "recorded" | "estimated" | "awaiting" | "frozen";
  label: string;
}

function observationAge(meta: DatasetMeta, serverUTC: number) {
  const updated = Date.parse(meta.updated_at ?? "");
  const upstream = meta.upstream_as_of
    ? Date.parse(meta.upstream_as_of)
    : updated;
  return Number.isFinite(serverUTC) &&
    Number.isFinite(updated) &&
    Number.isFinite(upstream)
    ? Math.max(0, serverUTC - Math.min(updated, upstream))
    : undefined;
}

function observationIssue(meta: DatasetMeta, serverUTC: number) {
  if (meta.error) {
    return "recording failed";
  }
  if (meta.complete === false) {
    return "recording incomplete";
  }
  if (meta.stale) {
    return "recording stale";
  }
  const age = observationAge(meta, serverUTC);
  if (meta.complete !== true || age === undefined) {
    return "freshness unknown";
  }
  return age >= 120_000 ? "recording stale" : undefined;
}

export function shipPosition(
  detail: ShipDetail,
  shipMeta: DatasetMeta,
  clock: Clock | undefined,
  clockMeta: DatasetMeta,
  serverUTC: number,
  previous?: ChartPosition,
): ChartPosition | undefined {
  const shipIssue = observationIssue(shipMeta, serverUTC);
  if (detail.ship.status !== "sailing") {
    const coordinate = shipCoordinate(detail, Number.NaN);
    return coordinate
      ? {
          coordinate,
          voyageKey: "",
          kind: "recorded",
          label: `Recorded port${shipIssue ? `; ${shipIssue}` : ""}`,
        }
      : undefined;
  }

  const leg = detail.route_legs?.[detail.ship.current_leg_idx];
  const voyageKey = [
    detail.ship.current_leg_idx,
    leg?.from_node,
    leg?.to_node,
    detail.ship.leg_departs_at,
    detail.ship.leg_arrives_at,
  ].join("|");
  const clockUTC = Date.parse(clock?.real_utc ?? "");
  const clockObserved = Date.parse(clockMeta.updated_at ?? "");
  const clockIssue = observationIssue(clockMeta, serverUTC);
  const unavailable =
    (clock?.paused ? "world paused" : undefined) ??
    (shipIssue ? `ship ${shipIssue}` : undefined) ??
    (!clock || !Number.isFinite(clockUTC)
      ? "recorded clock unavailable"
      : clockIssue
        ? `clock ${clockIssue}`
        : serverUTC - clockUTC >= 120_000
          ? "recorded clock stale"
          : undefined);

  if (unavailable) {
    return previous?.voyageKey === voyageKey
      ? {
          ...previous,
          kind: "frozen",
          label: `Estimate frozen; ${unavailable}. Awaiting a usable recording.`,
        }
      : undefined;
  }

  const estimatedCurrentUTC = clockUTC + Math.max(0, serverUTC - clockObserved);
  const coordinate = shipCoordinate(detail, estimatedCurrentUTC);
  if (!coordinate) {
    return undefined;
  }
  const awaiting =
    estimatedCurrentUTC >= Date.parse(detail.ship.leg_arrives_at ?? "");
  const delayed =
    (observationAge(shipMeta, serverUTC) ?? 0) >= 30_000 ||
    (observationAge(clockMeta, serverUTC) ?? 0) >= 30_000;
  return {
    coordinate,
    voyageKey,
    kind: awaiting ? "awaiting" : "estimated",
    label: awaiting
      ? "Estimated leg endpoint; scheduled arrival time reached, awaiting recording. Arrival is not confirmed."
      : `Estimated current-leg position${delayed ? "; delayed recording" : ""}`,
  };
}
