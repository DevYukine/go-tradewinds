package engine

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

func oceanicCapacityAcceptanceFixture(t *testing.T, class string, mortgage bool) (*recoveryHarness, *Runner) {
	t.Helper()
	h, runner := playerLoanAcceptanceFixture(t)
	port, _ := findPort(runner.stateCache.Catalog, recoveryHomePort)
	port.ID = "hamburg"
	runner.stateCache.Catalog.Ports = append(runner.stateCache.Catalog.Ports, port)
	runner.stateCache.Factors = append(runner.stateCache.Factors, port.ID)
	market := runner.stateCache.Markets[recoveryHomePort]
	var copied Market
	if err := json.Unmarshal(encode(market), &copied); err != nil {
		t.Fatal(err)
	}
	copied.PortID = port.ID
	runner.stateCache.Markets[port.ID] = copied
	detail := runner.stateCache.Ports[recoveryHomePort]
	detail.ID = port.ID
	runner.stateCache.Ports[port.ID] = detail
	for _, prefix := range []string{"books/", "ports/"} {
		runner.meta[prefix+port.ID] = runner.meta[prefix+recoveryHomePort]
	}
	hull, _ := findHull(runner.stateCache.Catalog, recoveryHull)
	hull.ID, hull.HullCategory = class, "oceanic"
	hull.CanSealDispatches = class == "packet"
	runner.stateCache.Catalog.ShipClasses = append(runner.stateCache.Catalog.ShipClasses, hull)
	for _, pair := range [][2]string{{recoveryHomePort, recoveryFarPort}, {recoveryFarPort, recoveryHomePort}} {
		route := runner.stateCache.Routes[routeKey(pair[0], pair[1], recoveryHull)]
		route.Legs = append([]RouteLeg(nil), route.Legs...)
		if route.FromPort == recoveryHomePort {
			route.FromPort = port.ID
		}
		if route.ToPort == recoveryHomePort {
			route.ToPort = port.ID
		}
		for index := range route.Legs {
			if route.Legs[index].FromNode == recoveryHomePort {
				route.Legs[index].FromNode = port.ID
			}
			if route.Legs[index].ToNode == recoveryHomePort {
				route.Legs[index].ToNode = port.ID
			}
		}
		sourceKey := routeKey(route.FromPort, route.ToPort, recoveryHull)
		runner.stateCache.Routes[sourceKey] = route
		runner.meta["routes/"+sourceKey] = runner.meta["routes/"+routeKey(pair[0], pair[1], recoveryHull)]
		route.ShipClass = hull.ID
		runner.stateCache.Routes[routeKey(route.FromPort, route.ToPort, hull.ID)] = route
		runner.meta["routes/"+routeKey(route.FromPort, route.ToPort, hull.ID)] = runner.meta["routes/"+routeKey(pair[0], pair[1], recoveryHull)]
	}
	hamburgQuotes := make(map[string]HazardQuote)
	for _, observed := range runner.stateCache.HazardQuotes {
		if observed.Origin == recoveryHomePort {
			observed.Origin = port.ID
		}
		if observed.Destination == recoveryHomePort {
			observed.Destination = port.ID
		}
		key := hazardHullKey(observed.ShipID, observed.Origin, observed.Destination)
		if observed.Laden {
			key = hazardLadenKey(observed.Origin, observed.Destination, observed.ShipClass, observed.CannonsCount, observed.Month)
		}
		hamburgQuotes[key] = observed
	}
	for key, quote := range hamburgQuotes {
		runner.stateCache.HazardQuotes[key] = quote
	}
	quote := runner.stateCache.Quotes[recoveryHomePort+"|"+recoveryHull]
	price := int64(600_000)
	quote.PortID, quote.ShipClass, quote.TotalCostPence = port.ID, hull.ID, &price
	quote.Mortgage = nil
	if mortgage {
		installment := int64(20_000)
		quote.Mortgage = &gameapi.MortgageTerms{
			Available: true, LenderCompanyID: "goldsmiths", PrincipalPence: 300_000,
			AnnualRateBPS: 3_000, TermGameDays: 180, Repayment: "amortised", Installments: 18,
			InstallmentPence: &installment, TotalRepayablePence: 360_000,
		}
	}
	key := port.ID + "|" + hull.ID
	runner.stateCache.Quotes[key] = quote
	runner.meta["yard_quotes/"+key] = runner.meta["yard_quotes/"+recoveryHomePort+"|"+recoveryHull]
	base := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v1/shipyard/quote" && req.URL.Query().Get("port_id") == port.ID && req.URL.Query().Get("ship_class") == hull.ID {
			h.api.writeJSON(w, http.StatusOK, quote)
			return
		}
		base.ServeHTTP(w, req)
	})
	// The candidate can fund an observed cargo lot, but 50,000p remains
	// below the 90,000p own-cash floor (15% of this 600,000p hull).
	reserves, err := runner.treasuryReserves(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for index := range runner.stateCache.Vaults {
		vault := &runner.stateCache.Vaults[index]
		vault.BalancePence = reserves[vault.BankingRegion]
		if vault.BankingRegion == port.BankingRegion {
			vault.BalancePence += 50_000
		}
	}
	return h, runner
}

