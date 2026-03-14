export interface Company {
  id: number
  game_id: string
  name: string
  ticker: string
  strategy: string
  status: 'running' | 'paused' | 'error' | 'bankrupt'
  treasury: number
  reputation: number
}

export interface PnLPoint {
  id: number
  treasury: number
  net_pnl: number
  ship_count: number
  created_at: string
}

export interface TradeLog {
  id: number
  action: 'buy' | 'sell'
  good_name: string
  port_name: string
  quantity: number
  unit_price: number
  total_price: number
  strategy: string
  agent_name: string
  created_at: string
}

export interface LogEntry {
  level: 'info' | 'warn' | 'error' | 'trade' | 'event' | 'optimizer' | 'agent'
  message: string
  created_at: string
}

export interface StrategyMetric {
  strategy_name: string
  company_count: number
  trades_executed: number
  total_profit: number
  avg_profit_per_trade: number
  std_dev_profit: number
  win_rate: number
  confidence_low: number
  confidence_high: number
  period_start: string
  period_end: string
}

export interface AgentDecision {
  id: number
  agent_name: string
  decision_type: 'trade' | 'fleet' | 'market' | 'strategy_eval'
  reasoning: string
  confidence: number
  latency_ms: number
  outcome: 'profit' | 'loss' | 'neutral' | 'pending'
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
