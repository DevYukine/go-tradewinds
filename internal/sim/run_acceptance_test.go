package sim

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/engine"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

func simAcceptanceRunTape(t *testing.T) (*Tape, time.Time, time.Time) {
	t.Helper()
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	datasets := map[string]string{
		"profile":   `{"company":{"id":"ours","home_port_id":"a"}}`,
		"catalog":   `{"ports":[{"id":"a","banking_region":"north","sea_zone_id":"coast","phase":1},{"id":"b","banking_region":"north","sea_zone_id":"coast","phase":1,"imports":["cloth"]}],"ship_classes":[{"id":"hoy","hull_category":"coastal","phase":1,"base_speed_centiknots":600,"hold_capacity_tons":40,"crew_required":10}],"sea_zones":[{"id":"coast","zone_class":"coastal"}],"commodities":[{"id":"cloth","base_price_pence":40000},{"id":"provisions","base_price_pence":100}]}`,
		"ships":     `[{"ship":{"id":"ship-a","ship_class":"hoy","status":"berthed","current_port_id":"a"},"hold_capacity_tons":40,"effective_hold_capacity_tons":30,"stores":{"provisions_tons":8},"cargo_manifest":{}},{"ship":{"id":"ship-b","ship_class":"hoy","status":"berthed","current_port_id":"b"},"hold_capacity_tons":40,"effective_hold_capacity_tons":30,"stores":{"provisions_tons":8},"cargo_manifest":{}}]`,
		"vaults":    `{"vaults":[{"account_id":"vault-north","account_type":"vault","banking_region":"north","balance_pence":10000000}]}`,
		"zones":     `[{"sea_zone_id":"coast","net_piracy_bps":0}]`,
		"contracts": `[]`, "factors": `[]`, "rates": `[]`, "fills": `[]`, "orders": `[]`,
		"ports/a": `{"id":"a","effective_berths":4,"berth_dues_per_day_pence":0}`,
		"ports/b": `{"id":"b","effective_berths":4,"berth_dues_per_day_pence":0}`,
		"books/a": `{"port_id":"a","visibility":"quay","customs_bps":0,"books":[{"commodity_id":"cloth","depth":{"asks":[{"order_id":"cloth-ask","price_pence":40000,"remaining_quantity":30,"is_npc":true}]}},{"commodity_id":"provisions","depth":{"asks":[{"order_id":"stores-a","price_pence":100,"remaining_quantity":100,"is_npc":true}]}}]}`,
		"books/b": `{"port_id":"b","visibility":"quay","customs_bps":0,"books":[{"commodity_id":"cloth","depth":{"bids":[{"order_id":"bid-top","price_pence":44726,"remaining_quantity":66,"is_npc":true},{"order_id":"bid-middle","price_pence":33943,"remaining_quantity":101,"is_npc":true},{"order_id":"bid-low","price_pence":27505,"remaining_quantity":101,"is_npc":true}]}},{"commodity_id":"provisions","depth":{"asks":[{"order_id":"stores-b","price_pence":100,"remaining_quantity":100,"is_npc":true}]}}]}`,
	}
	for _, pair := range [][2]string{{"a", "b"}, {"b", "a"}} {
		key := "routes/" + pair[0] + "|" + pair[1] + "|hoy"
		datasets[key] = fmt.Sprintf(`{"from_port":%q,"to_port":%q,"ship_class":"hoy","total_distance_nm":30,"safe_provisions_tons":0.1,"legs":[{"from_node":%q,"to_node":%q,"distance_nm":30,"edge_class":"coastal"}]}`, pair[0], pair[1], pair[0], pair[1])
	}
	simAcceptanceRecordedHazards(t, at, datasets, 0)
	first := simAcceptanceFrame(t, at, 100, datasets)
	first.Decision = true
	simAcceptanceRecordedHazards(t, at.Add(12*time.Minute), datasets, 0)
	return &Tape{Frames: []store.ReplayFrame{first, simAcceptanceFrame(t, at.Add(12*time.Minute), 112, datasets)}}, at, at.Add(12 * time.Minute)
}

// These explicit synthetic quotes are tape evidence, not a production fallback.
func simAcceptanceRecordedHazards(t *testing.T, at time.Time, datasets map[string]string, bps int64) {
	t.Helper()
	var ships []engine.ShipState
	if err := json.Unmarshal([]byte(datasets["ships"]), &ships); err != nil {
		t.Fatal(err)
	}
	quotes := make(map[string]engine.HazardQuote)
	for key, data := range datasets {
		if len(key) < len("routes/") || key[:len("routes/")] != "routes/" {
			continue
		}
		var route engine.Route
		if err := json.Unmarshal([]byte(data), &route); err != nil {
			t.Fatal(err)
		}
		for _, ship := range ships {
			quotes["hull|"+ship.Ship.ID+"|"+route.FromPort+"|"+route.ToPort] = engine.HazardQuote{
				ShipID: ship.Ship.ID, Origin: route.FromPort, Destination: route.ToPort,
				ShipClass: ship.Ship.ShipClass, CannonsCount: ship.Ship.CannonsCount, Month: 3,
				ExpectedLossBPS: bps, MaxInsurablePence: 10_000_000, ObservedAt: at, AttemptedAt: at,
				NetPiracyBPS: map[string]int64{"coast": 0},
			}
		}
	}
	encoded, err := json.Marshal(quotes)
	if err != nil {
		t.Fatal(err)
	}
	datasets["_hazard_quotes"] = string(encoded)
}

