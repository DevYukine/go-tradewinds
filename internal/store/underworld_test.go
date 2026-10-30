package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"
)

func TestUnderworldReadReservationsSurviveRestartWithoutResponse(t *testing.T) {
	db, dsn, key := integrationStore(t)
	ctx := context.Background()
	saveTestAccount(t, db, "observer")
	visit, err := db.TrackUnderworldVisit(ctx, "observer", "ship", "flushing")
	if err != nil || visit == "" {
		t.Fatalf("start visit: %q %v", visit, err)
	}
	reserved, err := db.ReserveUnderworldRead(ctx, "observer", "ship", visit, "wants", "")
	if err != nil || !reserved {
		t.Fatalf("reserve wants: %t %v", reserved, err)
	}
	for index := range UnderworldBooksPerVisit {
		reserved, err := db.ReserveUnderworldRead(ctx, "observer", "ship", visit, "book", fmt.Sprintf("good-%d", index))
		if err != nil || !reserved {
			t.Fatalf("reserve book %d: %t %v", index, reserved, err)
		}
	}
	if err := db.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, dsn, key, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	current, err := reopened.TrackUnderworldVisit(ctx, "observer", "ship", "flushing")
	if err != nil || current != visit {
		t.Fatalf("restart changed active visit: %q %v", current, err)
	}
	for _, read := range [][2]string{{"wants", ""}, {"book", "good-0"}, {"book", "extra"}} {
		reserved, err := reopened.ReserveUnderworldRead(ctx, "observer", "ship", visit, read[0], read[1])
		if err != nil || reserved {
			t.Fatalf("restart refunded attempted %v: %t %v", read, reserved, err)
		}
	}
	if _, err := reopened.TrackUnderworldVisit(ctx, "observer", "ship", ""); err != nil {
		t.Fatal(err)
	}
	next, err := reopened.TrackUnderworldVisit(ctx, "observer", "ship", "flushing")
	if err != nil || next == "" || next == visit {
		t.Fatalf("new arrival reused old visit: %q %v", next, err)
	}
	reserved, err = reopened.ReserveUnderworldRead(ctx, "observer", "ship", next, "wants", "")
	if err != nil || !reserved {
		t.Fatalf("new arrival has no wants allowance: %t %v", reserved, err)
	}
}

func TestUnderworldConcurrentBookReservationsEnforceFourAndAccountIsolation(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	for _, account := range []string{"first", "second"} {
		saveTestAccount(t, db, account)
		visit, err := db.TrackUnderworldVisit(ctx, account, "ship", "flushing")
		if err != nil {
			t.Fatal(err)
		}
		var accepted atomic.Int64
		var workers sync.WaitGroup
		for index := range 12 {
			workers.Add(1)
			go func() {
				defer workers.Done()
				reserved, err := db.ReserveUnderworldRead(ctx, account, "ship", visit, "book", fmt.Sprintf("good-%d", index))
				if err != nil {
					t.Error(err)
				} else if reserved {
					accepted.Add(1)
				}
			}()
		}
		workers.Wait()
		if count := accepted.Load(); count != UnderworldBooksPerVisit {
			t.Fatalf("%s reserved %d books, want %d", account, count, UnderworldBooksPerVisit)
		}
	}
}

func TestUnderworldUnchangedVisitAvoidsWrites(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	saveTestAccount(t, db, "observer")
	visit, err := db.TrackUnderworldVisit(ctx, "observer", "ship", "flushing")
	if err != nil {
		t.Fatal(err)
	}
	var writes int
	count := func(tx *gorm.DB) {
		if tx.Statement.Table == "underworld_visits" {
			writes++
		}
	}
	if err := db.db.Callback().Create().Before("gorm:create").Register("test_underworld_create", count); err != nil {
		t.Fatal(err)
	}
	if err := db.db.Callback().Update().Before("gorm:update").Register("test_underworld_update", count); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		current, err := db.TrackUnderworldVisit(ctx, "observer", "ship", "flushing")
		if err != nil || current != visit {
			t.Fatalf("unchanged visit: %q %v", current, err)
		}
	}
	if writes != 0 {
		t.Fatalf("unchanged visit made %d writes", writes)
	}
}

func TestUnderworldRedactBoundsRequestAndPermanentPayloads(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload string
		marker  string
	}{
		{"numeric expansion", `{"values":[1e130000,0e-16000]}`, "gap"},
		{"long numeric token", `{"value":` + strings.Repeat("1", 33) + `}`, "gap"},
		{"deep nesting", `{"values":` + strings.Repeat("[", 65) + strings.Repeat("]", 65) + `}`, "gap"},
		{"post-redaction expansion", `{"value":"` + strings.Repeat("<", MaxSnapshotBytes/2) + `"}`, "truncated"},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, _, _ := integrationStore(t)
			ctx := context.Background()
			saveTestAccount(t, db, "observer")
			if err := db.RecordRequest(ctx, "observer", "GET", "/v1/underworld/wants?port_id=flushing", 200,
				time.Millisecond, nil, []byte(test.payload), ""); err != nil {
				t.Fatal(err)
			}
			var request Request
			if err := db.db.Where("account_id = ?", "observer").Take(&request).Error; err != nil {
				t.Fatal(err)
			}
			var diagnostic map[string]any
			if err := json.Unmarshal(request.Response, &diagnostic); err != nil {
				t.Fatal(err)
			}
			if len(request.Response) > maxDiagnosticBytes || diagnostic[test.marker] != true {
				t.Fatalf("request response not bounded: bytes=%d", len(request.Response))
			}
			if err := db.RecordUnderworldObservation(ctx, &UnderworldObservation{
				AccountID: "observer", ShipID: "ship", VisitID: "visit", PortID: "flushing",
				Kind: "wants", ObservedAt: time.Now(), Payload: json.RawMessage(test.payload),
			}); err != nil {
				t.Fatal(err)
			}
			var observation UnderworldObservation
			if err := db.db.Where("account_id = ?", "observer").Take(&observation).Error; err != nil {
				t.Fatal(err)
			}
			var permanent map[string]any
			if err := json.Unmarshal(observation.Payload, &permanent); err != nil {
				t.Fatal(err)
			}
			if len(observation.Payload) > MaxSnapshotBytes || permanent[test.marker] != true {
				t.Fatalf("permanent payload not bounded: bytes=%d", len(observation.Payload))
			}
		})
	}
}
