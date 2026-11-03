package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestShipLossesSurviveSnapshotsPruningAndRestart(t *testing.T) {
	db, dsn, key := integrationStore(t)
	ctx := context.Background()
	const account = "ship-loss-durable"
	saveTestAccount(t, db, account)
	observedAt := time.Now().UTC().Add(-10 * 24 * time.Hour).Truncate(time.Microsecond)
	lastObservedAt := observedAt.Add(-time.Hour)
	port, zone, origin, destination := "london", "north-sea", "london", "amsterdam"
	cause := "storm"
	claim := int64(1200)
	loss := ShipLoss{
		AccountID: account, ShipID: "hull-z", ShipName: "Lapras", ShipClass: "hoy",
		Fate: "lost_at_sea", Status: "sunk", ObservedAt: observedAt, LastObservedAt: &lastObservedAt,
		LastPortID: &port, LastSeaZoneID: &zone, VoyageOriginID: &origin, VoyageDestinationID: &destination,
		CargoManifest: json.RawMessage(`{"grain":7,"timber":0}`),
		Stores:        json.RawMessage(`{"provisions_tons":2.5,"sea_days_remaining":3.25}`),
		HazardCause:   &cause, InsuranceClaimPaidPence: &claim,
	}
	missing := ShipLoss{ShipID: "hull-a", Fate: "missing", Status: "sunk", ObservedAt: observedAt}
	recoveredAt := observedAt.Add(-30 * time.Second)
	earlier := ShipLoss{
		ShipID: "hull-first", Fate: "missing", Status: "sunk",
		ObservedAt: observedAt.Add(-time.Minute), RecoveredAt: &recoveredAt,
	}
	if err := db.SaveSnapshot(ctx, account, map[string]json.RawMessage{"ships": json.RawMessage(`[{"ship":{"id":"hull-z"}}]`)}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveShipLosses(ctx, account, []ShipLoss{loss, missing, earlier}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveSnapshot(ctx, account, map[string]json.RawMessage{"ships": json.RawMessage(`[]`)}); err != nil {
		t.Fatal(err)
	}
	if err := db.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, dsn, key, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	rows, err := reopened.ShipLosses(ctx, account)
	if err != nil || len(rows) != 3 {
		t.Fatalf("durable losses after replacement, pruning and restart: %+v, %v", rows, err)
	}
	if rows[0].ShipID != "hull-first" || rows[1].ShipID != "hull-a" || rows[2].ShipID != "hull-z" {
		t.Fatalf("loss ordering changed: %+v", rows)
	}
	if rows[0].RecoveredAt == nil || !rows[0].RecoveredAt.Equal(recoveredAt) {
		t.Fatalf("recovery marker did not survive replacement, pruning and restart: %+v", rows[0])
	}
	got := rows[2]
	if got.AccountID != account || got.ShipName != loss.ShipName || got.ShipClass != loss.ShipClass ||
		got.Fate != loss.Fate || got.Status != loss.Status || !got.ObservedAt.Equal(observedAt) ||
		got.LastObservedAt == nil || !got.LastObservedAt.Equal(lastObservedAt) ||
		got.LastPortID == nil || *got.LastPortID != port || got.LastSeaZoneID == nil || *got.LastSeaZoneID != zone ||
		got.VoyageOriginID == nil || *got.VoyageOriginID != origin || got.VoyageDestinationID == nil || *got.VoyageDestinationID != destination ||
		got.HazardCause == nil || *got.HazardCause != cause || got.InsuranceClaimPaidPence == nil || *got.InsuranceClaimPaidPence != claim {
		t.Fatalf("loss evidence did not round trip: %+v", got)
	}
	assertShipLossJSON(t, got.CargoManifest, loss.CargoManifest)
	assertShipLossJSON(t, got.Stores, loss.Stores)
	payload, err := json.Marshal(rows[1])
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"recovered_at", "last_observed_at", "last_port_id", "last_sea_zone_id", "voyage_origin_id", "voyage_destination_id", "cargo_manifest", "stores", "hazard_cause", "insurance_claim_paid_pence"} {
		if string(fields[name]) != "null" {
			t.Errorf("unknown %s was not preserved as null: %s", name, payload)
		}
	}
	if err := reopened.SaveShipLosses(ctx, account, rows); err != nil {
		t.Fatalf("retained losses could not be replayed after restart: %v", err)
	}
}

func TestShipLossesKeepEarliestObservationAndFirstKnownEvidence(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	const account = "ship-loss-upsert"
	saveTestAccount(t, db, account)
	at := time.Now().UTC().Truncate(time.Microsecond)
	lastAfloat := at.Add(-time.Hour)
	port, zone, origin, destination, cause := "london", "north-sea", "london", "amsterdam", "storm"
	zeroClaim := int64(0)
	for _, initialFate := range []string{"missing", "unknown"} {
		t.Run(initialFate, func(t *testing.T) {
			initial := ShipLoss{ShipID: initialFate, Fate: initialFate, Status: "sunk", ObservedAt: at}
			if err := db.SaveShipLosses(ctx, account, []ShipLoss{initial}); err != nil {
				t.Fatal(err)
			}
			known := initial
			known.ShipName, known.ShipClass, known.Fate = "Lapras", "hoy", "lost_at_sea"
			known.ObservedAt = at.Add(time.Hour)
			known.LastObservedAt, known.LastPortID, known.LastSeaZoneID = &lastAfloat, &port, &zone
			known.VoyageOriginID, known.VoyageDestinationID = &origin, &destination
			known.CargoManifest = json.RawMessage(`{"grain":7}`)
			known.Stores = json.RawMessage(`{"provisions_tons":2.5,"sea_days_remaining":3}`)
			known.HazardCause, known.InsuranceClaimPaidPence = &cause, &zeroClaim
			if err := db.SaveShipLosses(ctx, account, []ShipLoss{known}); err != nil {
				t.Fatal(err)
			}
			changed := known
			changed.ShipName, changed.ShipClass, changed.Fate = "Other", "brig", "captured"
			changed.ObservedAt = at.Add(-time.Minute)
			laterAfloat := at.Add(-time.Second)
			otherPort, otherZone, otherCause := "boston", "atlantic", "piracy"
			otherClaim := int64(900)
			changed.LastObservedAt, changed.LastPortID, changed.LastSeaZoneID = &laterAfloat, &otherPort, &otherZone
			changed.VoyageOriginID, changed.VoyageDestinationID = &otherPort, &otherPort
			changed.CargoManifest = json.RawMessage(`{}`)
			changed.Stores = json.RawMessage(`{"provisions_tons":0,"sea_days_remaining":0}`)
			changed.HazardCause, changed.InsuranceClaimPaidPence = &otherCause, &otherClaim
			for _, replay := range []ShipLoss{changed, initial, known} {
				if err := db.SaveShipLosses(ctx, account, []ShipLoss{replay}); err != nil {
					t.Fatal(err)
				}
			}
			rows, err := db.ShipLosses(ctx, account)
			if err != nil {
				t.Fatal(err)
			}
			var got *ShipLoss
			for i := range rows {
				if rows[i].ShipID == initialFate {
					got = &rows[i]
				}
			}
			if got == nil || got.ShipName != "Lapras" || got.ShipClass != "hoy" || got.Fate != "lost_at_sea" || got.Status != "sunk" ||
				!got.ObservedAt.Equal(changed.ObservedAt) || got.LastObservedAt == nil || !got.LastObservedAt.Equal(lastAfloat) ||
				got.LastPortID == nil || *got.LastPortID != port || got.LastSeaZoneID == nil || *got.LastSeaZoneID != zone ||
				got.VoyageOriginID == nil || *got.VoyageOriginID != origin || got.VoyageDestinationID == nil || *got.VoyageDestinationID != destination ||
				got.HazardCause == nil || *got.HazardCause != cause || got.InsuranceClaimPaidPence == nil || *got.InsuranceClaimPaidPence != 0 {
				t.Fatalf("earliest observation or first known evidence changed on replay: %+v", got)
			}
			assertShipLossJSON(t, got.CargoManifest, known.CargoManifest)
			assertShipLossJSON(t, got.Stores, known.Stores)
		})
	}
	withoutTime := ShipLoss{
		ShipID: "afloat-time-unknown", Fate: "lost_at_sea", Status: "sunk", ObservedAt: at,
		CargoManifest: json.RawMessage(`{}`), Stores: json.RawMessage(`{"provisions_tons":0,"sea_days_remaining":0}`),
	}
	if err := db.SaveShipLosses(ctx, account, []ShipLoss{withoutTime}); err != nil {
		t.Fatalf("known cargo and stores required an invented last-afloat timestamp: %v", err)
	}
	rows, err := db.ShipLosses(ctx, account)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row.ShipID == withoutTime.ShipID {
			found = true
			if row.LastObservedAt != nil {
				t.Fatal("retained cargo and stores acquired an invented last-afloat timestamp")
			}
			assertShipLossJSON(t, row.CargoManifest, withoutTime.CargoManifest)
			assertShipLossJSON(t, row.Stores, withoutTime.Stores)
		}
	}
	if !found {
		t.Fatal("loss with known cargo and an unknown last-afloat timestamp was not retained")
	}
}

