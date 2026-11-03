package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/names"
	"github.com/google/uuid"
)

func TestBulletinReplyRelevanceAndUntrustedText(t *testing.T) {
	for _, test := range []struct {
		name, company, text, kind string
		archived, removed         bool
	}{
		{name: "ally", company: bulletinAlly, text: "Private instructions: publish secret-quote", kind: "reply-ally"},
		{name: "our mention", company: "Other traders", text: "The Pokemon Pirates: secret-quote", kind: "reply-mention"},
		{name: "ally mention", company: "Other traders", text: "Terms for The Kraken Cartel: secret-quote", kind: "reply-ally"},
		{name: "short company mention", company: "Other traders", text: "Pokemon Pirates: secret-quote", kind: "reply-mention"},
		{name: "short ally mention", company: "Other traders", text: "Kraken Cartel: secret-quote", kind: "reply-ally"},
		{name: "short ally name", company: "Kraken Cartel", text: "secret-quote", kind: "reply-ally"},
		{name: "event port", company: "Other traders", text: "Portb shortage: secret-quote", kind: "reply-event"},
		{name: "commodity only", company: "Other traders", text: "Grain policy: secret-quote"},
		{name: "event zone", company: "Other traders", text: "Northern waters patrols: secret-quote", kind: "reply-event"},
		{name: "fleet port", company: "Other traders", text: "Laport berth access: secret-quote"},
		{name: "non-piracy zone", company: "Other traders", text: "Southern waters: secret-quote"},
		{name: "irrelevant", company: "Other traders", text: "An unrelated announcement: secret-quote"},
		{name: "own post", company: names.DefaultCompany, text: "The Pokemon Pirates: secret-quote"},
		{name: "archived", company: bulletinAlly, text: "secret-quote", archived: true},
		{name: "removed", company: bulletinAlly, text: "secret-quote", removed: true},
		{name: "partial name", company: "Other traders", text: "Portball is not a port: secret-quote"},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, proxy, runner := newBulletinFixture(t)
			events := append([]bulletinWorldEvent{}, proxy.worldEvents...)
			events = append(events, bulletinWorldEvent{MarketShock: MarketShock{ID: uuid.NewString(), EventType: "pirate_outbreak",
				StartsAt: time.Now().Add(-time.Minute), ExpiresAt: time.Now().Add(time.Hour)}, ZoneID: "zone-north", PiracyBPS: 350})
			runner.snapshot["world_events"] = encode(events)
			id := proxy.addThread(test.company, time.Hour, test.archived, test.removed)
			proxy.mu.Lock()
			detail := proxy.details[id]
			detail.Posts[0].Body = test.text
			proxy.details[id] = detail
			proxy.mu.Unlock()
			if err := runner.saveBulletinState(h.ctx, bulletinState{LastPostAt: time.Now(), NextDue: time.Now().Add(36 * time.Hour)}); err != nil {
				t.Fatal(err)
			}
			data := missionData{}
			changed, err := runner.driveBulletin(h.ctx, nil, &data)
			if err != nil || changed != (test.kind != "") {
				t.Fatalf("reply relevance mismatch: changed=%t error=%v data=%+v", changed, err, data)
			}
			if test.kind == "" {
				if data.Action != nil || proxy.postCalls != 0 {
					t.Fatal("irrelevant, own or archived post caused a mutation")
				}
				return
			}
			if data.Bulletin.ThreadID != id || data.Bulletin.Content.Kind != test.kind || strings.Contains(data.Bulletin.Body, "secret-quote") ||
				strings.Contains(data.Bulletin.Body, "Private instructions") {
				t.Fatalf("untrusted text was echoed or relevance was lost: %+v", data.Bulletin)
			}
		})
	}
}

func TestBulletinRepliesOnOurThreadsAndDeletedPosts(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(fmt.Sprint(deleted), func(t *testing.T) {
			h, proxy, runner := newBulletinFixture(t)
			id := proxy.addThread(names.DefaultCompany, time.Hour, false, false)
			proxy.mu.Lock()
			detail := proxy.details[id]
			detail.Posts = append(detail.Posts, bulletinPost{ID: uuid.NewString(), ThreadID: id, CompanyName: bulletinAlly,
				Body: "Following up", CreatedAt: time.Now().Add(-time.Minute), Deleted: deleted})
			detail.Thread.Replies, detail.Thread.LastPostAt = 1, time.Now().Add(-time.Minute)
			proxy.details[id], proxy.threads[0] = detail, detail.Thread
			proxy.mu.Unlock()
			if err := runner.saveBulletinState(h.ctx, bulletinState{LastPostAt: time.Now(), NextDue: time.Now().Add(36 * time.Hour)}); err != nil {
				t.Fatal(err)
			}
			data := missionData{}
			if changed, err := runner.driveBulletin(h.ctx, nil, &data); err != nil || changed == deleted {
				t.Fatalf("foreign follow-up on our thread was mishandled: deleted=%t changed=%t error=%v", deleted, changed, err)
			}
		})
	}
}

