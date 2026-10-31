package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"

	"go.uber.org/zap"
)

func TestPostgresSnapshotRoutesExceedSnapshotRowLimit(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	credentials := saveTestAccount(t, db, "large-routes")
	routes, err := json.Marshal(map[string]any{
		"first":            map[string]any{"padding": strings.Repeat("x", MaxSnapshotBytes/2), "distance": 42},
		"second":           map[string]any{"padding": strings.Repeat("y", MaxSnapshotBytes/2), "token": credentials.APIKey},
		credentials.APIKey: map[string]any{"public_count": 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) <= MaxSnapshotBytes {
		t.Fatal("fixture must exceed the snapshot row budget")
	}
	snapshot := map[string]json.RawMessage{"routes": routes, "ships": json.RawMessage(`[{"id":"ship"}]`)}
	if err := db.SaveSnapshot(ctx, "large-routes", snapshot); err != nil {
		t.Fatal(err)
	}
	var row snapshotRow
	if err := db.db.First(&row, "account_id = ?", "large-routes").Error; err != nil {
		t.Fatal(err)
	}
	var stored map[string]json.RawMessage
	if err := json.Unmarshal(row.Data, &stored); err != nil {
		t.Fatal(err)
	}
	if _, exists := stored["routes"]; exists || len(row.Data) > MaxSnapshotBytes {
		t.Fatalf("routes still consume the snapshot row: %d bytes", len(row.Data))
	}
	loaded, err := db.Snapshot(ctx, "large-routes")
	if err != nil {
		t.Fatal(err)
	}
	expected, err := redactJSON(routes, credentialSecrets(credentials), 0)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := redactJSON(loaded["routes"], nil, 0)
	if err != nil || string(actual) != string(expected) {
		t.Fatalf("large route cache did not round-trip with redaction: %v", err)
	}
	if strings.Contains(string(loaded["routes"]), credentials.APIKey) {
		t.Fatal("route member names or values retained account credentials")
	}
	var ships []struct{ ID string }
	if err := json.Unmarshal(loaded["ships"], &ships); err != nil || len(ships) != 1 || ships[0].ID != "ship" {
		t.Fatalf("ordinary snapshot data changed: %s, %v", loaded["ships"], err)
	}
}

func TestPostgresSnapshotRoutesChangesRemovalAndAccountIsolation(t *testing.T) {
	db, dsn, key := integrationStore(t)
	ctx := context.Background()
	for _, account := range []string{"route-one", "route-two"} {
		saveTestAccount(t, db, account)
	}
	initial := map[string]json.RawMessage{"routes": json.RawMessage(`{"same":{"distance":10},"changed":{"distance":20},"removed":{"distance":30}}`)}
	if err := db.SaveSnapshot(ctx, "route-one", initial); err != nil {
		t.Fatal(err)
	}
	other := map[string]json.RawMessage{"routes": json.RawMessage(`{"same":{"distance":99}}`)}
	if err := db.SaveSnapshot(ctx, "route-two", other); err != nil {
		t.Fatal(err)
	}
	var unchangedVersion string
	if err := db.db.Raw("SELECT xmin::text FROM snapshot_routes WHERE account_id = ? AND route_key = ?", "route-one", "same").Scan(&unchangedVersion).Error; err != nil {
		t.Fatal(err)
	}
	// A new Store must seed its cache from PostgreSQL rather than rewriting
	// every route during the first save after a restart.
	reopened, err := Open(ctx, dsn, key, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.SaveSnapshot(ctx, "route-one", initial); err != nil {
		t.Fatal(err)
	}
	updated := map[string]json.RawMessage{"routes": json.RawMessage(`{"same":{"distance":10},"changed":{"distance":21},"added":{"distance":40}}`)}
	if err := reopened.SaveSnapshot(ctx, "route-one", updated); err != nil {
		t.Fatal(err)
	}
	var afterVersion string
	if err := db.db.Raw("SELECT xmin::text FROM snapshot_routes WHERE account_id = ? AND route_key = ?", "route-one", "same").Scan(&afterVersion).Error; err != nil || afterVersion != unchangedVersion {
		t.Fatalf("unchanged route was rewritten: before=%s after=%s error=%v", unchangedVersion, afterVersion, err)
	}
	for account, expected := range map[string]map[string]int{
		"route-one": {"same": 10, "changed": 21, "added": 40},
		"route-two": {"same": 99},
	} {
		loaded, err := db.Snapshot(ctx, account)
		if err != nil {
			t.Fatal(err)
		}
		var routes map[string]struct{ Distance int }
		if err := json.Unmarshal(loaded["routes"], &routes); err != nil || len(routes) != len(expected) {
			t.Fatalf("wrong route membership for %s: %s, %v", account, loaded["routes"], err)
		}
		for route, distance := range expected {
			if routes[route].Distance != distance {
				t.Fatalf("wrong route distance for %s/%s: %+v", account, route, routes[route])
			}
		}
	}
	if err := reopened.SaveSnapshot(ctx, "route-one", map[string]json.RawMessage{}); err != nil {
		t.Fatal(err)
	}
	loaded, err := db.Snapshot(ctx, "route-one")
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := loaded["routes"]; exists {
		t.Fatal("empty route table must omit the routes key")
	}
	otherLoaded, err := db.Snapshot(ctx, "route-two")
	if err != nil || len(otherLoaded["routes"]) == 0 {
		t.Fatalf("removing one account's routes affected another: %v", err)
	}
}

func TestPostgresSnapshotRoutesRollbackInvalidatesCache(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	saveTestAccount(t, db, "route-rollback")
	initial := map[string]json.RawMessage{"routes": json.RawMessage(`{"lane":{"distance":10}}`), "generation": json.RawMessage(`1`)}
	if err := db.SaveSnapshot(ctx, "route-rollback", initial); err != nil {
		t.Fatal(err)
	}
	if err := db.db.Exec(`ALTER TABLE snapshot_routes ADD CONSTRAINT reject_route_distance CHECK (data ->> 'distance' <> '20')`).Error; err != nil {
		t.Fatal(err)
	}
	updated := map[string]json.RawMessage{"routes": json.RawMessage(`{"lane":{"distance":20}}`), "generation": json.RawMessage(`2`)}
	if err := db.SaveSnapshot(ctx, "route-rollback", updated); err == nil {
		t.Fatal("route constraint failure did not roll back the snapshot")
	}
	if db.snapshotRouteCache("route-rollback").digests != nil {
		t.Fatal("failed transaction retained its route digest cache")
	}
	loaded, err := db.Snapshot(ctx, "route-rollback")
	if err != nil || string(loaded["generation"]) != "1" || !strings.Contains(string(loaded["routes"]), "10") {
		t.Fatalf("snapshot and routes did not roll back together: %v", err)
	}
	if err := db.db.Exec(`ALTER TABLE snapshot_routes DROP CONSTRAINT reject_route_distance`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.SaveSnapshot(ctx, "route-rollback", updated); err != nil {
		t.Fatal(err)
	}
	loaded, err = db.Snapshot(ctx, "route-rollback")
	if err != nil || string(loaded["generation"]) != "2" || !strings.Contains(string(loaded["routes"]), "20") {
		t.Fatalf("retry did not persist after cache invalidation: %v", err)
	}
}

func TestSnapshotRoutesIndependentBounds(t *testing.T) {
	t.Run("raw limit", func(t *testing.T) {
		raw := json.RawMessage(`{"lane":"` + strings.Repeat("x", MaxSnapshotRoutesBytes) + `"}`)
		if _, err := encodeSnapshotRoutes(raw, nil); !errors.Is(err, ErrResponseTooLarge) {
			t.Fatalf("route cache exceeded its independent raw budget: %v", err)
		}
	})
	t.Run("redacted limit", func(t *testing.T) {
		raw := json.RawMessage(`{"lane":"` + strings.Repeat("x", MaxSnapshotRoutesBytes/8) + `"}`)
		if _, err := encodeSnapshotRoutes(raw, []string{"x"}); !errors.Is(err, ErrResponseTooLarge) {
			t.Fatalf("route cache exceeded its independent redacted budget: %v", err)
		}
	})
	for _, raw := range []json.RawMessage{json.RawMessage(`[]`), json.RawMessage(`{"lane":`)} {
		if _, err := encodeSnapshotRoutes(raw, nil); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("non-object route cache was accepted: %s, %v", raw, err)
		}
	}
}

func TestPostgresSnapshotRoutesKeepSnapshotRowBound(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	saveTestAccount(t, db, "row-bound")
	snapshot := map[string]json.RawMessage{
		"routes": json.RawMessage(`{"lane":{"distance":10}}`),
		"other":  json.RawMessage(`"` + strings.Repeat("x", MaxSnapshotBytes) + `"`),
	}
	if err := db.SaveSnapshot(ctx, "row-bound", snapshot); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("ordinary snapshot row escaped its existing cap: %v", err)
	}
	loaded, err := db.Snapshot(ctx, "row-bound")
	if err != nil || len(loaded) != 0 {
		t.Fatalf("oversized snapshot wrote partial state: keys=%d error=%v", len(loaded), err)
	}
}

func TestPostgresSnapshotRoutesMigrationRoundTrip(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	saveTestAccount(t, db, "migrated-routes")
	saveTestAccount(t, db, "migrated-empty")
	initial := map[string]json.RawMessage{
		"routes": json.RawMessage(`{"lane":{"distance":10,"unknown":{"integer":9007199254740993}}}`),
		"ships":  json.RawMessage(`[]`),
	}
	if err := db.SaveSnapshot(ctx, "migrated-routes", initial); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveSnapshot(ctx, "migrated-empty", map[string]json.RawMessage{}); err != nil {
		t.Fatal(err)
	}
	migration, err := migrations.ReadFile("migrations/00021_snapshot_routes.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, down, found := strings.Cut(string(migration), "-- +goose Down")
	if !found {
		t.Fatal("route migration has no rollback")
	}
	if err := db.db.Exec(down).Error; err != nil {
		t.Fatal(err)
	}
	var embedded bool
	if err := db.db.Raw("SELECT jsonb_exists(data, 'routes') FROM snapshots WHERE account_id = ?", "migrated-routes").Scan(&embedded).Error; err != nil || !embedded {
		t.Fatalf("rollback did not restore embedded routes: %v", err)
	}
	if err := db.db.Exec(up).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.db.Raw("SELECT jsonb_exists(data, 'routes') FROM snapshots WHERE account_id = ?", "migrated-routes").Scan(&embedded).Error; err != nil || embedded {
		t.Fatalf("migration did not remove embedded routes: %v", err)
	}
	loaded, err := db.Snapshot(ctx, "migrated-routes")
	if err != nil || !strings.Contains(string(loaded["routes"]), "9007199254740993") {
		t.Fatalf("migration lost route data or integer precision: %s, %v", loaded["routes"], err)
	}
	empty, err := db.Snapshot(ctx, "migrated-empty")
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := empty["routes"]; exists {
		t.Fatal("migration invented routes for an empty snapshot")
	}
}

func TestPostgresSnapshotRoutesReseedAfterOwnershipTransfer(t *testing.T) {
	first, dsn, key := integrationStore(t)
	ctx := context.Background()
	const accountID = "route-ownership-transfer"
	saveTestAccount(t, first, accountID)
	second, err := Open(ctx, dsn, key, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	firstCtx, releaseFirst, err := first.AcquireAccountContext(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseFirst()
	original := map[string]json.RawMessage{"routes": json.RawMessage(`{"lane":{"distance":10}}`)}
	if err := first.SaveSnapshot(firstCtx, accountID, original); err != nil {
		t.Fatal(err)
	}
	releaseFirst()

	secondCtx, releaseSecond, err := second.AcquireAccountContext(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseSecond()
	changed := map[string]json.RawMessage{
		"routes": json.RawMessage(`{"lane":{"distance":99},"another-owner":{"distance":20}}`),
	}
	if err := second.SaveSnapshot(secondCtx, accountID, changed); err != nil {
		t.Fatal(err)
	}
	releaseSecond()

	firstCtx, releaseFirstAgain, err := first.AcquireAccountContext(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseFirstAgain()
	if err := first.SaveSnapshot(firstCtx, accountID, original); err != nil {
		t.Fatal(err)
	}
	var rows []snapshotRouteRow
	if err := first.db.Where("account_id = ?", accountID).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].RouteKey != "lane" {
		t.Fatalf("previous owner's route keys survived reacquisition: %+v", rows)
	}
	var route struct{ Distance int }
	if err := json.Unmarshal(rows[0].Data, &route); err != nil || route.Distance != 10 {
		t.Fatalf("stale digest skipped restoring the reacquired owner's route: %s, %v", rows[0].Data, err)
	}
}

func decodeSnapshotRoutesValue(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var decoded map[string]any
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatalf("route cache was not a JSON object: %s, %v", raw, err)
	}
	return decoded
}

func checkSnapshotRouteNumbers(t *testing.T, loaded map[string]any, tokens []string) {
	t.Helper()
	for index, token := range tokens {
		key := "n" + string(rune('0'+index))
		number, ok := loaded[key].(json.Number)
		if !ok {
			t.Fatalf("route number %s was not preserved as a number: %#v", token, loaded[key])
		}
		want, ok := new(big.Rat).SetString(token)
		if !ok {
			t.Fatalf("test fixture token was not a rational number: %s", token)
		}
		got, ok := new(big.Rat).SetString(number.String())
		if !ok || got.Cmp(want) != 0 {
			t.Fatalf("route number changed value across storage: token=%s loaded=%s", token, number)
		}
		if token == "9007199254740993" && number.String() != token {
			t.Fatalf("exact integer was re-encoded: %s", number)
		}
	}
	text, ok := loaded["literal"].(string)
	if !ok || text != "<>&\u2028\u2029" {
		t.Fatalf("literal route text was not preserved: %#v", loaded["literal"])
	}
}

func TestSnapshotRoutesNumericRoundTrip(t *testing.T) {
	db, dsn, key := integrationStore(t)
	ctx := context.Background()
	saveTestAccount(t, db, "route-numbers")
	tokens := []string{
		"1e32", "1e-32", "1e33",
		strings.Repeat("9", 33),
		strings.Repeat("7", 64),
		"2.5e-7", "9007199254740993",
	}
	members := make([]string, 0, len(tokens))
	for index, token := range tokens {
		members = append(members, `"n`+string(rune('0'+index))+`":`+token)
	}
	routes := json.RawMessage(`{` + strings.Join(members, ",") + `,"literal":"<>&` + `\u2028\u2029"}`)
	if err := db.SaveSnapshot(ctx, "route-numbers", map[string]json.RawMessage{"routes": routes}); err != nil {
		t.Fatalf("admissible route numbers were rejected on save: %v", err)
	}
	loaded, err := db.Snapshot(ctx, "route-numbers")
	if err != nil {
		t.Fatalf("admissible route numbers were rejected on load: %v", err)
	}
	first := decodeSnapshotRoutesValue(t, loaded["routes"])
	checkSnapshotRouteNumbers(t, first, tokens)
	canonical, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if len(canonical) > 64<<20 {
		t.Fatalf("canonical route object exceeded the final budget: %d bytes", len(canonical))
	}
	// The canonicalized reload must itself be storable: exponent lexemes are
	// gone but the rendered decimals stay within the route number policy.
	if err := db.SaveSnapshot(ctx, "route-numbers", map[string]json.RawMessage{"routes": canonical}); err != nil {
		t.Fatalf("canonicalized route numbers were rejected on re-save: %v", err)
	}
	reopened, err := Open(ctx, dsn, key, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	again, err := reopened.Snapshot(ctx, "route-numbers")
	if err != nil {
		t.Fatalf("reopened store rejected the canonical route numbers: %v", err)
	}
	checkSnapshotRouteNumbers(t, decodeSnapshotRoutesValue(t, again["routes"]), tokens)
	// The reopened store must also accept a cold save of the canonical form.
	if err := reopened.SaveSnapshot(ctx, "route-numbers", map[string]json.RawMessage{"routes": canonical}); err != nil {
		t.Fatalf("reopened store rejected a cold save of canonical route numbers: %v", err)
	}
	final, err := reopened.Snapshot(ctx, "route-numbers")
	if err != nil {
		t.Fatalf("cold save broke the canonical route numbers: %v", err)
	}
	checkSnapshotRouteNumbers(t, decodeSnapshotRoutesValue(t, final["routes"]), tokens)
}

func TestSnapshotRoutesNumericBoundsReject(t *testing.T) {
	for _, test := range []struct {
		name  string
		token string
	}{
		{"huge exponent", "1e130000"},
		{"zero extreme scale", "0e-16000"},
		{"65 digit integer", strings.Repeat("8", 65)},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, _, _ := integrationStore(t)
			ctx := context.Background()
			const account = "route-number-reject"
			saveTestAccount(t, db, account)
			prior := map[string]json.RawMessage{"routes": json.RawMessage(`{"lane":{"distance":10}}`), "generation": json.RawMessage(`1`)}
			if err := db.SaveSnapshot(ctx, account, prior); err != nil {
				t.Fatal(err)
			}
			var before snapshotRow
			if err := db.db.Where("account_id = ?", account).Take(&before).Error; err != nil {
				t.Fatal(err)
			}
			rejected := json.RawMessage(`{"lane":{"distance":11},"bad":` + test.token + `}`)
			if err := db.SaveSnapshot(ctx, account, map[string]json.RawMessage{"routes": rejected, "generation": json.RawMessage(`2`)}); !errors.Is(err, ErrResponseTooLarge) {
				t.Fatalf("unsafe route number was not rejected: %v", err)
			}
			var after snapshotRow
			if err := db.db.Where("account_id = ?", account).Take(&after).Error; err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before.Data, after.Data) || !before.UpdatedAt.Equal(after.UpdatedAt) {
				t.Fatal("rejected route save mutated the snapshot row")
			}
			loaded, err := db.Snapshot(ctx, account)
			if err != nil || string(loaded["generation"]) != "1" || !strings.Contains(string(loaded["routes"]), "10") {
				t.Fatalf("prior routes and metadata did not survive the rejection: %v", err)
			}
		})
	}
}

func TestSnapshotRoutesUnsafeLegacyRouteValuesFailClosed(t *testing.T) {
	db, dsn, key := integrationStore(t)
	ctx := context.Background()
	const account = "route-poisoned-rows"
	saveTestAccount(t, db, account)
	if err := db.SaveSnapshot(ctx, account, map[string]json.RawMessage{
		"routes": json.RawMessage(`{"lane":{"distance":10},"extra":{"distance":20}}`),
	}); err != nil {
		t.Fatal(err)
	}
	poison := []struct{ route, token string }{
		{"lane", "1e130000"},
		{"extra", "0e-16000"},
	}
	for _, row := range poison {
		if err := db.db.Model(&snapshotRouteRow{}).Where("account_id = ? AND route_key = ?", account, row.route).
			Update("data", json.RawMessage(row.token)).Error; err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Snapshot(ctx, account); !errors.Is(err, ErrResponseTooLarge) && !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Snapshot hydrated unsafe legacy route values instead of failing closed: %v", err)
	}
	// A fresh store must reject before its cold save hydrates the poison.
	cold, err := Open(ctx, dsn, key, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer cold.Close()
	if err := cold.SaveSnapshot(ctx, account, map[string]json.RawMessage{"routes": json.RawMessage(`{"fresh":{"distance":1}}`)}); !errors.Is(err, ErrResponseTooLarge) && !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("cold save seeded from unsafe legacy route values: %v", err)
	}
	var lanes []snapshotRouteRow
	if err := db.db.Where("account_id = ?", account).Order("route_key").Find(&lanes).Error; err != nil {
		t.Fatal(err)
	}
	if len(lanes) != 2 || lanes[0].RouteKey != "extra" || lanes[1].RouteKey != "lane" {
		t.Fatalf("failed cold save disturbed the poisoned route table: %+v", lanes)
	}
}

func TestSnapshotRoutesGoEscapeCanonicalRejected(t *testing.T) {
	db, dsn, key := integrationStore(t)
	ctx := context.Background()
	const account = "route-go-escape-bound"
	saveTestAccount(t, db, account)
	prior := map[string]json.RawMessage{"routes": json.RawMessage(`{"lane":{"distance":10}}`)}
	if err := db.SaveSnapshot(ctx, account, prior); err != nil {
		t.Fatal(err)
	}
	var before snapshotRow
	if err := db.db.Where("account_id = ?", account).Take(&before).Error; err != nil {
		t.Fatal(err)
	}
	// 12 MiB of literal '<' stays ~12 MiB as PostgreSQL text but Go's canonical
	// escaping renders each as \u003c, past the final 64 MiB route budget. An
	// admissible 1e32 rides along; only the canonical guard can catch this.
	seeded := json.RawMessage(`{"padding":"` + strings.Repeat("<", 12<<20) + `","value":1e32}`)
	if err := db.db.Model(&snapshotRouteRow{}).Where("account_id = ? AND route_key = ?", account, "lane").
		Update("data", seeded).Error; err != nil {
		t.Fatal(err)
	}
	// PostgreSQL normalized the seeded bytes on write; the immutability
	// baseline is the persisted row, not the original compact JSON.
	var poisoned snapshotRouteRow
	if err := db.db.Where("account_id = ? AND route_key = ?", account, "lane").Take(&poisoned).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := db.Snapshot(ctx, account); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("Snapshot served a route cache whose canonical form exceeds the budget: %v", err)
	}
	// A fresh store rejects before committing a cold save seeded from the row.
	cold, err := Open(ctx, dsn, key, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer cold.Close()
	if err := cold.SaveSnapshot(ctx, account, map[string]json.RawMessage{"routes": json.RawMessage(`{"fresh":{"distance":1}}`)}); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("cold save seeded a route cache whose canonical form exceeds the budget: %v", err)
	}
	var after snapshotRow
	if err := db.db.Where("account_id = ?", account).Take(&after).Error; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before.Data, after.Data) || !before.UpdatedAt.Equal(after.UpdatedAt) {
		t.Fatal("canonical rejection mutated the snapshot row")
	}
	var lanes []snapshotRouteRow
	if err := db.db.Where("account_id = ?", account).Find(&lanes).Error; err != nil {
		t.Fatal(err)
	}
	if len(lanes) != 1 || lanes[0].RouteKey != poisoned.RouteKey || !bytes.Equal([]byte(lanes[0].Data), []byte(poisoned.Data)) {
		t.Fatal("canonical rejection mutated the poisoned route row")
	}
}

func TestSnapshotRoutesCompactExpansionRejectedAtomically(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	const account = "route-aggregate-expansion"
	saveTestAccount(t, db, account)
	prior := map[string]json.RawMessage{"routes": json.RawMessage(`{"lane":{"distance":10}}`), "generation": json.RawMessage(`1`)}
	if err := db.SaveSnapshot(ctx, account, prior); err != nil {
		t.Fatal(err)
	}
	var before snapshotRow
	if err := db.db.Where("account_id = ?", account).Take(&before).Error; err != nil {
		t.Fatal(err)
	}
	// 2,000,000 admissible 1e32 tokens stay compact on the wire but render as
	// roughly 68 MB of PostgreSQL jsonb text, past the final route budget.
	const count = 2_000_000
	compact := json.RawMessage(`{"wide":[` + strings.Repeat("1e32,", count-1) + "1e32]}")
	if len(compact) >= 64<<20 {
		t.Fatalf("compact fixture must fit the input budget: %d bytes", len(compact))
	}
	if err := db.SaveSnapshot(ctx, account, map[string]json.RawMessage{"routes": compact, "generation": json.RawMessage(`2`)}); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("compact input that expands past the rendered budget was accepted: %v", err)
	}
	var after snapshotRow
	if err := db.db.Where("account_id = ?", account).Take(&after).Error; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before.Data, after.Data) || !before.UpdatedAt.Equal(after.UpdatedAt) {
		t.Fatal("aggregate rejection mutated the snapshot row")
	}
	loaded, err := db.Snapshot(ctx, account)
	if err != nil || string(loaded["generation"]) != "1" || !strings.Contains(string(loaded["routes"]), "10") {
		t.Fatalf("prior routes and metadata did not survive the aggregate rejection: %v", err)
	}
}

