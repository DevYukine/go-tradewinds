-- +goose Up

-- FIFO buy matching: WHERE company_id=? AND action='buy' AND good_id=? AND matched=false
-- ORDER BY created_at ASC. Extends idx_trade_buy_lookup with matched + created_at for
-- index-only scans during sell-side route performance recording.
CREATE INDEX IF NOT EXISTS idx_trade_fifo_match
    ON trade_logs (company_id, action, good_id, matched, created_at);

-- Market decision queries: WHERE company_id=? AND decision_type=? ORDER BY created_at DESC.
CREATE INDEX IF NOT EXISTS idx_decision_company_type_time
    ON agent_decision_logs (company_id, decision_type, created_at);

-- Ship analytics: WHERE event_type='purchase' without company filter.
CREATE INDEX IF NOT EXISTS idx_ship_event_type
    ON ship_event_logs (event_type, created_at);

-- Strategy metrics ordered by time: ORDER BY created_at DESC LIMIT N.
CREATE INDEX IF NOT EXISTS idx_strategy_metrics_created_at
    ON strategy_metrics (created_at);

-- Parameter experiment direction lookup: WHERE company_id=? AND param_name=?
-- ORDER BY created_at DESC.
CREATE INDEX IF NOT EXISTS idx_experiment_company_param_time
    ON param_experiment_logs (company_id, param_name, created_at);

-- +goose Down

DROP INDEX IF EXISTS idx_experiment_company_param_time;
DROP INDEX IF EXISTS idx_strategy_metrics_created_at;
DROP INDEX IF EXISTS idx_ship_event_type;
DROP INDEX IF EXISTS idx_decision_company_type_time;
DROP INDEX IF EXISTS idx_trade_fifo_match;