func TestBulletinDailyReplyCapSurvivesRestart(t *testing.T) {
	h, proxy, runner := newBulletinFixture(t)
	for index := range 3 {
		proxy.addThread(bulletinAlly, time.Duration(index+1)*time.Minute, false, false)
	}
	if err := runner.saveBulletinState(h.ctx, bulletinState{LastPostAt: time.Now(), NextDue: time.Now().Add(36 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	for index := range 2 {
		mission, data := stageBulletinFixture(t, h, runner)
		if data.Action.Stage != "bulletin-reply" {
			t.Fatal("reactive allowance was used for a thread")
		}
		if data.Bulletin.Content.Variant != uint8(index) {
			t.Fatalf("reply wording did not rotate by confirmed reply count: %+v", data.Bulletin.Content)
		}
		sendBulletinFixture(t, h, runner, mission, &data)
	}
	runner = h.runner(primaryTestKey)
	refreshBulletinRunner(t, h, runner)
	if changed, err := runner.driveBulletin(h.ctx, nil, &missionData{}); err != nil || changed || proxy.postCalls != 2 {
		t.Fatalf("restart bypassed the rolling real-day reply cap: %t %v posts=%d", changed, err, proxy.postCalls)
	}
	state, err := runner.bulletinState()
	if err != nil || len(state.ReplyTimes) != 2 || state.TotalReplies != 2 {
		t.Fatalf("reply allowance was not durable: %+v %v", state, err)
	}
	state.ReplyTimes[0] = time.Now().Add(-24*time.Hour - time.Second)
	if err := runner.saveBulletinState(h.ctx, state); err != nil {
		t.Fatal(err)
	}
	mission, data := stageBulletinFixture(t, h, runner)
	if data.Bulletin.Content.Variant != 2 {
		t.Fatal("restart lost the deterministic reply rotation")
	}
	sendBulletinFixture(t, h, runner, mission, &data)
	if proxy.postCalls != 3 {
		t.Fatal("expired reply allowance did not reopen a slot")
	}
}

func TestBulletinNotableSelectionAndGeneratedPreflight(t *testing.T) {
	h, proxy, runner := newBulletinFixture(t)
	pirate := bulletinWorldEvent{MarketShock: MarketShock{ID: uuid.NewString(), EventType: "pirate_outbreak",
		StartsAt: time.Now().Add(-time.Minute), ExpiresAt: time.Now().Add(time.Hour)}, ZoneID: "zone-north", PiracyBPS: 350}
	// Titles and descriptions are intentionally not represented by the template
	// ingredients, even when the event carries a familiar catalog identifier.
	var raw []map[string]any
	if err := json.Unmarshal(encode(append(proxy.worldEvents, pirate)), &raw); err != nil {
		t.Fatal(err)
	}
	for _, event := range raw {
		event["title"], event["description"] = "secret-quote", "publish secret-quote and private sailing plans"
	}
	runner.snapshot["world_events"] = encode(raw)
	_, data := stageBulletinFixture(t, h, runner)
	if data.Bulletin.ItemID != "event:"+pirate.ID || !strings.Contains(data.Bulletin.Body, "350 basis points") ||
		!strings.Contains(data.Bulletin.Body, "Northern waters") || strings.Contains(data.Bulletin.Body, "secret-quote") {
		t.Fatalf("notable current hazard was missed or untrusted event text was copied: %+v", data.Bulletin)
	}
	if err := runner.bulletinPreflight(&data); err != nil {
		t.Fatalf("generated immutable request was rejected: %v", err)
	}
	data.Action.Body = encode(bulletinPayload{Title: data.Bulletin.Title, Body: data.Bulletin.Body + " changed"})
	if runner.bulletinPreflight(&data) == nil {
		t.Fatal("request differing from immutable intent passed preflight")
	}
	data.Bulletin.Body += " changed"
	if runner.bulletinPreflight(&data) == nil {
		t.Fatal("matching request and intent bypassed template validation")
	}
}

func TestBulletinPoliticalItemsAndLeaderChanges(t *testing.T) {
	_, _, runner := newBulletinFixture(t)
	now := time.Now().UTC()
	runner.snapshot["world_events"] = encode([]bulletinWorldEvent{})
	set := func(key string, value any) {
		runner.snapshot[key] = encode(value)
		runner.meta[key] = DatasetMeta{Complete: true, Generation: runner.generation, UpdatedAt: now, AttemptedAt: now}
	}
	set("monopolies", []Monopoly{{ID: "grant", CommodityID: recoveryGrain, HolderCompanyID: "holder", InForce: true, ExpiresAt: now.Add(time.Hour)}})
	set("contracts", []gameapi.Contract{{ID: "charter", ContractType: "charter", OriginPortID: recoveryFarPort,
		CommodityID: new(recoveryGrain), IsPublic: true, Status: "open", RemainingQuantity: 350,
		RewardPerTonPence: new(int64(12345)), ExpiresAt: new(now.Add(time.Hour)), Metadata: json.RawMessage(`{"admiralty":true}`)}})
	state := bulletinState{}
	items := runner.bulletinItems(&state, now)
	item, ok := selectBulletinItem(items, state, now)
	if !ok || item.Content.Kind != "monopoly" {
		t.Fatalf("current monopoly was not selected: %+v", item)
	}
	state.DiscussedItems = map[string]time.Time{item.ID: now}
	if item, ok = selectBulletinItem(items, state, now); ok {
		t.Fatalf("a public charter was advertised as a topic: %+v", item)
	}
	set("monopolies", []Monopoly{})
	set("contracts", []gameapi.Contract{})
	board := gameapi.Leaderboard{SeasonNumber: 1, RankedPlayers: 10, RefreshedAt: &now,
		Entries: []gameapi.LeaderboardEntry{{Rank: 1, CompanyName: "untrusted-leader"}}}
	set("leaderboard", board)
	if items := runner.bulletinItems(&state, now); len(items) != 0 {
		t.Fatal("first leaderboard observation invented a change of leader")
	}
	board.Entries[0].CompanyName = bulletinAlly
	set("leaderboard", board)
	items = runner.bulletinItems(&state, now)
	item, ok = selectBulletinItem(items, state, now)
	if !ok || item.Content.Kind != "leader" || !item.Content.Ally {
		t.Fatalf("observed change to our ally was missed: %+v", items)
	}
	_, body, err := runner.renderBulletin(item.Content)
	if err != nil || strings.Contains(body, "untrusted-leader") || !strings.Contains(body, bulletinAlly) {
		t.Fatalf("leaderboard template echoed arbitrary company text: %q %v", body, err)
	}
	if next, ok := selectBulletinItem(runner.bulletinItems(&state, now), state, now); !ok || next.ID != item.ID {
		t.Fatal("an undiscussed leader change vanished on the next observation")
	}
	state.DiscussedItems = map[string]time.Time{item.ID: now}
	if err := json.Unmarshal(encode(state), &state); err != nil {
		t.Fatal(err)
	}
	board.Entries[0].CompanyName = "untrusted-leader"
	later := now.Add(time.Minute)
	board.RefreshedAt = &later
	set("leaderboard", board)
	returning, ok := selectBulletinItem(runner.bulletinItems(&state, later), state, later)
	if !ok || returning.ID == item.ID || returning.Content.Ally {
		t.Fatalf("a return to the original leader was not a distinct observed change: %+v", returning)
	}
}

func TestBulletinActiveBoardDebateCanBecomeAThread(t *testing.T) {
	h, proxy, runner := newBulletinFixture(t)
	runner.snapshot["world_events"] = encode([]bulletinWorldEvent{})
	id := proxy.addThread(bulletinAlly, 2*time.Hour, false, false)
	proxy.mu.Lock()
	detail := proxy.details[id]
	detail.Posts = append(detail.Posts,
		bulletinPost{ID: uuid.NewString(), ThreadID: id, CompanyName: "Other traders", Body: "The Kraken Cartel: secret-quote", CreatedAt: time.Now().Add(-time.Hour)})
	detail.Thread.LastPostAt, detail.Thread.Replies = time.Now().Add(-time.Hour), 1
	proxy.details[id], proxy.threads[0] = detail, detail.Thread
	proxy.mu.Unlock()
	_, data := stageBulletinFixture(t, h, runner)
	if data.Action.Stage != "bulletin-thread" || data.Bulletin.ItemID != "debate:"+id || strings.Contains(data.Bulletin.Body, "secret-quote") {
		t.Fatalf("current relevant debate was missed or quoted: %+v", data.Bulletin)
	}
}

func TestBulletinExpiredFutureAndStaleItemsDoNotPost(t *testing.T) {
	for _, mode := range []string{"expired", "future", "stale", "unknown event", "unknown catalog"} {
		t.Run(mode, func(t *testing.T) {
			h, proxy, runner := newBulletinFixture(t)
			event := proxy.worldEvents[0]
			switch mode {
			case "expired":
				event.ExpiresAt = time.Now().Add(-time.Minute)
			case "future":
				event.StartsAt = time.Now().Add(time.Minute)
			case "stale":
				meta := runner.meta["world_events"]
				meta.Stale = true
				runner.meta["world_events"] = meta
			case "unknown event":
				event.EventType = "secret-quote"
			case "unknown catalog":
				event.PortID = "secret-quote"
			}
			runner.snapshot["world_events"] = encode([]bulletinWorldEvent{event})
			if changed, err := runner.driveBulletin(h.ctx, nil, &missionData{}); err != nil || changed || proxy.postCalls != 0 {
				t.Fatalf("unsupported or non-current event produced a post: %t %v", changed, err)
			}
		})
	}
}

func TestBulletinFleetPositionRouteAndCargoNeverCreateRelevance(t *testing.T) {
	for _, subject := range []string{"Laport", "Portb", "Northern waters", "Southern waters", "Grain"} {
		t.Run(subject, func(t *testing.T) {
			h, proxy, runner := newBulletinFixture(t)
			runner.snapshot["world_events"] = encode([]bulletinWorldEvent{})
			ship := ShipState{RouteLegs: []RouteLeg{{FromNode: recoveryHomePort, ToNode: recoveryFarPort}},
				CargoManifest: map[string]int64{recoveryGrain: 40}}
			ship.Ship.CurrentPortID = recoveryHomePort
			runner.stateCache.Ships = []ShipState{ship}
			id := proxy.addThread("Other traders", time.Hour, false, false)
			proxy.mu.Lock()
			detail := proxy.details[id]
			detail.Posts[0].Body = "Public conditions for " + subject
			proxy.details[id] = detail
			proxy.mu.Unlock()
			if err := runner.saveBulletinState(h.ctx, bulletinState{LastPostAt: time.Now(), NextDue: time.Now().Add(36 * time.Hour)}); err != nil {
				t.Fatal(err)
			}
			if changed, err := runner.driveBulletin(h.ctx, nil, &missionData{}); err != nil || changed || proxy.postCalls != 0 {
				t.Fatalf("a private fleet subject triggered a public reply: subject=%s changed=%t error=%v", subject, changed, err)
			}
		})
	}
}

func TestBulletinLeaderChangesExpireAndResetWithSeason(t *testing.T) {
	_, _, runner := newBulletinFixture(t)
	now := time.Now().UTC()
	runner.snapshot["world_events"] = encode([]bulletinWorldEvent{})
	board := gameapi.Leaderboard{SeasonNumber: 1, RankedPlayers: 10, RefreshedAt: &now,
		Entries: []gameapi.LeaderboardEntry{{Rank: 1, CompanyName: "First leader"}}}
	state := bulletinState{}
	runner.snapshot["leaderboard"] = encode(board)
	runner.bulletinItems(&state, now)
	board.Entries[0].CompanyName = bulletinAlly
	runner.snapshot["leaderboard"] = encode(board)
	item, ok := selectBulletinItem(runner.bulletinItems(&state, now), state, now)
	if !ok || item.Content.Kind != "leader" {
		t.Fatal("leader transition was not recorded")
	}
	later := now.Add(25 * time.Hour)
	board.RefreshedAt = &later
	runner.snapshot["leaderboard"] = encode(board)
	if _, ok := selectBulletinItem(runner.bulletinItems(&state, later), state, later); ok {
		t.Fatal("hourly refresh renewed an old leader-change story indefinitely")
	}
	board.SeasonNumber++
	runner.snapshot["leaderboard"] = encode(board)
	if items := runner.bulletinItems(&state, later); len(items) != 0 || state.LeaderChange != nil {
		t.Fatal("new season with the same leader invented a leadership change or retained the old story")
	}
}

func TestBulletinAllianceMonopoliesAreNotCriticized(t *testing.T) {
	_, _, runner := newBulletinFixture(t)
	runner.snapshot["world_events"] = encode([]bulletinWorldEvent{})
	now := time.Now().UTC()
	runner.meta["monopolies"] = DatasetMeta{Complete: true, Generation: runner.generation, UpdatedAt: now, AttemptedAt: now}
	runner.snapshot["monopolies"] = encode([]bulletinMonopoly{
		{Monopoly: Monopoly{ID: "own-grant", HolderCompanyID: runner.account.CompanyID, CommodityID: recoveryGrain, InForce: true, ExpiresAt: now.Add(time.Hour)}},
		{Monopoly: Monopoly{ID: "ally-id", HolderCompanyID: krakenCompanyID, CommodityID: recoveryGrain, InForce: true, ExpiresAt: now.Add(time.Hour)}},
		{Monopoly: Monopoly{ID: "ally-name", HolderCompanyID: "another-id", CommodityID: recoveryGrain, InForce: true, ExpiresAt: now.Add(time.Hour)}, HolderCompanyName: bulletinAlly},
	})
	if items := runner.bulletinItems(&bulletinState{}, now); len(items) != 0 {
		t.Fatalf("alliance monopoly was selected for criticism: %+v", items)
	}
}

func TestBulletinForeignReplyDoesNotInheritOurThreadTitle(t *testing.T) {
	h, proxy, runner := newBulletinFixture(t)
	id := proxy.addThread(names.DefaultCompany, time.Hour, false, false)
	proxy.mu.Lock()
	detail := proxy.details[id]
	detail.Thread.Title, detail.Thread.Replies = "Trade watch at Portb", 1
	detail.Posts = append(detail.Posts, bulletinPost{ID: uuid.NewString(), ThreadID: id, CompanyName: "Other traders", Body: "Thanks",
		CreatedAt: time.Now().Add(-time.Minute)})
	detail.Thread.LastPostAt = time.Now().Add(-time.Minute)
	proxy.details[id], proxy.threads[0] = detail, detail.Thread
	proxy.mu.Unlock()
	if err := runner.saveBulletinState(h.ctx, bulletinState{LastPostAt: time.Now(), NextDue: time.Now().Add(36 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if changed, err := runner.driveBulletin(h.ctx, nil, &missionData{}); err != nil || changed || proxy.postCalls != 0 {
		t.Fatalf("an irrelevant follow-up inherited our opening title: %t %v", changed, err)
	}
}

func TestBulletinReadBudgetAndOpeningPriority(t *testing.T) {
	for _, mode := range []string{"opening head", "reply detail", "due opening avoids detail"} {
		t.Run(mode, func(t *testing.T) {
			h, proxy, runner := newBulletinFixture(t)
			id := proxy.addThread("Other traders", time.Hour, false, false)
			if mode == "reply detail" {
				runner.snapshot["world_events"] = encode([]bulletinWorldEvent{})
				if err := runner.saveBulletinState(h.ctx, bulletinState{LastPostAt: time.Now(), NextDue: time.Now().Add(36 * time.Hour)}); err != nil {
					t.Fatal(err)
				}
			}
			stalledReads := 0
			h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				stall := mode == "opening head" && request.URL.Path == "/v1/bulletin" ||
					mode != "opening head" && request.URL.Path == "/v1/bulletin/thread"
				if request.Method == http.MethodGet && stall {
					stalledReads++
					<-request.Context().Done()
					return
				}
				proxy.ServeHTTP(writer, request)
			})
			data := missionData{}
			changed, err := runner.driveBulletin(h.ctx, nil, &data)
			state, stateErr := runner.bulletinState()
			if err != nil || stateErr != nil || !state.CooldownUntil.IsZero() {
				t.Fatalf("read budget became a company posting failure: changed=%t state=%+v errors=%v,%v", changed, state, err, stateErr)
			}
			if mode == "due opening avoids detail" {
				if !changed || data.Action == nil || data.Action.Stage != "bulletin-thread" || !slices.Contains(data.Bulletin.BeforeIDs, id) {
					t.Fatal("due opening did not precede reply reads and capture page-one evidence")
				}
				return
			}
			if changed || data.Action != nil || !state.ReplyCheckAfter.After(time.Now()) {
				t.Fatalf("exhausted optional read staged a post or lost its read-only backoff: %+v", state)
			}
			if mode == "reply detail" && state.CheckedThreads[id].ObservedAt.IsZero() {
				t.Fatal("failed thread detail was not recorded as checked")
			}
			if mode == "opening head" {
				if !state.OpeningCheckAfter.After(time.Now()) || stalledReads != 1 {
					t.Fatalf("opening head timeout lost its separate retry deadline: %+v reads=%d", state, stalledReads)
				}
				restarted := h.runner(primaryTestKey)
				refreshBulletinRunner(t, h, restarted)
				if changed, err := restarted.driveBulletin(h.ctx, nil, &missionData{}); err != nil || changed || stalledReads != 1 {
					t.Fatalf("next pass ignored the persisted opening read backoff: changed=%t error=%v reads=%d", changed, err, stalledReads)
				}
			}
		})
	}
}

func TestBulletinChartersNeverBecomeTopics(t *testing.T) {
	_, _, runner := newBulletinFixture(t)
	now := time.Now().UTC()
	runner.snapshot["world_events"] = encode([]bulletinWorldEvent{})
	runner.snapshot["contracts"] = encode([]gameapi.Contract{{ID: "public-charter", ContractType: "charter", OriginPortID: recoveryFarPort,
		CommodityID: new(recoveryGrain), IsPublic: true, Status: "open", RemainingQuantity: 350, ExpiresAt: new(now.Add(2 * time.Hour))}})
	runner.meta["contracts"] = DatasetMeta{Complete: true, Generation: runner.generation, UpdatedAt: now, AttemptedAt: now}
	state := bulletinState{}
	if items := runner.bulletinItems(&state, now); len(items) != 0 {
		t.Fatalf("public procurement opportunities were advertised: %+v", items)
	}
	if _, _, err := runner.renderBulletin(bulletinContent{Kind: "charter", PortID: recoveryFarPort, CommodityID: recoveryGrain}); err == nil {
		t.Fatal("retired charter template was still accepted")
	}
}

func TestBulletinPiracyTemplateRequiresAnObservedPositiveMagnitude(t *testing.T) {
	_, _, runner := newBulletinFixture(t)
	if _, _, err := runner.renderBulletin(bulletinContent{Kind: "event", Event: "pirate_outbreak", ZoneID: "zone-north"}); err == nil {
		t.Fatal("missing or null piracy magnitude was published as a measured zero")
	}
}

func TestBulletinFailedStateSaveDoesNotSuppressDurableRetry(t *testing.T) {
	h, _, runner := newBulletinFixture(t)
	before := string(runner.snapshot["_bulletin"])
	state := bulletinState{DiscussedItems: map[string]time.Time{"event:confirmed": time.Now().UTC()}}
	canceled, cancel := context.WithCancel(h.ctx)
	cancel()
	if err := runner.saveBulletinState(canceled, state); err == nil {
		t.Fatal("canceled state save unexpectedly succeeded")
	}
	if string(runner.snapshot["_bulletin"]) != before {
		t.Fatal("failed persistence left a false unchanged-state fingerprint")
	}
	if err := runner.saveBulletinState(h.ctx, state); err != nil {
		t.Fatal(err)
	}
	snapshot, err := h.db.Snapshot(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	var restored bulletinState
	if err := json.Unmarshal(snapshot["_bulletin"], &restored); err != nil || restored.DiscussedItems["event:confirmed"].IsZero() {
		t.Fatalf("retry did not durably save the confirmed discussion: %+v %v", restored, err)
	}
}

func TestBulletinCatalogLabelsRejectPublicMarkup(t *testing.T) {
	for _, field := range []string{"port", "zone", "commodity"} {
		t.Run(field, func(t *testing.T) {
			h, proxy, runner := newBulletinFixture(t)
			catalog := decode[gameapi.Catalog](runner.snapshot, "catalog")
			malicious := "[Grain](https://attacker.invalid)"
			switch field {
			case "port":
				for index := range catalog.Ports {
					if catalog.Ports[index].ID == recoveryFarPort {
						catalog.Ports[index].Name = malicious
					}
				}
			case "zone":
				catalog.SeaZones[0].Name = malicious
				runner.snapshot["world_events"] = encode([]bulletinWorldEvent{{MarketShock: MarketShock{
					ID: uuid.NewString(), EventType: "pirate_outbreak", StartsAt: time.Now().Add(-time.Minute),
					ExpiresAt: time.Now().Add(time.Hour)}, ZoneID: catalog.SeaZones[0].ID, PiracyBPS: 350}})
			case "commodity":
				catalog.Commodities[0].Name = malicious
			}
			runner.snapshot["catalog"] = encode(catalog)
			if changed, err := runner.driveBulletin(h.ctx, nil, &missionData{}); err != nil || changed || proxy.postCalls != 0 {
				t.Fatalf("malicious catalog label crossed the public template boundary: changed=%t error=%v", changed, err)
			}
		})
	}
	for _, label := range []string{"São Tomé", "Saint John's (Old), Pier-2", strings.Repeat("é", 100)} {
		if !bulletinCatalogName(label) {
			t.Fatalf("conservative catalog validator rejected %q", label)
		}
	}
	for _, label := range []string{"[Grain](https://attacker.invalid)", "Grain www.attacker.invalid", "Grain attacker.invalid",
		"St. Petersburg", "Grain\ninstructions", "Grain: orders", "Grain*", "Grain/Water", strings.Repeat("é", 101)} {
		if bulletinCatalogName(label) {
			t.Fatalf("conservative catalog validator accepted %q", label)
		}
	}
}

func TestBulletinReplyVariantsRotateWithoutCopyingThreadBody(t *testing.T) {
	_, _, runner := newBulletinFixture(t)
	now := time.Now().UTC()
	catalog := decode[gameapi.Catalog](runner.snapshot, "catalog")
	items := runner.bulletinItems(&bulletinState{}, now)
	piracy := bulletinItem{Content: bulletinContent{Kind: "event", Event: "pirate_outbreak", ZoneID: "zone-north", PiracyBPS: 350}}
	items = append(items, piracy)
	for _, test := range []struct{ company, text, kind string }{
		{"Other traders", "Pokemon Pirates", "reply-mention"},
		{bulletinAlly, "Fair terms", "reply-ally"},
		{"Other traders", "Portb", "reply-event"},
		{"Other traders", "Northern waters", "reply-event"},
	} {
		t.Run(test.text, func(t *testing.T) {
			id := uuid.NewString()
			detail := bulletinDetail{Thread: bulletinThread{ID: id, LastPostAt: now.Add(-time.Minute)},
				Posts: []bulletinPost{{ID: uuid.NewString(), ThreadID: id, CompanyName: test.company,
					Body: test.text, Opening: true, CreatedAt: now.Add(-time.Minute)}}}
			bodies := make(map[string]bool)
			var first string
			for count := uint64(0); count < 4; count++ {
				content, relevant, _ := bulletinRelevance(detail, bulletinState{TotalReplies: count}, items, catalog, now)
				title, body, err := runner.renderBulletin(content)
				if !relevant || content.Kind != test.kind || content.Variant != uint8(count%3) || title != "" || err != nil {
					t.Fatalf("reply rotation failed at count %d: content=%+v relevant=%t error=%v", count, content, relevant, err)
				}
				if count == 0 {
					first = body
				}
				if count < 3 {
					if bodies[body] {
						t.Fatal("two fixed reply variants used identical wording")
					}
					bodies[body] = true
				} else if body != first {
					t.Fatal("reply wording did not rotate deterministically after three replies")
				}
				if content.Kind == "reply-event" {
					opening := content
					opening.Kind = "event"
					_, openingBody, err := runner.renderBulletin(opening)
					if err != nil || strings.Contains(body, openingBody) {
						t.Fatal("event reply repeated its opening-thread body")
					}
				}
			}
		})
	}
}

func TestBulletinOnlyRecentForeignPostsTriggerReplies(t *testing.T) {
	for _, age := range []time.Duration{11*time.Hour + 59*time.Minute, 12*time.Hour + time.Minute, 48 * time.Hour} {
		t.Run(age.String(), func(t *testing.T) {
			h, proxy, runner := newBulletinFixture(t)
			proxy.addThread(bulletinAlly, age, false, false)
			if err := runner.saveBulletinState(h.ctx, bulletinState{LastPostAt: time.Now(), NextDue: time.Now().Add(36 * time.Hour)}); err != nil {
				t.Fatal(err)
			}
			changed, err := runner.driveBulletin(h.ctx, nil, &missionData{})
			if err != nil || changed != (age < 12*time.Hour) {
				t.Fatalf("foreign-post age was not enforced: age=%s changed=%t error=%v", age, changed, err)
			}
		})
	}
}

func TestBulletinSubjectCooldownSurvivesRestart(t *testing.T) {
	h, proxy, runner := newBulletinFixture(t)
	mission, data := stageBulletinFixture(t, h, runner)
	subject := data.Bulletin.Content.subjectKey()
	sendBulletinFixture(t, h, runner, mission, &data)
	event := proxy.worldEvents[0]
	event.ID = uuid.NewString()
	proxy.worldEvents = []bulletinWorldEvent{event}
	runner, _, _ = resumeBulletinFixture(t, h)
	state := makeBulletinDue(t, h, runner)
	if state.DiscussedSubjects[subject].IsZero() {
		t.Fatal("confirmed subject lost its durable discussion timestamp")
	}
	if changed, err := runner.driveBulletin(h.ctx, nil, &missionData{}); err != nil || changed {
		t.Fatalf("renewed event ID bypassed the seven-day subject cooldown: %t %v", changed, err)
	}
	state, _ = runner.bulletinState()
	state.DiscussedSubjects[subject] = time.Now().Add(-7*24*time.Hour - time.Second)
	if err := runner.saveBulletinState(h.ctx, state); err != nil {
		t.Fatal(err)
	}
	_, data = stageBulletinFixture(t, h, runner)
	if data.Bulletin.ItemID != "event:"+event.ID {
		t.Fatal("subject did not reopen after seven real days")
	}
}

func TestBulletinDebateRequiresDistinctForeignCompaniesAndNoOwnPost(t *testing.T) {
	_, _, runner := newBulletinFixture(t)
	now := time.Now().UTC()
	catalog := decode[gameapi.Catalog](runner.snapshot, "catalog")
	for _, test := range []struct {
		name, secondCompany string
		ownPost, wantDebate bool
	}{
		{"one company twice", bulletinAlly, false, false},
		{"distinct foreign companies", "Other traders", false, true},
		{"already participated", "Other traders", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			id := uuid.NewString()
			detail := bulletinDetail{Thread: bulletinThread{ID: id, LastPostAt: now.Add(-time.Minute)},
				Posts: []bulletinPost{
					{ID: uuid.NewString(), ThreadID: id, CompanyName: bulletinAlly, Body: "Fair terms", Opening: true, CreatedAt: now.Add(-time.Hour)},
					{ID: uuid.NewString(), ThreadID: id, CompanyName: test.secondCompany, Body: "Kraken Cartel", CreatedAt: now.Add(-time.Minute)},
				}}
			if test.ownPost {
				detail.Posts = append(detail.Posts, bulletinPost{ID: uuid.NewString(), ThreadID: id, CompanyName: names.DefaultCompany, CreatedAt: now.Add(-2 * time.Hour)})
			}
			_, _, debate := bulletinRelevance(detail, bulletinState{}, nil, catalog, now)
			if (debate != nil) != test.wantDebate {
				t.Fatalf("debate qualification mismatch: %+v", debate)
			}
		})
	}
}

func TestBulletinLeaderboardTiesHaveOneStableIdentity(t *testing.T) {
	_, _, runner := newBulletinFixture(t)
	now := time.Now().UTC()
	runner.snapshot["world_events"] = encode([]bulletinWorldEvent{})
	board := gameapi.Leaderboard{SeasonNumber: 1, RankedPlayers: 10, RefreshedAt: &now,
		Entries: []gameapi.LeaderboardEntry{{Rank: 1, CompanyName: "Zulu"}, {Rank: 1, CompanyName: "Alpha"}}}
	runner.snapshot["leaderboard"] = encode(board)
	state := bulletinState{}
	if items := runner.bulletinItems(&state, now); len(items) != 0 {
		t.Fatal("initial tied leaderboard invented a leadership change")
	}
	baseline := state.LeaderBaseline
	slices.Reverse(board.Entries)
	later := now.Add(time.Minute)
	board.RefreshedAt = &later
	runner.snapshot["leaderboard"] = encode(board)
	if items := runner.bulletinItems(&state, later); len(items) != 0 || state.LeaderBaseline != baseline {
		t.Fatal("rank-one entry order changed the leader identity")
	}
	board.Entries = []gameapi.LeaderboardEntry{{Rank: 1, CompanyName: "Zulu"}}
	runner.snapshot["leaderboard"] = encode(board)
	items := runner.bulletinItems(&state, later)
	if len(items) != 1 || items[0].Content.Kind != "leader" || state.LeaderBaseline == baseline {
		t.Fatal("loss of the selected tied leader was not recorded as one change")
	}
}

func TestBulletinFailedThreadDetailsSkipWithoutLosingScanProgress(t *testing.T) {
	for _, mode := range []string{"removed", "server error", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			h, proxy, runner := newBulletinFixture(t)
			bad := proxy.addThread("Other traders", time.Minute, false, false)
			good := proxy.addThread(bulletinAlly, 2*time.Minute, false, false)
			attempts := 0
			h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodGet && request.URL.Path == "/v1/bulletin/thread" && request.URL.Query().Get("id") == bad {
					attempts++
					switch mode {
					case "removed":
						h.api.writeProblem(writer, http.StatusNotFound, "not_found", "thread removed")
					case "server error":
						h.api.writeProblem(writer, http.StatusInternalServerError, "unavailable", "thread unavailable")
					case "malformed":
						h.api.writeJSON(writer, http.StatusOK, map[string]any{"posts": []any{}})
					}
					return
				}
				proxy.ServeHTTP(writer, request)
			})
			if err := runner.saveBulletinState(h.ctx, bulletinState{LastPostAt: time.Now(), NextDue: time.Now().Add(36 * time.Hour)}); err != nil {
				t.Fatal(err)
			}
			mission, data := stageBulletinFixture(t, h, runner)
			if data.Bulletin.ThreadID != good {
				t.Fatal("failed thread detail aborted the pass before the next candidate")
			}
			sendBulletinFixture(t, h, runner, mission, &data)
			state, err := runner.bulletinState()
			if err != nil || state.CheckedThreads[bad].ObservedAt.IsZero() || !state.CooldownUntil.IsZero() {
				t.Fatalf("failed detail was not durably skipped without a posting cooldown: %+v %v", state, err)
			}
			state.ReplyCheckAfter, state.ReplyScan = time.Time{}, nil
			if err := runner.saveBulletinState(h.ctx, state); err != nil {
				t.Fatal(err)
			}
			runner = h.runner(primaryTestKey)
			refreshBulletinRunner(t, h, runner)
			if changed, err := runner.driveBulletin(h.ctx, nil, &missionData{}); err != nil || changed || attempts != 1 {
				t.Fatalf("restart immediately retried an unchanged failed thread: changed=%t error=%v attempts=%d", changed, err, attempts)
			}
		})
	}
}

