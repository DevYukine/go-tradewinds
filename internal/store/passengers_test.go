package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pressly/goose/v3"
	"go.uber.org/zap"
)

func passengerEvidenceFixture(accountID, id, lane string) PassengerEvidence {
	return PassengerEvidence{
		ID: id, AccountID: accountID, Lane: lane, Kind: "travellers",
		FarePerHeadPence: 150, Outcome: "boarded", Heads: 12, GameHour: 240.5,
		LeewayGameHours: new(4.5), LateCutPerGameDayPence: new(int64(0)),
	}
}

func retainedPassengerEvent(t *testing.T, database *Store, accountID, id, kind string, payload json.RawMessage) {
	t.Helper()
	if err := database.RecordEvent(context.Background(), accountID, id, kind, payload); err != nil {
		t.Fatal(err)
	}
}

func passageSettlementFixture(accountID, id, eventID string, proceeds int64, completedAt time.Time) Settlement {
	return Settlement{
		ID: id, AccountID: accountID, ShipID: "passenger-ship", MissionID: "passenger-mission",
		EventID: eventID, Lane: "london|calais", Kind: "passage", Origin: "london", Destination: "calais",
		QuantityTons: 12, CostsKnown: true, ProceedsPence: proceeds,
		ProvisionsPence: 20, FeesPence: 5, ExpensesPence: 5, GameSeconds: 3_600,
		StartedAt: completedAt.Add(-time.Hour), CompletedAt: completedAt,
	}
}

func TestPassengerEvidenceEncryptedImmutableRestart(t *testing.T) {
	database, dsn, key := integrationStore(t)
	ctx := context.Background()
	const accountID = "passenger-evidence"
	credentials := saveTestAccount(t, database, accountID)
	saveTestAccount(t, database, "passenger-evidence-other")
	lane := credentials.APIKey + "|" + credentials.Passphrase
	row := passengerEvidenceFixture(accountID, "passenger-observation", lane)
	row.CreatedAt = time.Now().UTC().Add(-8 * 24 * time.Hour).Truncate(time.Microsecond)
	if err := database.PutPassengerEvidence(ctx, &row); err != nil {
		t.Fatal(err)
	}
	refused := row
	refused.ID, refused.Outcome, refused.GameHour = "passenger-refusal", "refused", 241.5
	refused.LeewayGameHours, refused.LateCutPerGameDayPence = nil, nil
	if err := database.PutPassengerEvidence(ctx, &refused); err != nil {
		t.Fatal(err)
	}
	otherLane := row
	otherLane.ID, otherLane.Lane = "passenger-other-lane", "hamburg|amsterdam"
	if err := database.PutPassengerEvidence(ctx, &otherLane); err != nil {
		t.Fatal(err)
	}
	otherAccount := row
	otherAccount.ID, otherAccount.AccountID = "passenger-other-account", "passenger-evidence-other"
	if err := database.PutPassengerEvidence(ctx, &otherAccount); err != nil {
		t.Fatal(err)
	}
	var raw []string
	if err := database.db.Raw("SELECT to_jsonb(evidence)::text FROM passenger_evidence AS evidence").Scan(&raw).Error; err != nil {
		t.Fatal(err)
	}
	for _, stored := range raw {
		for _, private := range []string{lane, credentials.APIKey, credentials.Passphrase, "travellers"} {
			if strings.Contains(stored, private) {
				t.Fatal("passenger lane or kind was stored in plaintext")
			}
		}
	}
	if err := database.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, dsn, key, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	history, err := reopened.PassengerEvidenceHistory(ctx, accountID, lane, 10)
	if err != nil || len(history) != 2 || history[0].ID != refused.ID || history[1].ID != row.ID ||
		!samePassengerEvidence(history[1], row) || !history[1].CreatedAt.Equal(row.CreatedAt) {
		t.Fatalf("private passenger evidence did not survive restart and retention: %+v, %v", history, err)
	}
	if history[0].LeewayGameHours != nil || history[0].LateCutPerGameDayPence != nil ||
		history[1].LateCutPerGameDayPence == nil || *history[1].LateCutPerGameDayPence != 0 {
		t.Fatalf("unmeasured values and measured zero were conflated: %+v", history)
	}
	bounded, err := reopened.PassengerEvidenceHistory(ctx, accountID, lane, 1)
	if err != nil || len(bounded) != 1 || bounded[0].ID != refused.ID {
		t.Fatalf("passenger evidence limit lost latest-first ordering: %+v, %v", bounded, err)
	}
	replay := history[1]
	replay.CreatedAt = time.Time{}
	if err := reopened.PutPassengerEvidence(ctx, &replay); err != nil || !replay.CreatedAt.Equal(row.CreatedAt) {
		t.Fatalf("timestamp-free restart replay was not idempotent: %+v, %v", replay, err)
	}
	for name, change := range map[string]func(*PassengerEvidence){
		"account":      func(row *PassengerEvidence) { row.AccountID = otherAccount.AccountID },
		"lane":         func(row *PassengerEvidence) { row.Lane = "london|portsmouth" },
		"kind":         func(row *PassengerEvidence) { row.Kind = "emigrants" },
		"fare":         func(row *PassengerEvidence) { row.FarePerHeadPence++ },
		"outcome":      func(row *PassengerEvidence) { row.Outcome = "refused" },
		"heads":        func(row *PassengerEvidence) { row.Heads++ },
		"game hour":    func(row *PassengerEvidence) { row.GameHour += 0.5 },
		"leeway":       func(row *PassengerEvidence) { row.LeewayGameHours = new(4.6) },
		"erase leeway": func(row *PassengerEvidence) { row.LeewayGameHours = nil },
		"late cut":     func(row *PassengerEvidence) { row.LateCutPerGameDayPence = new(int64(1)) },
		"erase cut":    func(row *PassengerEvidence) { row.LateCutPerGameDayPence = nil },
		"created at":   func(row *PassengerEvidence) { row.CreatedAt = row.CreatedAt.Add(time.Second) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := replay
			change(&changed)
			if err := reopened.PutPassengerEvidence(ctx, &changed); !errors.Is(err, ErrActionConflict) {
				t.Fatalf("immutable passenger evidence changed: %v", err)
			}
		})
	}
	if err := reopened.db.Exec("UPDATE passenger_evidence SET heads = heads + 1 WHERE id = ?", row.ID).Error; err == nil {
		t.Fatal("database allowed passenger evidence to be rewritten")
	}
}

