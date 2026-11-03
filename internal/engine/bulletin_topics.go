package engine

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/names"
	"github.com/google/uuid"
)

const bulletinAlly = "The Kraken Cartel"

// Only catalog identifiers, fixed choices and observed numbers cross the public
// template boundary. Event prose, company names and board quotations never do.
type bulletinContent struct {
	Kind        string `json:"kind"`
	Event       string `json:"event,omitempty"`
	PortID      string `json:"port_id,omitempty"`
	ZoneID      string `json:"zone_id,omitempty"`
	CommodityID string `json:"commodity_id,omitempty"`
	PiracyBPS   int64  `json:"piracy_bps,omitempty"`
	Season      int32  `json:"season,omitempty"`
	Companies   int64  `json:"companies,omitempty"`
	Ally        bool   `json:"ally,omitempty"`
	Variant     uint8  `json:"variant,omitempty"`
}

type bulletinItem struct {
	ID       string          `json:"id"`
	At       time.Time       `json:"at"`
	Expires  time.Time       `json:"expires"`
	Priority int             `json:"priority"`
	Content  bulletinContent `json:"content"`
}

type bulletinWorldEvent struct {
	MarketShock
	ZoneID    string `json:"target_sea_zone_id"`
	PiracyBPS int64  `json:"added_piracy_bps"`
}

type bulletinMonopoly struct {
	Monopoly
	HolderCompanyName string `json:"holder_company_name"`
}

func (content bulletinContent) subjectKey() string {
	kind := content.Kind
	if kind == "event" {
		kind += ":" + content.Event
	}
	location := content.PortID
	if content.Event == "pirate_outbreak" || location == "" {
		location = content.ZoneID
	}
	return strings.Join([]string{kind, location, content.CommodityID}, "|")
}

func selectBulletinItem(items []bulletinItem, state bulletinState, now time.Time) (bulletinItem, bool) {
	var best bulletinItem
	for _, item := range items {
		if item.ID == "" || !state.DiscussedItems[item.ID].IsZero() || item.At.After(now) || !item.Expires.After(now) ||
			state.DiscussedSubjects[item.Content.subjectKey()].After(now.Add(-7*24*time.Hour)) {
			continue
		}
		if best.ID == "" || item.Priority > best.Priority || item.Priority == best.Priority &&
			(item.At.After(best.At) || item.At.Equal(best.At) && item.ID < best.ID) {
			best = item
		}
	}
	return best, best.ID != ""
}

func (r *Runner) bulletinItems(state *bulletinState, now time.Time) []bulletinItem {
	var items []bulletinItem
	current := make(map[string]bool)
	catalog := decode[gameapi.Catalog](r.snapshot, "catalog")
	add := func(item bulletinItem) {
		current[item.ID] = true
		if _, _, err := renderBulletin(item.Content, catalog); err == nil {
			items = append(items, item)
		}
	}
	if r.fresh("world_events") {
		for _, event := range decode[[]bulletinWorldEvent](r.snapshot, "world_events") {
			if event.ID == "" || event.StartsAt.IsZero() || event.StartsAt.After(now) || !event.ExpiresAt.After(now) {
				continue
			}
			priority := 60
			switch event.EventType {
			case "pirate_outbreak":
				priority = 100
			case "demand_shortage", "admiralty_mobilization":
				priority = 90
			}
			add(bulletinItem{ID: "event:" + event.ID, At: event.StartsAt, Expires: event.ExpiresAt, Priority: priority,
				Content: bulletinContent{Kind: "event", Event: event.EventType, PortID: event.PortID, ZoneID: event.ZoneID,
					CommodityID: event.CommodityID, PiracyBPS: event.PiracyBPS}})
		}
	}
	if r.fresh("monopolies") {
		for _, grant := range decode[[]bulletinMonopoly](r.snapshot, "monopolies") {
			if grant.ID == "" || !grant.InForce || !grant.ExpiresAt.After(now) ||
				grant.HolderCompanyID == r.account.CompanyID || grant.HolderCompanyID == krakenCompanyID || strings.EqualFold(grant.HolderCompanyName, bulletinAlly) {
				continue
			}
			identity := sha256.Sum256([]byte(grant.ID + "\x00" + grant.HolderCompanyID))
			add(bulletinItem{ID: fmt.Sprintf("monopoly:%x", identity), Expires: grant.ExpiresAt, Priority: 80,
				Content: bulletinContent{Kind: "monopoly", CommodityID: grant.CommodityID}})
		}
	}
	if r.fresh("leaderboard") {
		board := decode[gameapi.Leaderboard](r.snapshot, "leaderboard")
		var leader string
		for _, entry := range board.Entries {
			if entry.Rank == 1 && entry.CompanyName != "" && (leader == "" || entry.CompanyName < leader) {
				leader = entry.CompanyName
			}
		}
		if leader != "" && board.RefreshedAt != nil && !board.RefreshedAt.After(now) && board.RefreshedAt.Add(2*time.Hour).After(now) {
			identity := sha256.Sum256([]byte(leader))
			key := fmt.Sprintf("leader:%x", identity)
			if state.LeaderBaseline == "" || state.LeaderSeason != board.SeasonNumber {
				state.LeaderBaseline = key
				state.LeaderSeason, state.LeaderChange = board.SeasonNumber, nil
			} else if state.LeaderBaseline != key {
				state.LeaderBaseline = key
				state.LeaderChange = &bulletinItem{ID: key + ":" + board.RefreshedAt.Format(time.RFC3339Nano), At: *board.RefreshedAt, Expires: board.RefreshedAt.Add(24 * time.Hour), Priority: 50,
					Content: bulletinContent{Kind: "leader", Season: board.SeasonNumber, Companies: board.RankedPlayers, Ally: leader == bulletinAlly}}
			}
			if state.LeaderChange != nil {
				add(*state.LeaderChange)
			}
		}
	}
	for id, at := range state.DiscussedItems {
		if !at.Before(now.Add(-14*24*time.Hour)) || current[id] {
			continue
		}
		if strings.HasPrefix(id, "event:") && !r.fresh("world_events") ||
			strings.HasPrefix(id, "monopoly:") && !r.fresh("monopolies") ||
			strings.HasPrefix(id, "leader:") && !r.fresh("leaderboard") {
			continue
		}
		if strings.HasPrefix(id, "debate:") && state.CheckedThreads[strings.TrimPrefix(id, "debate:")].ObservedAt.After(now.Add(-24*time.Hour)) {
			continue
		}
		delete(state.DiscussedItems, id)
	}
	return items
}