func TestOceanicCapacityHamburgScoresMortgageAndLoanPackage(t *testing.T) {
	for _, test := range []struct {
		class    string
		mortgage bool
		verdict  string
	}{
		{"barque", true, "own_cash_buffer"},
		{"barque", false, "mortgage_unavailable"},
		{"frigate", false, "mortgage_unavailable"},
		{"indiaman", false, "mortgage_unavailable"},
	} {
		t.Run(test.class+"/"+test.verdict, func(t *testing.T) {
			h, runner := oceanicCapacityAcceptanceFixture(t, test.class, test.mortgage)
			evidence := &growthEvidence{workingByRegion: make(map[string]int64)}
			if reason, err := runner.growthCashFunding(h.ctx, evidence); err != nil || reason != "" {
				t.Fatalf("oceanic treasury authority: %s, %v", reason, err)
			}
			if _, err := runner.considerCapacityGrowthAt(h.ctx, evidence, "hamburg", test.class); err != nil {
				t.Fatal(err)
			}
			row := overseasCapacityDecision(t, h, "hamburg", test.class)
			var payload struct {
				HullCategory  string `json:"hull_category"`
				EvidenceBasis string `json:"evidence_basis"`
				Verdict       string `json:"verdict"`
				Package       struct {
					MortgagePence  int64           `json:"mortgage_pence"`
					Source         string          `json:"source"`
					PrincipalPence int64           `json:"principal_pence"`
					Installments   json.RawMessage `json:"installments"`
				} `json:"package"`
			}
			if err := json.Unmarshal(row.Details, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.HullCategory != "oceanic" || payload.Verdict != test.verdict || !strings.Contains(payload.EvidenceBasis, "min(depth-plan rate, measured fleet rate)") {
				t.Fatalf("oceanic capacity was skipped or its failed gate/evidence was hidden: %s", row.Details)
			}
			if payload.Package.Source != "player_request" || payload.Package.PrincipalPence <= 0 || len(payload.Package.Installments) == 0 {
				t.Fatalf("oceanic verdict omits the single-source debt package: %s", row.Details)
			}
			if test.mortgage && payload.Package.MortgagePence != 300_000 {
				t.Fatalf("oceanic package discarded yard mortgage: %s", row.Details)
			}
			if h.api.mutationCount("/v1/loan/request") != 0 || h.api.mutationCount("/v1/shipyard/build-ship") != 0 || len(h.pending()) != 0 {
				t.Fatal("rejected oceanic package created borrowing or construction")
			}
		})
	}
}

func TestOceanicCapacityDispatchPacketIsExcluded(t *testing.T) {
	h, runner := oceanicCapacityAcceptanceFixture(t, "packet", true)
	evidence := &growthEvidence{workingByRegion: make(map[string]int64)}
	if reason, err := runner.growthCashFunding(h.ctx, evidence); err != nil || reason != "" {
		t.Fatalf("packet treasury authority: %s, %v", reason, err)
	}
	if _, err := runner.considerCapacityGrowthAt(h.ctx, evidence, "hamburg", "packet"); err != nil {
		t.Fatal(err)
	}
	rows, err := h.db.History(h.ctx, h.account.ID, "decisions", store.MaxHistoryLimit)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows.([]store.Decision) {
		if row.Kind == "capacity_candidate:hamburg|packet" || row.Kind == "capacity_financing:hamburg|packet" {
			t.Fatalf("dispatch packet entered trading capacity competition: %s", row.Details)
		}
	}
	if h.api.mutationCount("/v1/loan/request") != 0 || h.api.mutationCount("/v1/shipyard/build-ship") != 0 || len(h.pending()) != 0 {
		t.Fatal("excluded dispatch packet created a capacity action")
	}
}