func TestPassengerEvidenceValidationAndOwnedWrites(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	const accountID = "passenger-evidence-owner"
	saveTestAccount(t, database, accountID)
	saveTestAccount(t, database, "passenger-evidence-foreign")
	if err := database.PutPassengerEvidence(ctx, nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("nil passenger evidence accepted: %v", err)
	}
	for name, change := range map[string]func(*PassengerEvidence){
		"id":             func(row *PassengerEvidence) { row.ID = "" },
		"account":        func(row *PassengerEvidence) { row.AccountID = "" },
		"lane":           func(row *PassengerEvidence) { row.Lane = " " },
		"long lane":      func(row *PassengerEvidence) { row.Lane = strings.Repeat("x", 1025) },
		"kind":           func(row *PassengerEvidence) { row.Kind = "unknown" },
		"outcome":        func(row *PassengerEvidence) { row.Outcome = "paid" },
		"zero fare":      func(row *PassengerEvidence) { row.FarePerHeadPence = 0 },
		"negative fare":  func(row *PassengerEvidence) { row.FarePerHeadPence = -1 },
		"zero heads":     func(row *PassengerEvidence) { row.Heads = 0 },
		"negative heads": func(row *PassengerEvidence) { row.Heads = -1 },
		"negative hour":  func(row *PassengerEvidence) { row.GameHour = -1 },
		"nan hour":       func(row *PassengerEvidence) { row.GameHour = math.NaN() },
		"infinite hour":  func(row *PassengerEvidence) { row.GameHour = math.Inf(1) },
		"nan leeway":     func(row *PassengerEvidence) { row.LeewayGameHours = new(math.NaN()) },
		"infinite leeway": func(row *PassengerEvidence) {
			row.LeewayGameHours = new(math.Inf(-1))
		},
		"negative cut": func(row *PassengerEvidence) { row.LateCutPerGameDayPence = new(int64(-1)) },
	} {
		t.Run(name, func(t *testing.T) {
			row := passengerEvidenceFixture(accountID, "invalid-passenger-evidence", "london|calais")
			change(&row)
			if err := database.PutPassengerEvidence(ctx, &row); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("invalid passenger evidence accepted: %v", err)
			}
		})
	}
	for _, query := range []struct {
		account string
		lane    string
		limit   int
	}{
		{"", "", 1},
		{accountID, strings.Repeat("x", 1025), 1},
		{accountID, "", 0},
		{accountID, "", MaxHistoryLimit + 1},
	} {
		if _, err := database.PassengerEvidenceHistory(ctx, query.account, query.lane, query.limit); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("invalid passenger history query accepted: %v", err)
		}
	}
	owner, release, err := database.AcquireAccountContext(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	foreign := passengerEvidenceFixture("passenger-evidence-foreign", "foreign-owned-passenger-evidence", "london|calais")
	if err := database.PutPassengerEvidence(owner, &foreign); !errors.Is(err, ErrOwnershipLost) {
		t.Fatalf("owner wrote another account's passenger evidence: %v", err)
	}
	row := passengerEvidenceFixture(accountID, "owned-passenger-evidence", "london|calais")
	row.LeewayGameHours = new(-0.5)
	if err := database.PutPassengerEvidence(owner, &row); err != nil {
		t.Fatalf("owned signed leeway observation failed: %v", err)
	}
	release()
	row.ID = "released-owner-passenger-evidence"
	if err := database.PutPassengerEvidence(owner, &row); err == nil {
		t.Fatal("released owner wrote passenger evidence")
	}
}