func TestShipLossesUpgradeConfirmedUnknownFateWithoutDowngradingTerminalEvidence(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	const account = "ship-loss-confirmed-unknown"
	saveTestAccount(t, db, account)
	at := time.Now().UTC().Truncate(time.Microsecond)
	for _, initialFate := range []string{"missing", "unknown", "lost_at_sea", "captured", "broken_up", "royal_navy", "seized_customs"} {
		initial := ShipLoss{ShipID: initialFate, Fate: initialFate, Status: "sunk", ObservedAt: at}
		if err := db.SaveShipLosses(ctx, account, []ShipLoss{initial}); err != nil {
			t.Fatal(err)
		}
		confirmed := initial
		confirmed.Fate = "unknown"
		confirmed.ObservedAt = at.Add(time.Hour)
		cause := "storm"
		confirmed.HazardCause = &cause
		if err := db.SaveShipLosses(ctx, account, []ShipLoss{confirmed}); err != nil {
			t.Fatal(err)
		}
		if err := db.SaveShipLosses(ctx, account, []ShipLoss{initial}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := db.ShipLosses(ctx, account)
	if err != nil || len(rows) != 7 {
		t.Fatalf("confirmed fate records: %+v, %v", rows, err)
	}
	for _, row := range rows {
		wantFate := row.ShipID
		if wantFate == "missing" {
			wantFate = "unknown"
		}
		if row.Fate != wantFate || row.Status != "sunk" || !row.ObservedAt.Equal(at) ||
			row.HazardCause == nil || *row.HazardCause != "storm" {
			t.Fatalf("confirmation downgraded terminal evidence or kept an inferred absence: %+v", row)
		}
	}
}

func TestShipLossesAreAccountScopedAndCascadeOnDeletion(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	const one, two = "ship-loss-one", "ship-loss-two"
	saveTestAccount(t, db, one)
	saveTestAccount(t, db, two)
	loss := ShipLoss{ShipID: "same-hull", ShipName: "Lapras", Fate: "missing", Status: "sunk", ObservedAt: time.Now().UTC()}
	if err := db.SaveShipLosses(ctx, one, []ShipLoss{loss}); err != nil {
		t.Fatal(err)
	}
	loss.ShipName = "Blastoise"
	if err := db.SaveShipLosses(ctx, two, []ShipLoss{loss}); err != nil {
		t.Fatal(err)
	}
	for account, name := range map[string]string{one: "Lapras", two: "Blastoise"} {
		rows, err := db.ShipLosses(ctx, account)
		if err != nil || len(rows) != 1 || rows[0].AccountID != account || rows[0].ShipName != name {
			t.Fatalf("loss read crossed account boundary: %+v, %v", rows, err)
		}
	}
	if err := db.db.Where("id = ?", one).Delete(&Account{}).Error; err != nil {
		t.Fatal(err)
	}
	rows, err := db.ShipLosses(ctx, one)
	if err != nil || rows == nil || len(rows) != 0 {
		t.Fatalf("deleted account retained loss rows or lost the empty array shape: %+v, %v", rows, err)
	}
	rows, err = db.ShipLosses(ctx, two)
	if err != nil || len(rows) != 1 || rows[0].ShipName != "Blastoise" {
		t.Fatalf("account deletion removed another account's loss: %+v, %v", rows, err)
	}
}

func TestShipLossesRespectOwnedWriteFence(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	saveTestAccount(t, db, "loss-owner")
	saveTestAccount(t, db, "loss-other-owner")
	ownedCtx, release, err := db.AcquireAccountContext(ctx, "loss-owner")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	loss := ShipLoss{ShipID: "hull", Fate: "missing", Status: "sunk", ObservedAt: time.Now().UTC()}
	if err := db.SaveShipLosses(ownedCtx, "loss-owner", []ShipLoss{loss}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveShipLosses(ownedCtx, "loss-other-owner", []ShipLoss{loss}); !errors.Is(err, ErrOwnershipLost) {
		t.Fatalf("foreign ownership context wrote another account's loss: %v", err)
	}
	rows, err := db.ShipLosses(ctx, "loss-other-owner")
	if err != nil || len(rows) != 0 {
		t.Fatalf("fenced write persisted a loss: %+v, %v", rows, err)
	}
}

func TestShipLossesRecoveredMissingHullStartsOnlyStrictlyLaterEpisode(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Microsecond)
	lastAfloat := at.Add(-time.Hour)
	recoveredAt := at.Add(time.Hour)
	port, zone, origin, destination, cause := "london", "north-sea", "london", "amsterdam", "storm"
	claim := int64(1200)
	for _, fate := range []string{"lost_at_sea", "unknown", "missing"} {
		t.Run(fate, func(t *testing.T) {
			account := "ship-recovery-" + fate
			saveTestAccount(t, db, account)
			initial := ShipLoss{
				AccountID: account, ShipID: "hull", ShipName: "Lapras", ShipClass: "hoy",
				Fate: "missing", Status: "sunk", ObservedAt: at, LastObservedAt: &lastAfloat,
				LastPortID: &port, LastSeaZoneID: &zone, VoyageOriginID: &origin, VoyageDestinationID: &destination,
				CargoManifest: json.RawMessage(`{"grain":7}`),
				Stores:        json.RawMessage(`{"provisions_tons":2.5,"sea_days_remaining":3}`),
				HazardCause:   &cause, InsuranceClaimPaidPence: &claim,
			}
			assertLoss := func(want ShipLoss) {
				t.Helper()
				rows, err := db.ShipLosses(ctx, account)
				if err != nil || len(rows) != 1 {
					t.Fatalf("loss episode read: %+v, %v", rows, err)
				}
				got := rows[0]
				got.ObservedAt = got.ObservedAt.UTC()
				for _, observed := range []*time.Time{got.LastObservedAt, got.RecoveredAt} {
					if observed != nil {
						*observed = observed.UTC()
					}
				}
				actual, err := json.Marshal(got)
				if err != nil {
					t.Fatal(err)
				}
				expected, err := json.Marshal(want)
				if err != nil {
					t.Fatal(err)
				}
				assertShipLossJSON(t, actual, expected)
			}
			if err := db.SaveShipLosses(ctx, account, []ShipLoss{initial}); err != nil {
				t.Fatal(err)
			}
			recovery := initial
			recovery.RecoveredAt = &recoveredAt
			recovery.ShipName, recovery.ShipClass = "Other", "brig"
			recovery.CargoManifest = json.RawMessage(`{}`)
			if err := db.SaveShipLosses(ctx, account, []ShipLoss{recovery}); err != nil {
				t.Fatal(err)
			}
			expected := initial
			expected.RecoveredAt = &recoveredAt
			assertLoss(expected)

			dashboard, err := db.Dashboard(ctx)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, displayed := range dashboard.Accounts {
				if displayed.ID != account {
					continue
				}
				found = true
				if len(displayed.ShipLosses) != 1 || displayed.ShipLosses[0].RecoveredAt == nil ||
					!displayed.ShipLosses[0].RecoveredAt.Equal(recoveredAt) {
					t.Fatalf("dashboard omitted typed recovery marker: %+v", displayed.ShipLosses)
				}
				raw, err := json.Marshal(displayed.ShipLosses[0])
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(raw, &fields); err != nil {
					t.Fatal(err)
				}
				var exposedAt time.Time
				if err := json.Unmarshal(fields["recovered_at"], &exposedAt); err != nil || !exposedAt.Equal(recoveredAt) {
					t.Fatalf("dashboard recovery marker was not a JSON time: %s, %v", raw, err)
				}
			}
			if !found {
				t.Fatal("dashboard omitted recovered hull's account")
			}
			for _, retriedAt := range []time.Time{recoveredAt.Add(time.Hour), recoveredAt.Add(-30 * time.Minute)} {
				recovery.RecoveredAt = &retriedAt
				if err := db.SaveShipLosses(ctx, account, []ShipLoss{recovery}); err != nil {
					t.Fatal(err)
				}
				assertLoss(expected)
			}

			fresh := ShipLoss{
				AccountID: account, ShipID: initial.ShipID, ShipName: "Blastoise", ShipClass: "cutter",
				Fate: fate, Status: "sunk", ObservedAt: recoveredAt,
			}
			for _, earlierAt := range []time.Time{recoveredAt, recoveredAt.Add(-time.Microsecond)} {
				fresh.ObservedAt = earlierAt
				if err := db.SaveShipLosses(ctx, account, []ShipLoss{fresh}); err != nil {
					t.Fatal(err)
				}
				assertLoss(expected)
			}
			fresh.ObservedAt = at.Add(-time.Minute)
			if err := db.SaveShipLosses(ctx, account, []ShipLoss{fresh}); err != nil {
				t.Fatal(err)
			}
			expected.ObservedAt = fresh.ObservedAt
			assertLoss(expected)

			fresh.ObservedAt = recoveredAt.Add(time.Microsecond)
			if err := db.SaveShipLosses(ctx, account, []ShipLoss{fresh}); err != nil {
				t.Fatal(err)
			}
			assertLoss(fresh)
			recovery.RecoveredAt = &recoveredAt
			if err := db.SaveShipLosses(ctx, account, []ShipLoss{recovery}); err != nil {
				t.Fatal(err)
			}
			assertLoss(fresh)
			if fate != "missing" {
				laterRecovery := fresh.ObservedAt.Add(time.Hour)
				recovery.RecoveredAt = &laterRecovery
				if err := db.SaveShipLosses(ctx, account, []ShipLoss{recovery}); err != nil {
					t.Fatal(err)
				}
				assertLoss(fresh)
			}
			invalidRecovery := fresh.ObservedAt
			if fate != "missing" {
				invalidRecovery = invalidRecovery.Add(time.Hour)
			}
			if err := db.db.Exec("UPDATE ship_losses SET recovered_at = ? WHERE account_id = ? AND ship_id = ?",
				invalidRecovery, account, initial.ShipID).Error; err == nil {
				t.Fatal("database accepted a terminal or non-later recovery")
			}
			assertLoss(fresh)
		})
	}
}

func TestShipLossEventsReturnAllRetainedRelevantAccountEvidenceInSequence(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	credentials := saveTestAccount(t, db, "loss-events")
	saveTestAccount(t, db, "loss-events-other")
	at := time.Now().UTC().Truncate(time.Microsecond)
	anchor := "hazard-" + credentials.Passphrase
	for _, event := range []Event{
		{AccountID: "loss-events", EventID: "claim", EventType: "insurance_claim_paid", Payload: json.RawMessage(`{"ship_id":"hull","paid_pence":1200}`), CreatedAt: at},
		{AccountID: "loss-events", EventID: "closed", EventType: "insurance_policy_closed", Payload: json.RawMessage(`{"ship_id":"hull"}`), CreatedAt: at},
		{AccountID: "loss-events", EventID: anchor, EventType: "voyage_hazard", Payload: json.RawMessage(`{"ship_id":"hull","cause":"storm"}`), CreatedAt: at.Add(-24 * time.Hour)},
		{AccountID: "loss-events", EventID: "unrelated", EventType: "ship_arrived", Payload: json.RawMessage(`{}`), CreatedAt: at.Add(-time.Hour)},
		{AccountID: "loss-events-other", EventID: "foreign", EventType: "voyage_hazard", Payload: json.RawMessage(`{}`), CreatedAt: at.Add(-time.Hour)},
	} {
		if err := db.db.Create(&event).Error; err != nil {
			t.Fatal(err)
		}
	}
	// Loss reconstruction cannot use the dashboard's 100-event or history limit.
	later := make([]Event, MaxHistoryLimit+1)
	for i := range later {
		later[i] = Event{AccountID: "loss-events", EventID: fmt.Sprintf("later-%d", i), EventType: "voyage_hazard", Payload: json.RawMessage(`{}`), CreatedAt: at.Add(time.Hour)}
	}
	if err := db.db.Create(&later).Error; err != nil {
		t.Fatal(err)
	}
	rows, err := db.ShipLossEvents(ctx, "loss-events", 0)
	if err != nil || len(rows) != MaxHistoryLimit+4 {
		t.Fatalf("retained loss evidence was capped or mixed accounts: count=%d, %v", len(rows), err)
	}
	if rows[0].EventID != "claim" || rows[1].EventID != "closed" || rows[2].EventID != anchor {
		t.Fatalf("loss event sequence or exact event anchor changed: %+v", rows[:3])
	}
	for i, event := range rows {
		if event.AccountID != "loss-events" || i > 0 && event.ID <= rows[i-1].ID {
			t.Fatalf("loss event sequence or account scope changed at %d: %+v", i, event)
		}
	}
	incremental, err := db.ShipLossEvents(ctx, "loss-events", rows[1].ID)
	if err != nil || len(incremental) != MaxHistoryLimit+2 || incremental[0].EventID != anchor {
		t.Fatalf("loss event cursor repeated old rows, mixed types or skipped retained evidence: %+v, %v", incremental, err)
	}
	for _, event := range incremental {
		if event.ID <= rows[1].ID || event.AccountID != "loss-events" || event.EventType != "voyage_hazard" {
			t.Fatalf("incremental loss event read crossed its cursor, account or type boundary: %+v", event)
		}
	}
	empty, err := db.ShipLossEvents(ctx, "loss-events", rows[len(rows)-1].ID)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("current loss event cursor did not return an empty array: %+v, %v", empty, err)
	}
}

func TestDashboardShipLossesAreTypedScrubbedAndWithheldWithoutCredentials(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	const healthyID, emptyID = "loss-display-healthy", "loss-display-empty"
	credentials := saveTestAccount(t, db, healthyID)
	saveTestAccount(t, db, emptyID)
	at := time.Now().UTC().Truncate(time.Microsecond)
	shipID, portID, zoneID := "ship-"+credentials.Passphrase, "port-"+credentials.Passphrase, "zone-"+credentials.APIKey
	originID, destinationID := "origin-"+credentials.APIKey, "destination-"+credentials.Passphrase
	cause := "storm " + credentials.Passphrase
	cargo := json.RawMessage(fmt.Sprintf(`{"grain":7,%q:1,"tw_test_unlisted_commodity":2}`, credentials.Passphrase))
	loss := ShipLoss{
		ShipID: shipID, ShipName: "Lapras " + credentials.Passphrase, ShipClass: "hoy " + credentials.APIKey,
		Fate: "lost_at_sea", Status: "sunk", ObservedAt: at,
		LastPortID: &portID, LastSeaZoneID: &zoneID, VoyageOriginID: &originID, VoyageDestinationID: &destinationID,
		CargoManifest: cargo, Stores: json.RawMessage(`{"provisions_tons":2.5,"sea_days_remaining":3}`), HazardCause: &cause,
	}
	if err := db.SaveShipLosses(ctx, healthyID, []ShipLoss{loss}); err != nil {
		t.Fatal(err)
	}
	stored, err := db.ShipLosses(ctx, healthyID)
	if err != nil || len(stored) != 1 || stored[0].ShipID != shipID || stored[0].LastPortID == nil || *stored[0].LastPortID != portID || stored[0].HazardCause == nil {
		t.Fatalf("internal loss evidence anchors were display-scrubbed: %+v, %v", stored, err)
	}
	for _, value := range []string{stored[0].ShipName, stored[0].ShipClass, *stored[0].HazardCause, string(stored[0].CargoManifest)} {
		if strings.Contains(value, credentials.Passphrase) || strings.Contains(value, credentials.APIKey) || strings.Contains(value, "tw_test_unlisted_commodity") {
			t.Fatal("loss payload retained a credential at write time")
		}
	}
	// Existing recordings can predate a write-time scrub or credential rotation.
	if err := db.db.Exec(`UPDATE ship_losses SET ship_name = ?, ship_class = ?, fate = ?, status = ?, hazard_cause = ?, cargo_manifest = ?::jsonb
		WHERE account_id = ? AND ship_id = ?`, loss.ShipName, loss.ShipClass, "lost_at_sea "+credentials.Passphrase,
		"sunk "+credentials.APIKey, cause, string(cargo), healthyID, shipID).Error; err != nil {
		t.Fatal(err)
	}
	canaries := []string{credentials.Passphrase, credentials.APIKey, "tw_test_unlisted_commodity"}
	for _, mode := range []string{"missing", "corrupt"} {
		id := "loss-display-" + mode
		private := saveTestAccount(t, db, id)
		unavailable := ShipLoss{ShipID: private.Passphrase, ShipName: private.Passphrase, Fate: "unknown", Status: "sunk", ObservedAt: at}
		if err := db.SaveShipLosses(ctx, id, []ShipLoss{unavailable}); err != nil {
			t.Fatal(err)
		}
		if mode == "missing" {
			if err := db.db.Where("account_id = ?", id).Delete(&accountSecret{}).Error; err != nil {
				t.Fatal(err)
			}
		} else if err := db.db.Model(&accountSecret{}).Where("account_id = ?", id).Update("ciphertext", []byte("corrupt")).Error; err != nil {
			t.Fatal(err)
		}
		canaries = append(canaries, private.Passphrase, private.APIKey)
	}
	dashboard, err := db.Dashboard(ctx)
	if err != nil {
		t.Fatal(err)
	}
	accounts := make(map[string]DashboardAccount)
	for _, account := range dashboard.Accounts {
		accounts[account.ID] = account
	}
	if len(accounts) != 4 || len(accounts[healthyID].ShipLosses) != 1 {
		t.Fatalf("dashboard omitted typed account loss history: %+v", accounts)
	}
	for _, id := range []string{emptyID, "loss-display-missing", "loss-display-corrupt"} {
		if accounts[id].ShipLosses == nil || len(accounts[id].ShipLosses) != 0 {
			t.Errorf("empty or unavailable account %s did not return a safe empty loss array", id)
		}
	}
	for _, id := range []string{"loss-display-missing", "loss-display-corrupt"} {
		var meta map[string]struct {
			Complete bool   `json:"complete"`
			Stale    bool   `json:"stale"`
			Error    string `json:"error"`
		}
		if err := json.Unmarshal(accounts[id].Snapshot["_meta"], &meta); err != nil {
			t.Fatal(err)
		}
		lossMeta, exists := meta["ship_losses"]
		if !exists || lossMeta.Complete || !lossMeta.Stale || lossMeta.Error == "" {
			t.Errorf("unavailable account %s presented withheld loss history as a successful empty observation", id)
		}
	}
	displayed := accounts[healthyID].ShipLosses[0]
	if !strings.Contains(displayed.ShipID, redacted) || displayed.LastPortID == nil || !strings.Contains(*displayed.LastPortID, redacted) {
		t.Fatalf("dashboard exposed exact untrusted ID anchors: %+v", displayed)
	}
	var manifest map[string]any
	if err := json.Unmarshal(displayed.CargoManifest, &manifest); err != nil || manifest["grain"] != float64(7) {
		t.Fatalf("cargo display lost its object shape or safe values: %s, %v", displayed.CargoManifest, err)
	}
	var stores struct {
		ProvisionsTons   float64 `json:"provisions_tons"`
		SeaDaysRemaining float64 `json:"sea_days_remaining"`
	}
	if err := json.Unmarshal(displayed.Stores, &stores); err != nil || stores.ProvisionsTons != 2.5 || stores.SeaDaysRemaining != 3 {
		t.Fatalf("stores display lost its typed values: %s, %v", displayed.Stores, err)
	}
	payload, err := json.Marshal(dashboard)
	if err != nil {
		t.Fatal(err)
	}
	for _, canary := range canaries {
		if bytes.Contains(payload, []byte(canary)) {
			t.Fatal("dashboard exposed a loss credential canary")
		}
	}
	for _, account := range dashboard.Accounts {
		encoded, err := json.Marshal(account)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &fields); err != nil {
			t.Fatal(err)
		}
		if len(fields["ship_losses"]) == 0 || fields["ship_losses"][0] != '[' {
			t.Fatalf("dashboard ship_losses was not an array: %s", encoded)
		}
	}
	stored, err = db.ShipLosses(ctx, healthyID)
	if err != nil || len(stored) != 1 || stored[0].ShipID != shipID || stored[0].ShipName != loss.ShipName {
		t.Fatalf("dashboard scrub mutated retained internal evidence: %+v, %v", stored, err)
	}
}