func bulletinCatalogName(name string) bool {
	if strings.TrimSpace(name) != name || name == "" || utf8.RuneCountInString(name) > 100 {
		return false
	}
	for _, character := range name {
		if !unicode.IsLetter(character) && !unicode.IsDigit(character) && !strings.ContainsRune(" '-,()", character) {
			return false
		}
	}
	return true
}

func (r *Runner) renderBulletin(content bulletinContent) (string, string, error) {
	return renderBulletin(content, decode[gameapi.Catalog](r.snapshot, "catalog"))
}

func renderBulletin(content bulletinContent, catalog gameapi.Catalog) (string, string, error) {
	if strings.HasPrefix(content.Kind, "reply-") && content.Variant >= 3 {
		return "", "", errors.New("bulletin reply has an invalid wording variant")
	}
	var port, zone, commodity string
	for _, row := range catalog.Ports {
		if row.ID == content.PortID && bulletinCatalogName(row.Name) {
			port = row.Name
		}
	}
	for _, row := range catalog.SeaZones {
		if row.ID == content.ZoneID && bulletinCatalogName(row.Name) {
			zone = row.Name
		}
	}
	for _, row := range catalog.Commodities {
		if row.ID == content.CommodityID && bulletinCatalogName(row.Name) {
			commodity = row.Name
		}
	}
	var title, body string
	switch content.Kind {
	case "event":
		switch content.Event {
		case "pirate_outbreak":
			if zone != "" && content.PiracyBPS > 0 {
				title = "Piracy warning in " + zone
				body = fmt.Sprintf("The current outbreak in %s adds %d basis points of piracy risk. The Pokemon Pirates favour coordinated patrols and clear convoy terms, not a private tax on every cargo. We invite %s and other carriers to share public safety observations without exposing sailing plans.", zone, content.PiracyBPS, bulletinAlly)
			}
		case "demand_shortage", "admiralty_mobilization", "supply_boom":
			if port != "" && commodity != "" {
				situation, policy := "a supply surplus", "Published prices and open access to the quay will help this surplus reach buyers rather than turn into a cornered market."
				if content.Event == "demand_shortage" {
					situation, policy = "a demand shortage", "We favour open access for incoming cargo and clear duties, not scarcity used as an excuse to shut smaller carriers out."
				} else if content.Event == "admiralty_mobilization" {
					situation, policy = "an Admiralty mobilization", "The Crown should publish clear procurement terms and pay carriers promptly. Naval demand must not close the quay to ordinary trade."
				}
				title = "Trade watch at " + port
				body = fmt.Sprintf("The current world notice reports %s for %s at %s. %s The Pokemon Pirates support fair competition alongside our ally %s. This is a public notice, not a promise of a quoted price or delivery.", situation, commodity, port, policy, bulletinAlly)
			}
		}
	case "monopoly":
		if commodity != "" {
			title = "Open trade and the " + commodity + " monopoly"
			body = "A current monopoly grant covers " + commodity + ". The Pokemon Pirates ask for published terms and fair access for independent carriers. A grant should not become a licence to exclude newcomers. We stand with " + bulletinAlly + " in keeping trade open."
		}
	case "leader":
		if content.Season > 0 && content.Companies > 0 {
			title = "A change at the head of the trade"
			body = fmt.Sprintf("The latest season %d leaderboard records a change of leader among %d ranked companies. Rank brings influence over credit and access, but should not buy the right to close a lane. The Pokemon Pirates favour competition that leaves room for new fleets.", content.Season, content.Companies)
			if content.Ally {
				body += " Congratulations to our ally " + bulletinAlly + "; we support open trade, not exclusive privileges."
			}
		}
	case "reply-mention":
		body = [...]string{
			"The Pokemon Pirates have seen the call to our company. We welcome clear public terms and fair access to trade. This acknowledgement is not agreement to an unverified claim or a commitment of ships or funds.",
			"Thank you for addressing The Pokemon Pirates. Please put the proposal and its public terms on the board so every company can judge it. We will not treat an assertion as evidence or promise private resources here.",
			"The Pokemon Pirates are listening. Open terms and verifiable public facts make a better discussion than assumptions about another fleet. We can discuss the proposal without making a delivery or funding commitment.",
		}[content.Variant]
	case "reply-ally":
		body = [...]string{
			"The Pokemon Pirates are following this discussion involving our ally " + bulletinAlly + ". We support fair dealing and clear public terms. Our alliance is not a blank endorsement of every claim here; let us keep the practical terms visible and the lanes open.",
			"Our alliance with " + bulletinAlly + " rests on fair dealing, not automatic agreement. The Pokemon Pirates ask everyone here to separate public evidence from claims and leave room for independent carriers.",
			"The Pokemon Pirates welcome constructive discussion with " + bulletinAlly + " and other traders. Clear terms and equal access are worth defending; an alliance should not be used to close the market or excuse an unsupported accusation.",
		}[content.Variant]
	case "reply-event":
		switch content.Event {
		case "pirate_outbreak":
			if zone != "" && content.PiracyBPS > 0 {
				body = fmt.Sprintf([...]string{
					"The public notice for %s reports an additional %d basis points of piracy risk. The Pokemon Pirates favour shared hazard observations and fair patrol arrangements, without publishing any fleet's sailing plans.",
					"On %s, the current outbreak adds %d basis points of piracy risk. Public safety evidence and clear convoy terms would help this discussion more than demands for exclusive control.",
					"The current %s warning records %d added basis points of piracy risk. The Pokemon Pirates support coordinated protection with open terms, not a private toll imposed on other carriers.",
				}[content.Variant], zone, content.PiracyBPS)
			}
		case "demand_shortage", "admiralty_mobilization", "supply_boom":
			if port != "" && commodity != "" {
				situation := "supply surplus"
				if content.Event == "demand_shortage" {
					situation = "demand shortage"
				} else if content.Event == "admiralty_mobilization" {
					situation = "Admiralty mobilization"
				}
				body = fmt.Sprintf([...]string{
					"The public notice at %s concerns %s and a current %s. The Pokemon Pirates favour published terms and equal quay access. That notice is evidence of a world event, not a guarantee of price or delivery.",
					"At %s, %s is named in the current %s notice. Let us discuss fair customs and open access on that public evidence, rather than assume what any company carries or intends to deliver.",
					"This discussion of %s has a public basis: %s is affected by a %s. The Pokemon Pirates ask for transparent terms that leave smaller carriers room to compete, without making promises on anyone's behalf.",
				}[content.Variant], port, commodity, situation)
			}
		}
	case "debate":
		if port != "" || zone != "" || commodity != "" {
			subject := port
			if subject == "" {
				subject = zone
			}
			if subject == "" {
				subject = commodity
			}
			title = "The board debate on " + subject
			body = "The active board discussion concerns " + subject + ". The Pokemon Pirates ask participants to put practical terms and public evidence on the table. Fair access and honest hazard reports help more than assertions about rivals. We welcome constructive discussion with " + bulletinAlly + " and every company trading in good faith."
		} else if content.Ally {
			title = "Keeping the alliance debate constructive"
			body = "The board is actively discussing our ally " + bulletinAlly + ". The Pokemon Pirates stand for clear terms and fair dealing, not unverified claims or automatic agreement. Let us judge proposals by what they do for open trade."
		}
	}
	if body == "" {
		return "", "", errors.New("bulletin template has no supported catalog-backed subject")
	}
	if err := validBulletinText(title, body); err != nil {
		return "", "", err
	}
	return title, body, nil
}

