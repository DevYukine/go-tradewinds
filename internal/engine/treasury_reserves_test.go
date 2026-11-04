package engine

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
)

func TestObligationReservesFundObservedArrivalNotSailingOrigin(t *testing.T) {
	t.Parallel()
	const reserve int64 = 100_000
	state := AccountState{
		HomePortID: recoveryHomePort,
		Catalog: Catalog{
			Ports: []Port{
				{ID: recoveryHomePort, BankingRegion: "north"},
				{ID: recoveryFarPort, BankingRegion: "south"},
			},
			ShipClasses: []Hull{{ID: recoveryHull, HullCategory: "coastal", HoldCapacityTons: 60}},
		},
		Ports: map[string]PortDetail{
			recoveryHomePort: {ID: recoveryHomePort, BerthDuesPerDayPence: 100},
			recoveryFarPort:  {ID: recoveryFarPort, BerthDuesPerDayPence: 100},
		},
	}
	outgoing := ShipState{RouteLegs: []RouteLeg{{FromNode: recoveryHomePort, ToNode: recoveryFarPort}}}
	outgoing.Ship.ID = recoveryShipBlastoise
	outgoing.Ship.ShipClass = recoveryHull
	outgoing.Ship.Status = "sailing"
	outgoing.Ship.CurrentPortID = recoveryHomePort
	returning := ShipState{RouteLegs: []RouteLeg{
		{FromNode: recoveryFarPort, ToNode: "sea-zone"},
		{FromNode: "sea-zone", ToNode: recoveryHomePort},
	}}
	returning.Ship.ID = recoveryShipCharizard
	returning.Ship.ShipClass = recoveryHull
	returning.Ship.Status = "sailing"
	returning.Ship.CurrentPortID = recoveryFarPort
	state.Ships = []ShipState{outgoing, returning}

	// Both committed arrivals need funding before the treasury recovery can
	// finish: north for the returning ship, south for the outgoing ship.
	required := obligationReserves(state, nil, reserve)
	if required["south"] != reserve+300 {
		t.Fatalf("outgoing arrival did not preserve its regional reserve and 300p harbour envelope: %v", required)
	}
	wantNorth := reserve + 3*DailyUpkeep(state.Ships, state.Catalog, 0) + 300
	if required["north"] != wantNorth {
		t.Fatalf("returning arrival and fixed home bills need %dp, got %v", wantNorth, required)
	}

	// A subsequent authoritative fleet replaces the outgoing ship with a
	// different berthed ship. Charizard still retains portb as its origin,
	// but neither remaining ship has an observed south-region obligation.
	berthed := ShipState{}
	berthed.Ship.ID = recoveryShipLapras
	berthed.Ship.ShipClass = recoveryHull
	berthed.Ship.Status = "berthed"
	berthed.Ship.CurrentPortID = recoveryHomePort
	state.Ships = []ShipState{berthed, returning}
	state.Factors = []string{recoveryFarPort}
	required = obligationReserves(state, nil, reserve)
	if required["south"] != 0 {
		t.Fatalf("sailing retained origin invented another protected south berth: %v", required)
	}
	wantNorth = reserve + 3*DailyUpkeep(state.Ships, state.Catalog, len(state.Factors)) + 600
	if required["north"] != wantNorth {
		t.Fatalf("stationary presence, incoming arrival and factor-inclusive home bills need %dp, got %v", wantNorth, required)
	}
}

func TestRegionalReserveFloorAddsUpkeepOnlyAtHome(t *testing.T) {
	t.Parallel()
	loan := snapshotTestLoan()
	loan.BankingRegion = "north"
	state := AccountState{
		HomePortID: "home",
		Catalog: Catalog{
			Ports: []Port{
				{ID: "home", BankingRegion: "north"},
				{ID: "away", BankingRegion: "south"},
			},
			ShipClasses: []Hull{{ID: recoveryHull, HullCategory: "coastal", HoldCapacityTons: 60}},
		},
		Factors: []string{"one", "two", "three", "four"},
		Loans:   []gameapi.Loan{loan},
	}
	ship := ShipState{}
	ship.Ship.ID = "away-ship"
	ship.Ship.ShipClass = recoveryHull
	ship.Ship.Status = "berthed"
	ship.Ship.CurrentPortID = "away"
	state.Ships = []ShipState{ship}
	floor := ProtectedReservePence(state)
	if floor != 100_000 {
		t.Fatalf("regional operating floor = %d, want 100000", floor)
	}
	required := obligationReserves(state, nil, floor)
	upkeep := DailyUpkeep(state.Ships, state.Catalog, len(state.Factors))
	if want := floor + 3*upkeep + loanReserve(state.Loans[0]); required["north"] != want {
		t.Fatalf("home upkeep and debt not protected: got %d, want %d", required["north"], want)
	}
	if required["south"] != floor {
		t.Fatalf("fleet upkeep incorrectly reserved away from home: %v", required)
	}
}