func TestShipLossesRejectInvalidInput(t *testing.T) {
	valid := ShipLoss{ShipID: "hull", Fate: "missing", Status: "sunk", ObservedAt: time.Now().UTC()}
	for _, test := range []struct {
		name   string
		change func(*ShipLoss)
	}{
		{"foreign account", func(loss *ShipLoss) { loss.AccountID = "foreign" }},
		{"empty hull", func(loss *ShipLoss) { loss.ShipID = "" }},
		{"oversized hull", func(loss *ShipLoss) { loss.ShipID = strings.Repeat("x", 256) }},
		{"oversized name", func(loss *ShipLoss) { loss.ShipName = strings.Repeat("x", 256) }},
		{"oversized class", func(loss *ShipLoss) { loss.ShipClass = strings.Repeat("x", 256) }},
		{"empty fate", func(loss *ShipLoss) { loss.Fate = "" }},
		{"empty status", func(loss *ShipLoss) { loss.Status = "" }},
		{"afloat missing hull", func(loss *ShipLoss) { loss.Status = "sailing" }},
		{"missing observation", func(loss *ShipLoss) { loss.ObservedAt = time.Time{} }},
		{"zero prior observation", func(loss *ShipLoss) {
			value := time.Time{}
			loss.LastObservedAt = &value
		}},
		{"recovery at observation", func(loss *ShipLoss) {
			recoveredAt := loss.ObservedAt
			loss.RecoveredAt = &recoveredAt
		}},
		{"recovery before observation", func(loss *ShipLoss) {
			recoveredAt := loss.ObservedAt.Add(-time.Microsecond)
			loss.RecoveredAt = &recoveredAt
		}},
		{"recovery within stored timestamp", func(loss *ShipLoss) {
			loss.ObservedAt = loss.ObservedAt.Truncate(time.Microsecond)
			recoveredAt := loss.ObservedAt.Add(time.Nanosecond)
			loss.RecoveredAt = &recoveredAt
		}},
		{"terminal recovery", func(loss *ShipLoss) {
			loss.Fate = "lost_at_sea"
			recoveredAt := loss.ObservedAt.Add(time.Hour)
			loss.RecoveredAt = &recoveredAt
		}},
		{"empty location", func(loss *ShipLoss) {
			value := ""
			loss.LastPortID = &value
		}},
		{"negative claim", func(loss *ShipLoss) {
			value := int64(-1)
			loss.InsuranceClaimPaidPence = &value
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			loss := valid
			test.change(&loss)
			if err := (*Store)(nil).SaveShipLosses(context.Background(), "account", []ShipLoss{loss}); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("invalid loss accepted: %v", err)
			}
		})
	}
	if err := (*Store)(nil).SaveShipLosses(context.Background(), "", nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty account accepted: %v", err)
	}
	if err := (*Store)(nil).SaveShipLosses(context.Background(), "account", make([]ShipLoss, MaxShipLossBatch+1)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("oversized batch accepted: %v", err)
	}
	if _, err := (*Store)(nil).ShipLosses(context.Background(), ""); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty account loss read accepted: %v", err)
	}
	if _, err := (*Store)(nil).ShipLossEvents(context.Background(), "", 0); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty account loss event read accepted: %v", err)
	}
	if _, err := (*Store)(nil).ShipLossEvents(context.Background(), "account", -1); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("negative loss event cursor accepted: %v", err)
	}
}

