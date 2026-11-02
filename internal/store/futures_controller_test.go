package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
)

func futuresControllerIntent(side string) FuturesIntent {
	now := time.Now().UTC().Truncate(time.Microsecond)
	row := FuturesIntent{
		AccountID: "pilot", IntentID: side, ActionID: "accept-pair", CohortID: "crossed-listings",
		PortID: "boston", CommodityID: "timber", Side: side, LotTons: 4,
		StrikePricePence: 1000, MarginPence: 1000, ExposurePence: 5000,
		ReservedCashPence: 1000, Status: "intent", CreatedAt: now, MaturesAt: now.Add(48 * time.Minute),
		Strategy: "accept-listing", ListingID: "listing-" + side, CounterpartyID: "counterparty-" + side,
		MaturityGameHours: 48, Admission: json.RawMessage(`{"ev_pence":800,"source":"player_listing"}`),
	}
	if side == "ask" {
		row.ReservedTons = row.LotTons
	} else {
		row.ReservedCashPence = 5000
	}
	return row
}

func sameFuturesControllerIntent(t *testing.T, got, want FuturesIntent) bool {
	t.Helper()
	for _, row := range []*FuturesIntent{&got, &want} {
		var admission any
		if err := json.Unmarshal(row.Admission, &admission); err != nil {
			t.Fatal(err)
		}
		var err error
		row.Admission, err = json.Marshal(admission)
		if err != nil {
			t.Fatal(err)
		}
	}
	return sameFuturesIntent(got, want)
}

func TestFuturesControllerIntentSupportsOtherHubsGoodsAndMultipleTons(t *testing.T) {
	for _, test := range []struct {
		port, commodity string
		tons            int64
	}{
		{port: "boston", commodity: "timber", tons: 4},
		{port: "havana", commodity: "sugar", tons: 8},
		{port: "london", commodity: "coal", tons: 2},
	} {
		t.Run(test.port, func(t *testing.T) {
			db, _, _ := integrationStore(t)
			saveTestAccount(t, db, "pilot")
			ctx := context.Background()
			row := futuresControllerIntent("ask")
			row.PortID, row.CommodityID, row.LotTons = test.port, test.commodity, test.tons
			row.ReservedTons = row.LotTons
			row.MarginPence = row.LotTons * row.StrikePricePence / 4
			row.ReservedCashPence, row.ExposurePence = row.MarginPence, row.MarginPence
			if err := db.PutFuturesIntent(ctx, &row); err != nil {
				t.Fatalf("covered multi-ton intent rejected: %v", err)
			}
			rows, err := db.FuturesIntents(ctx, "pilot")
			if err != nil || len(rows) != 1 {
				t.Fatalf("persisted intents = %+v, error = %v", rows, err)
			}
			if !sameFuturesControllerIntent(t, rows[0], row) {
				t.Fatalf("admitted hub, good or coverage changed: got %+v, want %+v", rows[0], row)
			}
			reserve, err := db.FuturesCashReserve(ctx, "pilot")
			if err != nil || reserve != row.ReservedCashPence {
				t.Fatalf("multi-ton cash reserve = %d, error = %v, want %d", reserve, err, row.ReservedCashPence)
			}
		})
	}
}

func TestFuturesControllerAdmissionTermsAreImmutable(t *testing.T) {
	db, _, _ := integrationStore(t)
	saveTestAccount(t, db, "pilot")
	ctx := context.Background()
	row := futuresControllerIntent("ask")
	if err := db.PutFuturesIntent(ctx, &row); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(*FuturesIntent)
	}{
		{name: "strategy", change: func(row *FuturesIntent) { row.Strategy = "covered-forward" }},
		{name: "listing", change: func(row *FuturesIntent) { row.ListingID = "other-listing" }},
		{name: "counterparty", change: func(row *FuturesIntent) { row.CounterpartyID = "other-company" }},
		{name: "maturity hours", change: func(row *FuturesIntent) { row.MaturityGameHours++ }},
		{name: "admission", change: func(row *FuturesIntent) {
			row.Admission = json.RawMessage(`{"ev_pence":801,"source":"player_listing"}`)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := row
			test.change(&candidate)
			if err := db.PutFuturesIntent(ctx, &candidate); !errors.Is(err, ErrActionConflict) {
				t.Fatalf("changed admission returned %v, want action conflict", err)
			}
			rows, err := db.FuturesIntents(ctx, "pilot")
			if err != nil || len(rows) != 1 {
				t.Fatalf("persisted intents = %+v, error = %v", rows, err)
			}
			if !sameFuturesControllerIntent(t, rows[0], row) {
				t.Fatalf("conflicting replay rewrote the admitted terms: %+v", rows[0])
			}
		})
	}
	row.Admission = json.RawMessage(`{ "source": "player_listing", "ev_pence": 800 }`)
	if err := db.PutFuturesIntent(ctx, &row); err != nil {
		t.Fatalf("equivalent JSON admission was not idempotent: %v", err)
	}
}