func TestRunAcceptanceIncidentVariantsHaveHandComputedCashDifference(t *testing.T) {
	tape, from, to := simAcceptanceRunTape(t)
	rows, err := Run(context.Background(), tape, RunOptions{From: from, To: to, Regen: 0, ReservePence: 75_000, CompanyID: "ours", Variants: []Variant{{Name: "baseline"}, {Name: "i4", NextRungReceiptStress: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("variants produced %d rows", len(rows))
	}
	byName := make(map[string]VariantResult)
	for _, row := range rows {
		byName[row.Variant] = row
	}
	baseline, stress := byName["baseline"], byName["i4"]
	if baseline.SettlementCount != 1 || len(baseline.Settlements) != 1 || baseline.Settlements[0].QuantityTons != 30 || baseline.Settlements[0].AcquisitionPence != 1_200_000 || baseline.Settlements[0].ProceedsPence != 1_341_780 {
		t.Fatalf("baseline did not execute the exact top-rung incident lot: %+v", baseline)
	}
	if stress.SettlementCount != 0 || stress.FinalCashPence != 10_000_000 || stress.FinalNetWorthPence != 10_000_000 {
		t.Fatalf("I4 variant executed receipt-risk-rejected cargo: %+v", stress)
	}
	if baseline.FinalCashPence != 10_141_780 || baseline.FinalNetWorthPence != 10_141_780 || stress.DeltaVsBaselinePence != -141_780 {
		t.Fatalf("hand-computed receipt net (30*(44726-40000)) differs: baseline=%+v i4=%+v", baseline, stress)
	}
	if !baseline.Settlements[0].CompletedAt.Before(to) {
		t.Fatalf("arrival/settlement waited for the next frame instead of the exact event time: %+v", baseline.Settlements[0])
	}
}

func TestRunAcceptanceIdenticalTapeProducesByteIdenticalJSON(t *testing.T) {
	tape, from, to := simAcceptanceRunTape(t)
	options := RunOptions{From: from, To: to, Regen: 0.5, ReservePence: 75_000, CompanyID: "ours", Variants: []Variant{{Name: "baseline"}, {Name: "i4", NextRungReceiptStress: true}, {Name: "probe0", SourcingCashBPS: new(int64(0))}}}
	before, err := json.Marshal(tape)
	if err != nil {
		t.Fatal(err)
	}
	var first []byte
	for iteration := range 11 {
		rows, err := Run(context.Background(), tape, options)
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(rows)
		if err != nil {
			t.Fatal(err)
		}
		if iteration == 0 {
			first = data
		} else if !bytes.Equal(first, data) {
			t.Fatalf("parallel variants produced nondeterministic JSON on iteration %d", iteration)
		}
		for _, row := range rows {
			for i := 1; i < len(row.Ships); i++ {
				if row.Ships[i-1].Ship.ID > row.Ships[i].Ship.ID {
					t.Fatalf("ship output is not ID ordered: %+v", row.Ships)
				}
			}
		}
	}
	after, err := json.Marshal(tape)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("variant run mutated shared recorded tape")
	}
}

func TestRunAcceptanceFreightIsNeverExecuted(t *testing.T) {
	tape, from, to := simAcceptanceRunTape(t)
	for i := range tape.Frames {
		frame := &tape.Frames[i]
		contracts := frame.Datasets["contracts"]
		contracts.Data = []byte(`[{"id":"tempting-freight","contract_type":"freight","status":"open","origin_port_id":"a","dest_port_id":"b","commodity_id":"cloth","issuer_company_id":"competitor","quantity_required":30,"remaining_quantity":30,"reward_pence":99999999,"collateral_pence":100}]`)
		frame.Datasets["contracts"] = contracts
	}
	rows, err := Run(context.Background(), tape, RunOptions{From: from, To: to, Regen: 0, ReservePence: 75_000, CompanyID: "ours", Variants: []Variant{{Name: "baseline"}, {Name: "i4", NextRungReceiptStress: true}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		for _, settlement := range row.Settlements {
			if settlement.Kind != "spot" {
				t.Fatalf("offline non-goal freight executed in %s: %+v", row.Variant, settlement)
			}
		}
	}
}

func simAcceptanceSourcingTape(t *testing.T, darkSource bool, minutes int) (*Tape, time.Time, time.Time) {
	t.Helper()
	tape, at, _ := simAcceptanceRunTape(t)
	datasets := make(map[string]string)
	for key, dataset := range tape.Frames[0].Datasets {
		datasets[key] = string(dataset.Data)
	}
	catalog := simAcceptanceJSON[engine.Catalog](t, datasets["catalog"])
	catalog.Ports = append(catalog.Ports, engine.Port{ID: "c", BankingRegion: "north", SeaZoneID: "coast", Phase: 1, Manufactures: []string{"cloth"}})
	data, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	datasets["catalog"] = string(data)
	ships := simAcceptanceJSON[[]engine.ShipState](t, datasets["ships"])
	data, err = json.Marshal(ships[:1])
	if err != nil {
		t.Fatal(err)
	}
	datasets["ships"] = string(data)
	datasets["books/a"] = `{"port_id":"a","visibility":"quay","books":[{"commodity_id":"provisions","depth":{"asks":[{"order_id":"stores-a","price_pence":100,"remaining_quantity":100,"is_npc":true}]}}]}`
	// The delivery gate pays the better of the source charter and the port b
	// bid, so the fixture bids below the 3,000p reward: the probe's cargo
	// genuinely executes the charter, fulfilling it instead of leaving an open
	// lane that invites a second probe.
	datasets["books/b"] = `{"port_id":"b","visibility":"quay","customs_bps":0,"books":[{"commodity_id":"cloth","depth":{"bids":[{"order_id":"bid-under-charter","price_pence":2900,"remaining_quantity":66,"is_npc":true}]}},{"commodity_id":"provisions","depth":{"asks":[{"order_id":"stores-b","price_pence":100,"remaining_quantity":100,"is_npc":true}]}}]}`
	datasets["books/c"] = `{"port_id":"c","visibility":"quay","books":[{"commodity_id":"cloth","depth":{"asks":[{"order_id":"source-cloth","price_pence":500,"remaining_quantity":30,"is_npc":true}]}},{"commodity_id":"provisions","depth":{"asks":[{"order_id":"stores-c","price_pence":100,"remaining_quantity":100,"is_npc":true}]}}]}`
	if darkSource {
		delete(datasets, "books/c")
	}
	datasets["contracts"] = `[{"id":"source-charter","contract_type":"charter","origin_port_id":"b","commodity_id":"cloth","quantity_required":30,"remaining_quantity":30,"reward_per_ton_pence":3000,"status":"open","is_public":true,"stale":false}]`
	datasets["ports/c"] = `{"id":"c","effective_berths":4,"berth_dues_per_day_pence":100}`
	for _, pair := range [][2]string{{"a", "c"}, {"c", "a"}, {"c", "b"}, {"b", "c"}} {
		datasets["routes/"+pair[0]+"|"+pair[1]+"|hoy"] = fmt.Sprintf(`{"from_port":%q,"to_port":%q,"ship_class":"hoy","total_distance_nm":30,"safe_provisions_tons":0.1,"legs":[{"from_node":%q,"to_node":%q,"distance_nm":30,"edge_class":"coastal"}]}`, pair[0], pair[1], pair[0], pair[1])
	}
	var frames []store.ReplayFrame
	for minute := range minutes + 1 {
		current := at.Add(time.Duration(minute) * time.Minute)
		datasets["ports/a"] = fmt.Sprintf(`{"id":"a","effective_berths":4,"coffee_house_gossip":[{"subject_port_id":"c","snapshot_at":%q,"commodities":[{"commodity_id":"cloth","snapshot_at":%q,"best_ask_pence":500}]}]}`, current.Format(time.RFC3339Nano), current.Format(time.RFC3339Nano))
		simAcceptanceRecordedHazards(t, current, datasets, 0)
		frames = append(frames, simAcceptanceFrame(t, current, 100+minute, datasets))
	}
	return &Tape{Frames: frames}, at, at.Add(time.Duration(minutes) * time.Minute)
}

func TestRunAcceptanceCashKnobControlsCharterSourcingEndToEnd(t *testing.T) {
	tape, from, to := simAcceptanceSourcingTape(t, false, 30)
	rows, err := Run(context.Background(), tape, RunOptions{From: from, To: to, Regen: 0, ReservePence: 75_000, CompanyID: "ours", Variants: []Variant{{Name: "probe0", SourcingCashBPS: new(int64(0))}, {Name: "baseline", SourcingCashBPS: new(int64(200))}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Variant == "probe0" {
			if row.Probes != 0 || row.SettlementCount != 0 || len(row.Ships) != 1 || row.Ships[0].Ship.CurrentPortID != "a" || row.IdleGameHours <= 0 {
				t.Fatalf("zero cash share departed, traded or lost idle time: %+v", row)
			}
			continue
		}
		kinds := make(map[string]int)
		for _, settlement := range row.Settlements {
			kinds[settlement.Kind]++
		}
		if row.Probes != 1 || row.Conversions != 1 || kinds["relocation"] != 1 || kinds["charter"] != 1 || row.SettlementCount != 2 {
			t.Fatalf("bounded sourcing did not sail, inspect, buy and deliver: %+v", row)
		}
	}
}

func TestRunAcceptanceDarkArrivalSettlesFailedProbeWithCosts(t *testing.T) {
	// Seventy-five real minutes run far past the failed return, the six idle
	// game hours and the 24-minute reinspection cooldown: a re-probe of the
	// dark source must never launch.
	tape, from, to := simAcceptanceSourcingTape(t, true, 75)
	rows, err := Run(context.Background(), tape, RunOptions{From: from, To: to, Regen: 0, ReservePence: 75_000, CompanyID: "ours", Variants: []Variant{{Name: "baseline"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Probes != 1 || rows[0].FailedProbes != 1 || rows[0].Conversions != 0 || rows[0].SettlementCount != 1 {
		t.Fatalf("dark arrival fabricated source stock or re-probed the cooled-down source: %+v", rows)
	}
	row := rows[0].Settlements[0]
	if row.Kind != "relocation" || row.ProbeOutcome != store.ProbeOutcomeFailed || row.ExplorationExpensePence == nil || *row.ExplorationExpensePence <= 0 || rows[0].FinalCashPence >= rows[0].InitialCashPence {
		t.Fatalf("failed inspection lost physical/berth costs: %+v", rows[0])
	}
	if len(rows[0].Ships) != 1 || rows[0].Ships[0].Voyage != nil || rows[0].Ships[0].Ship.CurrentPortID != "c" {
		t.Fatalf("dark-arrival ship left port c or kept a phantom voyage: %+v", rows[0].Ships)
	}
}

func TestRunAcceptanceEffectiveChartersRestoreRecordedAndSubtractVariantDeliveries(t *testing.T) {
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	contract := simAcceptanceJSON[engine.Contract](t, `{"id":"charter","contract_type":"charter","status":"open","remaining_quantity":50,"quantity_required":100}`)
	view := FrameView{At: at.Add(10 * time.Minute), State: engine.AccountState{Contracts: []engine.Contract{contract}}, Meta: map[string]engine.DatasetMeta{"contracts": {UpdatedAt: at.Add(5 * time.Minute), Complete: true}}}
	actions := []store.Action{
		{ID: "recorded", Method: "POST", Path: "/v1/contract/deliver-charter", Status: 200, State: store.ActionComplete, Body: json.RawMessage(`{"contract_id":"charter","quantity":20}`), Response: json.RawMessage(`{"delivered_quantity":20,"reward_paid_pence":60000}`), CreatedAt: at.Add(2 * time.Minute), UpdatedAt: at.Add(2 * time.Minute)},
		{ID: "future", Method: "POST", Path: "/v1/contract/deliver-charter", Status: 200, State: store.ActionComplete, Body: json.RawMessage(`{"contract_id":"charter","quantity":99}`), Response: json.RawMessage(`{"delivered_quantity":99,"reward_paid_pence":297000}`), CreatedAt: at.Add(20 * time.Minute), UpdatedAt: at.Add(20 * time.Minute)},
	}
	deliveries := []ContractDelivery{{ContractID: "charter", Quantity: 10, At: at.Add(3 * time.Minute)}, {ContractID: "charter", Quantity: 2, At: at.Add(7 * time.Minute)}, {ContractID: "charter", Quantity: 99, At: at.Add(20 * time.Minute)}}
	got := EffectiveContracts(view, at, actions, deliveries)
	if len(got) != 1 || got[0].RemainingQuantity != 58 {
		t.Fatalf("effective remaining must be observed 50 + recorded 20 - own 10 - own 2, without future deliveries: %+v", got)
	}
	if view.State.Contracts[0].RemainingQuantity != 50 {
		t.Fatal("variant delivery depletion modified shared tape contracts")
	}
}

func TestRunAcceptanceRegionalFundingChargesFeeNotPrincipal(t *testing.T) {
	tape, from, to := simAcceptanceRunTape(t)
	for i := range tape.Frames {
		vaults := tape.Frames[i].Datasets["vaults"]
		vaults.Data = []byte(`{"vaults":[{"account_id":"local","account_type":"vault","banking_region":"north","balance_pence":75000},{"account_id":"donor","account_type":"vault","banking_region":"donor","balance_pence":9925000}]}`)
		tape.Frames[i].Datasets["vaults"] = vaults
		rates := tape.Frames[i].Datasets["rates"]
		rates.Data = []byte(`[{"from_region":"donor","to_region":"north","fee_bps":100}]`)
		tape.Frames[i].Datasets["rates"] = rates
	}
	rows, err := Run(context.Background(), tape, RunOptions{From: from, To: to, Regen: 0, ReservePence: 75_000, CompanyID: "ours", Variants: []Variant{{Name: "baseline"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || len(rows[0].Settlements) != 1 {
		t.Fatalf("regional surplus did not fund observed profitable cargo: %+v", rows)
	}
	// Rebuild the decision input the funded world acted on and price its
	// remittance with the production funding path.
	view, err := NewTapeReader(tape.Frames).Advance(from)
	if err != nil {
		t.Fatal(err)
	}
	world, err := newVariantWorld(tape, RunOptions{From: from, To: to, Regen: 0, ReservePence: 75_000, CompanyID: "ours"}, Variant{Name: "baseline"}, view)
	if err != nil {
		t.Fatal(err)
	}
	world.market.Observe(view)
	input, err := world.planningInput(view, world.fleet.Ships()[0])
	if err != nil {
		t.Fatal(err)
	}
	plan, accepted := world.bestOrdinary(input)
	if !accepted {
		t.Fatal("funded world lost its observed profitable cargo plan")
	}
	_, transfers := engine.PlanFunding(input, plan)
	if len(transfers) == 0 {
		t.Fatal("funding scenario no longer requires a donor remittance")
	}
	fees := int64(0)
	for _, transfer := range transfers {
		fees += transfer.Gross - transfer.Net
	}
	if rows[0].Settlements[0].FeesPence != fees || rows[0].FinalCashPence != 10_141_780-fees {
		t.Fatalf("settlement must carry exactly the PlanFunding transfer fee %d and never the principal: %+v", fees, rows[0])
	}
}

func TestRunAcceptanceFixedFleetAndUpkeepAtEachGameDayBoundary(t *testing.T) {
	tape, from, _ := simAcceptanceRunTape(t)
	for i := range tape.Frames {
		for _, key := range []string{"books/a", "books/b"} {
			book := tape.Frames[i].Datasets[key]
			book.Data = []byte(`{"port_id":"` + key[len("books/"):] + `","visibility":"quay","books":[]}`)
			tape.Frames[i].Datasets[key] = book
		}
	}
	future := tape.Frames[1]
	future.ObservedAt = from.Add(time.Hour)
	future.Clock = json.RawMessage(fmt.Sprintf(`{"real_utc":%q,"unlocked_phase":1,"calendar":{"month":3,"day":7,"time_scale":60,"total_game_hours":160}}`, future.ObservedAt.Format(time.RFC3339Nano)))
	ships := future.Datasets["ships"]
	initialShips := simAcceptanceJSON[[]engine.ShipState](t, string(ships.Data))
	added := initialShips[0]
	added.Ship.ID = "future-new-hull"
	data, err := json.Marshal(append(initialShips, added))
	if err != nil {
		t.Fatal(err)
	}
	ships.Data = data
	future.Datasets["ships"] = ships
	tape.Frames = []store.ReplayFrame{tape.Frames[0], future}
	rows, err := Run(context.Background(), tape, RunOptions{From: from, To: from.Add(time.Hour), Regen: 0, ReservePence: 75_000, CompanyID: "ours", Variants: []Variant{{Name: "baseline"}}})
	if err != nil {
		t.Fatal(err)
	}
	catalog := simAcceptanceJSON[engine.Catalog](t, string(tape.Frames[0].Datasets["catalog"].Data))
	daily := engine.DailyUpkeep(initialShips, catalog, 0)
	if daily <= 0 || len(rows) != 1 || len(rows[0].Ships) != 2 || rows[0].FinalCashPence != 10_000_000-2*daily {
		t.Fatalf("fixed starting fleet must pay upkeep once at game hours 120 and 144, not per frame: daily=%d rows=%+v", daily, rows)
	}
	// The fixture's home port a bills north; an empty accident region must
	// never absorb the fleet upkeep.
	if len(rows[0].Vaults) != 1 || rows[0].Vaults[0].Region != "north" || rows[0].Vaults[0].BalancePence != 10_000_000-2*daily {
		t.Fatalf("upkeep must bill the fleet's north home region exactly: %+v", rows[0].Vaults)
	}
}

func TestRunAcceptanceInitialVoyageCargoArrivesIdenticallyForEveryVariant(t *testing.T) {
	tape, from, to := simAcceptanceRunTape(t)
	for i := range tape.Frames {
		ships := tape.Frames[i].Datasets["ships"]
		ships.Data = []byte(fmt.Sprintf(`[{"ship":{"id":"initial-voyage","ship_class":"hoy","status":"sailing","current_port_id":"a"},"hold_capacity_tons":40,"effective_hold_capacity_tons":30,"cargo_used_tons":5,"cargo_manifest":{"cloth":5},"stores":{"provisions_tons":8},"voyage":{"estimated_arrival_at":%q},"route_legs":[{"from_node":"a","to_node":"b","distance_nm":30,"edge_class":"coastal"}]}]`, from.Add(3*time.Minute).Format(time.RFC3339Nano)))
		tape.Frames[i].Datasets["ships"] = ships
		book := tape.Frames[i].Datasets["books/a"]
		book.Data = []byte(`{"port_id":"a","visibility":"quay","books":[]}`)
		tape.Frames[i].Datasets["books/a"] = book
	}
	rows, err := Run(context.Background(), tape, RunOptions{From: from, To: to, Regen: 0, ReservePence: 75_000, CompanyID: "ours", Variants: []Variant{{Name: "baseline"}, {Name: "i4", NextRungReceiptStress: true}, {Name: "probe0", SourcingCashBPS: new(int64(0))}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("starting voyage lost variants: %+v", rows)
	}
	for _, row := range rows {
		if len(row.Ships) != 1 || row.Ships[0].Voyage != nil || row.Ships[0].Ship.CurrentPortID != "b" || row.FinalNetWorthPence != rows[0].FinalNetWorthPence || row.FinalCashPence != rows[0].FinalCashPence {
			t.Fatalf("initial owned cargo/voyage was discarded or treated differently by a policy knob: %+v", rows)
		}
	}
}

func TestRunAcceptanceLaterDestinationObservationChargesCurrentBerthDues(t *testing.T) {
	tape, from, to := simAcceptanceRunTape(t)
	datasets := make(map[string]string)
	for key, dataset := range tape.Frames[0].Datasets {
		datasets[key] = string(dataset.Data)
	}
	datasets["ports/b"] = `{"id":"b","effective_berths":4,"berth_dues_per_day_pence":2400}`
	updated := simAcceptanceFrame(t, from.Add(2*time.Minute), 102, datasets)
	finalPort := tape.Frames[1].Datasets["ports/b"]
	finalPort.Data = []byte(datasets["ports/b"])
	tape.Frames[1].Datasets["ports/b"] = finalPort
	tape.Frames = []store.ReplayFrame{tape.Frames[0], updated, tape.Frames[1]}
	rows, err := Run(context.Background(), tape, RunOptions{From: from, To: to, Regen: 0, ReservePence: 75_000, CompanyID: "ours", Variants: []Variant{{Name: "baseline"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || len(rows[0].Settlements) != 1 {
		t.Fatalf("updated destination observation prevented the planned delivery: %+v", rows)
	}
	if rows[0].Settlements[0].ExpensesPence != 600 || rows[0].FinalNetWorthPence != 10_141_180 {
		t.Fatalf("arrival retained initial zero dues instead of charging six hours at the later observed 2400p/day: %+v", rows[0])
	}
}

func TestRunAcceptanceDeliveryPaysHighestNominalCharter(t *testing.T) {
	tape, from, to := simAcceptanceRunTape(t)
	contracts := `[{"id":"planned-low","contract_type":"charter","origin_port_id":"b","commodity_id":"cloth","quantity_required":30,"remaining_quantity":30,"reward_per_ton_pence":49000,"status":"open","is_public":true},{"id":"nominal-best","contract_type":"charter","origin_port_id":"b","commodity_id":"cloth","quantity_required":10,"remaining_quantity":10,"reward_per_ton_pence":50000,"status":"open","is_public":true}]`
	for i := range tape.Frames {
		row := tape.Frames[i].Datasets["contracts"]
		row.Data = []byte(contracts)
		tape.Frames[i].Datasets["contracts"] = row
	}
	rows, err := Run(context.Background(), tape, RunOptions{From: from, To: to, Regen: 0, ReservePence: 75_000, CompanyID: "ours", Variants: []Variant{{Name: "baseline"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || len(rows[0].Settlements) != 1 || rows[0].Settlements[0].Kind != "charter" || rows[0].Settlements[0].QuantityTons != 30 {
		t.Fatalf("charter plan did not sail and settle once: %+v", rows)
	}
	// The planner pins planned-low (49,000p over 30 tons nets more than
	// 50,000p over 10), but delivery must pay the highest nominal per-ton
	// charter first, then offer the remaining twenty tons to planned-low on
	// the next recorded pass without filling the lower 44,726p spot bid.
	if got := rows[0].Settlements[0].ProceedsPence; got != 10*50_000+20*49_000 {
		t.Fatalf("delivery did not pay both charters in nominal reward order: %d", got)
	}
	if !rows[0].Settlements[0].CompletedAt.Equal(to) || rows[0].FinalCashPence != 10_280_000 {
		t.Fatalf("charter remainder did not wait for the next pass and settle without spot sales: %+v", rows[0])
	}
}

func TestRunAcceptanceDeliveryPrefersHigherBidOverPlannedCharter(t *testing.T) {
	tape, from, to := simAcceptanceRunTape(t)
	contract := `[{"id":"planned","contract_type":"charter","origin_port_id":"b","commodity_id":"cloth","quantity_required":30,"remaining_quantity":30,"reward_per_ton_pence":45000,"status":"open","is_public":true}]`
	for i := range tape.Frames {
		row := tape.Frames[i].Datasets["contracts"]
		row.Data = []byte(contract)
		tape.Frames[i].Datasets["contracts"] = row
	}
	datasets := make(map[string]string)
	for key, dataset := range tape.Frames[0].Datasets {
		datasets[key] = string(dataset.Data)
	}
	datasets["books/b"] = `{"port_id":"b","visibility":"quay","customs_bps":0,"books":[{"commodity_id":"cloth","depth":{"bids":[{"order_id":"bid-raised","price_pence":50000,"remaining_quantity":66,"is_npc":true}]}},{"commodity_id":"provisions","depth":{"asks":[{"order_id":"stores-b","price_pence":100,"remaining_quantity":100,"is_npc":true}]}}]}`
	updated := simAcceptanceFrame(t, from.Add(2*time.Minute), 102, datasets)
	finalBook := tape.Frames[1].Datasets["books/b"]
	finalBook.Data = []byte(datasets["books/b"])
	tape.Frames[1].Datasets["books/b"] = finalBook
	tape.Frames = []store.ReplayFrame{tape.Frames[0], updated, tape.Frames[1]}
	rows, err := Run(context.Background(), tape, RunOptions{From: from, To: to, Regen: 0, ReservePence: 75_000, CompanyID: "ours", Variants: []Variant{{Name: "baseline"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || len(rows[0].Settlements) != 1 || rows[0].Settlements[0].Kind != "charter" || rows[0].Settlements[0].QuantityTons != 30 {
		t.Fatalf("planned charter voyage did not sail and settle once: %+v", rows)
	}
	// The plan pinned the 45,000p charter while the bid paid 44,726p; the bid
	// rises to 50,000p before arrival and the whole lot must sell spot.
	if got := rows[0].Settlements[0].ProceedsPence; got != 30*50_000 {
		t.Fatalf("delivery forced the planned charter under a better bid: %d", got)
	}
}

func TestRunAcceptanceCharterTaperUsesActualDeliveryAmount(t *testing.T) {
	tape, from, to := simAcceptanceRunTape(t)
	for i := range tape.Frames {
		ships := tape.Frames[i].Datasets["ships"]
		ships.Data = []byte(fmt.Sprintf(`[{"ship":{"id":"taper-run","ship_class":"hoy","status":"sailing","current_port_id":"a"},"hold_capacity_tons":40,"effective_hold_capacity_tons":30,"cargo_used_tons":12,"cargo_manifest":{"cloth":12},"stores":{"provisions_tons":8},"voyage":{"estimated_arrival_at":%q},"route_legs":[{"from_node":"a","to_node":"b","distance_nm":30,"edge_class":"coastal"}]}]`, from.Add(3*time.Minute).Format(time.RFC3339Nano)))
		tape.Frames[i].Datasets["ships"] = ships
		book := tape.Frames[i].Datasets["books/a"]
		book.Data = []byte(`{"port_id":"a","visibility":"quay","books":[]}`)
		tape.Frames[i].Datasets["books/a"] = book
		contracts := tape.Frames[i].Datasets["contracts"]
		contracts.Data = []byte(`[{"id":"taper-charter","contract_type":"charter","origin_port_id":"b","commodity_id":"cloth","quantity_required":40,"remaining_quantity":40,"reward_per_ton_pence":50000,"status":"open","is_public":true}]`)
		tape.Frames[i].Datasets["contracts"] = contracts
	}
	tape.Settlements = []store.Settlement{{ID: "seed", Kind: "charter", ProceedsPence: 4_990_000, CompletedAt: from}}
	rows, err := Run(context.Background(), tape, RunOptions{From: from, To: to, Regen: 0, ReservePence: 75_000, CompanyID: "ours", Variants: []Variant{{Name: "baseline"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || len(rows[0].Settlements) != 1 || rows[0].Settlements[0].Kind != "recovery" || rows[0].Settlements[0].QuantityTons != 12 {
		t.Fatalf("recovery cargo did not arrive and settle once: %+v", rows)
	}
	// Seed proceeds sit 10,000p under the 5,000,000p Crown taper threshold.
	// Planning pre-tapers by hold capacity (30 tons would earn 6,490,000p),
	// but the ship carries 12 tons against a 40-ton charter, so the delivery
	// must restore the posted 50,000p, taper it by the actual 12 tons
	// (earnings 5,590,000p -> 40,000p + floor(10,000p*sqrt(5e6/5.59e6)) =
	// 49,457p per ton) and pay 12*49,457p - never the capacity taper
	// (12*48,777p), a second taper (12*48,311p) or the raw reward (600,000p).
	if got := rows[0].Settlements[0].ProceedsPence; got != 12*49_457 {
		t.Fatalf("charter taper did not use the actual partial delivery amount: %d", got)
	}
}

func TestRunAcceptanceCharterTaperUsesOnlySettledCharterHistory(t *testing.T) {
	for _, kind := range []string{"charter", "spot", "recovery"} {
		t.Run(kind, func(t *testing.T) {
			tape, from, to := simAcceptanceRunTape(t)
			for i := range tape.Frames {
				contracts := tape.Frames[i].Datasets["contracts"]
				contracts.Data = []byte(`[{"id":"planned-low","contract_type":"charter","origin_port_id":"b","commodity_id":"cloth","quantity_required":30,"remaining_quantity":30,"reward_per_ton_pence":49000,"status":"open","is_public":true},{"id":"nominal-best","contract_type":"charter","origin_port_id":"b","commodity_id":"cloth","quantity_required":10,"remaining_quantity":10,"reward_per_ton_pence":50000,"status":"open","is_public":true}]`)
				tape.Frames[i].Datasets["contracts"] = contracts
			}
			tape.Settlements = []store.Settlement{{ID: "seed", Kind: "charter", ProceedsPence: 4_000_000, CompletedAt: from}}
			reader := NewTapeReader(tape.Frames)
			view, err := reader.Advance(from)
			if err != nil {
				t.Fatal(err)
			}
			world, err := newVariantWorld(tape, RunOptions{From: from, To: to, Regen: 0, ReservePence: 75_000, CompanyID: "ours"}, Variant{Name: "baseline"}, view)
			if err != nil {
				t.Fatal(err)
			}
			world.market.Observe(view)
			input, err := world.planningInput(view, *world.fleet.Ship("ship-a"))
			if err != nil {
				t.Fatal(err)
			}
			plan, accepted := world.bestOrdinary(input)
			if !accepted || plan.Kind != "charter" || plan.ContractID != "planned-low" || plan.Quantity != 30 {
				t.Fatalf("fixture did not choose the larger planned-low lot: %+v", plan)
			}
			var initialLowReward int64
			for _, contract := range input.Contracts {
				if contract.ID == "planned-low" && contract.RewardPerTonPence != nil {
					initialLowReward = *contract.RewardPerTonPence
				}
			}
			// Charter receipts can also belong to spot or recovery missions;
			// only the mission's eventual settlement kind enters live history.
			plan.Kind = kind
			if err := world.startMission(view, input, plan, false); err != nil {
				t.Fatal(err)
			}
			mission := world.missions["ship-a"]
			advanceAt := func(at time.Time) FrameView {
				t.Helper()
				next, err := reader.Advance(at)
				if err != nil {
					t.Fatal(err)
				}
				if err := world.advance(next, false); err != nil {
					t.Fatal(err)
				}
				return next
			}
			for _, stage := range []string{"sailing", "deliver", "deliver"} {
				view = advanceAt(mission.nextAt)
				if world.missions["ship-a"] != mission || mission.stage != stage {
					t.Fatalf("partial charter did not retain its mission for another pass: stage=%s mission=%+v", stage, world.missions["ship-a"])
				}
			}
			if len(world.deliveries) != 1 || world.deliveries[0].ContractID != "nominal-best" || world.deliveries[0].Quantity != 10 || !world.deliveries[0].At.Equal(view.At) ||
				len(world.sellFills) != 0 || world.fleet.Ship("ship-a").CargoManifest["cloth"] != 20 || len(world.accounting.Settlements()) != 0 ||
				world.accounting.missions[mission.id].money.ProceedsPence != 500_000 {
				t.Fatalf("first pass did not bank just the highest nominal charter and retain twenty tons: deliveries=%+v fills=%+v mission=%+v", world.deliveries, world.sellFills, world.accounting.missions[mission.id])
			}
			if got, err := world.charterProceedsPence(view); err != nil || got != 4_000_000 {
				t.Fatalf("unsettled charter receipt entered taper history: %d, %v", got, err)
			}
			input, err = world.planningInput(view, *world.fleet.Ship("ship-a"))
			if err != nil {
				t.Fatal(err)
			}
			for _, contract := range input.Contracts {
				switch contract.ID {
				case "nominal-best":
					if contract.RemainingQuantity != 0 || contract.QuantityDelivered != 10 {
						t.Fatalf("highest nominal charter was not decremented before the next pass: %+v", contract)
					}
				case "planned-low":
					if contract.RemainingQuantity != 30 || contract.QuantityDelivered != 0 || contract.RewardPerTonPence == nil || *contract.RewardPerTonPence != initialLowReward {
						t.Fatalf("open mission receipts changed the next charter or its planning taper: %+v", contract)
					}
				}
			}
			firstDeliveryAt := view.At
			view = advanceAt(to)
			if len(world.deliveries) != 2 || world.deliveries[1].ContractID != "planned-low" || world.deliveries[1].Quantity != 20 ||
				!world.deliveries[1].At.Equal(to) || !world.deliveries[1].At.After(firstDeliveryAt) || len(world.sellFills) != 0 || world.missions["ship-a"] != nil {
				t.Fatalf("subsequent pass did not deliver the remainder to the next charter without spot sales: deliveries=%+v fills=%+v", world.deliveries, world.sellFills)
			}
			rows := world.accounting.Settlements()
			if len(rows) != 1 || rows[0].Kind != kind || rows[0].QuantityTons != 30 || rows[0].ProceedsPence != 1_480_000 || !rows[0].CompletedAt.Equal(to) {
				t.Fatalf("same-mission receipts incorrectly tapered the second delivery or settlement: %+v", rows)
			}
			nextView, err := reader.Advance(to.Add(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			wantHistory := int64(4_000_000)
			if kind == "charter" {
				wantHistory += 1_480_000
			}
			if got, err := world.charterProceedsPence(nextView); err != nil || got != wantHistory {
				t.Fatalf("next-pass taper history must contain only settled charter rows: got %d want %d, %v", got, wantHistory, err)
			}
			input, err = world.planningInput(nextView, *world.fleet.Ship("ship-a"))
			if err != nil {
				t.Fatal(err)
			}
			for _, contract := range input.Contracts {
				if contract.ID != "planned-low" {
					continue
				}
				nominal := contract
				nominal.RewardPerTonPence = new(int64(49_000))
				wantReward := engine.TaperedCharterReward(nominal, nextView.State.Catalog, wantHistory, input.Ship.EffectiveHoldCapacityTons)
				if contract.RemainingQuantity != 10 || contract.QuantityDelivered != 20 || contract.RewardPerTonPence == nil || *contract.RewardPerTonPence != wantReward {
					t.Fatalf("next-pass planning lost the remaining charter or settled-only taper: %+v want reward %d", contract, wantReward)
				}
			}
		})
	}
}

func TestRunAcceptanceRollingPrefixAugmentsWindowHistoryOnce(t *testing.T) {
	tape, from, to := simAcceptanceSourcingTape(t, false, 30)
	calls := 0
	tape.readSourcingPrefix = func(since, asOf time.Time) ([]store.Settlement, error) {
		calls++
		if !since.Equal(from.Add(-7*24*time.Hour)) || !asOf.Equal(from) {
			t.Fatalf("rolling prefix omitted possible source probes or changed its cutover: %s through %s", since, asOf)
		}
		return []store.Settlement{{ID: "prior-probe", Kind: "relocation", Destination: "amsterdam", CostsKnown: true,
			ExplorationExpensePence: new(int64(19_000)), CompletedAt: from.Add(-2 * time.Hour)}}, nil
	}
	tape.Settlements = []store.Settlement{
		{ID: "cutover", Kind: "spot", Origin: "unprobed", StartedAt: from.Add(-time.Hour), ProceedsPence: 100_000, CostsKnown: true, QuantityTons: 10, GameSeconds: 3600, CompletedAt: from},
		{ID: "future", Kind: "spot", Origin: "amsterdam", StartedAt: from.Add(-time.Hour), ProceedsPence: 9_000_000, CostsKnown: true, QuantityTons: 10, GameSeconds: 3600, CompletedAt: from.Add(time.Minute)},
	}
	view, err := NewTapeReader(tape.Frames).Advance(from)
	if err != nil {
		t.Fatal(err)
	}
	world, err := newVariantWorld(tape, RunOptions{From: from, To: to, Regen: 0, ReservePence: 75_000, CompanyID: "ours"}, Variant{Name: "baseline"}, view)
	if err != nil {
		t.Fatal(err)
	}
	world.market.Observe(view)
	for pass := range 2 {
		view.At = from.Add(time.Duration(pass) * 2 * time.Minute)
		input, err := world.planningInput(view, *world.fleet.Ship("ship-a"))
		if err != nil {
			t.Fatal(err)
		}
		// Unattributed profit and future recorded returns cannot replenish the
		// rolling seed; applying the same prefix twice cannot spend it twice.
		if input.SourcingAllowancePence == nil || *input.SourcingAllowancePence != 1_000 ||
			input.SourcingProbeExpensePence != 19_000 || input.SourcingProbeCostsUnknown {
			t.Fatalf("pass %d did not combine rolling and cutover economics exactly once: allowance=%v expense=%d unknown=%v", pass, input.SourcingAllowancePence, input.SourcingProbeExpensePence, input.SourcingProbeCostsUnknown)
		}
	}
	if calls != 1 {
		t.Fatalf("rolling prefix was not cached across worlds: %d queries", calls)
	}
}

func TestRunUsesRecordedHazardPricingAndNeverInventsZero(t *testing.T) {
	tape, from, to := simAcceptanceRunTape(t)
	view, err := tape.Reader().Advance(from)
	if err != nil {
		t.Fatal(err)
	}
	quote := view.State.HazardQuotes["hull|ship-a|a|b"]
	quote.ExpectedLossBPS = 2_000
	for index := range tape.Frames {
		dataset := tape.Frames[index].Datasets["_hazard_quotes"]
		var quotes map[string]engine.HazardQuote
		if err := json.Unmarshal(dataset.Data, &quotes); err != nil {
			t.Fatal(err)
		}
		quotes["hull|ship-a|a|b"] = quote
		dataset.Data, _ = json.Marshal(quotes)
		tape.Frames[index].Datasets["_hazard_quotes"] = dataset
	}
	options := RunOptions{From: from, To: to, Variants: []Variant{{Name: "baseline"}}}
	rows, err := Run(context.Background(), tape, options)
	if err != nil || len(rows) != 1 || !rows[0].HazardAvailable || rows[0].SettlementCount != 0 {
		t.Fatalf("recorded 20%% hazard did not reject the thin-margin lot: %+v, %v", rows, err)
	}
	for index := range tape.Frames {
		delete(tape.Frames[index].Datasets, "_hazard_quotes")
	}
	rows, err = Run(context.Background(), tape, options)
	if err != nil || len(rows) != 1 || rows[0].HazardAvailable || rows[0].SettlementCount != 0 {
		t.Fatalf("missing hazard was replaced with zero: %+v, %v", rows, err)
	}
	if !bytes.Contains([]byte(fmt.Sprint(rows[0].Omissions)), []byte("UNTRUSTED: no recorded Lloyd's quotes")) {
		t.Fatalf("direct run hid unavailable hazard pricing: %+v", rows[0])
	}
}

func TestRunReloadsRollingPrefixWhenClockSlowsWithoutDoubleCounting(t *testing.T) {
	tape, from, to := simAcceptanceSourcingTape(t, false, 30)
	recent := store.Settlement{ID: "recent-probe", Kind: "relocation", CostsKnown: true,
		ExplorationExpensePence: new(int64(19_000)), CompletedAt: from.Add(-time.Hour)}
	old := store.Settlement{ID: "old-probe", Kind: "relocation", CostsKnown: true,
		ExplorationExpensePence: new(int64(500)), CompletedAt: from.Add(-20 * 24 * time.Hour)}
	unknown := store.Settlement{ID: "old-unknown-probe", Kind: "relocation", CompletedAt: from.Add(-21 * 24 * time.Hour)}
	calls := 0
	tape.readSourcingPrefix = func(since, asOf time.Time) ([]store.Settlement, error) {
		calls++
		if !asOf.Equal(from) {
			t.Fatalf("clock change loaded post-cutover evidence through %s", asOf)
		}
		if calls == 1 {
			if !since.Equal(from.Add(-7 * 24 * time.Hour)) {
				t.Fatalf("unexpected initial prefix horizon: %s", since)
			}
			return []store.Settlement{recent}, nil
		}
		if !since.Equal(from.Add(-60 * 24 * time.Hour)) {
			t.Fatalf("slow clock did not enlarge the live rolling horizon: %s", since)
		}
		return []store.Settlement{old, unknown, recent}, nil
	}
	view, err := tape.Reader().Advance(from)
	if err != nil {
		t.Fatal(err)
	}
	world, err := newVariantWorld(tape, RunOptions{From: from, To: to, ReservePence: 75_000, CompanyID: "ours"}, Variant{Name: "baseline"}, view)
	if err != nil {
		t.Fatal(err)
	}
	world.market.Observe(view)
	input, err := world.planningInput(view, *world.fleet.Ship("ship-a"))
	if err != nil || input.SourcingProbeCostsUnknown || input.SourcingAllowancePence == nil || *input.SourcingAllowancePence != 1_000 {
		t.Fatalf("initial clock lost the known rolling prefix: %+v err=%v", input, err)
	}
	view.At = from.Add(time.Minute)
	view.State.Clock.Calendar.TimeScale = 1
	for range 2 {
		input, err = world.planningInput(view, *world.fleet.Ship("ship-a"))
		if err != nil || !input.SourcingProbeCostsUnknown || input.SourcingAllowancePence == nil ||
			*input.SourcingAllowancePence != 0 || input.SourcingProbeExpensePence != 19_500 {
			t.Fatalf("slow clock omitted old unknown costs or counted a prefix twice: %+v err=%v", input, err)
		}
	}
	if calls != 2 {
		t.Fatalf("prefix refresh did not remain cached per horizon: %d calls", calls)
	}
}

func TestRunMarksUnquotedSimulatedLaneUntrustedWithoutFailingSupply(t *testing.T) {
	tape, from, to := simAcceptanceSourcingTape(t, false, 30)
	for index := range tape.Frames {
		dataset := tape.Frames[index].Datasets["_hazard_quotes"]
		var quotes map[string]engine.HazardQuote
		if err := json.Unmarshal(dataset.Data, &quotes); err != nil {
			t.Fatal(err)
		}
		for key, quote := range quotes {
			if quote.Origin == "c" {
				delete(quotes, key)
			}
		}
		dataset.Data, _ = json.Marshal(quotes)
		tape.Frames[index].Datasets["_hazard_quotes"] = dataset
	}
	view, err := tape.Reader().Advance(from)
	if err != nil || !recordedHazardAvailable(view) {
		t.Fatalf("fixture must retain recorded quotes on other lanes: %v", err)
	}
	rows, err := Run(context.Background(), tape, RunOptions{From: from, To: to, ReservePence: 75_000,
		CompanyID: "ours", Variants: []Variant{{Name: "baseline"}}})
	if err != nil || len(rows) != 1 {
		t.Fatalf("run unquoted source lane: %+v err=%v", rows, err)
	}
	row := rows[0]
	if row.HazardAvailable || row.HazardUnavailablePlans == 0 || row.Probes != 1 ||
		row.FailedProbes != 0 || row.Conversions != 0 || len(row.Settlements) != 1 {
		t.Fatalf("missing lane quote hid the evidence gap or fabricated failed supply: %+v", row)
	}
	if row.Settlements[0].ProbeOutcome != "" || row.Settlements[0].Kind != "relocation" {
		t.Fatalf("unpriced inspection acquired a supply failure or continuation: %+v", row.Settlements[0])
	}
	input := engine.PlanningInput{Now: to, Ship: row.Ships[0], Clock: view.State.Clock}
	if err := engine.NewSourcingLedger(row.Settlements, nil).Apply(&input, nil); err != nil {
		t.Fatal(err)
	}
	for _, source := range input.SourcingProbeHistory.Sources {
		if !source.FailedAt.IsZero() || !source.CatalogueFailedAt.IsZero() {
			t.Fatalf("missing Lloyd's evidence invented a source cooldown: %+v", source)
		}
	}
	for _, ship := range input.SourcingProbeHistory.Ships {
		if ship.Failures != 0 {
			t.Fatalf("missing Lloyd's evidence invented a failed-probe streak: %+v", ship)
		}
	}
}

func TestLoadTapeHoldoutKeepsPreTapeProbeForInWindowConversion(t *testing.T) {
	db, _ := backfillAcceptanceStore(t)
	ctx := context.Background()
	fixture, cutover, to := simAcceptanceSourcingTape(t, false, 30)
	tapeFrom := cutover.Add(-10 * 24 * time.Hour)
	for _, frame := range fixture.Frames {
		frame.AccountID, frame.Source = "backfill", "live"
		if _, err := db.RecordReplayFrame(ctx, frame); err != nil {
			t.Fatal(err)
		}
	}
	probeAt := cutover.Add(-14 * 24 * time.Hour)
	for _, row := range []store.Settlement{
		{ID: "holdout-old-probe", ShipID: "ship-a", Kind: "relocation", Origin: "a", Destination: "c",
			Commodity: "cloth", Lane: "c|b|cloth|probe", CostsKnown: true, ExplorationExpensePence: new(int64(100)),
			StartedAt: probeAt.Add(-time.Hour), CompletedAt: probeAt, CreatedAt: probeAt},
		{ID: "holdout-conversion", ShipID: "ship-a", Kind: "spot", Origin: "c", Destination: "b",
			Commodity: "cloth", Lane: "c|b|cloth", CostsKnown: true, ProceedsPence: 100_000,
			QuantityTons: 1, GameSeconds: 3600, StartedAt: cutover.Add(-9 * 24 * time.Hour),
			CompletedAt: cutover.Add(-time.Hour), CreatedAt: cutover.Add(-time.Hour)},
	} {
		row.AccountID, row.MissionID, row.ActionID = "backfill", row.ID+"-mission", row.ID+"-action"
		if _, err := db.BeginAction(ctx, &store.Action{ID: row.ActionID, AccountID: row.AccountID,
			Method: "POST", Path: "/v1/market/place-order", Body: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
		if err := db.CompleteAction(ctx, row.ActionID, 200, json.RawMessage(`{}`), nil); err != nil {
			t.Fatal(err)
		}
		if err := db.PutSettlement(ctx, &row); err != nil {
			t.Fatal(err)
		}
	}
	tape, err := LoadTape(ctx, db, "backfill", tapeFrom, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(tape.Settlements) != 1 || tape.Settlements[0].ID != "holdout-conversion" {
		t.Fatalf("fixture must leave attribution outside the loaded tape: %+v", tape.Settlements)
	}
	view, err := tape.Reader().Advance(cutover)
	if err != nil {
		t.Fatal(err)
	}
	world, err := newVariantWorld(tape, RunOptions{From: cutover, To: to, ReservePence: 75_000,
		CompanyID: "company-backfill"}, Variant{Name: "baseline"}, view)
	if err != nil {
		t.Fatal(err)
	}
	world.market.Observe(view)
	input, err := world.planningInput(view, *world.fleet.Ship("ship-a"))
	if err != nil || input.SourcingAllowancePence == nil || *input.SourcingAllowancePence != 70_000 ||
		input.SourcingProbeHistory.ConversionNetPence != 100_000 || len(input.SourcingProbeHistory.Conversions) != 1 ||
		!input.SourcingProbeHistory.Conversions[0].ProbeAt.Equal(probeAt) {
		t.Fatalf("holdout lost the live rolling conversion credit: %+v err=%v", input, err)
	}
}