func TestSnapshotRoutesMixedGenerationSnapshotFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name     string
		embedded string
	}{
		{"null embedded routes", `null`},
		{"empty object embedded routes", `{}`},
		{"populated embedded routes", `{"lane":{"distance":99}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, _, _ := integrationStore(t)
			ctx := context.Background()
			const account = "route-mixed-generation"
			saveTestAccount(t, db, account)
			if err := db.SaveSnapshot(ctx, account, map[string]json.RawMessage{
				"routes": json.RawMessage(`{"lane":{"distance":10}}`),
			}); err != nil {
				t.Fatal(err)
			}
			if err := db.db.Exec(`UPDATE snapshots SET data = jsonb_set(data, '{routes}', ?::jsonb) WHERE account_id = ?`,
				test.embedded, account).Error; err != nil {
				t.Fatal(err)
			}
			loaded, err := db.Snapshot(ctx, account)
			if err == nil {
				t.Fatalf("mixed-generation snapshot was served instead of failing closed: routes=%s", loaded["routes"])
			}
			var lanes []snapshotRouteRow
			if err := db.db.Where("account_id = ?", account).Find(&lanes).Error; err != nil {
				t.Fatal(err)
			}
			if len(lanes) != 1 || !strings.Contains(string(lanes[0].Data), "10") {
				t.Fatalf("failed snapshot read disturbed the route table: %+v", lanes)
			}
		})
	}
}

func TestSnapshotRoutesStaleOwnerSaveAfterOwnershipLoss(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	const accountID = "route-stale-owner"
	saveTestAccount(t, db, accountID)
	ownerCtx, releaseOwner, err := db.AcquireAccountContext(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseOwner()
	current := map[string]json.RawMessage{"routes": json.RawMessage(`{"lane":{"distance":99},"another-owner":{"distance":20}}`)}
	if err := db.SaveSnapshot(ownerCtx, accountID, current); err != nil {
		t.Fatal(err)
	}
	var before snapshotRow
	if err := db.db.Where("account_id = ?", accountID).Take(&before).Error; err != nil {
		t.Fatal(err)
	}

	// Fence the live lease by invalidating its owner generation, exactly as
	// TestPostgresBootstrapLockAndGenerationFence does; the context stays live.
	if err := db.db.Exec("UPDATE accounts SET owner_generation = owner_generation + 1 WHERE id = ?", accountID).Error; err != nil {
		t.Fatal(err)
	}
	stale := map[string]json.RawMessage{"routes": json.RawMessage(`{"lane":{"distance":10}}`)}
	if err := db.SaveSnapshot(ownerCtx, accountID, stale); !errors.Is(err, ErrOwnershipLost) {
		t.Fatalf("stale owner saved routes after generation invalidation: %v", err)
	}
	var after snapshotRow
	if err := db.db.Where("account_id = ?", accountID).Take(&after).Error; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before.Data, after.Data) || !before.UpdatedAt.Equal(after.UpdatedAt) {
		t.Fatal("stale owner write disturbed the snapshot row or its metadata")
	}
	var lanes []snapshotRouteRow
	if err := db.db.Where("account_id = ?", accountID).Order("route_key").Find(&lanes).Error; err != nil {
		t.Fatal(err)
	}
	if len(lanes) != 2 || lanes[0].RouteKey != "another-owner" || lanes[1].RouteKey != "lane" {
		t.Fatalf("stale owner write disturbed the route table: %+v", lanes)
	}
	var lane struct{ Distance int }
	if err := json.Unmarshal(lanes[1].Data, &lane); err != nil || lane.Distance != 99 {
		t.Fatalf("stale owner write changed the current route content: %s, %v", lanes[1].Data, err)
	}

	// A fresh owner acquisition reseeds the cache and round-trips.
	releaseOwner()
	reacquiredCtx, releaseReacquired, err := db.AcquireAccountContext(ctx, accountID)
	if err != nil {
		t.Fatalf("ownership was not released after generation invalidation: %v", err)
	}
	defer releaseReacquired()
	if err := db.SaveSnapshot(reacquiredCtx, accountID, map[string]json.RawMessage{
		"routes": json.RawMessage(`{"lane":{"distance":100},"another-owner":{"distance":20}}`),
	}); err != nil {
		t.Fatalf("fresh owner could not reseed after generation invalidation: %v", err)
	}
	loaded, err := db.Snapshot(reacquiredCtx, accountID)
	if err != nil || !strings.Contains(string(loaded["routes"]), "100") {
		t.Fatalf("fresh owner reseed did not round-trip: %v", err)
	}
}

func TestSnapshotRoutesWarmCacheSecretsAfterRotation(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	const account = "route-rotation"
	original := Credentials{Passphrase: "rotation-passphrase-one", APIKey: "tw_live_rotation_old_key"}
	if err := db.SaveAccount(ctx, Account{ID: account, Label: account, CompanyID: "company-" + account, Enabled: true}, original); err != nil {
		t.Fatal(err)
	}
	canary := "future-secret-canary-value"
	raw := json.RawMessage(`{"lane":{"detail":"` + canary + `","distance":10}}`)
	if err := db.SaveSnapshot(ctx, account, map[string]json.RawMessage{"routes": raw}); err != nil {
		t.Fatal(err)
	}
	// Rotate so the value already sitting in the raw payload becomes a secret.
	rotated := Credentials{Passphrase: "rotation-passphrase-two", APIKey: canary}
	if err := db.SaveAccount(ctx, Account{ID: account, Label: account, CompanyID: "company-" + account, Enabled: true}, rotated); err != nil {
		t.Fatal(err)
	}
	// The identical raw bytes must be re-scrubbed with the new secret set; a
	// warm cache reusing the pre-rotation secrets would persist the canary.
	if err := db.SaveSnapshot(ctx, account, map[string]json.RawMessage{"routes": raw}); err != nil {
		t.Fatal(err)
	}
	loaded, err := db.Snapshot(ctx, account)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := redactJSON(raw, credentialSecrets(rotated), 0)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := redactJSON(loaded["routes"], nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(actual) != string(expected) {
		t.Fatalf("rotated secret was not re-scrubbed: got %s want %s", actual, expected)
	}
	if strings.Contains(string(loaded["routes"]), canary) || strings.Contains(string(loaded["routes"]), original.APIKey) {
		t.Fatal("route cache retained a plaintext credential after rotation")
	}
}