func TestAutoRemitReserveAddsHomeUpkeepToRegionalFloor(t *testing.T) {
	h := newRecoveryHarness(t)
	h.api.setFactorPorts(recoveryHomePort, recoveryFarPort)
	h.api.setAutoRemit(false, 0)
	runner := h.runner(primaryTestKey)
	if _, err := h.tick(runner); err != nil {
		t.Fatal(err)
	}
	_, data := h.mission("__treasury")
	if data.Action == nil || data.Action.Stage != "settings" {
		t.Fatalf("missing automatic bill-remittance settings action: %+v", data.Action)
	}
	var settings struct {
		Enabled bool  `json:"auto_remit_for_bills"`
		Reserve int64 `json:"auto_remit_min_reserve_pence"`
	}
	if err := json.Unmarshal(data.Action.Body, &settings); err != nil {
		t.Fatal(err)
	}
	want := int64(100_000) + 3*DailyUpkeep(runner.stateCache.Ships, runner.stateCache.Catalog, len(runner.stateCache.Factors))
	if !settings.Enabled || settings.Reserve != want {
		t.Fatalf("automatic bill-remittance reserve = %+v, want enabled with %dp", settings, want)
	}
}

func TestObligationReservesDeferInstallmentUntilLeadWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 4, 7, 0, 0, 0, time.UTC)
	loan := snapshotTestLoan()
	loan.BankingRegion = "america"
	state := AccountState{CompanyID: loan.BorrowerCompanyID, Loans: []gameapi.Loan{loan}}
	state.Clock.RealUTC = now
	state.Clock.Calendar.TimeScale = 60
	at := now.Add(serverClockStaleness)
	lead := gameDuration(loanServiceLeadGameDays, state.Clock.Calendar.TimeScale)
	for _, offset := range []time.Duration{time.Second, 0, -time.Second} {
		t.Run(offset.String(), func(t *testing.T) {
			due := at.Add(lead).Add(offset)
			state.Loans[0].NextPaymentDueAt = &due
			required, deferred := obligationReservePlan(state, nil, 0)
			want := *loan.NextPaymentPence
			if offset > 0 {
				want = 0
				if len(deferred) != 1 || deferred[0].Region != "america" ||
					deferred[0].FullReserve != loanReserve(loan) || !deferred[0].ReserveFrom.Equal(due.Add(-lead-serverClockStaleness)) {
					t.Fatalf("missing exact deferred reserve: %+v", deferred)
				}
				sources := PlanningSources{CompanyID: state.CompanyID, Generation: "installment-test", Metadata: map[string]DatasetMeta{
					"clock": {AttemptedAt: now, UpdatedAt: now, Generation: "installment-test", Complete: true},
				}}
				in := PlanningInputFor(now, ShipState{}, state, nil, sources)
				if len(in.DeferredLoanReserves) != 1 || in.ReserveByRegion["america"] != 0 ||
					fullLoanReserves(in)["america"] != loanReserve(loan) {
					t.Fatalf("planning input lost the phased/full cash distinction: %+v", in)
				}
			} else if len(deferred) != 0 {
				t.Fatalf("inside-window installment was deferred: %+v", deferred)
			}
			if required["america"] != want {
				t.Fatalf("due offset %s: reserve=%d want=%d", offset, required["america"], want)
			}
		})
	}
	due := at.Add(lead).Add(time.Hour)
	state.Loans[0].NextPaymentDueAt = &due
	for _, test := range []struct {
		name   string
		change func(*AccountState)
	}{
		{"arrears", func(s *AccountState) { s.Loans[0].ArrearsPence = 1 }},
		{"defaulted", func(s *AccountState) { s.Loans[0].Status = "defaulted" }},
		{"balloon", func(s *AccountState) { s.Loans[0].Repayment = "balloon" }},
		{"zero clock", func(s *AccountState) { s.Clock.RealUTC = time.Time{} }},
		{"paused clock", func(s *AccountState) { s.Clock.Paused = true }},
		{"unknown scale", func(s *AccountState) { s.Clock.Calendar.TimeScale = 0 }},
		{"unknown due", func(s *AccountState) { s.Loans[0].NextPaymentDueAt = nil }},
		{"zero due", func(s *AccountState) { s.Loans[0].NextPaymentDueAt = new(time.Time) }},
		{"unknown payment", func(s *AccountState) { s.Loans[0].NextPaymentPence = nil }},
		{"zero payment", func(s *AccountState) { s.Loans[0].NextPaymentPence = new(int64) }},
		{"foreign borrower", func(s *AccountState) { s.CompanyID = "other-company" }},
		{"unknown borrower", func(s *AccountState) { s.CompanyID = "" }},
		{"stale clock", func(s *AccountState) { s.loanClockUnknown = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := state
			candidate.Loans = append([]gameapi.Loan(nil), state.Loans...)
			test.change(&candidate)
			required, deferred := obligationReservePlan(candidate, nil, 0)
			if required["america"] != loanReserve(candidate.Loans[0]) || len(deferred) != 0 {
				t.Fatalf("unsafe loan service clock did not retain full reserve: %v %+v", required, deferred)
			}
		})
	}
	required, deferred := obligationReservePlan(state, nil, 0, loan.ID)
	if len(required) != 0 || len(deferred) != 0 {
		t.Fatalf("excluded loan leaked a reserve or deferral: %v %+v", required, deferred)
	}
}