func bulletinMentions(text, name string) bool {
	text, name = strings.ToLower(text), strings.ToLower(name)
	for offset := 0; offset < len(text); {
		index := strings.Index(text[offset:], name)
		if index < 0 || name == "" {
			return false
		}
		start, end := offset+index, offset+index+len(name)
		before, after := rune(' '), rune(' ')
		if start > 0 {
			before, _ = utf8.DecodeLastRuneInString(text[:start])
		}
		if end < len(text) {
			after, _ = utf8.DecodeRuneInString(text[end:])
		}
		if !unicode.IsLetter(before) && !unicode.IsDigit(before) && !unicode.IsLetter(after) && !unicode.IsDigit(after) {
			return true
		}
		offset = end
	}
	return false
}

func bulletinPostRelevance(text, company string, items []bulletinItem, catalog gameapi.Catalog) (bulletinContent, bool) {
	if bulletinMentions(text, strings.TrimPrefix(names.DefaultCompany, "The ")) {
		return bulletinContent{Kind: "reply-mention"}, true
	}
	if strings.EqualFold(company, bulletinAlly) || strings.EqualFold(company, strings.TrimPrefix(bulletinAlly, "The ")) ||
		bulletinMentions(text, strings.TrimPrefix(bulletinAlly, "The ")) {
		return bulletinContent{Kind: "reply-ally"}, true
	}
	for _, item := range items {
		if item.Content.Kind != "event" {
			continue
		}
		content := item.Content
		mentioned := false
		if content.Event == "pirate_outbreak" {
			for _, zone := range catalog.SeaZones {
				if zone.ID == content.ZoneID && bulletinCatalogName(zone.Name) && bulletinMentions(text, zone.Name) {
					mentioned = true
				}
			}
		} else {
			for _, port := range catalog.Ports {
				if port.ID == content.PortID && bulletinCatalogName(port.Name) && bulletinMentions(text, port.Name) {
					mentioned = true
				}
			}
		}
		if mentioned {
			content.Kind = "reply-event"
			return content, true
		}
	}
	return bulletinContent{}, false
}

