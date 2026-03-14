package bot

import (
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/DevYukine/go-tradewinds/internal/api"
	"github.com/DevYukine/go-tradewinds/internal/config"
)

const (
	// Estimated API calls per company per minute at steady state.
	perCompanyCostPerMinute = 5.0

	// Fixed overhead for shared services (price scanner, world cache refresh).
	sharedOverheadPerMinute = 6.0

	// Target utilization — leave 20% headroom for bursts.
	targetUtilization = 0.80

	// Minimum companies per strategy for statistical comparison.
	minCompaniesPerStrategy = 2
)

// ScaledAllocation is the result of scaling a strategy allocation to fit
// within the rate limit budget.
type ScaledAllocation struct {
	Strategy    string
	Count       int
	HomePorts   []uuid.UUID // Diversified across ports.
	AgentType   string      // Per-allocation agent override (from config).
	LLMProvider string      // LLM provider when AgentType is "llm".
	LLMModel    string      // LLM model ID (e.g. "google/gemini-3-flash-preview").
}

// Scaler calculates safe company counts based on the rate limit budget.
type Scaler struct {
	rateLimiter *api.RateLimiter
	logger      *zap.Logger
}

// NewScaler creates a new Scaler.
func NewScaler(rateLimiter *api.RateLimiter, logger *zap.Logger) *Scaler {
	return &Scaler{
		rateLimiter: rateLimiter,
		logger:      logger.Named("scaler"),
	}
}

// CalculateAllocation determines the actual company count per strategy,
// scaling down proportionally if the requested count exceeds the rate limit budget.
// It also assigns diversified home ports to each company.
func (s *Scaler) CalculateAllocation(
	requested []config.StrategyAlloc,
	rateBudgetPerMinute int,
	ports []api.Port,
) []ScaledAllocation {
	usableBudget := float64(rateBudgetPerMinute) * targetUtilization
	available := usableBudget - sharedOverheadPerMinute
	if available < 0 {
		available = 0
	}

	maxCompanies := int(available / perCompanyCostPerMinute)

	totalRequested := 0
	for _, a := range requested {
		totalRequested += a.Count
	}

	s.logger.Debug("calculating company allocation",
		zap.Int("total_requested", totalRequested),
		zap.Int("max_companies", maxCompanies),
		zap.Float64("usable_budget", usableBudget),
	)

	allocations := make([]ScaledAllocation, len(requested))

	if totalRequested <= maxCompanies {
		// Budget allows all requested companies.
		for i, a := range requested {
			allocations[i] = ScaledAllocation{
				Strategy:    a.Strategy,
				Count:       a.Count,
				AgentType:   a.AgentType,
				LLMProvider: a.LLMProvider,
				LLMModel:    a.LLMModel,
			}
		}
	} else {
		// Scale down proportionally.
		s.logger.Warn("scaling down company count to fit rate limit budget",
			zap.Int("requested", totalRequested),
			zap.Int("max", maxCompanies),
		)

		remaining := maxCompanies
		for i, a := range requested {
			scaled := int(float64(a.Count) / float64(totalRequested) * float64(maxCompanies))
			if scaled < 1 {
				scaled = 1
			}
			if scaled > remaining {
				scaled = remaining
			}
			allocations[i] = ScaledAllocation{
				Strategy:    a.Strategy,
				Count:       scaled,
				AgentType:   a.AgentType,
				LLMProvider: a.LLMProvider,
			}
			remaining -= scaled
		}

		// Distribute leftover slots to strategies that were scaled below minimum.
		for i := range allocations {
			if remaining <= 0 {
				break
			}
			if allocations[i].Count < minCompaniesPerStrategy && allocations[i].Count < requested[i].Count {
				allocations[i].Count++
				remaining--
			}
		}
	}

	// Assign diversified home ports.
	s.assignHomePorts(allocations, ports)

	for _, a := range allocations {
		s.logger.Debug("allocation result",
			zap.String("strategy", a.Strategy),
			zap.Int("count", a.Count),
			zap.Int("home_ports", len(a.HomePorts)),
		)
	}

	return allocations
}

// assignHomePorts distributes home ports across companies within each strategy
// to maximize route coverage diversity.
func (s *Scaler) assignHomePorts(allocations []ScaledAllocation, ports []api.Port) {
	if len(ports) == 0 {
		return
	}

	portIdx := 0
	for i := range allocations {
		allocations[i].HomePorts = make([]uuid.UUID, allocations[i].Count)
		for j := range allocations[i].Count {
			allocations[i].HomePorts[j] = ports[portIdx%len(ports)].ID
			portIdx++
		}
	}
}