func TestPassageSettlementEventAnchoredPaidLostRestart(t *testing.T) {
	database, dsn, key := integrationStore(t)
	ctx := context.Background()
	const accountID = "passage-settlement"
	saveTestAccount(t, database, accountID)
	now := time.Now().UTC().Truncate(time.Microsecond)
	fixtures := []struct {
		id       string
		kind     string
		payload  json.RawMessage
		proceeds int64
	}{
		{"paid", "passengers_paid", json.RawMessage(`{"fare_paid_pence":9007199254740993}`), 9_007_199_254_740_993},
		{"wrapped", "passengers_paid", json.RawMessage(`{"timestamp":"2026-10-28T12:00:00Z","payload":{"fare_paid_pence":42}}`), 42},
		{"lost", "passengers_lost", json.RawMessage(`{"cause":"captured"}`), 0},
		{"zero", "passengers_paid", json.RawMessage(`{"fare_paid_pence":0}`), 0},
	}
	for _, fixture := range fixtures {
		retainedPassengerEvent(t, database, accountID, fixture.id, fixture.kind, fixture.payload)
		row := passageSettlementFixture(accountID, "passage-"+fixture.id, fixture.id, fixture.proceeds, now)
		if err := database.PutSettlement(ctx, &row); err != nil {
			t.Fatalf("event-anchored %s settlement failed: %v", fixture.id, err)
		}
	}
	reopened, err := Open(ctx, dsn, key, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	rows, err := reopened.SettlementHistory(ctx, accountID, "london|calais", 10)
	if err != nil || len(rows) != len(fixtures) {
		t.Fatalf("passage settlements lost on restart: %+v, %v", rows, err)
	}
	for _, row := range rows {
		if row.ActionID != "" || row.EventID == "" || row.Kind != "passage" {
			t.Fatalf("passage invented an action or lost its event anchor: %+v", row)
		}
		if err := reopened.PutSettlement(ctx, &row); err != nil {
			t.Fatalf("restarted passage settlement was not idempotent: %v", err)
		}
		duplicate := row
		duplicate.ID += "-duplicate"
		if err := reopened.PutSettlement(ctx, &duplicate); !errors.Is(err, ErrActionConflict) {
			t.Fatalf("one fare event produced two settlements: %v", err)
		}
		changed := row
		changed.FeesPence++
		if err := reopened.PutSettlement(ctx, &changed); !errors.Is(err, ErrActionConflict) {
			t.Fatalf("passage immutable accounting changed: %v", err)
		}
	}
	var actions, metrics, nullAnchors int64
	if err := reopened.db.Model(&actionRow{}).Where("account_id = ?", accountID).Count(&actions).Error; err != nil || actions != 0 {
		t.Fatalf("passage synthesized an HTTP action: %d, %v", actions, err)
	}
	if err := reopened.db.Model(&Metric{}).Where("account_id = ? AND name = ?", accountID, "realized_profit_pence").Count(&metrics).Error; err != nil || metrics != int64(len(fixtures)) {
		t.Fatalf("idempotent passages duplicated profit metrics: %d, %v", metrics, err)
	}
	if err := reopened.db.Table("settlements").Where("account_id = ? AND action_id IS NULL AND event_id IS NOT NULL", accountID).Count(&nullAnchors).Error; err != nil || nullAnchors != int64(len(fixtures)) {
		t.Fatalf("event-only settlement did not use SQL NULL for action: %d, %v", nullAnchors, err)
	}
	retainedPassengerEvent(t, reopened, accountID, "paid", "passengers_paid", json.RawMessage(`{"fare_paid_pence":1}`))
	original := passageSettlementFixture(accountID, "passage-paid", "paid", 9_007_199_254_740_993, now)
	if err := reopened.PutSettlement(ctx, &original); err != nil {
		t.Fatalf("duplicate event changed retained payment evidence: %v", err)
	}
}

func TestPassageSettlementRequiresRetainedOwnedFareEvent(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	const accountID = "passage-anchor-validation"
	saveTestAccount(t, database, accountID)
	saveTestAccount(t, database, "passage-anchor-foreign")
	for _, fixture := range []struct {
		id      string
		kind    string
		payload json.RawMessage
	}{
		{"paid", "passengers_paid", json.RawMessage(`{"fare_paid_pence":100}`)},
		{"lost", "passengers_lost", json.RawMessage(`{"cause":"sunk"}`)},
		{"boarded", "passengers_boarded", json.RawMessage(`{"fare_paid_pence":100}`)},
		{"unrelated", "ship_arrived", json.RawMessage(`{"fare_paid_pence":100}`)},
		{"missing-amount", "passengers_paid", json.RawMessage(`{}`)},
		{"text-amount", "passengers_paid", json.RawMessage(`{"fare_paid_pence":"100"}`)},
		{"fractional-amount", "passengers_paid", json.RawMessage(`{"fare_paid_pence":100.5}`)},
		{"negative-amount", "passengers_paid", json.RawMessage(`{"fare_paid_pence":-1}`)},
		{"null-payload", "passengers_paid", json.RawMessage(`{"fare_paid_pence":100,"payload":null}`)},
		{"wrapped-mismatch", "passengers_paid", json.RawMessage(`{"fare_paid_pence":100,"payload":{"fare_paid_pence":200}}`)},
	} {
		retainedPassengerEvent(t, database, accountID, fixture.id, fixture.kind, fixture.payload)
	}
	retainedPassengerEvent(t, database, "passage-anchor-foreign", "foreign", "passengers_paid", json.RawMessage(`{"fare_paid_pence":100}`))
	for _, fixture := range []struct {
		id       string
		proceeds int64
		want     error
	}{
		{"missing", 100, ErrNotFound},
		{"foreign", 100, ErrNotFound},
		{"paid", 99, ErrActionConflict},
		{"lost", 1, ErrActionConflict},
		{"boarded", 100, ErrActionConflict},
		{"unrelated", 100, ErrActionConflict},
		{"missing-amount", 100, ErrActionConflict},
		{"text-amount", 100, ErrActionConflict},
		{"fractional-amount", 100, ErrActionConflict},
		{"negative-amount", 0, ErrActionConflict},
		{"null-payload", 100, ErrActionConflict},
		{"wrapped-mismatch", 100, ErrActionConflict},
	} {
		t.Run(fixture.id, func(t *testing.T) {
			row := passageSettlementFixture(accountID, "invalid-passage-"+fixture.id, fixture.id, fixture.proceeds, time.Now().UTC())
			if err := database.PutSettlement(ctx, &row); !errors.Is(err, fixture.want) {
				t.Fatalf("invalid passage evidence accepted: %v, want %v", err, fixture.want)
			}
		})
	}
	row := passageSettlementFixture(accountID, "invalid-passage-anchor", "", 100, time.Now().UTC())
	if err := database.PutSettlement(ctx, &row); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("passage without an event accepted: %v", err)
	}
	row.EventID, row.ActionID = "paid", "synthetic-action"
	if err := database.PutSettlement(ctx, &row); !errors.Is(err, ErrNotFound) {
		t.Fatalf("passage accepted a fake optional action: %v", err)
	}
	row.Kind, row.ActionID = "spot", ""
	if err := database.PutSettlement(ctx, &row); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("nonpassage used a fare event instead of an action: %v", err)
	}
}

