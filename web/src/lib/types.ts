export type JsonRecord = Record<string, unknown>;

export interface DatasetMeta {
  updated_at?: string;
  attempted_at?: string;
  upstream_as_of?: string;
  error?: string;
  stale?: boolean;
  generation?: string;
  complete?: boolean;
}

export interface EngineHold {
  ship_id: string;
  reason: string;
}

export interface EngineState {
  mode: "observe" | "execute";
  generation: string;
  observed_at: string;
  next_check_at: string;
  holds: EngineHold[];
  assessed_ship_ids: string[];
}

export interface Account {
  id: string;
  label: string;
  company_id: string;
  enabled: boolean;
  created_at: string;
  updated_at?: string | null;
  snapshot: Record<string, unknown>;
}

export interface Metric {
  id: number;
  account_id: string;
  name: string;
  value: number;
  recorded_at: string;
}

// Pence and game days retain the units recorded by the planner. Historical
// decisions can omit fields, including the destination valuation basis.
export interface Economics {
  basis?: "destination";
  revenue_pence?: number;
  purchase_pence?: number;
  customs_pence?: number;
  provisions_pence?: number;
  store_cash_pence?: number;
  berth_dues_pence?: number;
  upkeep_pence?: number;
  toll_pence?: number;
  collateral_pence?: number;
  expected_net_pence?: number;
  stress_haircut_bps?: number;
  stress_haircut_pence?: number;
  price_uncertainty_stress_pence?: number;
  stress_net_pence?: number;
  game_days?: number;
  net_pence_per_game_day?: number;
  ballast_repeat_game_days?: number;
  ballast_repeat_net_pence?: number;
  probability_estimate?: number | null;
}

export interface Decision {
  account_id: string;
  ship_id: string;
  kind: string;
  reason: string;
  details: unknown;
  created_at: string;
}

export interface GameEvent {
  account_id: string;
  event_id: string;
  event_type: string;
  payload: unknown;
  created_at: string;
}

export interface RequestLog {
  account_id: string;
  method: string;
  path: string;
  status: number;
  duration_ms: number;
  error: string;
  request?: unknown;
  response?: unknown;
  created_at: string;
}

export interface Settlement {
  id: string;
  account_id: string;
  ship_id: string;
  mission_id: string;
  lane: string;
  action_id: string;
  kind: string;
  origin: string;
  destination: string;
  commodity: string;
  quantity_tons: number;
  game_seconds: number;
  costs_known: boolean;
  acquisition_pence: number;
  proceeds_pence: number;
  fees_pence: number;
  provisions_pence: number;
  expenses_pence: number;
  started_at: string;
  completed_at: string;
  created_at: string;
}

export interface DashboardData {
  updated_at: string;
  accounts: Account[];
  metrics: Metric[];
  decisions: Decision[];
  events: GameEvent[];
  requests: RequestLog[];
  settlements: Settlement[];
}

export interface Port {
  id: string;
  name: string;
  banking_region: string;
  sea_zone_id: string;
  phase: number;
  is_hub: boolean;
  has_shipyard: boolean;
  customs_bps: number;
  base_berths: number;
  effective_berths: number;
  total_warehouse_cap: number;
  produces: string[];
  manufactures: string[];
  imports: string[];
}

export interface PortDetail extends Port {
  health_score_bps: number;
  occupied_berths: number;
  queued_ships: number;
  berth_dues_per_day_pence: number;
  visibility: string;
}

export interface ShipClass {
  id: string;
  name: string;
  hull_category: string;
  phase: number;
  base_speed_centiknots: number;
  hold_capacity_tons: number;
  max_cannons: number;
  crew_required: number;
  base_labor_cost_pence: number;
  build_materials: { commodity_id: string; quantity: number }[];
}

export interface Catalog {
  ports: Port[];
  ship_classes: ShipClass[];
  commodities: {
    id: string;
    name: string;
    category: string;
    base_price_pence: number;
  }[];
  sea_zones: {
    id: string;
    name: string;
    zone_class: string;
    phase: number;
    base_storm_bps: number;
    base_piracy_bps: number;
  }[];
}