func TestBulletinDetailReadsAreCappedAndOnlyChangedThreadsAreReadAgain(t *testing.T) {
	h, proxy, runner := newBulletinFixture(t)
	var ids []string
	for index := range 7 {
		ids = append(ids, proxy.addThread("Other traders", time.Duration(index+1)*time.Minute, false, false))
	}
	if err := runner.saveBulletinState(h.ctx, bulletinState{LastPostAt: time.Now(), NextDue: time.Now().Add(36 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	for pass := range 3 {
		if pass > 0 {
			runner, _, _ = resumeBulletinFixture(t, h)
		}
		before := 0
		for _, reads := range proxy.detailReads {
			before += reads
		}
		if changed, err := runner.driveBulletin(h.ctx, nil, &missionData{}); err != nil || changed {
			t.Fatalf("irrelevant detail scan staged a post: changed=%t error=%v", changed, err)
		}
		after := 0
		for _, reads := range proxy.detailReads {
			after += reads
		}
		if after-before > 3 || after-before == 0 {
			t.Fatalf("detail read cap or scan progress failed: pass=%d reads=%d", pass, after-before)
		}
	}
	for _, id := range ids {
		if proxy.detailReads[id] != 1 {
			t.Fatalf("candidate was skipped or read repeatedly: %s reads=%d", id, proxy.detailReads[id])
		}
	}
	state, _ := runner.bulletinState()
	state.ReplyCheckAfter = time.Time{}
	if err := runner.saveBulletinState(h.ctx, state); err != nil {
		t.Fatal(err)
	}
	if changed, err := runner.driveBulletin(h.ctx, nil, &missionData{}); err != nil || changed {
		t.Fatalf("unchanged scan staged a post: %t %v", changed, err)
	}
	for _, id := range ids {
		if proxy.detailReads[id] != 1 {
			t.Fatal("unchanged LastPostAt triggered another detail read")
		}
	}
	proxy.mu.Lock()
	detail := proxy.details[ids[0]]
	detail.Thread.LastPostAt = time.Now().Add(-time.Second)
	proxy.details[ids[0]] = detail
	for index := range proxy.threads {
		if proxy.threads[index].ID == ids[0] {
			proxy.threads[index] = detail.Thread
		}
	}
	proxy.mu.Unlock()
	state, _ = runner.bulletinState()
	state.ReplyCheckAfter = time.Time{}
	if err := runner.saveBulletinState(h.ctx, state); err != nil {
		t.Fatal(err)
	}
	if changed, err := runner.driveBulletin(h.ctx, nil, &missionData{}); err != nil || changed || proxy.detailReads[ids[0]] != 2 {
		t.Fatalf("changed LastPostAt did not refresh exactly once: changed=%t error=%v reads=%v", changed, err, proxy.detailReads)
	}
}

func TestBulletinEventReplyUsesOnlyTheMatchedEventLocation(t *testing.T) {
	_, _, runner := newBulletinFixture(t)
	now := time.Now().UTC()
	runner.snapshot["world_events"] = encode([]bulletinWorldEvent{
		{MarketShock: MarketShock{ID: "unmentioned", EventType: "demand_shortage", PortID: recoveryHomePort, CommodityID: recoveryGrain, StartsAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)}},
		{MarketShock: MarketShock{ID: "mentioned", EventType: "supply_boom", PortID: recoveryFarPort, CommodityID: recoveryGrain, StartsAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)}},
	})
	catalog := decode[gameapi.Catalog](runner.snapshot, "catalog")
	items := runner.bulletinItems(&bulletinState{}, now)
	content, relevant := bulletinPostRelevance("Portb public policy for Grain", "Other traders", items, catalog)
	_, body, err := runner.renderBulletin(content)
	if !relevant || content.PortID != recoveryFarPort || err != nil || !strings.Contains(body, "Portb") || strings.Contains(body, "Laport") {
		t.Fatalf("reply referenced an unrelated event's location: content=%+v body=%q error=%v", content, body, err)
	}
}

func TestBulletinSubjectKeysUseTheEventLocationAndSevenRealDays(t *testing.T) {
	now := time.Now().UTC()
	content := bulletinContent{Kind: "event", Event: "demand_shortage", PortID: recoveryFarPort, ZoneID: "zone-south", CommodityID: recoveryGrain}
	subject := content.subjectKey()
	content.ZoneID = "different-zone-metadata"
	if content.subjectKey() != subject {
		t.Fatal("a recurring port event bypassed its subject cooldown through sea-zone metadata")
	}
	state := bulletinState{DiscussedSubjects: map[string]time.Time{subject: now}}
	items := []bulletinItem{{ID: "renewed-event", At: now, Expires: now.Add(10 * 24 * time.Hour), Content: content}}
	if _, ok := selectBulletinItem(items, state, now.Add(7*24*time.Hour-time.Second)); ok {
		t.Fatal("a subject repeated before seven real days")
	}
	if _, ok := selectBulletinItem(items, state, now.Add(7*24*time.Hour)); !ok {
		t.Fatal("a new item with the same subject stayed blocked after seven real days")
	}
	content.Event, content.ZoneID = "pirate_outbreak", "zone-north"
	subject = content.subjectKey()
	content.PortID = "different-port-metadata"
	if content.subjectKey() != subject {
		t.Fatal("a recurring piracy event bypassed its sea-zone subject through port metadata")
	}
}

func TestBulletinDuplicateThreadListingsAreReadOnce(t *testing.T) {
	for _, company := range []string{bulletinAlly, "Other traders"} {
		t.Run(company, func(t *testing.T) {
			h, proxy, runner := newBulletinFixture(t)
			id := proxy.addThread(company, time.Minute, false, false)
			proxy.threads = append(proxy.threads, proxy.threads[0], proxy.threads[0], proxy.threads[0])
			if err := runner.saveBulletinState(h.ctx, bulletinState{LastPostAt: time.Now(), NextDue: time.Now().Add(36 * time.Hour)}); err != nil {
				t.Fatal(err)
			}
			data := missionData{}
			changed, err := runner.driveBulletin(h.ctx, nil, &data)
			state, stateErr := runner.bulletinState()
			if err != nil || stateErr != nil || changed != (company == bulletinAlly) || proxy.detailReads[id] != 1 || state.ReplyScan != nil {
				t.Fatalf("duplicate listing left duplicate reads or an unreadable scan: changed=%t state=%+v errors=%v,%v reads=%d",
					changed, state, err, stateErr, proxy.detailReads[id])
			}
			restarted := h.runner(primaryTestKey)
			refreshBulletinRunner(t, h, restarted)
			if _, err := restarted.bulletinState(); err != nil {
				t.Fatalf("duplicate listing poisoned the durable scan after restart: %v", err)
			}
		})
	}
}

func TestBulletinTemporaryGatesDoNotConsumeUnchangedConversations(t *testing.T) {
	for _, gate := range []string{"daily allowance", "thread day", "opening cadence"} {
		t.Run(gate, func(t *testing.T) {
			h, proxy, runner := newBulletinFixture(t)
			proxy.worldEvents = nil
			runner.snapshot["world_events"] = encode([]bulletinWorldEvent{})
			now := time.Now().UTC()
			state := bulletinState{LastPostAt: now, NextDue: now.Add(36 * time.Hour)}
			age := time.Hour
			if gate == "opening cadence" {
				age = 13 * time.Hour
			}
			id := proxy.addThread(bulletinAlly, age, false, false)
			detail := proxy.details[id]
			switch gate {
			case "daily allowance":
				state.LastPostAt, state.NextDue = time.Time{}, time.Time{}
				state.ReplyTimes = []time.Time{now.Add(-23 * time.Hour), now.Add(-23 * time.Hour)}
			case "thread day":
				detail.Thread.CreatedAt = now.Add(-30 * time.Hour)
				detail.Posts[0].CreatedAt = detail.Thread.CreatedAt
				detail.Posts = append(detail.Posts,
					bulletinPost{ID: uuid.NewString(), ThreadID: id, CompanyName: names.DefaultCompany, CreatedAt: now.Add(-23 * time.Hour)},
					bulletinPost{ID: uuid.NewString(), ThreadID: id, CompanyName: bulletinAlly, CreatedAt: detail.Thread.LastPostAt})
				detail.Thread.Replies = 2
				state.rememberReply(id, now.Add(-23*time.Hour))
			case "opening cadence":
				detail.Posts = append(detail.Posts, bulletinPost{ID: uuid.NewString(), ThreadID: id, CompanyName: "Other traders",
					Body: "Terms for Kraken Cartel", CreatedAt: detail.Thread.LastPostAt})
				detail.Thread.Replies = 1
			}
			proxy.details[id], proxy.threads[0] = detail, detail.Thread
			lastPostAt := detail.Thread.LastPostAt
			if err := runner.saveBulletinState(h.ctx, state); err != nil {
				t.Fatal(err)
			}
			if changed, err := runner.driveBulletin(h.ctx, nil, &missionData{}); err != nil || changed {
				t.Fatalf("temporary gate was bypassed: changed=%t error=%v", changed, err)
			}
			state, err := runner.bulletinState()
			if err != nil || !state.CheckedThreads[id].ObservedAt.IsZero() || proxy.detailReads[id] != 1 {
				t.Fatalf("temporary gate marked an unanswered conversation checked: %+v %v", state, err)
			}
			state.ReplyCheckAfter = time.Time{}
			switch gate {
			case "daily allowance":
				state.ReplyTimes = nil
			case "thread day":
				reply := state.Replies[id]
				reply.At = now.Add(-25 * time.Hour)
				state.Replies[id] = reply
				detail.Posts[1].CreatedAt = reply.At
				proxy.details[id] = detail
			case "opening cadence":
				state.LastPostAt, state.NextDue = now.Add(-25*time.Hour), now.Add(-time.Hour)
			}
			if err := runner.saveBulletinState(h.ctx, state); err != nil {
				t.Fatal(err)
			}
			restarted := h.runner(primaryTestKey)
			refreshBulletinRunner(t, h, restarted)
			_, data := stageBulletinFixture(t, h, restarted)
			if proxy.detailReads[id] != 2 || !proxy.details[id].Thread.LastPostAt.Equal(lastPostAt) {
				t.Fatal("lifting a temporary gate did not re-read the unchanged conversation")
			}
			if gate == "opening cadence" {
				if data.Action.Stage != "bulletin-thread" || data.Bulletin.ItemID != "debate:"+id {
					t.Fatal("unchanged eligible debate did not reopen after the cadence gate")
				}
			} else if data.Action.Stage != "bulletin-reply" || data.Bulletin.ThreadID != id {
				t.Fatal("unchanged fresh foreign post did not receive its deferred reply")
			}
		})
	}
}

func TestBulletinDebateSurvivesOpeningHeadTimeout(t *testing.T) {
	h, proxy, runner := newBulletinFixture(t)
	proxy.worldEvents = nil
	runner.snapshot["world_events"] = encode([]bulletinWorldEvent{})
	id := proxy.addThread(bulletinAlly, time.Hour, false, false)
	detail := proxy.details[id]
	detail.Posts = append(detail.Posts, bulletinPost{ID: uuid.NewString(), ThreadID: id, CompanyName: "Other traders",
		Body: "Kraken Cartel terms", CreatedAt: detail.Thread.LastPostAt})
	detail.Thread.Replies = 1
	proxy.details[id], proxy.threads[0] = detail, detail.Thread
	headReads := 0
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == "/v1/bulletin" {
			headReads++
			if headReads > 1 {
				<-request.Context().Done()
				return
			}
		}
		proxy.ServeHTTP(writer, request)
	})
	if changed, err := runner.driveBulletin(h.ctx, nil, &missionData{}); err != nil || changed {
		t.Fatalf("opening head timeout staged an unevidenced debate: changed=%t error=%v", changed, err)
	}
	state, err := runner.bulletinState()
	if err != nil || !state.CheckedThreads[id].ObservedAt.IsZero() || !state.OpeningCheckAfter.After(time.Now()) || headReads != 2 {
		t.Fatalf("timeout consumed the debate or lost its opening retry deadline: %+v %v reads=%d", state, err, headReads)
	}
	if changed, err := runner.driveBulletin(h.ctx, nil, &missionData{}); err != nil || changed || headReads != 2 {
		t.Fatalf("opening head backoff did not hold the next pass: changed=%t error=%v reads=%d", changed, err, headReads)
	}
	h.api.server.Config.Handler = proxy
	state.OpeningCheckAfter, state.ReplyCheckAfter = time.Time{}, time.Time{}
	if err := runner.saveBulletinState(h.ctx, state); err != nil {
		t.Fatal(err)
	}
	restarted := h.runner(primaryTestKey)
	refreshBulletinRunner(t, h, restarted)
	_, data := stageBulletinFixture(t, h, restarted)
	if data.Bulletin.ItemID != "debate:"+id || data.Action.Stage != "bulletin-thread" ||
		proxy.detailReads[id] != 2 || !slices.Contains(data.Bulletin.BeforeIDs, id) {
		t.Fatal("unchanged debate was lost after a required opening-head timeout")
	}
}