func TestPassageSettlementsMayShareActualDepartureAction(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	const accountID = "passage-real-action"
	saveTestAccount(t, database, accountID)
	action := Action{ID: "real-passenger-departure", AccountID: accountID, Method: http.MethodPost, Path: "/v1/fleet/depart", Body: json.RawMessage(`{"ship_id":"passenger-ship","dest_port_id":"calais"}`)}
	if _, err := database.BeginAction(ctx, &action); err != nil {
		t.Fatal(err)
	}
	retainedPassengerEvent(t, database, accountID, "paid-first", "passengers_paid", json.RawMessage(`{"fare_paid_pence":100}`))
	first := passageSettlementFixture(accountID, "passage-first", "paid-first", 100, time.Now().UTC())
	first.ActionID = action.ID
	if err := database.PutSettlement(ctx, &first); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("pending optional action accepted as settlement evidence: %v", err)
	}
	if err := database.CompleteAction(ctx, action.ID, http.StatusOK, json.RawMessage(`{}`), nil); err != nil {
		t.Fatal(err)
	}
	if err := database.PutSettlement(ctx, &first); err != nil {
		t.Fatal(err)
	}
	retainedPassengerEvent(t, database, accountID, "paid-second", "passengers_paid", json.RawMessage(`{"fare_paid_pence":100}`))
	second := first
	second.ID, second.EventID = "passage-second", "paid-second"
	if err := database.PutSettlement(ctx, &second); err != nil {
		t.Fatalf("separate passenger groups could not share a real departure action: %v", err)
	}
	spot := putEconomicEvidence(t, database, accountID, "nonpassage-action", "spot", "other", 1, 100, 0, true, nil)
	duplicate := spot
	duplicate.ID += "-duplicate"
	if err := database.PutSettlement(ctx, &duplicate); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("nonpassage lost its unique action anchor: %v", err)
	}
	spot.EventID = "paid-first"
	if err := database.PutSettlement(ctx, &spot); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("nonpassage accepted a passenger event anchor: %v", err)
	}
}

