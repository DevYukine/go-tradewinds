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
  active_companies: number
}

export interface CargoItem {
  good_id: string
  good_name: string
  quantity: number
}

export interface ShipDetail {
  ship_id: string
  ship_name: string
  ship_type: string
  capacity: number
  passenger_cap: number
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
  company_name: string
  company_id: string
  strategy: string
  status: string
  port_id?: string
  from_port_id?: string
  to_port_id?: string
  arriving_at?: string
  cargo_total: number
  capacity: number
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
