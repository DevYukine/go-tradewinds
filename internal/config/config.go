package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
	"go.uber.org/fx"
)

// Module provides the application configuration to the fx DI container.
var Module = fx.Module("config",
	fx.Provide(Load),
)

// Config holds all application configuration loaded from environment variables.
type Config struct {
	DB                 DBConfig
	BaseURL            string
	APIPort            int
	PlayerEmail        string
	PlayerPassword     string
	StrategyAllocation []StrategyAlloc
	RateLimitPerMinute int
	Agent              AgentConfig
}

// DBConfig holds PostgreSQL connection parameters.
type DBConfig struct {
	Host     string
	Port     int
	User     string
	Password string
	Name     string
	SSLMode  string
}

// DSN returns the PostgreSQL connection string.
func (c DBConfig) DSN() string {
	return fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		c.Host, c.Port, c.User, c.Password, c.Name, c.SSLMode,
	)
}

// StrategyAlloc maps a strategy name to the number of companies that should run it.
type StrategyAlloc struct {
	Strategy string
	Count    int
}

// AgentConfig holds configuration for the AI agent integration layer.
type AgentConfig struct {
	Type               string // "heuristic", "llm", "composite"
	LLMProvider        string // "claude", "openai", "ollama"
	LLMModel           string
	LLMAPIKey          string
	LLMMaxTokens       int
	CompositeFastAgent string
	CompositeSlowAgent string
}

// Load reads the .env file and parses all configuration values.
// Missing required values cause a descriptive error.
func Load() (*Config, error) {
	// Load .env file if present; ignore errors (env vars may be set directly).
	_ = godotenv.Load()

	cfg := &Config{
		DB: DBConfig{
			Host:     envOrDefault("DB_HOST", "localhost"),
			Port:     envIntOrDefault("DB_PORT", 5432),
			User:     envOrDefault("DB_USER", "tradewinds"),
			Password: envOrDefault("DB_PASSWORD", ""),
			Name:     envOrDefault("DB_NAME", "tradewinds"),
			SSLMode:  envOrDefault("DB_SSLMODE", "disable"),
		},
		BaseURL:            envOrDefault("BOT_BASE_URL", "https://tradewinds.fly.dev"),
		APIPort:            envIntOrDefault("API_PORT", 3001),
		PlayerEmail:        os.Getenv("PLAYER_EMAIL"),
		PlayerPassword:     os.Getenv("PLAYER_PASSWORD"),
		RateLimitPerMinute: envIntOrDefault("RATE_LIMIT_PER_MINUTE", 300),
		Agent: AgentConfig{
			Type:               envOrDefault("AGENT_TYPE", "heuristic"),
			LLMProvider:        os.Getenv("LLM_PROVIDER"),
			LLMModel:           os.Getenv("LLM_MODEL"),
			LLMAPIKey:          os.Getenv("LLM_API_KEY"),
			LLMMaxTokens:       envIntOrDefault("LLM_MAX_TOKENS", 4096),
			CompositeFastAgent: envOrDefault("COMPOSITE_FAST_AGENT", "heuristic"),
			CompositeSlowAgent: envOrDefault("COMPOSITE_SLOW_AGENT", "llm"),
		},
	}

	// Parse strategy allocation.
	allocStr := envOrDefault("STRATEGY_ALLOCATION", "arbitrage:3,bulk_hauler:2,market_maker:2")
	allocs, err := parseStrategyAllocation(allocStr)
	if err != nil {
		return nil, fmt.Errorf("invalid STRATEGY_ALLOCATION: %w", err)
	}
	cfg.StrategyAllocation = allocs

	// Validate required fields.
	if cfg.PlayerEmail == "" {
		return nil, fmt.Errorf("PLAYER_EMAIL is required")
	}
	if cfg.PlayerPassword == "" {
		return nil, fmt.Errorf("PLAYER_PASSWORD is required")
	}

	return cfg, nil
}

// TotalCompanies returns the total number of companies across all strategies.
func (c *Config) TotalCompanies() int {
	total := 0
	for _, alloc := range c.StrategyAllocation {
		total += alloc.Count
	}
	return total
}

// ToJSON returns a redacted JSON representation for logging.
func (c *Config) ToJSON() string {
	redacted := struct {
		BaseURL            string         `json:"base_url"`
		APIPort            int            `json:"api_port"`
		DBHost             string         `json:"db_host"`
		DBName             string         `json:"db_name"`
		PlayerEmail        string         `json:"player_email"`
		RateLimitPerMinute int            `json:"rate_limit_per_minute"`
		AgentType          string         `json:"agent_type"`
		StrategyAllocation []StrategyAlloc `json:"strategy_allocation"`
		TotalCompanies     int            `json:"total_companies"`
	}{
		BaseURL:            c.BaseURL,
		APIPort:            c.APIPort,
		DBHost:             c.DB.Host,
		DBName:             c.DB.Name,
		PlayerEmail:        c.PlayerEmail,
		RateLimitPerMinute: c.RateLimitPerMinute,
		AgentType:          c.Agent.Type,
		StrategyAllocation: c.StrategyAllocation,
		TotalCompanies:     c.TotalCompanies(),
	}
	data, _ := json.MarshalIndent(redacted, "", "  ")
	return string(data)
}

// parseStrategyAllocation parses "arbitrage:3,bulk_hauler:2" into StrategyAlloc slices.
func parseStrategyAllocation(s string) ([]StrategyAlloc, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("strategy allocation cannot be empty")
	}

	var allocs []StrategyAlloc
	pairs := strings.Split(s, ",")

	for _, pair := range pairs {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}

		parts := strings.SplitN(pair, ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid format %q, expected strategy:count", pair)
		}

		strategy := strings.TrimSpace(parts[0])
		if strategy == "" {
			return nil, fmt.Errorf("empty strategy name in %q", pair)
		}

		count, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			return nil, fmt.Errorf("invalid count in %q: %w", pair, err)
		}
		if count < 1 {
			return nil, fmt.Errorf("count must be >= 1 in %q", pair)
		}

		allocs = append(allocs, StrategyAlloc{
			Strategy: strategy,
			Count:    count,
		})
	}

	if len(allocs) == 0 {
		return nil, fmt.Errorf("no valid strategy allocations found")
	}

	return allocs, nil
}

// envOrDefault returns the environment variable value or a default.
func envOrDefault(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

// envIntOrDefault returns the environment variable as an int or a default.
func envIntOrDefault(key string, defaultVal int) int {
	val := os.Getenv(key)
	if val == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(val)
	if err != nil {
		return defaultVal
	}
	return n
}