func TestPassageSettlementAuthenticatesEventAnchor(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	const accountID = "passage-anchor-authentication"
	saveTestAccount(t, database, accountID)
	for _, id := range []string{"original", "replacement"} {
		retainedPassengerEvent(t, database, accountID, id, "passengers_paid", json.RawMessage(`{"fare_paid_pence":100}`))
	}
	row := passageSettlementFixture(accountID, "authenticated-passage", "original", 100, time.Now().UTC())
	if err := database.PutSettlement(ctx, &row); err != nil {
		t.Fatal(err)
	}
	changed := row
	changed.EventID = "replacement"
	if err := database.PutSettlement(ctx, &changed); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("event anchor changed on immutable replay: %v", err)
	}
	if err := database.db.Table("settlements").Where("id = ?", row.ID).Update("event_id", "replacement").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := database.SettlementHistory(ctx, accountID, "", 10); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("changed event anchor was not authenticated against private context: %v", err)
	}
}

func TestEconomicHistoryWindowCountsPassage(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	const accountID = "passage-economic-window"
	saveTestAccount(t, database, accountID)
	asOf := time.Now().UTC().Truncate(time.Microsecond)
	for _, fixture := range []struct {
		id        string
		kind      string
		proceeds  int64
		completed time.Time
		known     bool
	}{
		{"winner", "passengers_paid", 200, asOf.Add(-time.Hour), true},
		{"loss", "passengers_lost", 0, asOf.Add(-30 * time.Minute), true},
		{"outside", "passengers_paid", 10_000, asOf.Add(-25 * time.Hour), true},
	} {
		payload := json.RawMessage(fmt.Sprintf(`{"fare_paid_pence":%d}`, fixture.proceeds))
		retainedPassengerEvent(t, database, accountID, fixture.id, fixture.kind, payload)
		row := passageSettlementFixture(accountID, "economic-passage-"+fixture.id, fixture.id, fixture.proceeds, fixture.completed)
		row.CostsKnown = fixture.known
		if err := database.PutSettlement(ctx, &row); err != nil {
			t.Fatal(err)
		}
	}
	history, err := database.EconomicHistoryWindow(ctx, accountID, "london|calais", asOf, 24*time.Hour)
	if err != nil || history.CommercialNetPence != 140 || history.NetPence != 140 || history.PositiveCycles != 1 ||
		history.PositiveQuantityTons != 12 || history.PositiveNetPence != 170 || history.PositiveGameSeconds != 3_600 ||
		history.GameSeconds != 7_200 || history.UnknownCosts || history.UnmeasuredDuration || history.LastCompletion == nil ||
		!history.LastCompletion.Equal(asOf.Add(-30*time.Minute)) {
		t.Fatalf("window excluded passage profit, loss or delivery evidence: %+v, %v", history, err)
	}
	retainedPassengerEvent(t, database, accountID, "unknown", "passengers_paid", json.RawMessage(`{"fare_paid_pence":10}`))
	unknown := passageSettlementFixture(accountID, "economic-passage-unknown", "unknown", 10, asOf)
	unknown.CostsKnown = false
	if err := database.PutSettlement(ctx, &unknown); err != nil {
		t.Fatal(err)
	}
	history, err = database.EconomicHistoryWindow(ctx, accountID, "", asOf, 24*time.Hour)
	if err != nil || history.NetPence != 140 || history.UnknownLossPence != -20 || !history.UnknownCosts || history.PositiveCycles != 1 || history.GameSeconds != 10_800 {
		t.Fatalf("unknown passage costs became positive profit evidence: %+v, %v", history, err)
	}
}

