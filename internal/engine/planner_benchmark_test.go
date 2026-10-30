package engine

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"
)

var benchmarkPlans []Plan

// Five observed quays and six cargoes exercise the normal whole-ton sizing
// loop. Report per-pass p95 as well as Go's average, excluding fixture setup.
func BenchmarkPlanVoyages(b *testing.B) {
	for _, hold := range []int64{30, 60} {
		b.Run(fmt.Sprintf("hold_%d", hold), func(b *testing.B) {
			in := benchmarkPlanningInput(b, hold)
			if plans := PlanVoyages(in); len(plans) == 0 || !plans[0].Accepted {
				b.Fatalf("benchmark lacks an executable voyage: %+v", plans)
			}
			durations := make([]int64, b.N)
			b.ReportAllocs()
			b.ResetTimer()
			for i := range durations {
				started := time.Now()
				benchmarkPlans = PlanVoyages(in)
				durations[i] = time.Since(started).Nanoseconds()
			}
			b.StopTimer()
			slices.Sort(durations)
			b.ReportMetric(float64(durations[(len(durations)-1)*95/100]), "p95-ns/pass")
		})
	}
}

func benchmarkPlanningInput(b *testing.B, hold int64) PlanningInput {
	b.Helper()
	now := time.Date(2026, 10, 28, 11, 0, 0, 0, time.UTC)
	in := PlanningInput{
		Now: now, Generation: "benchmark", CompanyID: "benchmark-company",
		Catalog: Catalog{ShipClasses: []Hull{{ID: "hoy", HullCategory: "coastal", BaseSpeedCentiknots: 850, HoldCapacityTons: hold, CrewRequired: 6}}},
		Markets: make(map[string]Market), Ports: make(map[string]PortDetail), Routes: make(map[string]Route),
		Metadata: make(map[string]DatasetMeta), Balances: map[string]int64{"channel": 100_000_000}, ReservePence: 75_000,
		Zones: []ZoneObservation{{SeaZoneID: "channel"}},
	}
	in.Clock.RealUTC, in.Clock.UnlockedPhase = now, 1
	in.Clock.Calendar.Month, in.Clock.Calendar.Day, in.Clock.Calendar.TimeScale = 6, 15, 60
	in.Catalog.SeaZones = []Zone{{ID: "channel", ZoneClass: "coastal"}}
	in.Ship.Ship.ID, in.Ship.Ship.ShipClass, in.Ship.Ship.Status, in.Ship.Ship.CurrentPortID = "benchmark-hull", "hoy", "berthed", "port-0"
	in.Ship.EffectiveHoldCapacityTons, in.Ship.HoldCapacityTons, in.Ship.Stores.ProvisionsTons = hold, hold, 8
	for _, key := range []string{"catalog", "clock", "zones", "vaults", "contracts", "ships", "world_events"} {
		in.Metadata[key] = DatasetMeta{AttemptedAt: now, UpdatedAt: now, Complete: true, Generation: in.Generation}
	}
	commodities := []string{"provisions", "grain", "timber", "iron", "canvas_sails", "pitch_tar"}
	for portIndex := range 5 {
		id := fmt.Sprintf("port-%d", portIndex)
		in.Catalog.Ports = append(in.Catalog.Ports, Port{ID: id, BankingRegion: "channel", SeaZoneID: "channel", Phase: 1, Imports: commodities})
		in.Ports[id] = PortDetail{ID: id, EffectiveBerths: 4, BerthDuesPerDayPence: 50, Visibility: "quay"}
		book := Market{PortID: id, Visibility: "quay", CustomsBPS: 100}
		for commodityIndex, commodity := range commodities {
			var row CommodityBook
			ask := 1000 + int64((portIndex+commodityIndex)%5)*100
			bid := 1600 + int64((portIndex*3+commodityIndex)%5)*100
			raw := fmt.Sprintf(`{"commodity_id":%q,"depth":{"asks":[{"price_pence":%d,"remaining_quantity":1000}],"bids":[{"price_pence":%d,"remaining_quantity":1000}]}}`, commodity, ask, bid)
			if err := json.Unmarshal([]byte(raw), &row); err != nil {
				b.Fatal(err)
			}
			book.Books = append(book.Books, row)
		}
		in.Markets[id] = book
		in.Metadata["books/"+id], in.Metadata["ports/"+id] = in.Metadata["catalog"], in.Metadata["catalog"]
		presence := in.Ship
		presence.Ship.ID, presence.Ship.CurrentPortID = "presence-"+id, id
		in.Fleet = append(in.Fleet, presence)
	}
	for _, from := range in.Catalog.Ports {
		for _, to := range in.Catalog.Ports {
			if from.ID == to.ID {
				continue
			}
			key := routeKey(from.ID, to.ID, "hoy")
			in.Routes[key] = Route{FromPort: from.ID, ToPort: to.ID, ShipClass: "hoy", TotalDistanceNM: 150, SafeProvisionsTons: 2,
				Legs: []RouteLeg{{FromNode: from.ID, ToNode: to.ID, DistanceNM: 150, EdgeClass: "coastal"}}}
			in.Metadata["routes/"+key] = in.Metadata["catalog"]
		}
	}
	seedZeroHazardQuotes(&in)
	return in
}
