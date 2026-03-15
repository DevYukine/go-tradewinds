export interface Company {
  id: number
  game_id: string
  name: string
  ticker: string
  strategy: string
  status: string
  treasury: number
  reputation: number
  home_port_id: string
  agent_name: string
}

export interface PnLPoint {
  id: number
  company_id: number
  treasury: number
  total_costs: number
  total_rev: number
  passenger_rev: number
  ship_costs: number
  net_pnl: number
  ship_count: number
  created_at: string
}

export interface TradeLog {
  id: number
  company_id: number
  action: string
  good_id: string
  good_name: string
  port_id: string
  port_name: string
  quantity: number
  unit_price: number
  total_price: number
  tax_paid: number
  strategy: string
  agent_name: string
  created_at: string
}

export interface LogEntry {
  level: string
  message: string
  created_at: string
}

export interface StrategyMetric {
  id: number
  strategy_name: string
  company_count: number
  trades_executed: number
  total_profit: number
  total_loss: number
  avg_profit_per_trade: number
  std_dev_profit: number
  win_rate: number
  confidence_low: number
  confidence_high: number
  period_start: string
  period_end: string
  created_at: string
}

export interface AgentDecision {
  id: number
  company_id: number
  agent_name: string
  decision_type: string
  request: string
  response: string
  reasoning: string
  confidence: number
  latency_ms: number
  outcome: string
  outcome_value: number
  created_at: string
}

export interface RateLimitStatus {
  max_per_minute: number
  current_utilization: number
  used: number
  remaining: number
  resets_at: string
  active_companies: number
}

export interface CargoItem {
  good_id: string
  good_name: string
  quantity: number
  buy_price: number
}

export interface ShipDetail {
  ship_id: string
  ship_name: string
  ship_type: string
  capacity: number
  passenger_cap: number
  passenger_count: number
  speed: number
  upkeep: number
  status: string
  port_id?: string
  port_name?: string
  route_id?: string
  from_port_name?: string
  to_port_name?: string
  distance?: number
  arriving_at?: string
  cargo: CargoItem[]
  cargo_total: number
  cargo_value: number
}

export interface WarehouseItem {
  good_id: string
  good_name: string
  quantity: number
}

export interface WarehouseDetail {
  warehouse_id: string
  port_id: string
  port_name: string
  level: number
  capacity: number
  items: WarehouseItem[]
}

export interface CompanyInventory {
  company_id: string
  treasury: number
  total_upkeep: number
  cargo_value: number
  ships: ShipDetail[]
  warehouses: WarehouseDetail[]
}

// World data types
export interface PortInfo {
  id: string
  name: string
  code: string
  is_hub: boolean
  tax_rate: number
  has_shipyard: boolean
  latitude: number
  longitude: number
}

export interface MapShip {
  ship_id: string
  ship_name: string
  ship_type: string
  company_name: string
  company_id: string
  strategy: string
  status: string
  port_id?: string
  port_name?: string
  from_port_id?: string
  to_port_id?: string
  from_port_name?: string
  to_port_name?: string
  arriving_at?: string
  cargo_total: number
  capacity: number
  speed: number
  upkeep: number
  passenger_cap: number
  passenger_count: number
  cargo: CargoItem[]
}

export interface GlobalPnLCompany {
  company_id: number
  company_name: string
  strategy: string
  treasury: number
  trade_rev: number
  trade_costs: number
  passenger_rev: number
  net_pnl: number
  trade_count: number
  win_count: number
  win_rate: number
}

export interface GlobalPnLTotals {
  trade_rev: number
  trade_costs: number
  passenger_rev: number
  net_pnl: number
  trade_count: number
  win_count: number
  win_rate: number
}

export interface GlobalPnLResponse {
  companies: GlobalPnLCompany[]
  totals: GlobalPnLTotals
}

export interface GoodInfo {
  id: string
  name: string
  description: string
  category: string
}

export interface RouteInfo {
  id: string
  from_port_id: string
  to_port_id: string
  from_port_name: string
  to_port_name: string
  distance: number
}

export interface ShipTypeInfo {
  id: string
  name: string
  capacity: number
  passenger_cap: number
  speed: number
  upkeep: number
  base_price: number
}

export interface WorldData {
  ports: PortInfo[]
  goods: GoodInfo[]
  routes: RouteInfo[]
  ship_types: ShipTypeInfo[]
}

export interface PassengerLog {
  id: number
  company_id: number
  passenger_id: string
  count: number
  bid: number
  origin_port_id: string
  origin_port_name: string
  destination_port_id: string
  destination_port_name: string
  ship_id: string
  ship_name: string
  strategy: string
  agent_name: string
  created_at: string
}

export interface PriceEntry {
  port_id: string
  port_name: string
  good_id: string
  good_name: string
  buy_price: number
  sell_price: number
  spread: number
  updated_at: string
}

export interface GameTradeEntry {
  id: string
  buyer_id: string
  seller_id: string
  good_id: string
  good_name: string
  port_id: string
  port_name: string
  price: number
  quantity: number
  source: string
  occurred_at: string
}

// Analytics types

export interface GoodAnalytics {
  good_id: string
  good_name: string
  total_profit: number
  total_revenue: number
  total_cost: number
  trade_count: number
  total_quantity: number
  avg_profit_per_trade: number
  win_count: number
  loss_count: number
  win_rate: number
  best_profit: number
  worst_profit: number
  first_trade: string
  last_trade: string
}

export interface RouteAnalytics {
  from_port_id: string
  from_port_name: string
  to_port_id: string
  to_port_name: string
  total_profit: number
  trade_count: number
  total_quantity: number
  avg_profit_per_trade: number
  win_count: number
  loss_count: number
  win_rate: number
  top_good_id: string
  top_good_name: string
  first_trade: string
  last_trade: string
}

export interface TimelinePoint {
  time: string
  profit: number
  count: number
  quantity: number
}

export interface TimelineSeries {
  name: string
  points: TimelinePoint[]
  total_profit: number
}

export interface TimelineResponse {
  bucket_size: string
  hours: number
  series: TimelineSeries[]
}

export interface PassengerRouteAnalytics {
  origin_port_id: string
  origin_port_name: string
  destination_port_id: string
  destination_port_name: string
  total_revenue: number
  total_passengers: number
  snipe_count: number
  avg_bid: number
  max_bid: number
  first_snipe: string
  last_snipe: string
}

export interface PassengerShipAnalytics {
  ship_id: string
  ship_name: string
  total_revenue: number
  total_passengers: number
  snipe_count: number
  avg_bid: number
}

export interface PassengerAnalyticsResponse {
  summary: {
    total_revenue: number
    total_passengers: number
    total_snipes: number
    avg_bid_per_snipe: number
    top_ship_name: string
    top_ship_snipes: number
  }
  routes: PassengerRouteAnalytics[]
  ships: PassengerShipAnalytics[]
}