func TestFuturesControllerReplayRejectsMalformedAdmissionBeforeComparison(t *testing.T) {
	db, _, _ := integrationStore(t)
	saveTestAccount(t, db, "pilot")
	ctx := context.Background()
	row := futuresControllerIntent("ask")
	row.Admission = nil
	if err := db.PutFuturesIntent(ctx, &row); err != nil {
		t.Fatal(err)
	}
	row.Admission = json.RawMessage(`{`)
	if err := db.PutFuturesIntent(ctx, &row); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("malformed replay normalized to the admitted empty evidence: %v", err)
	}
}

func TestFuturesControllerPairReservesBothLegsWithOneAction(t *testing.T) {
	db, _, _ := integrationStore(t)
	saveTestAccount(t, db, "pilot")
	ctx := context.Background()
	ask, bid := futuresControllerIntent("ask"), futuresControllerIntent("bid")
	pair := []*FuturesIntent{&ask, &bid}
	if err := db.PutFuturesIntents(ctx, pair); err != nil {
		t.Fatal(err)
	}
	if err := db.PutFuturesIntents(ctx, pair); err != nil {
		t.Fatalf("pair replay failed: %v", err)
	}
	rows, err := db.FuturesIntents(ctx, "pilot")
	if err != nil || len(rows) != 2 {
		t.Fatalf("pair reservations = %+v, error = %v", rows, err)
	}
	for _, row := range rows {
		want := ask
		if row.Side == "bid" {
			want = bid
		}
		if row.ActionID != "accept-pair" || !sameFuturesControllerIntent(t, row, want) {
			t.Fatalf("pair leg lost its shared action or frozen coverage: %+v", row)
		}
	}
	reserve, err := db.FuturesCashReserve(ctx, "pilot")
	if err != nil || reserve != ask.ReservedCashPence+bid.ReservedCashPence {
		t.Fatalf("pair reserve = %d, error = %v, want %d", reserve, err, ask.ReservedCashPence+bid.ReservedCashPence)
	}
}

func TestFuturesControllerPairRollsBackWhenSecondLegIsInvalid(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*FuturesIntent)
	}{
		{name: "uncovered buyer", change: func(row *FuturesIntent) { row.ReservedCashPence = row.MarginPence }},
		{name: "different account", change: func(row *FuturesIntent) { row.AccountID = "other" }},
		{name: "invalid admission", change: func(row *FuturesIntent) { row.Admission = json.RawMessage(`{`) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, _, _ := integrationStore(t)
			saveTestAccount(t, db, "pilot")
			ctx := context.Background()
			ask, bid := futuresControllerIntent("ask"), futuresControllerIntent("bid")
			test.change(&bid)
			if err := db.PutFuturesIntents(ctx, []*FuturesIntent{&ask, &bid}); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("bad second leg returned %v, want invalid input", err)
			}
			rows, err := db.FuturesIntents(ctx, "pilot")
			if err != nil || len(rows) != 0 {
				t.Fatalf("failed pair left a first-leg reservation: %+v, error = %v", rows, err)
			}
			reserve, err := db.FuturesCashReserve(ctx, "pilot")
			if err != nil || reserve != 0 {
				t.Fatalf("failed pair retained cash reserve %d, error = %v", reserve, err)
			}
		})
	}
}