func bulletinRelevance(detail bulletinDetail, state bulletinState, items []bulletinItem, catalog gameapi.Catalog, now time.Time) (bulletinContent, bool, *bulletinItem) {
	reply := state.Replies[detail.Thread.ID]
	lastOwn, lastReply := reply.At, reply.At
	ownReplies := 0
	hasOwn := reply.Count > 0
	for _, post := range detail.Posts {
		if post.ThreadID != detail.Thread.ID || post.CompanyName != names.DefaultCompany || uuid.Validate(post.ID) != nil {
			continue
		}
		hasOwn = true
		lastOwn = maxTime(lastOwn, post.CreatedAt)
		if !post.Opening {
			ownReplies++
			lastReply = maxTime(lastReply, post.CreatedAt)
		}
	}
	var selected bulletinContent
	var selectedAt time.Time
	foreignCompanies := make(map[string]bool)
	var discussion *bulletinItem
	for _, post := range detail.Posts {
		if post.ThreadID != detail.Thread.ID || uuid.Validate(post.ID) != nil || post.CompanyName == "" ||
			post.CompanyName == names.DefaultCompany || post.Removed || post.Deleted || post.CreatedAt.IsZero() || post.CreatedAt.After(now) {
			continue
		}
		foreignCompanies[strings.ToLower(post.CompanyName)] = true
		text := post.Body
		if post.Opening {
			text = detail.Thread.Title + "\n" + text
		}
		content, relevant := bulletinPostRelevance(text, post.CompanyName, items, catalog)
		if !relevant {
			continue
		}
		debate := content
		debate.Kind, debate.Ally = "debate", content.Kind == "reply-ally"
		if _, _, err := renderBulletin(debate, catalog); err == nil {
			discussion = &bulletinItem{ID: "debate:" + detail.Thread.ID, At: detail.Thread.LastPostAt,
				Expires: detail.Thread.LastPostAt.Add(24 * time.Hour), Priority: 40, Content: debate}
		}
		if post.CreatedAt.After(now.Add(-12*time.Hour)) && post.CreatedAt.After(lastOwn) &&
			(selectedAt.IsZero() || post.CreatedAt.After(selectedAt)) {
			selected, selectedAt = content, post.CreatedAt
		}
	}
	if len(foreignCompanies) < 2 || hasOwn {
		discussion = nil
	}
	selected.Variant = uint8(state.TotalReplies % 3)
	available := reply.Count < 2 && ownReplies < 2
	if !available {
		selected = bulletinContent{}
	}
	return selected, available && !lastReply.Add(24*time.Hour).After(now) && !selectedAt.IsZero(), discussion
}