func TestPassengerEventsCompleteChronologicalRetentionRestart(t *testing.T) {
	database, dsn, key := integrationStore(t)
	ctx := context.Background()
	const accountID = "passenger-events"
	saveTestAccount(t, database, accountID)
	saveTestAccount(t, database, "passenger-events-foreign")
	old := time.Now().UTC().Add(-9 * 24 * time.Hour).Truncate(time.Microsecond)
	const total = MaxHistoryLimit*2 + 7
	if err := database.db.Exec(`INSERT INTO events (account_id, event_id, event_type, payload, created_at)
		SELECT ?, 'retained-' || n,
			CASE n % 3 WHEN 0 THEN 'passengers_boarded' WHEN 1 THEN 'passengers_paid' ELSE 'passengers_lost' END,
			jsonb_build_object('fare_paid_pence', n), ?::timestamptz + (n % 3) * INTERVAL '1 second'
		FROM generate_series(1, ?) AS n`, accountID, old, total).Error; err != nil {
		t.Fatal(err)
	}
	unpaid := Event{AccountID: accountID, EventID: "oldest-unpaid", EventType: "passengers_boarded", Payload: json.RawMessage(`{"timestamp":"2026-10-01T00:00:00Z","payload":{"ship_id":"passenger-ship","heads":12}}`), CreatedAt: old.Add(-time.Hour)}
	if err := database.db.Create(&unpaid).Error; err != nil {
		t.Fatal(err)
	}
	for _, event := range []Event{
		{AccountID: accountID, EventID: "old-noise", EventType: "ship_arrived", Payload: json.RawMessage(`{}`), CreatedAt: old},
		{AccountID: "passenger-events-foreign", EventID: "foreign-passenger", EventType: "passengers_paid", Payload: json.RawMessage(`{"fare_paid_pence":500}`), CreatedAt: old},
	} {
		if err := database.db.Create(&event).Error; err != nil {
			t.Fatal(err)
		}
	}
	anchor := passageSettlementFixture(accountID, "retained-payment-anchor", "retained-1", 1, old)
	if err := database.PutSettlement(ctx, &anchor); err != nil {
		t.Fatal(err)
	}
	if err := database.Prune(ctx); err != nil {
		t.Fatalf("pruning removed a passenger payment anchor: %v", err)
	}
	reopened, err := Open(ctx, dsn, key, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	events, err := reopened.PassengerEvents(ctx, accountID)
	if err != nil || len(events) != total+1 || events[0].EventID != unpaid.EventID || string(events[0].Payload) == "" {
		t.Fatalf("keyset passenger recovery omitted retained unpaid history: count=%d, %v", len(events), err)
	}
	var oldestPayload, wantPayload any
	if err := json.Unmarshal(events[0].Payload, &oldestPayload); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(unpaid.Payload, &wantPayload); err != nil {
		t.Fatal(err)
	}
	actualJSON, _ := json.Marshal(oldestPayload)
	wantJSON, _ := json.Marshal(wantPayload)
	if string(actualJSON) != string(wantJSON) {
		t.Fatal("passenger events changed the retained SSE envelope")
	}
	seen := make(map[string]bool, len(events))
	for index, event := range events {
		if seen[event.EventID] || event.AccountID != accountID || event.EventID == "old-noise" {
			t.Fatalf("keyset recovery duplicated or leaked events: %+v", event)
		}
		seen[event.EventID] = true
		if index != 0 {
			previous := events[index-1]
			if previous.CreatedAt.After(event.CreatedAt) || previous.CreatedAt.Equal(event.CreatedAt) && previous.ID >= event.ID {
				t.Fatalf("passenger recovery is not stable chronological: %+v then %+v", previous, event)
			}
		}
	}
	var noise int64
	if err := reopened.db.Model(&Event{}).Where("account_id = ? AND event_id = ?", accountID, "old-noise").Count(&noise).Error; err != nil || noise != 0 {
		t.Fatalf("ordinary diagnostic retention changed: %d, %v", noise, err)
	}
	if err := reopened.PutSettlement(ctx, &anchor); err != nil {
		t.Fatalf("retained old passage could not replay after restart: %v", err)
	}
	if _, err := reopened.PassengerEvents(ctx, ""); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid passenger event query accepted: %v", err)
	}
}