func TestBulletinExpiredReplyHistoryStillHonoursAuthoritativeLifetimeCap(t *testing.T) {
	now := time.Now().UTC()
	id := uuid.NewString()
	state := bulletinState{}
	state.rememberReply(id, now.Add(-16*24*time.Hour))
	state.rememberReply(id, now.Add(-15*24*time.Hour))
	reply := state.Replies[id]
	reply.ObservedAt = now.Add(-15 * 24 * time.Hour)
	state.Replies[id] = reply
	state.prune(now)
	detail := bulletinDetail{Thread: bulletinThread{ID: id, LastPostAt: now.Add(-time.Minute)}, Posts: []bulletinPost{
		{ID: uuid.NewString(), ThreadID: id, CompanyName: names.DefaultCompany, CreatedAt: now.Add(-16 * 24 * time.Hour)},
		{ID: uuid.NewString(), ThreadID: id, CompanyName: names.DefaultCompany, CreatedAt: now.Add(-15 * 24 * time.Hour)},
		{ID: uuid.NewString(), ThreadID: id, CompanyName: bulletinAlly, CreatedAt: now.Add(-time.Minute)},
	}}
	content, relevant, debate := bulletinRelevance(detail, state, nil, gameapi.Catalog{}, now)
	if len(state.Replies) != 0 || relevant || content.Kind != "" || debate != nil {
		t.Fatalf("expired local history bypassed the authoritative two-reply cap: state=%+v content=%+v relevant=%t", state, content, relevant)
	}
}