func TestShipLossesRetainHullAndLaterUpdatesWhenOptionalEvidenceIsUnsafe(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	const account = "ship-loss-optional-bounds"
	saveTestAccount(t, db, account)
	at := time.Now().UTC().Truncate(time.Microsecond)
	members := make([]string, 1000)
	for i := range members {
		members[i] = fmt.Sprintf(`"k%d":1e60`, i)
	}
	fixtures := []struct {
		name string
		raw  string
	}{
		{"aggregate-expansion", `{` + strings.Join(members, ",") + `}`},
		{"magnitude", `{"grain":1e130000}`},
		{"positive-width", `{"grain":1e64}`},
		{"negative-width", `{"grain":-1e63}`},
		{"positive-scale", `{"grain":1e-63}`},
		{"negative-scale", `{"grain":-1e-62}`},
		{"zero-scale", `{"grain":0e-63}`},
		{"canonical-quoting", `{"grain":"` + strings.Repeat("<", 6000) + `"}`},
		{"malformed", `{"grain":`},
		{"non-object", `[1,2,3]`},
		{"oversized", `{"grain":"` + strings.Repeat("x", 32768) + `"}`},
		{"invalid-utf8", `{"grain":"` + string([]byte{0xff}) + `"}`},
		{"deep-nesting", `{"grain":` + strings.Repeat("[", 65) + `0` + strings.Repeat("]", 65) + `}`},
	}
	unsafeHazards := []string{strings.Repeat("x", 4097), "storm\x00surge", string([]byte{0xff}), strings.Repeat("嵐", 1366)}
	losses := make([]ShipLoss, 0, len(fixtures)+1)
	for i, fixture := range fixtures {
		cause := unsafeHazards[i%len(unsafeHazards)]
		losses = append(losses, ShipLoss{
			ShipID: fixture.name, ShipName: "Lapras", ShipClass: "hoy",
			Fate: "missing", Status: "sunk", ObservedAt: at,
			CargoManifest: json.RawMessage(fixture.raw), Stores: json.RawMessage(fixture.raw), HazardCause: &cause,
			CargoUnavailableReason: "untrusted reason", StoresUnavailableReason: "untrusted reason",
			HazardUnavailableReason: "untrusted reason",
		})
	}
	cause := "storm"
	normal := ShipLoss{
		ShipID: "normal", Fate: "missing", Status: "sunk", ObservedAt: at,
		CargoManifest: json.RawMessage(`{"grain":7}`), Stores: json.RawMessage(`{"provisions_tons":2.5}`), HazardCause: &cause,
	}
	losses = append(losses, normal)
	if err := db.SaveShipLosses(ctx, account, losses); err != nil {
		t.Fatalf("optional evidence prevented hull retention: %v", err)
	}
	read := func() map[string]ShipLoss {
		t.Helper()
		rows, err := db.ShipLosses(ctx, account)
		if err != nil || len(rows) != len(losses) {
			t.Fatalf("optional evidence affected the hull batch: %d rows, %v", len(rows), err)
		}
		found := make(map[string]ShipLoss, len(rows))
		for _, row := range rows {
			found[row.ShipID] = row
		}
		return found
	}
	rows := read()
	gotNormal := rows[normal.ShipID]
	assertShipLossJSON(t, gotNormal.CargoManifest, normal.CargoManifest)
	assertShipLossJSON(t, gotNormal.Stores, normal.Stores)
	if gotNormal.HazardCause == nil || *gotNormal.HazardCause != cause ||
		gotNormal.CargoUnavailableReason != "" || gotNormal.StoresUnavailableReason != "" || gotNormal.HazardUnavailableReason != "" {
		t.Fatalf("unsafe sibling evidence changed the normal hull: %+v", gotNormal)
	}
	recoveredAt := at.Add(time.Hour)
	claim := int64(1200)
	updates := make([]ShipLoss, 0, len(fixtures))
	for _, fixture := range fixtures {
		row := rows[fixture.name]
		if row.CargoManifest != nil || row.Stores != nil || row.HazardCause != nil ||
			row.CargoUnavailableReason == "" || row.StoresUnavailableReason == "" || row.HazardUnavailableReason == "" ||
			row.Fate != "missing" || !row.ObservedAt.Equal(at) {
			t.Fatalf("%s lost its hull or retained unsafe optional evidence: %+v", fixture.name, row)
		}
		payload, err := json.Marshal(row)
		if err != nil || bytes.Contains(payload, []byte("untrusted reason")) {
			t.Fatalf("%s retained untrusted reason text: %s, %v", fixture.name, payload, err)
		}
		row.HazardCause, row.InsuranceClaimPaidPence, row.RecoveredAt = &cause, &claim, &recoveredAt
		updates = append(updates, row)
	}
	if err := db.SaveShipLosses(ctx, account, updates); err != nil {
		t.Fatalf("discarded evidence poisoned hazard, claim or recovery writes: %v", err)
	}
	rows = read()
	for i := range updates {
		row := rows[updates[i].ShipID]
		if row.CargoManifest != nil || row.Stores != nil ||
			row.CargoUnavailableReason == "" || row.StoresUnavailableReason == "" || row.HazardUnavailableReason != "" ||
			row.HazardCause == nil || *row.HazardCause != cause || row.InsuranceClaimPaidPence == nil ||
			*row.InsuranceClaimPaidPence != claim || row.RecoveredAt == nil || !row.RecoveredAt.Equal(recoveredAt) {
			t.Fatalf("later updates failed after optional evidence was discarded: %+v", row)
		}
		row.CargoManifest = json.RawMessage(`{"grain":9007199254740993}`)
		row.Stores = json.RawMessage(`{"provisions_tons":2.5}`)
		updates[i] = row
	}
	if err := db.SaveShipLosses(ctx, account, updates); err != nil {
		t.Fatalf("later valid evidence could not fill an unavailable observation: %v", err)
	}
	if err := db.SaveShipLosses(ctx, account, losses); err != nil {
		t.Fatalf("unsafe replay poisoned previously retained valid evidence: %v", err)
	}
	rows = read()
	for _, want := range updates {
		got := rows[want.ShipID]
		if got.CargoUnavailableReason != "" || got.StoresUnavailableReason != "" || got.HazardUnavailableReason != "" ||
			got.RecoveredAt == nil || !got.RecoveredAt.Equal(recoveredAt) || got.HazardCause == nil || *got.HazardCause != cause {
			t.Fatalf("valid evidence kept an obsolete reason or lost recovery on replay: %+v", got)
		}
		if !bytes.Contains(got.CargoManifest, []byte("9007199254740993")) {
			t.Fatalf("exact cargo quantity was rounded or replaced: %s", got.CargoManifest)
		}
		assertShipLossJSON(t, got.CargoManifest, want.CargoManifest)
		assertShipLossJSON(t, got.Stores, want.Stores)
	}
}

