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
	RedisURL           string // Optional: "redis://localhost:6379/0" or "" for no Redis
	BaseURL            string
	APIPort            int
	Env                string // "development" or "production"
	LogLevel           string // "debug", "info", "warn", "error"
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
// Optional AgentHint overrides the default agent for companies in this allocation.
// Format in env: "arbitrage:3" or "arbitrage/llm-openrouter@google/gemini-3-flash-preview:1".
type StrategyAlloc struct {
	Strategy    string
	Count       int
	AgentType   string // "heuristic" (default), "llm"
	LLMProvider string // "claude", "openai", "openrouter", "ollama"
	LLMModel    string // e.g. "google/gemini-3.1-flash-lite-preview" (OpenRouter model ID)
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

	// Per-provider API keys for multi-LLM setups.
	// When set, these override LLMAPIKey for their respective provider.
	ClaudeAPIKey     string
	OpenAIAPIKey     string
	OpenRouterAPIKey string
}

// APIKeyForProvider returns the API key for the given LLM provider,
// checking provider-specific keys first then falling back to the generic LLM_API_KEY.
func (a AgentConfig) APIKeyForProvider(provider string) string {
	switch provider {
	case "claude":
		if a.ClaudeAPIKey != "" {
			return a.ClaudeAPIKey
		}
	case "openai":
		if a.OpenAIAPIKey != "" {
			return a.OpenAIAPIKey
		}
	case "openrouter":
		if a.OpenRouterAPIKey != "" {
			return a.OpenRouterAPIKey
		}
	}
	return a.LLMAPIKey
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
		RedisURL:           os.Getenv("REDIS_URL"),
		BaseURL:            envOrDefault("BOT_BASE_URL", "https://tradewinds.fly.dev"),
		APIPort:            envIntOrDefault("API_PORT", 3002),
		Env:                envOrDefault("ENV", "development"),
		LogLevel:           envOrDefault("LOG_LEVEL", "debug"),
		PlayerEmail:        os.Getenv("PLAYER_EMAIL"),
		PlayerPassword:     os.Getenv("PLAYER_PASSWORD"),
		RateLimitPerMinute: envIntOrDefault("RATE_LIMIT_PER_MINUTE", 900),
		Agent: AgentConfig{
			Type:               envOrDefault("AGENT_TYPE", "heuristic"),
			LLMProvider:        os.Getenv("LLM_PROVIDER"),
			LLMModel:           os.Getenv("LLM_MODEL"),
			LLMAPIKey:          os.Getenv("LLM_API_KEY"),
			LLMMaxTokens:       envIntOrDefault("LLM_MAX_TOKENS", 8192),
			CompositeFastAgent: envOrDefault("COMPOSITE_FAST_AGENT", "heuristic"),
			CompositeSlowAgent: envOrDefault("COMPOSITE_SLOW_AGENT", "llm"),
			ClaudeAPIKey:       os.Getenv("CLAUDE_API_KEY"),
			OpenAIAPIKey:       os.Getenv("OPENAI_API_KEY"),
			OpenRouterAPIKey:   os.Getenv("OPENROUTER_API_KEY"),
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
	redisStatus := "disabled"
	if c.RedisURL != "" {
		redisStatus = "enabled"
	}
	redacted := struct {
		BaseURL            string         `json:"base_url"`
		APIPort            int            `json:"api_port"`
		DBHost             string         `json:"db_host"`
		DBName             string         `json:"db_name"`
		Redis              string         `json:"redis"`
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
		Redis:              redisStatus,
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

		strategyPart := strings.TrimSpace(parts[0])
		if strategyPart == "" {
			return nil, fmt.Errorf("empty strategy name in %q", pair)
		}

		count, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			return nil, fmt.Errorf("invalid count in %q: %w", pair, err)
		}
		if count < 1 {
			return nil, fmt.Errorf("count must be >= 1 in %q", pair)
		}

		alloc := StrategyAlloc{Count: count}

		// Support "strategy/agentType-provider@model" format.
		// Examples: "arbitrage/llm-openrouter@google/gemini-3-flash-preview"
		if slashIdx := strings.Index(strategyPart, "/"); slashIdx >= 0 {
			alloc.Strategy = strategyPart[:slashIdx]
			agentPart := strategyPart[slashIdx+1:]
			if dashIdx := strings.Index(agentPart, "-"); dashIdx >= 0 {
				alloc.AgentType = agentPart[:dashIdx]
				providerAndModel := agentPart[dashIdx+1:]
				// Split provider@model (model may contain "/" e.g. "google/gemini-3-flash").
				if atIdx := strings.Index(providerAndModel, "@"); atIdx >= 0 {
					alloc.LLMProvider = providerAndModel[:atIdx]
					alloc.LLMModel = providerAndModel[atIdx+1:]
				} else {
					alloc.LLMProvider = providerAndModel
				}
			} else {
				alloc.AgentType = agentPart
			}
		} else {
			alloc.Strategy = strategyPart
		}

		allocs = append(allocs, alloc)
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