func TestObligationReservesKeepFullInstallmentWhenClockMetadataStale(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	loan := snapshotTestLoan()
	due := now.Add(24 * time.Hour)
	loan.NextPaymentDueAt = &due
	state := AccountState{CompanyID: loan.BorrowerCompanyID, Loans: []gameapi.Loan{loan}}
	state.Clock.RealUTC, state.Clock.Calendar.TimeScale = now, 60
	sources := PlanningSources{CompanyID: state.CompanyID, Generation: "installment-stale", Metadata: map[string]DatasetMeta{
		"clock": {AttemptedAt: now.Add(-serverClockStaleness - time.Second), UpdatedAt: now.Add(-serverClockStaleness - time.Second), Generation: "installment-stale", Complete: true},
	}}
	input := PlanningInputFor(now, ShipState{}, state, nil, sources)
	if input.ReserveByRegion[loan.BankingRegion] != loanReserve(loan) || len(input.DeferredLoanReserves) != 0 {
		t.Fatalf("stale clock released installment capital: %+v", input)
	}
	runner := &Runner{stateCache: state, generation: sources.Generation, meta: sources.Metadata}
	if got := runner.loanServiceReserve(loan); got != loanReserve(loan) {
		t.Fatalf("controller subtraction used stale phased amount: %d", got)
	}
}

func TestObligationReservesPhaseEachAmortisedLoanIndependently(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 4, 7, 0, 0, 0, time.UTC)
	state := AccountState{CompanyID: recoveryCompanyID}
	state.Clock.RealUTC = now
	state.Clock.Calendar.TimeScale = 60
	for index, delay := range []time.Duration{time.Hour, 4 * time.Hour, 5 * time.Hour} {
		loan := snapshotTestLoan()
		loan.ID = []string{"current-installment", "bridge-charmeleon", "bridge-charmander"}[index]
		loan.BankingRegion = "america"
		if index > 0 {
			loan.InstallmentPence, loan.NextPaymentPence = new(int64(24_201)), new(int64(24_201))
		}
		due := now.Add(delay)
		loan.NextPaymentDueAt = &due
		state.Loans = append(state.Loans, loan)
	}
	required, deferred := obligationReservePlan(state, nil, 0)
	if required["america"] != *state.Loans[0].NextPaymentPence || len(deferred) != 2 {
		t.Fatalf("mixed due dates were collapsed to one regional schedule: %v %+v", required, deferred)
	}
	if !deferred[0].ReserveFrom.Equal(state.Loans[1].NextPaymentDueAt.Add(-72*time.Minute-serverClockStaleness)) ||
		!deferred[1].ReserveFrom.Equal(state.Loans[2].NextPaymentDueAt.Add(-72*time.Minute-serverClockStaleness)) {
		t.Fatalf("distinct bridge due dates lost their own reserve windows: %+v", deferred)
	}
	in := PlanningInput{ReserveByRegion: required, DeferredLoanReserves: deferred, Clock: state.Clock}
	if want := *state.Loans[0].NextPaymentPence + loanReserve(state.Loans[1]) + loanReserve(state.Loans[2]); fullLoanReserves(in)["america"] != want {
		t.Fatalf("restored multiple deferrals did not sum each loan once: %v", fullLoanReserves(in))
	}
	state.Clock.RealUTC = state.Clock.RealUTC.Add(3 * time.Hour)
	required, deferred = obligationReservePlan(state, nil, 0)
	if want := *state.Loans[0].NextPaymentPence + *state.Loans[1].NextPaymentPence; required["america"] != want ||
		len(deferred) != 1 || !deferred[0].ReserveFrom.Equal(state.Loans[2].NextPaymentDueAt.Add(-72*time.Minute-serverClockStaleness)) {
		t.Fatalf("one bridge entering its window changed another loan's phase: %v %+v", required, deferred)
	}
}