func TestFuturesControllerAcceptedListingMustMatchFrozenTermsAndParty(t *testing.T) {
	for _, side := range []string{"ask", "bid"} {
		t.Run(side, func(t *testing.T) {
			db, _, _ := integrationStore(t)
			saveTestAccount(t, db, "pilot")
			ctx := context.Background()
			row := futuresControllerIntent(side)
			if err := db.PutFuturesIntent(ctx, &row); err != nil {
				t.Fatal(err)
			}
			company, counterparty := "company-pilot", row.CounterpartyID
			contract := gameapi.FuturesContract{
				ID: row.ListingID, CreatorCompanyID: counterparty, PortID: row.PortID, CommodityID: row.CommodityID,
				LotTons: int32(row.LotTons), StrikePricePence: row.StrikePricePence, Status: "active",
				CreatedAt: row.CreatedAt.Add(-time.Minute), MaturesAt: row.MaturesAt, UpdatedAt: row.CreatedAt,
				SellerMarginPence: row.MarginPence, BuyerMarginPence: row.MarginPence,
			}
			if side == "ask" {
				contract.Side, contract.SellerCompanyID, contract.BuyerCompanyID = "bid", &company, &counterparty
			} else {
				contract.Side, contract.BuyerCompanyID, contract.SellerCompanyID = "ask", &company, &counterparty
			}
			for _, test := range []struct {
				name   string
				change func(*gameapi.FuturesContract)
			}{
				{name: "listing", change: func(c *gameapi.FuturesContract) { c.ID = "different-listing" }},
				{name: "creator", change: func(c *gameapi.FuturesContract) { c.CreatorCompanyID = "different-company" }},
				{name: "side", change: func(c *gameapi.FuturesContract) { c.Side = row.Side }},
				{name: "port", change: func(c *gameapi.FuturesContract) { c.PortID = "havana" }},
				{name: "commodity", change: func(c *gameapi.FuturesContract) { c.CommodityID = "sugar" }},
				{name: "quantity", change: func(c *gameapi.FuturesContract) { c.LotTons++ }},
				{name: "strike", change: func(c *gameapi.FuturesContract) { c.StrikePricePence++ }},
				{name: "maturity", change: func(c *gameapi.FuturesContract) { c.MaturesAt = c.MaturesAt.Add(time.Second) }},
				{name: "missing own party", change: func(c *gameapi.FuturesContract) {
					if side == "ask" {
						c.SellerCompanyID = nil
					} else {
						c.BuyerCompanyID = nil
					}
				}},
				{name: "wrong own party", change: func(c *gameapi.FuturesContract) {
					if side == "ask" {
						c.SellerCompanyID = &counterparty
					} else {
						c.BuyerCompanyID = &counterparty
					}
				}},
				{name: "missing counterparty", change: func(c *gameapi.FuturesContract) {
					if side == "ask" {
						c.BuyerCompanyID = nil
					} else {
						c.SellerCompanyID = nil
					}
				}},
				{name: "wrong counterparty", change: func(c *gameapi.FuturesContract) {
					if side == "ask" {
						c.BuyerCompanyID = &company
					} else {
						c.SellerCompanyID = &company
					}
				}},
				{name: "margin exceeds reservation", change: func(c *gameapi.FuturesContract) {
					if side == "ask" {
						c.SellerMarginPence++
					} else {
						c.BuyerMarginPence++
					}
				}},
			} {
				t.Run(test.name, func(t *testing.T) {
					candidate := contract
					test.change(&candidate)
					if err := db.ObserveFuturesContract(ctx, "pilot", row.IntentID, candidate); !errors.Is(err, ErrActionConflict) {
						t.Fatalf("mismatched listing returned %v, want action conflict", err)
					}
					rows, err := db.FuturesIntents(ctx, "pilot")
					if err != nil || len(rows) != 1 {
						t.Fatalf("persisted intents = %+v, error = %v", rows, err)
					}
					if rows[0].Status != "intent" || rows[0].FuturesID != "" || rows[0].ObservedMaturesAt != nil {
						t.Fatalf("mismatched listing changed durable observation: %+v", rows[0])
					}
				})
			}
			if err := db.ObserveFuturesContract(ctx, "pilot", row.IntentID, contract); err != nil {
				t.Fatalf("matching accepted third-party listing rejected: %v", err)
			}
			if err := db.ObserveFuturesContract(ctx, "pilot", row.IntentID, contract); err != nil {
				t.Fatalf("matching observation replay rejected: %v", err)
			}
			rows, err := db.FuturesIntents(ctx, "pilot")
			if err != nil || len(rows) != 1 {
				t.Fatalf("persisted intents = %+v, error = %v", rows, err)
			}
			if rows[0].FuturesID != contract.ID || rows[0].Status != "active" || rows[0].ObservedMaturesAt == nil || !rows[0].ObservedMaturesAt.Equal(row.MaturesAt) {
				t.Fatalf("accepted listing was not durably observed: %+v", rows[0])
			}
		})
	}
}

func TestFuturesControllerAggregateClaimRetainsItsPossibleBillingRegions(t *testing.T) {
	db, _, _ := integrationStore(t)
	saveTestAccount(t, db, "pilot")
	ctx := context.Background()
	for _, port := range []string{"boston", "havana"} {
		row := futuresControllerIntent("bid")
		row.IntentID, row.ActionID, row.CohortID = port, "action-"+port, "cohort-"+port
		row.PortID, row.ListingID, row.CounterpartyID = port, "", ""
		if err := db.PutFuturesIntent(ctx, &row); err != nil {
			t.Fatal(err)
		}
		company := "company-pilot"
		contract := gameapi.FuturesContract{ID: "future-" + port, CreatorCompanyID: company, PortID: port, CommodityID: row.CommodityID,
			Side: row.Side, LotTons: int32(row.LotTons), StrikePricePence: row.StrikePricePence, BuyerCompanyID: &company,
			BuyerMarginPence: row.MarginPence, Status: "defaulted", CreatedAt: row.CreatedAt,
			UpdatedAt: row.CreatedAt, MaturesAt: row.MaturesAt}
		if err := db.ObserveFuturesContract(ctx, "pilot", row.IntentID, contract); err != nil {
			t.Fatal(err)
		}
	}
	if reserved, _, err := db.ReserveFuturesCreditRead(ctx, "pilot", time.Now().UTC()); err != nil || !reserved {
		t.Fatalf("reserve credit observation: %t %v", reserved, err)
	}
	if err := db.RecordFuturesCredit(ctx, "pilot", json.RawMessage(`{"company_id":"company-pilot","futures_claims_open_pence":5000}`)); err != nil {
		t.Fatal(err)
	}
	reserve, err := db.FuturesCashReserveByRegion(ctx, "pilot", map[string]string{"boston": "america", "havana": "caribbean"})
	if err != nil || reserve["america"] != 5000 || reserve["caribbean"] != 5000 || reserve["north_sea"] != 0 {
		t.Fatalf("aggregate claims released a possible billing hub: %+v %v", reserve, err)
	}
}