func TestShipLossEvidenceNumbersRoundTripWithoutRounding(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	const account = "ship-loss-exact-numbers"
	saveTestAccount(t, db, account)
	tokens := []string{
		"1e60", "1e63", "-1e62", "1e-62", "-1e-61", "0e-62", "-0.00",
		strings.Repeat("9", 64), "-" + strings.Repeat("7", 63), "2.5e-7", "9007199254740993", "1.2300e2",
	}
	value := map[string]any{"literal": "<>&\u2028\u2029"}
	for i, token := range tokens {
		value[fmt.Sprintf("n%d", i)] = json.Number(token)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	loss := ShipLoss{
		ShipID: "hull", Fate: "lost_at_sea", Status: "sunk", ObservedAt: time.Now().UTC(),
		CargoManifest: raw, Stores: raw,
	}
	if err := db.SaveShipLosses(ctx, account, []ShipLoss{loss}); err != nil {
		t.Fatalf("bounded exact numbers were rejected: %v", err)
	}
	var firstCanonical json.RawMessage
	for range 3 {
		rows, err := db.ShipLosses(ctx, account)
		if err != nil || len(rows) != 1 {
			t.Fatalf("numeric evidence reload: %+v, %v", rows, err)
		}
		for _, evidence := range []json.RawMessage{rows[0].CargoManifest, rows[0].Stores} {
			decoder := json.NewDecoder(bytes.NewReader(evidence))
			decoder.UseNumber()
			var loaded map[string]any
			if err := decoder.Decode(&loaded); err != nil {
				t.Fatalf("numeric evidence disappeared after JSONB rendering: %s, %v", evidence, err)
			}
			for i, token := range tokens {
				number, ok := loaded[fmt.Sprintf("n%d", i)].(json.Number)
				if !ok {
					t.Fatalf("exact evidence number became a different type: %#v", loaded)
				}
				want, valid := new(big.Rat).SetString(token)
				if !valid {
					t.Fatalf("invalid numeric fixture: %s", token)
				}
				got, valid := new(big.Rat).SetString(number.String())
				if !valid || got.Cmp(want) != 0 {
					t.Fatalf("number changed across JSONB storage: got %s, want %s", number, token)
				}
			}
			if loaded["literal"] != value["literal"] {
				t.Fatalf("canonical quoting changed literal evidence: %#v", loaded["literal"])
			}
			canonical, err := NormalizeShipLossEvidence(evidence)
			if err != nil || canonical == nil {
				t.Fatalf("PostgreSQL-expanded evidence was not admissible on reload: %v", err)
			}
			if firstCanonical == nil {
				firstCanonical = canonical
			} else if !bytes.Equal(firstCanonical, canonical) {
				t.Fatalf("canonical evidence changed on repeated save: %s", canonical)
			}
		}
		if rows[0].CargoUnavailableReason != "" || rows[0].StoresUnavailableReason != "" {
			t.Fatalf("accepted numeric evidence became unavailable: %+v", rows[0])
		}
		if err := db.SaveShipLosses(ctx, account, rows); err != nil {
			t.Fatalf("PostgreSQL-expanded numbers poisoned the next save: %v", err)
		}
	}
}

func TestShipLossEvidenceRoundTripsAtEachJSONByteBoundary(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	expanded := "1" + strings.Repeat("0", 60)
	postgresBase := `{"amount": ` + expanded + `, "text": ""}`
	canonicalBase := `{"amount":` + expanded + `,"literal":"\u003c\u003e\u0026\u2028\u2029","text":""}`
	for _, fixture := range []struct {
		name             string
		raw              string
		postgresBoundary bool
	}{
		{"postgres", `{"amount":1e60,"text":"` + strings.Repeat("x", 32768-len(postgresBase)) + `"}`, true},
		{"canonical", `{"amount":1e60,"literal":"<>&\u2028\u2029","text":"` + strings.Repeat("x", 32768-len(canonicalBase)) + `"}`, false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			account := "ship-loss-byte-boundary-" + fixture.name
			saveTestAccount(t, db, account)
			at := time.Now().UTC().Truncate(time.Microsecond)
			accepted := ShipLoss{
				ShipID: "accepted", Fate: "missing", Status: "sunk", ObservedAt: at,
				CargoManifest: json.RawMessage(fixture.raw), Stores: json.RawMessage(fixture.raw),
			}
			oversized := accepted
			oversized.ShipID = "oversized"
			overflow := strings.TrimSuffix(fixture.raw, `"}`) + `x"}`
			oversized.CargoManifest, oversized.Stores = json.RawMessage(overflow), json.RawMessage(overflow)
			if err := db.SaveShipLosses(ctx, account, []ShipLoss{accepted, oversized}); err != nil {
				t.Fatalf("byte boundary prevented retaining either hull: %v", err)
			}
			var firstCanonical json.RawMessage
			for range 3 {
				rows, err := db.ShipLosses(ctx, account)
				if err != nil || len(rows) != 2 {
					t.Fatalf("boundary evidence reload: %+v, %v", rows, err)
				}
				for _, row := range rows {
					if row.ShipID == oversized.ShipID {
						if row.CargoManifest != nil || row.Stores != nil ||
							row.CargoUnavailableReason == "" || row.StoresUnavailableReason == "" {
							t.Fatalf("one excess byte was persisted as admissible evidence: %+v", row)
						}
						continue
					}
					canonical, err := NormalizeShipLossEvidence(row.CargoManifest)
					if err != nil || canonical == nil || row.CargoUnavailableReason != "" || row.StoresUnavailableReason != "" {
						t.Fatalf("accepted boundary evidence could not be normalized after JSONB rendering: %v", err)
					}
					if len(row.CargoManifest) > 32768 || len(canonical) > 32768 {
						t.Fatalf("readback exceeded the evidence budget: postgres=%d canonical=%d", len(row.CargoManifest), len(canonical))
					}
					if fixture.postgresBoundary && len(row.CargoManifest) != 32768 ||
						!fixture.postgresBoundary && len(canonical) != 32768 {
						t.Fatalf("fixture missed its actual storage boundary: postgres=%d canonical=%d", len(row.CargoManifest), len(canonical))
					}
					if firstCanonical == nil {
						firstCanonical = canonical
					} else if !bytes.Equal(firstCanonical, canonical) {
						t.Fatal("boundary evidence changed across repeated JSONB saves")
					}
					assertShipLossJSON(t, row.Stores, row.CargoManifest)
				}
				if err := db.SaveShipLosses(ctx, account, rows); err != nil {
					t.Fatalf("accepted boundary evidence poisoned a later save: %v", err)
				}
			}
		})
	}
}