func TestPassengerMigrationRoundTripPreservesLegacySettlements(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	const accountID = "passenger-migration"
	saveTestAccount(t, database, accountID)
	legacy := putEconomicEvidence(t, database, accountID, "passenger-legacy", "spot", "legacy-lane", 4, 100, 20, true, nil)
	raw, err := migrations.ReadFile("migrations/00013_passengers.sql")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, database.sqlDB,
		fstest.MapFS{"00013_passengers.sql": {Data: raw}},
		goose.WithDisableGlobalRegistry(true), goose.WithDisableVersioning(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Down(ctx); err != nil {
		t.Fatalf("passenger migration down: %v", err)
	}
	var nullable string
	if err := database.db.Raw(`SELECT is_nullable FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'settlements' AND column_name = 'action_id'`).Scan(&nullable).Error; err != nil || nullable != "NO" {
		t.Fatalf("down migration did not restore required action evidence: %q, %v", nullable, err)
	}
	var removed bool
	if err := database.db.Raw("SELECT to_regclass('passenger_evidence') IS NULL").Scan(&removed).Error; err != nil || !removed {
		t.Fatalf("down migration retained passenger table: %v, %v", removed, err)
	}
	if _, err := provider.UpTo(ctx, 13); err != nil {
		t.Fatalf("passenger migration up: %v", err)
	}
	if err := database.db.Raw(`SELECT is_nullable FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'settlements' AND column_name = 'action_id'`).Scan(&nullable).Error; err != nil || nullable != "YES" {
		t.Fatalf("passage action evidence remained required: %q, %v", nullable, err)
	}
	rows, err := database.SettlementHistory(ctx, accountID, legacy.Lane, 10)
	if err != nil || len(rows) != 1 || !sameSettlement(rows[0], legacy) || rows[0].EventID != "" {
		t.Fatalf("passenger migration changed legacy evidence: %+v, %v", rows, err)
	}
	if err := database.PutSettlement(ctx, &legacy); err != nil {
		t.Fatalf("legacy action-anchored replay changed after migration: %v", err)
	}
	retainedPassengerEvent(t, database, accountID, "migration-paid", "passengers_paid", json.RawMessage(`{"fare_paid_pence":100}`))
	row := passageSettlementFixture(accountID, "migration-passage", "migration-paid", 100, time.Now().UTC())
	if err := database.PutSettlement(ctx, &row); err != nil {
		t.Fatal(err)
	}
	evidence := passengerEvidenceFixture(accountID, "migration-passenger-evidence", "london|calais")
	if err := database.PutPassengerEvidence(ctx, &evidence); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Down(ctx); err == nil {
		t.Fatal("rollback discarded a passage's only event anchor")
	}
	if history, err := database.PassengerEvidenceHistory(ctx, accountID, "", 10); err != nil || len(history) != 1 {
		t.Fatalf("failed rollback lost passenger evidence: %+v, %v", history, err)
	}
	if err := database.PutSettlement(ctx, &row); err != nil {
		t.Fatalf("failed rollback changed event-anchored accounting: %v", err)
	}
}

func TestPassengerEventsSinceAccountBoundaryRetentionRestart(t *testing.T) {
	database, dsn, key := integrationStore(t)
	ctx := context.Background()
	const accountID = "passenger-events-since"
	saveTestAccount(t, database, accountID)
	saveTestAccount(t, database, "passenger-events-since-foreign")
	since := time.Now().UTC().Add(-6 * 24 * time.Hour).Truncate(time.Microsecond)
	const total = MaxHistoryLimit*2 + 7
	if err := database.db.Exec(`INSERT INTO events (account_id, event_id, event_type, payload, created_at)
		SELECT ?, 'window-' || n,
			CASE n % 3 WHEN 0 THEN 'passengers_boarded' WHEN 1 THEN 'passengers_paid' ELSE 'passengers_lost' END,
			jsonb_build_object('fare_paid_pence', n), ?::timestamptz + (n % 2) * INTERVAL '1 second'
		FROM generate_series(1, ?) AS n`, accountID, since, total).Error; err != nil {
		t.Fatal(err)
	}
	for _, event := range []Event{
		{AccountID: accountID, EventID: "before-window", EventType: "passengers_boarded", Payload: json.RawMessage(`{"heads":12}`), CreatedAt: since.Add(-time.Microsecond)},
		{AccountID: accountID, EventID: "window-boundary", EventType: "passengers_boarded", Payload: json.RawMessage(`{"heads":12}`), CreatedAt: since},
		{AccountID: accountID, EventID: "window-noise", EventType: "ship_arrived", Payload: json.RawMessage(`{}`), CreatedAt: since},
		{AccountID: "passenger-events-since-foreign", EventID: "foreign-window", EventType: "passengers_paid", Payload: json.RawMessage(`{"fare_paid_pence":500}`), CreatedAt: since},
	} {
		if err := database.db.Create(&event).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := database.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, dsn, key, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	events, err := reopened.PassengerEventsSince(ctx, accountID, since.In(time.FixedZone("receipt", 2*60*60)))
	if err != nil || len(events) != total+1 {
		t.Fatalf("mission-window passenger recovery omitted keyset history after retention: count=%d, %v", len(events), err)
	}
	seen := make(map[string]bool, len(events))
	for index, event := range events {
		if event.AccountID != accountID || event.CreatedAt.Before(since) || seen[event.EventID] ||
			event.EventID == "window-noise" || event.EventID == "before-window" || event.EventID == "foreign-window" {
			t.Fatalf("passenger recovery crossed its account or inclusive time boundary: %+v", event)
		}
		seen[event.EventID] = true
		if index != 0 {
			previous := events[index-1]
			if previous.CreatedAt.After(event.CreatedAt) || previous.CreatedAt.Equal(event.CreatedAt) && previous.ID >= event.ID {
				t.Fatalf("mission-window passenger recovery lost chronological keyset ordering: %+v then %+v", previous, event)
			}
		}
	}
	if !seen["window-boundary"] {
		t.Fatal("passenger event received exactly at mission start was omitted")
	}
	later, err := reopened.PassengerEventsSince(ctx, accountID, since.Add(time.Microsecond))
	if err != nil || len(later) != (total+1)/2 {
		t.Fatalf("passenger event time boundary was rounded or applied only to the first page: count=%d, %v", len(later), err)
	}
	for _, event := range later {
		if !event.CreatedAt.Equal(since.Add(time.Second)) {
			t.Fatalf("event before the requested boundary survived a later page: %+v", event)
		}
	}
	empty, err := reopened.PassengerEventsSince(ctx, accountID, since.Add(2*time.Second))
	if err != nil || len(empty) != 0 {
		t.Fatalf("passenger recovery returned events before its empty window: count=%d, %v", len(empty), err)
	}
	all, err := reopened.PassengerEvents(ctx, accountID)
	if err != nil || len(all) != total+2 || all[0].EventID != "before-window" {
		t.Fatalf("all-history passenger recovery lost retained pre-window boarding: count=%d, %v", len(all), err)
	}
	zeroSince, err := reopened.PassengerEventsSince(ctx, accountID, time.Time{})
	if err != nil || len(zeroSince) != len(all) {
		t.Fatalf("zero-since passenger recovery changed all-history behavior: count=%d, %v", len(zeroSince), err)
	}
	for index, event := range all {
		if zeroSince[index].ID != event.ID {
			t.Fatal("all-history and zero-since passenger recovery diverged")
		}
	}
	if _, err := reopened.PassengerEventsSince(ctx, "", since); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid passenger window account accepted: %v", err)
	}
}

func TestPassengerEventsSinceClampsOldAndZeroStartsToSafeScanHorizon(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	const accountID = "passenger-events-horizon"
	saveTestAccount(t, database, accountID)
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, event := range []Event{
		{AccountID: accountID, EventID: "outside-horizon", EventType: "passengers_boarded", Payload: json.RawMessage(`{"heads":12}`), CreatedAt: now.Add(-PassengerReceiptLifetime - time.Hour)},
		{AccountID: accountID, EventID: "inside-horizon", EventType: "passengers_paid", Payload: json.RawMessage(`{"fare_paid_pence":500}`), CreatedAt: now.Add(-PassengerReceiptLifetime + time.Hour)},
		{AccountID: accountID, EventID: "future-record", EventType: "passengers_paid", Payload: json.RawMessage(`{"fare_paid_pence":700}`), CreatedAt: now.Add(time.Hour)},
	} {
		if err := database.db.Create(&event).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, since := range []time.Time{time.Time{}, now.Add(-30 * 24 * time.Hour)} {
		rows, err := database.PassengerEventsSince(ctx, accountID, since)
		if err != nil || len(rows) != 1 || rows[0].EventID != "inside-horizon" {
			t.Fatalf("bounded recovery scanned outside its seven-day real window: %+v %v", rows, err)
		}
	}
	all, err := database.PassengerEvents(ctx, accountID)
	if err != nil || len(all) != 2 || all[0].EventID != "outside-horizon" {
		t.Fatalf("explicit retained-history API lost its older evidence: %+v %v", all, err)
	}
}