export interface RouteLeg {
  from_node: string;
  to_node: string;
  distance_nm: number;
  edge_class: string;
  is_sound_toll: boolean;
}

export interface ShipDetail {
  ship: {
    id: string;
    company_id: string;
    name: string;
    ship_class: string;
    status: string;
    current_port_id?: string | null;
    current_leg_idx: number;
    cannons_count: number;
    stores_provisions_units: number;
    turnaround_unlocks_at?: string | null;
    dock_queued_at?: string | null;
    leg_departs_at?: string | null;
    leg_arrives_at?: string | null;
    berthed_at?: string | null;
    convoy_id?: string | null;
    created_at?: string;
    updated_at: string;
  };
  hold_capacity_tons: number;
  effective_hold_capacity_tons: number;
  cargo_used_tons: number;
  cargo_manifest: Record<string, number>;
  stores: { provisions_tons: number; sea_days_remaining: number };
  route_legs: RouteLeg[];
  voyage?: {
    current_leg_idx: number;
    speed_centiknots: number;
    estimated_arrival_at: string;
    legs: {
      leg_idx: number;
      from_node: string;
      to_node: string;
      distance_nm: number;
      speed_centiknots: number;
      estimated_arrival_at: string;
      starving: boolean;
    }[];
  } | null;
}

export interface Vault {
  account_id: string;
  banking_region: string;
  account_type: string;
  balance_pence: number;
}

export interface CompanyProfile {
  company: {
    id: string;
    name: string;
    home_port_id: string;
    crown_prestige: number;
    created_at: string;
    player_id?: string;
    updated_at?: string;
  };
  handle: string;
  fleet_count: number;
  factor_ports: string[];
  vaults: Vault[];
}

export interface Warehouse {
  port_id: string;
  has_factor: boolean;
  leased_tonnage: number;
  reserved_tonnage: number;
  used_tonnage: number;
  free_tonnage: number;
  inventory: {
    commodity_id: string;
    quantity: number;
    locked_quantity: number;
    updated_at: string;
  }[];
}

export interface YardQuote {
  port_id: string;
  ship_class: string;
  build_mode: string;
  labour_pence: number;
  materials_pence: number;
  yard_markup_bps: number;
  yard_markup_pence: number;
  total_cost_pence?: number | null;
  available: boolean;
  materials: {
    commodity_id: string;
    quantity: number;
    in_stock: boolean;
    cost_pence?: number | null;
  }[];
}

export interface Contract {
  id: string;
  title?: string | null;
  contract_type: string;
  origin_port_id: string;
  dest_port_id?: string | null;
  commodity_id?: string | null;
  quantity_required: number;
  quantity_delivered: number;
  remaining_quantity: number;
  reward_pence: number;
  reward_per_ton_pence?: number | null;
  collateral_pence: number;
  status: string;
  stale: boolean;
  carrier_ship_id?: string | null;
  issuer_company_id?: string | null;
  carrier_company_id?: string | null;
  is_public?: boolean;
  expires_at?: string | null;
  metadata: unknown;
}

export interface Clock {
  real_utc: string;
  season_number: number;
  unlocked_phase: number;
  global_health_bps: number;
  active_player_volume_24h: number;
  paused: boolean;
  season_planned_end_at?: string | null;
  calendar: {
    year: number;
    month: number;
    day: number;
    hour: number;
    minute: number;
    season: string;
    time_scale: number;
    total_game_hours: number;
  };
}

export interface Leaderboard {
  season_number: number;
  ranked_players: number;
  refreshed_at?: string | null;
  next_refresh_at?: string | null;
  entries: {
    rank: number;
    company_name: string;
    home_port_id: string;
    net_worth_pence: number;
    titles: string[];
  }[];
  you?: {
    rank: number;
    company_name: string;
    net_worth_pence: number;
    is_calling_company: boolean;
  } | null;
}