func TestNormalizeShipLossEvidenceRejectsMalformedOrUnboundedData(t *testing.T) {
	for _, raw := range []string{`{`, `[]`, `"secret"`, `1`, `{"grain":1e130000}`} {
		if clean, err := NormalizeShipLossEvidence(json.RawMessage(raw)); err == nil || clean != nil {
			t.Errorf("invalid evidence was replaced with a successful observation: %q -> %s, %v", raw, clean, err)
		}
	}
	for _, raw := range []json.RawMessage{nil, json.RawMessage(`null`), json.RawMessage(" null ")} {
		if clean, err := NormalizeShipLossEvidence(raw); err != nil || clean != nil {
			t.Errorf("unknown JSON became invented evidence: %s, %v", clean, err)
		}
	}
}

func TestNormalizeShipLossHazardKeepsOnlyPostgresSafeText(t *testing.T) {
	for _, value := range []string{strings.Repeat("x", 4097), "storm\x00surge", string([]byte{0xff})} {
		if cause, err := NormalizeShipLossHazard(value); err == nil || cause != nil {
			t.Errorf("unsafe hazard was retained: %v, %v", cause, err)
		}
	}
	for _, value := range []string{"storm", "嵐", strings.Repeat("x", 4096)} {
		cause, err := NormalizeShipLossHazard(value)
		if err != nil || cause == nil || *cause != value {
			t.Errorf("valid hazard changed: %v, %v", cause, err)
		}
	}
}

func assertShipLossJSON(t *testing.T, got, want json.RawMessage) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("loss JSON changed: got %s, want %s", got, want)
	}
}