func TestBulletinActiveItemsRetainDiscussionUntilTheyLeaveCurrentInventory(t *testing.T) {
	for _, kind := range []string{"event", "monopoly"} {
		t.Run(kind, func(t *testing.T) {
			h, proxy, runner := newBulletinFixture(t)
			now := time.Now().UTC()
			proxy.worldEvents = nil
			set := func(runner *Runner, active bool) {
				runner.snapshot["world_events"] = encode([]bulletinWorldEvent{})
				runner.snapshot["monopolies"] = encode([]Monopoly{})
				if active && kind == "event" {
					runner.snapshot["world_events"] = encode([]bulletinWorldEvent{{MarketShock: MarketShock{
						ID: "long-event", EventType: "demand_shortage", PortID: recoveryFarPort, CommodityID: recoveryGrain,
						StartsAt: now.Add(-20 * 24 * time.Hour), ExpiresAt: now.Add(10 * 24 * time.Hour)}}})
				}
				if active && kind == "monopoly" {
					runner.snapshot["monopolies"] = encode([]Monopoly{{ID: "long-grant", CommodityID: recoveryGrain,
						HolderCompanyID: "holder", InForce: true, ExpiresAt: now.Add(10 * 24 * time.Hour)}})
				}
				runner.meta["monopolies"] = DatasetMeta{Complete: true, Generation: runner.generation, UpdatedAt: now, AttemptedAt: now}
			}
			set(runner, true)
			state := bulletinState{}
			item, ok := selectBulletinItem(runner.bulletinItems(&state, now), state, now)
			if !ok || item.Content.Kind != kind {
				t.Fatalf("missing long-lived item fixture: %+v", item)
			}
			state.DiscussedItems = map[string]time.Time{item.ID: now.Add(-15 * 24 * time.Hour)}
			state.DiscussedSubjects = map[string]time.Time{item.Content.subjectKey(): now.Add(-15 * 24 * time.Hour)}
			if err := runner.saveBulletinState(h.ctx, state); err != nil {
				t.Fatal(err)
			}
			restarted := h.runner(primaryTestKey)
			refreshBulletinRunner(t, h, restarted)
			set(restarted, true)
			if changed, err := restarted.driveBulletin(h.ctx, nil, &missionData{}); err != nil || changed {
				t.Fatalf("still-current item repeated after fourteen days: changed=%t error=%v", changed, err)
			}
			state, err := restarted.bulletinState()
			if err != nil || state.DiscussedItems[item.ID].IsZero() {
				t.Fatalf("current item lost its discussion timestamp: %+v %v", state, err)
			}
			set(restarted, false)
			restarted.bulletinItems(&state, now)
			if err := restarted.saveBulletinState(h.ctx, state); err != nil {
				t.Fatal(err)
			}
			state, err = restarted.bulletinState()
			if err != nil || !state.DiscussedItems[item.ID].IsZero() {
				t.Fatalf("old absent item history was not pruned: %+v %v", state, err)
			}
		})
	}
}
