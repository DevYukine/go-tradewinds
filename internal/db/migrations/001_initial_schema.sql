-- +goose Up

CREATE TABLE IF NOT EXISTS company_records (
    id             BIGSERIAL PRIMARY KEY,
    game_id        VARCHAR(255) NOT NULL,
    name           VARCHAR(255) NOT NULL,
    ticker         VARCHAR(5) NOT NULL,
    home_port_id   VARCHAR(255) NOT NULL,
    strategy       VARCHAR(255) NOT NULL,
    status         VARCHAR(255) NOT NULL DEFAULT 'running',
    treasury       BIGINT DEFAULT 0,
    reputation     BIGINT DEFAULT 0,
    created_at     TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMP NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_company_records_game_id ON company_records (game_id);
CREATE INDEX IF NOT EXISTS idx_company_records_status ON company_records (status);

CREATE TABLE IF NOT EXISTS trade_logs (
    id             BIGSERIAL PRIMARY KEY,
    company_id     BIGINT NOT NULL,
    action         VARCHAR(4) NOT NULL,
    good_id        VARCHAR(255) NOT NULL,
    good_name      VARCHAR(255) NOT NULL,
    port_id        VARCHAR(255) NOT NULL,
    port_name      VARCHAR(255) NOT NULL,
    quantity       INT NOT NULL,
    unit_price     INT NOT NULL,
    total_price    INT NOT NULL,
    tax_paid       INT DEFAULT 0,
    ship_id        VARCHAR(255) DEFAULT '',
    ship_name      VARCHAR(255) DEFAULT '',
    source         VARCHAR(255) DEFAULT '',
    dest_port_id   VARCHAR(255) DEFAULT '',
    dest_port_name VARCHAR(255) DEFAULT '',
    matched        BOOLEAN DEFAULT FALSE,
    strategy       VARCHAR(255) NOT NULL,
    agent_name     VARCHAR(255) DEFAULT '',
    created_at     TIMESTAMP NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_trade_company_time ON trade_logs (company_id, created_at);
CREATE INDEX IF NOT EXISTS idx_trade_company_action ON trade_logs (company_id, action);
CREATE INDEX IF NOT EXISTS idx_trade_buy_lookup ON trade_logs (company_id, action, good_id);

CREATE TABLE IF NOT EXISTS pn_l_snapshots (
    id               BIGSERIAL PRIMARY KEY,
    company_id       BIGINT NOT NULL,
    treasury         BIGINT NOT NULL,
    total_costs      BIGINT DEFAULT 0,
    total_rev        BIGINT DEFAULT 0,
    passenger_rev    BIGINT DEFAULT 0,
    ship_costs       BIGINT DEFAULT 0,
    net_pn_l         BIGINT DEFAULT 0,
    ship_count       INT DEFAULT 0,
    avg_capacity_util DOUBLE PRECISION DEFAULT 0,
    created_at       TIMESTAMP NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_pnl_company_time ON pn_l_snapshots (company_id, created_at);

CREATE TABLE IF NOT EXISTS ship_event_logs (
    id          BIGSERIAL PRIMARY KEY,
    company_id  BIGINT DEFAULT 0,
    ship_id     VARCHAR(255) DEFAULT '',
    ship_name   VARCHAR(255) DEFAULT '',
    ship_type   VARCHAR(255) DEFAULT '',
    event_type  VARCHAR(255) DEFAULT '',
    price       INT DEFAULT 0,
    treasury    INT DEFAULT 0,
    port_id     VARCHAR(255) DEFAULT '',
    port_name   VARCHAR(255) DEFAULT '',
    strategy    VARCHAR(255) DEFAULT '',
    agent_name  VARCHAR(255) DEFAULT '',
    created_at  TIMESTAMP NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_ship_event_logs_company_id ON ship_event_logs (company_id);
CREATE INDEX IF NOT EXISTS idx_ship_event_logs_created_at ON ship_event_logs (created_at);

CREATE TABLE IF NOT EXISTS warehouse_event_logs (
    id            BIGSERIAL PRIMARY KEY,
    company_id    BIGINT DEFAULT 0,
    warehouse_id  VARCHAR(255) DEFAULT '',
    port_id       VARCHAR(255) DEFAULT '',
    port_name     VARCHAR(255) DEFAULT '',
    event_type    VARCHAR(255) DEFAULT '',
    good_id       VARCHAR(255) DEFAULT '',
    good_name     VARCHAR(255) DEFAULT '',
    quantity      INT DEFAULT 0,
    level         INT DEFAULT 0,
    strategy      VARCHAR(255) DEFAULT '',
    agent_name    VARCHAR(255) DEFAULT '',
    created_at    TIMESTAMP NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_warehouse_event_logs_company_id ON warehouse_event_logs (company_id);
CREATE INDEX IF NOT EXISTS idx_warehouse_event_logs_created_at ON warehouse_event_logs (created_at);

CREATE TABLE IF NOT EXISTS p2p_order_logs (
    id          BIGSERIAL PRIMARY KEY,
    company_id  BIGINT DEFAULT 0,
    order_id    VARCHAR(255) DEFAULT '',
    order_type  VARCHAR(255) DEFAULT '',
    good_id     VARCHAR(255) DEFAULT '',
    good_name   VARCHAR(255) DEFAULT '',
    port_id     VARCHAR(255) DEFAULT '',
    port_name   VARCHAR(255) DEFAULT '',
    quantity    INT DEFAULT 0,
    price       INT DEFAULT 0,
    total_value INT DEFAULT 0,
    strategy    VARCHAR(255) DEFAULT '',
    agent_name  VARCHAR(255) DEFAULT '',
    created_at  TIMESTAMP NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_p2p_order_logs_company_id ON p2p_order_logs (company_id);
CREATE INDEX IF NOT EXISTS idx_p2p_order_logs_created_at ON p2p_order_logs (created_at);

CREATE TABLE IF NOT EXISTS strategy_change_logs (
    id             BIGSERIAL PRIMARY KEY,
    company_id     BIGINT DEFAULT 0,
    from_strategy  VARCHAR(255) DEFAULT '',
    to_strategy    VARCHAR(255) DEFAULT '',
    reason         VARCHAR(255) DEFAULT '',
    created_at     TIMESTAMP NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_strategy_change_logs_company_id ON strategy_change_logs (company_id);
CREATE INDEX IF NOT EXISTS idx_strategy_change_logs_created_at ON strategy_change_logs (created_at);

CREATE TABLE IF NOT EXISTS quote_failure_logs (
    id          BIGSERIAL PRIMARY KEY,
    company_id  BIGINT DEFAULT 0,
    ship_id     VARCHAR(255) DEFAULT '',
    good_id     VARCHAR(255) DEFAULT '',
    good_name   VARCHAR(255) DEFAULT '',
    port_id     VARCHAR(255) DEFAULT '',
    port_name   VARCHAR(255) DEFAULT '',
    action      VARCHAR(255) DEFAULT '',
    quantity    INT DEFAULT 0,
    exp_price   INT DEFAULT 0,
    act_price   INT DEFAULT 0,
    reason      VARCHAR(255) DEFAULT '',
    strategy    VARCHAR(255) DEFAULT '',
    created_at  TIMESTAMP NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_quote_failure_logs_company_id ON quote_failure_logs (company_id);
CREATE INDEX IF NOT EXISTS idx_quote_failure_logs_created_at ON quote_failure_logs (created_at);

CREATE TABLE IF NOT EXISTS strategy_metrics (
    id                 BIGSERIAL PRIMARY KEY,
    strategy_name      VARCHAR(255) NOT NULL,
    company_count      INT NOT NULL,
    trades_executed    INT DEFAULT 0,
    total_profit       BIGINT DEFAULT 0,
    total_loss         BIGINT DEFAULT 0,
    avg_profit_per_trade DOUBLE PRECISION DEFAULT 0,
    std_dev_profit     DOUBLE PRECISION DEFAULT 0,
    win_rate           DOUBLE PRECISION DEFAULT 0,
    confidence_low     DOUBLE PRECISION DEFAULT 0,
    confidence_high    DOUBLE PRECISION DEFAULT 0,
    period_start       TIMESTAMP,
    period_end         TIMESTAMP,
    created_at         TIMESTAMP NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_strategy_metrics_strategy_name ON strategy_metrics (strategy_name);

CREATE TABLE IF NOT EXISTS company_logs (
    id          BIGSERIAL PRIMARY KEY,
    company_id  BIGINT NOT NULL,
    level       VARCHAR(10) NOT NULL,
    message     TEXT NOT NULL,
    created_at  TIMESTAMP NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_log_company_time ON company_logs (company_id, created_at);

CREATE TABLE IF NOT EXISTS price_observations (
    id          BIGSERIAL PRIMARY KEY,
    port_id     VARCHAR(255) NOT NULL,
    good_id     VARCHAR(255) NOT NULL,
    buy_price   INT NOT NULL,
    sell_price  INT NOT NULL,
    created_at  TIMESTAMP NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_price_port_good ON price_observations (port_id, good_id);
CREATE INDEX IF NOT EXISTS idx_price_observations_created_at ON price_observations (created_at);

CREATE TABLE IF NOT EXISTS agent_decision_logs (
    id             BIGSERIAL PRIMARY KEY,
    company_id     BIGINT NOT NULL,
    agent_name     VARCHAR(255) NOT NULL,
    decision_type  VARCHAR(20) NOT NULL,
    request        TEXT,
    response       TEXT,
    reasoning      TEXT,
    confidence     DOUBLE PRECISION DEFAULT 0,
    latency_ms     BIGINT DEFAULT 0,
    outcome        VARCHAR(10) DEFAULT '',
    outcome_value  BIGINT DEFAULT 0,
    created_at     TIMESTAMP NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_decision_company_time ON agent_decision_logs (company_id, created_at);

CREATE TABLE IF NOT EXISTS route_performances (
    id           BIGSERIAL PRIMARY KEY,
    company_id   BIGINT NOT NULL,
    from_port_id VARCHAR(255) NOT NULL,
    to_port_id   VARCHAR(255) NOT NULL,
    good_id      VARCHAR(255) NOT NULL,
    buy_price    INT NOT NULL,
    sell_price   INT NOT NULL,
    quantity     INT NOT NULL,
    profit       INT NOT NULL,
    strategy     VARCHAR(255) NOT NULL,
    created_at   TIMESTAMP NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_route_company_time ON route_performances (company_id, created_at);

CREATE TABLE IF NOT EXISTS passenger_logs (
    id                    BIGSERIAL PRIMARY KEY,
    company_id            BIGINT NOT NULL,
    passenger_id          VARCHAR(255) NOT NULL,
    count                 INT NOT NULL,
    bid                   INT NOT NULL,
    origin_port_id        VARCHAR(255) NOT NULL,
    origin_port_name      VARCHAR(255) NOT NULL,
    destination_port_id   VARCHAR(255) NOT NULL,
    destination_port_name VARCHAR(255) NOT NULL,
    ship_id               VARCHAR(255) NOT NULL,
    ship_name             VARCHAR(255) NOT NULL,
    strategy              VARCHAR(255) NOT NULL,
    agent_name            VARCHAR(255) DEFAULT '',
    created_at            TIMESTAMP NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_passenger_company_time ON passenger_logs (company_id, created_at);

CREATE TABLE IF NOT EXISTS company_params (
    id                         BIGSERIAL PRIMARY KEY,
    company_id                 BIGINT NOT NULL,
    min_margin_pct             DOUBLE PRECISION NOT NULL DEFAULT 0.05,
    passenger_weight           DOUBLE PRECISION NOT NULL DEFAULT 5.0,
    speculative_trade_enabled  BOOLEAN NOT NULL DEFAULT TRUE,
    market_eval_interval_sec   INT NOT NULL DEFAULT 60,
    fleet_eval_interval_sec    INT NOT NULL DEFAULT 180,
    passenger_dest_bonus       DOUBLE PRECISION NOT NULL DEFAULT 5.0,
    agent_type                 VARCHAR(20) NOT NULL DEFAULT 'heuristic',
    llm_provider               VARCHAR(20) DEFAULT '',
    llm_model                  VARCHAR(100) DEFAULT '',
    created_at                 TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at                 TIMESTAMP NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_company_params_company_id ON company_params (company_id);

CREATE TABLE IF NOT EXISTS param_experiment_logs (
    id             BIGSERIAL PRIMARY KEY,
    company_id     BIGINT NOT NULL,
    param_name     VARCHAR(255) NOT NULL,
    old_value      DOUBLE PRECISION NOT NULL,
    new_value      DOUBLE PRECISION NOT NULL,
    profit_before  DOUBLE PRECISION DEFAULT 0,
    profit_after   DOUBLE PRECISION DEFAULT 0,
    status         VARCHAR(20) NOT NULL DEFAULT 'active',
    created_at     TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMP NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_experiment_company ON param_experiment_logs (company_id);
CREATE INDEX IF NOT EXISTS idx_param_experiment_logs_status ON param_experiment_logs (status);
CREATE INDEX IF NOT EXISTS idx_param_experiment_logs_created_at ON param_experiment_logs (created_at);

-- +goose Down

DROP TABLE IF EXISTS param_experiment_logs;
DROP TABLE IF EXISTS company_params;
DROP TABLE IF EXISTS passenger_logs;
DROP TABLE IF EXISTS route_performances;
DROP TABLE IF EXISTS agent_decision_logs;
DROP TABLE IF EXISTS price_observations;
DROP TABLE IF EXISTS company_logs;
DROP TABLE IF EXISTS strategy_metrics;
DROP TABLE IF EXISTS quote_failure_logs;
DROP TABLE IF EXISTS strategy_change_logs;
DROP TABLE IF EXISTS p2p_order_logs;
DROP TABLE IF EXISTS warehouse_event_logs;
DROP TABLE IF EXISTS ship_event_logs;
DROP TABLE IF EXISTS pn_l_snapshots;
DROP TABLE IF EXISTS trade_logs;
DROP TABLE IF EXISTS company_records;
