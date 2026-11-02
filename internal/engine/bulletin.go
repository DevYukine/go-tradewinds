package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/names"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

const (
	bulletinInterval          = 12 * time.Hour
	bulletinRetryDelay        = 15 * time.Minute
	bulletinRequestBudget     = 2 * time.Second
	bulletinReplyHistoryLimit = 128
	bulletinPageLimit         = 100
	bulletinCursorLimit       = 4096
)

type bulletinState struct {
	LastPostID       string             `json:"last_post_id,omitempty"`
	LastPostAt       time.Time          `json:"last_post_at"`
	NextDue          time.Time          `json:"next_due"`
	CooldownUntil    time.Time          `json:"cooldown_until"`
	ThreadCursor     int                `json:"thread_cursor"`
	ReplyCursor      int                `json:"reply_cursor"`
	NextReply        bool               `json:"next_reply"`
	RepliedThreadIDs []string           `json:"replied_thread_ids,omitempty"`
	LastOutcomeID    string             `json:"last_outcome_id,omitempty"`
	ReplyScan        *bulletinReplyScan `json:"reply_scan,omitempty"`
}

type bulletinReplyScan struct {
	Since      time.Time `json:"since"`
	Cursor     string    `json:"cursor,omitempty"`
	Candidates []string  `json:"candidates,omitempty"`
	Complete   bool      `json:"complete"`
}

type bulletinReconcileScan struct {
	Cursor     string        `json:"cursor,omitempty"`
	Candidates []string      `json:"candidates,omitempty"`
	Complete   bool          `json:"complete"`
	Match      *bulletinPost `json:"match,omitempty"`
}

type bulletinIntent struct {
	CompanyName      string                 `json:"company_name"`
	Title            string                 `json:"title,omitempty"`
	Body             string                 `json:"body"`
	ThreadID         string                 `json:"thread_id,omitempty"`
	TemplateIndex    int                    `json:"template_index"`
	ReplyTurn        bool                   `json:"reply_turn"`
	NotBefore        time.Time              `json:"not_before"`
	BeforeIDs        []string               `json:"before_ids,omitempty"`
	ReconcilePending bool                   `json:"reconcile_pending,omitempty"`
	ReconcileScan    *bulletinReconcileScan `json:"reconcile_scan,omitempty"`
}

type bulletinPayload struct {
	Title    string `json:"title,omitempty"`
	Body     string `json:"body"`
	ThreadID string `json:"thread_id,omitempty"`
}

type bulletinThread struct {
	ID          string          `json:"id"`
	Title       string          `json:"title"`
	CompanyName string          `json:"company_name"`
	Link        json.RawMessage `json:"link"`
	Replies     int             `json:"replies"`
	CreatedAt   time.Time       `json:"created_at"`
	LastPostAt  time.Time       `json:"last_post_at"`
	Archived    bool            `json:"archived"`
	Removed     bool            `json:"removed"`
}

type bulletinPost struct {
	ID            string     `json:"id"`
	ThreadID      string     `json:"thread_id"`
	CompanyName   string     `json:"company_name"`
	Body          string     `json:"body"`
	Opening       bool       `json:"is_opening"`
	CreatedAt     time.Time  `json:"created_at"`
	EditedAt      *time.Time `json:"edited_at"`
	EditableUntil *time.Time `json:"editable_until"`
	Removed       bool       `json:"removed"`
	Deleted       bool       `json:"deleted"`
}

type bulletinPage struct {
	Threads    []bulletinThread `json:"threads"`
	NextCursor string           `json:"next_cursor"`
}

type bulletinDetail struct {
	Thread bulletinThread `json:"thread"`
	Posts  []bulletinPost `json:"posts"`
}

type bulletinTemplate struct {
	Title string
	Body  string
}

var bulletinThreads = []bulletinTemplate{
	{"On the freedom of the seas", "The Pokemon Pirates hold that open water belongs to every honest hull. Customs houses may tax a cargo, but no harbour should own the sea lanes between them. We stand with The Kraken Cartel in defending free passage for all traders."},
	{"A word on harbour dues", "Harbour dues should pay for berths, lights and pilots, not fill a treasury that never dredges a channel. We ask every port authority to publish what its dues actually fund. Traders who pay deserve to know."},
	{"The Admiralty and the merchant fleet", "The Admiralty protects the lanes because merchants keep the Crown solvent. That bargain only holds if commissions are fairly priced and openly listed. We support The Kraken Cartel's call for clear Admiralty terms."},
	{"Why we back the expansion", "New harbours in the Americas mean new markets, new yards and fewer captains fighting over the same berth at Portsmouth. The Pokemon Pirates back the expansion effort and the allies funding it, The Kraken Cartel first among them."},
	{"Credit is a public good", "When credit is scarce, only the largest houses can build. When lenders compete, every honest company can grow. We welcome more lenders on the board and thank The Kraken Cartel for offering terms to smaller fleets."},
	{"Against the customs ladder", "A customs ladder that climbs with every ton punishes the very traders who keep ports supplied. We favour flat, published duties that a captain can price before leaving port. Predictable rules build trade."},
	{"Piracy is a tax on everyone", "Every raider prize is paid for by the next cargo's insurance premium. Piracy is not a private quarrel between a raider and a merchant. It is a cost borne by the whole board, and the whole board should resist it."},
	{"On letters of marque", "A letter of marque turns a pirate into a policeman for the season. We accept them as a necessary tool, but zones should be issued with restraint. Flooding a coast with privateers drives honest carriers away."},
	{"A charter for fair freight", "We propose a simple standard: freight contracts should state quantity, deadline and penalty in plain terms, and penalties should match the harm done. The Kraken Cartel's freight desk already works this way. Others should follow."},
	{"The case for open order books", "Hidden books reward whoever can afford to look first. Published depth lets small fleets plan voyages and keeps prices honest. We support every measure that brings more of the market into daylight."},
	{"Regional treasuries and the cost of moving money", "Moving money between regions should not cost more than moving the cargo it pays for. Bank drafts were a step forward. We would welcome further competition among the banks that carry remittances."},
	{"An alliance worth keeping", "The Pokemon Pirates and The Kraken Cartel do not agree on everything. We agree that the sea is large, that fair dealing pays, and that allies who keep their word are worth more than a full hold. That is enough."},
	{"On the Crown's commissions", "When the Crown commissions hulls, the yards that build them should be paid promptly and in full. A navy that pays late will find its hulls built late. We back prompt settlement of every Admiralty commission."},
	{"Insurance should price risk, not fear", "Lloyd's premiums ought to follow measured losses on a route. When premiums run far above recorded claims, merchants sail uninsured and everyone is poorer for it. We welcome the recent cuts and ask for more transparency."},
	{"Wages for the crews who carry the realm", "No cargo crosses the sea without a crew. Companies that skimp on stores and wages lose ships, and lost ships raise everyone's insurance. Feed your crews properly. It is policy, not charity."},
	{"Expansion should reward builders", "The companies that build hulls in new regions take the first risks. Port authorities in the Americas should remember who came first when berth rights are allocated. We ask for fair treatment of early builders."},
	{"Against monopoly on any lane", "No single house should hold every berth on a lane. Competition keeps freight rates honest and keeps ports supplied. We will always prefer a crowded lane to a captured one."},
	{"A plain statement of our aims", "The Pokemon Pirates trade to grow, build ships to carry more, and back allies who do the same. We do not raid, we do not hoard berths, and we pay our debts on schedule. We expect the same of our rivals."},
	{"The case for smaller, steadier repayments", "Lenders and borrowers both sleep better when a loan is repaid in steady instalments rather than one great sum at maturity. We favour amortised terms and thank The Kraken Cartel for championing them."},
	{"On sealed freight and trust", "Sealed freight works because carriers deliver what they were given. Every broken seal costs the whole trade some trust. We support firm penalties for tampering and fair hearings for honest delays."},
	{"Why stores matter to the realm", "A ship that runs short of water and beer is a ship that limps into the wrong harbour. Provisioning standards protect crews and cargo alike. We encourage every company to carry the safe minimum and then some."},
	{"The quiet value of factors", "A factor in port is a merchant's eyes and ears. Ports that make hiring factors simple attract more trade. We would welcome lower factor fees in the new American harbours."},
	{"A fair hearing for every company", "The board should be a place for honest argument, not insults. We will debate any policy on its merits and expect the same courtesy in return. Disagreement is healthy. Contempt is not."},
	{"On futures and honest prices", "A forward market lets a merchant fix a price before the voyage. It only works when both sides deliver. We support margin rules that protect the honest party and penalties that bite on default."},
	{"Convoys and the common defence", "Raiders pick off lone hulls. Convoys make that costly. We favour companies sailing together on dangerous water and would welcome a standing convoy schedule on the oceanic crossings."},
	{"What the Americas need", "The new harbours need timber, iron, tools and people. Merchants who bring them deserve fair duties and quick berths. We call on the American port authorities to keep their customs light while the colonies grow."},
	{"A note on the leaderboard", "Rank is a measure, not a purpose. We compete hard, but a board where every company prospers is better for all of us than one where a single house towers over ruins. Grow the trade, and rank follows."},
	{"The Goldsmiths and the rate floor", "Bank rates set the floor for private credit. When the Goldsmiths raise rates, private lenders follow. We ask the banks to remember that cheap credit builds hulls, and hulls carry the realm's trade."},
	{"Standing with our allies", "The Kraken Cartel has supported expansion with capital, freight and patience. The Pokemon Pirates stand with them in public and in practice. An alliance is proven by its ledgers, and ours are in order."},
	{"Against arbitrary seizure", "Seizure for unpaid upkeep should come only after clear warning and a fair chance to pay. Companies need certainty that their hulls will not vanish overnight. We ask for published notice periods."},
	{"Port rights for the many", "Every company should be able to hire a factor, lease a warehouse and berth a ship on the same terms as the largest houses. Equal access is the foundation of a fair market."},
	{"On hazard and honest charts", "Captains need honest charts of the hazards on each lane. Hidden danger rewards the reckless and punishes the careful. We would welcome published loss records for every sea zone."},
	{"The duty of the strong", "Large fleets carry large responsibilities. They set prices, fill berths and shape the board's mood. We ask every major house to trade fairly, pay promptly and leave room for newcomers."},
	{"Why we build overseas", "Europe's yards are crowded and its lanes well worked. The Pokemon Pirates now build in the Americas because that is where the realm is growing. We invite other companies to join us there."},
	{"On commerce raiding in wartime", "Even rivals should agree on limits. Crews taken at sea should be returned, cargo should be accounted, and hulls should not be burned for sport. Restraint today protects us all tomorrow."},
	{"Transparency in lending", "A loan request on the board should state its purpose, term and repayment plan. Lenders deserve to know what they fund. We publish ours openly and encourage every borrower to do the same."},
	{"A modest proposal on berth queues", "Berth queues should be served in order of arrival, not by the size of the purse. A predictable queue lets every captain plan. We support first come, first berthed."},
	{"The merchant's compact", "Deliver what you promise. Pay what you owe. Do not cheat the scale. Treat crews as people, not ballast. These are old rules, and they still make the best policy on any sea."},
	{"On the price of passage", "Passengers pay for a safe and timely crossing, not a gamble. Fares should be fair to both traveller and carrier. We favour published fare bands so passengers can compare honestly."},
	{"Shared risk, shared reward", "When lenders, carriers and builders share risk fairly, more hulls get built and more cargo moves. The Kraken Cartel understands this. We hope more houses will join them in backing the expansion."},
	{"Against the hoarding of cash", "Gold in a vault carries no cargo and builds no hulls. A healthy trade puts its capital to work. We encourage every company to invest in ships and routes rather than sit on idle reserves."},
	{"On the authority of the Crown", "The Crown keeps the peace on the lanes and enforces contracts in port. We accept its authority and ask only that it be exercised openly, predictably and with the merchant's interests in mind."},
	{"A call for honest weights", "Short weights and padded manifests cheat buyers and sellers alike. Every port should keep certified scales and publish its inspection record. Honest measure is the beginning of honest trade."},
	{"The colonies deserve a voice", "The American harbours are no longer outposts. Their merchants and port authorities should have a say in the rules that govern their trade. We support representation for the new regions on every matter of customs."},
	{"On the value of patience", "Quick profits tempt every captain. Steady trade builds companies that last. The Pokemon Pirates plan for the long voyage, and we hope our rivals do too."},
	{"Why we answer the board", "We post here because public argument improves policy. Every company that speaks openly about rates, dues and risks helps the rest of us trade better. Silence only benefits those who profit from confusion."},
	{"Ship names and honest records", "The registry should record every name a hull has carried. A ship's history matters to buyers, insurers and crews. We welcome the new record of former names and the fate of lost hulls."},
	{"On the Cape expedition", "The Admiralty's expedition beyond the coastal water opens routes few have tried. We support it, and we ask that its charters be open to every company with a suitable hull, not reserved for favoured houses."},
	{"Against price fixing", "Merchants who agree among themselves to hold prices high rob every port they supply. We oppose any such arrangement and will trade against it wherever we find it."},
	{"The balance between ports", "No single port should drain the trade of its neighbours. A healthy coast has many busy harbours. We favour policies that spread trade fairly rather than funnel it to one quay."},
	{"On debt and honour", "A company that repays its loans on schedule earns the right to borrow again. One that walks away harms every borrower who follows. We keep our record clean and expect lenders to reward that."},
	{"A pledge to newcomers", "Every great house was once a single hoy. The Pokemon Pirates will deal fairly with new companies, share lane news where we can and never undercut a newcomer out of spite."},
	{"Warehouses and the public interest", "Leased warehouses keep goods safe between voyages. Rents should reflect costs, not opportunity. We ask port authorities to publish warehouse tariffs alongside harbour dues."},
	{"On naval protection of trade", "A navy that patrols the lanes is worth every Crown it costs. We support funding for patrols on the busiest and most dangerous waters, paid fairly by those who benefit."},
	{"The cost of idle hulls", "A ship tied up in port still costs upkeep. Policies that keep hulls waiting, through slow customs or crowded berths, tax the whole trade. Faster ports mean cheaper goods."},
	{"What we owe our allies", "Allies are not owed obedience. They are owed honesty, fair dealing and support when it matters. The Pokemon Pirates offer exactly that to The Kraken Cartel, and we receive the same."},
	{"On regional banking", "Every region should have banks willing to lend locally. Capital that must cross an ocean to reach a borrower arrives late and costs more. We welcome lenders setting up in the Americas."},
	{"Tariffs and the common good", "Tariffs can protect a young port's trade, but high tariffs choke it. We favour low and stable duties that raise revenue without driving cargo elsewhere."},
	{"The right to refuse a contract", "No carrier should be forced into freight that its hull cannot safely carry. We support clear hull ratings on every contract and the right to decline without penalty before acceptance."},
	{"In defence of the small trader", "The small trader fills the gaps the great houses ignore. Rules written only for large fleets squeeze them out. We ask that every new policy be tested against its effect on single-ship companies."},
	{"On the record of losses", "Every lost hull should be recorded with its cause, whether foundered, seized or taken. Honest loss records make for honest insurance and better seamanship. We welcome the registry's new fate records."},
	{"A reasonable request to lenders", "The Pokemon Pirates seek credit to build a large hull in the Americas. We prefer steady instalments over a single balloon payment and will repay on schedule. Lenders are welcome to quote on our posted request."},
	{"Against secret dealing", "Private deals have their place, but the board works best when prices and terms are visible. Secret arrangements that move markets harm everyone outside the room."},
	{"On provisioning standards", "Ports should publish the provisions a safe crossing requires. Captains who sail short endanger crews and cargo. Clear standards protect the careful and expose the reckless."},
	{"The road to prosperity", "Prosperity comes from more ships, more routes and more trade, not from taking a larger slice of a fixed pie. We back every measure that grows the whole economy of the seas."},
	{"On fair competition", "We will race any company for a cargo and congratulate the winner. We will not sabotage, slander or undercut below cost to ruin a rival. Fair competition makes the board stronger."},
	{"The merchant and the state", "Merchants fund the Crown and the Crown protects the merchant. When either forgets the bargain, both suffer. We ask the Admiralty and the port authorities to keep their side as faithfully as we keep ours."},
	{"A word for the harbourmasters", "Harbourmasters keep the quays moving in all weathers. They deserve fair pay and the authority to keep queues orderly. We support any port that invests in its harbour staff."},
	{"Why the Americas matter", "The growth of the realm is in the West. The companies that build there now will shape its trade for years. We urge every serious house to consider an American yard."},
	{"On reckless expansion", "Growth funded by debt that cannot be repaid is not growth. We borrow only against measured returns and urge lenders to ask every borrower for the same evidence."},
	{"The duty to deliver", "A contract accepted is a promise made. Weather and raiders may delay a voyage, but a carrier who abandons freight without cause harms every honest carrier. We deliver what we accept."},
	{"Toward a better board", "This board is a public square. We will use it to argue for fair rules, honest prices and open water. We invite every company to do the same, and to leave the insults in port."},
	{"On the dignity of the quay", "Dockworkers load the realm's wealth and are paid least for it. Ports that treat their labourers fairly turn ships faster and lose less cargo to careless handling. Good policy starts at the quay."},
	{"Contracts must mean what they say", "A contract that can be reinterpreted after signing is no contract at all. We favour plain wording, published terms and penalties fixed in advance. Certainty is the cheapest insurance a merchant can buy."},
	{"The danger of a single lender", "When one house holds every loan, every borrower answers to it. A healthy credit market needs many lenders competing on rate and terms. We welcome every new lending line on the board."},
	{"On the timber trade", "Every hull begins as timber. Ports that tax shipbuilding timber heavily tax the growth of the whole fleet. We ask the American yards to keep timber duties low while the new regions build."},
	{"A case for public port reports", "Port reports are the eyes of every captain. When ports publish their stock and prices openly, ships go where they are needed. Hidden scarcity helps nobody but the speculator."},
	{"Neutral waters, neutral rules", "Where no Crown patrols, the board's own customs must hold. Honour contracts, spare crews, and settle disputes in port. Lawless water is bad for every flag, ours included."},
	{"On rebuilding after loss", "A company that loses a hull should be able to rebuild without ruin. Fair lending, honest insurance and reasonable yard prices make recovery possible. A board that punishes misfortune breeds recklessness."},
	{"The case against speculation in staples", "Grain, water and beer feed the crews of every fleet. Cornering the staples to force prices up endangers ships at sea. We will not do it and we urge others to refuse as well."},
	{"Why we publish our terms", "Our loan requests, freight offers and alliances are posted openly. We believe a company that trades in daylight earns more trust than one that trades in whispers. Trust, in the end, is capital."},
	{"On the expedition charters", "Charters beyond the coastal water carry real risk and should pay for it. We ask the Admiralty to price expedition cargo honestly, and we will carry it when the terms reflect the danger."},
	{"A fair price for passage to the colonies", "Settlers crossing to the Americas build the markets we will trade in tomorrow. Carriers should price their passage fairly. Gouging today's travellers poisons tomorrow's trade."},
	{"On mergers of fleets", "When great houses combine, the board should watch closely. Larger fleets can bring efficiency, but they can also close lanes to everyone else. We will judge each combination by what it does to open trade."},
	{"The merchant's vote", "Merchants have no seat at the Admiralty table, yet they pay for its ships. We ask that the Admiralty hear from the companies that carry the realm's trade before setting policy on patrols and commissions."},
	{"A word on reputation", "Ledgers can be audited, but reputation is earned voyage by voyage. The Pokemon Pirates intend to be known as the company that pays, delivers and speaks plainly. That is our whole platform."},
	{"Allies in open water", "The Kraken Cartel and The Pokemon Pirates sail different lanes toward the same end: a larger, fairer trade. We will keep backing our allies in public and keep proving it in practice."},
}

var bulletinReplies = []string{
	"The Pokemon Pirates agree. Open rules and published terms serve every company on the board.",
	"A fair point. We would add that predictable duties matter as much as low ones.",
	"We stand with The Kraken Cartel on this. Fair dealing has served both our houses well.",
	"Well argued. The expansion succeeds only if new harbours stay open to every company.",
	"We support this. Credit should reach small fleets as readily as large ones.",
	"Agreed. Honest records of losses and hazards make for honest insurance.",
	"This deserves a hearing from the port authorities. Merchants pay the dues, so merchants should see the accounts.",
	"Our position is the same: no single house should control a lane.",
	"We would welcome steady instalment terms on this. Smaller payments suit lender and borrower alike.",
	"The Admiralty would do well to read this. Prompt payment builds a reliable navy.",
	"A sound proposal. We would vote for it if the board held a vote.",
	"We respectfully disagree on the details, but the principle is right.",
	"Well said. Competition is healthy; sabotage is not.",
	"This is why we build in the Americas. The realm grows where the merchants go.",
	"Agreed. A loan repaid on schedule is the best reference a borrower can offer.",
	"We support fair access to berths and factors for every company, large or small.",
	"The Pokemon Pirates back this. Tariffs should raise revenue, not choke trade.",
	"A clear statement. We would add that convoys deserve support on the oceanic crossings.",
	"Hear, hear. Honest weights and honest measures are the foundation of trade.",
	"We agree that restraint at sea protects every crew, including our own.",
	"This matches our experience. Steady trade outlasts quick profits.",
	"Our allies in The Kraken Cartel have made the same case. We support it fully.",
	"Fair terms, published openly. That is all any merchant asks.",
	"We would add one thing: the new American ports deserve a voice in these rules.",
	"Agreed. Idle hulls cost everyone, and slow customs make them idle.",
	"A good principle. We hope the larger houses take it to heart.",
	"We welcome this. Transparency in lending helps both sides of the ledger.",
	"The Pokemon Pirates support any measure that spreads trade across many ports.",
	"Well put. A navy is worth its cost when it actually patrols the lanes.",
	"We take a slightly different view, but this is a debate worth having in public.",
	"Agreed. The board works best when terms are visible to all.",
	"We back this. Crews who are fed and paid sail better and lose fewer ships.",
	"This is sensible policy. We would support it at any port we trade through.",
	"Our record speaks for itself: we pay on schedule and deliver what we accept.",
	"We share this concern. Seizure without fair warning undermines trust in the whole system.",
	"A thoughtful post. Every new policy should be tested against its effect on small companies.",
	"We support open forward markets with firm margin rules. Defaults should cost the defaulter.",
	"Agreed. Private deals have their place, but markets need daylight.",
	"We welcome more lenders in the American region. Capital should not have to cross an ocean.",
	"The Kraken Cartel has been a reliable ally on exactly this question.",
	"This is a fair summary of the merchant's compact. We follow it.",
	"We would add that clear hull ratings protect carriers and shippers alike.",
	"Agreed. Hoarded cash builds nothing. Ships and routes do.",
	"A reasonable position. We hope the port authorities respond in kind.",
	"We support this, and we would back it with our own freight where we can.",
	"Well reasoned. Rank follows trade, not the other way round.",
	"The Pokemon Pirates agree that the Crown's authority should be exercised openly.",
	"We would welcome published fare bands. Passengers deserve to compare honestly.",
	"This is the right approach. Early builders take the first risks and deserve fair treatment.",
	"We share the view that piracy is a cost borne by the whole board.",
	"Agreed. Letters of marque are a tool, not a policy in themselves.",
	"We hope more houses will speak this plainly. Public argument improves policy.",
	"The Pokemon Pirates stand with this proposal and with our allies who made it.",
	"A fair challenge to the banks. Cheaper credit means more hulls on the water.",
	"Agreed. Equal access to warehouses and factors is a basic condition of a fair market.",
	"We support this, with one caution: rules should be simple enough that every captain can follow them.",
	"Well said. A crowded lane is better than a captured one.",
	"We would back any port that publishes its dues and what they pay for.",
	"This matches our policy. We borrow only against measured returns.",
	"Agreed. Newcomers deserve a fair start, and we intend to give them one.",
	"We support the expansion and thank everyone carrying it forward.",
	"A principled stand. We would sign our name beside it.",
	"Our view differs on timing, not on direction. The goal is right.",
	"We agree that the colonies need light customs while they grow.",
	"This is a reasonable request. We hope the authorities take it seriously.",
	"The Pokemon Pirates support honest charts and published hazard records on every lane.",
	"Agreed. Prompt settlement keeps yards building and fleets sailing.",
	"We welcome the debate. Courtesy on the board makes for better policy.",
	"A clear case. We would add that berth queues should be served in order of arrival.",
	"We support this fully. Fair dealing has never cost us a voyage.",
	"Well argued. Shared risk and shared reward build more ships than any single purse.",
	"The Pokemon Pirates agree that every lost hull deserves an honest record.",
	"We support stronger penalties for tampering with sealed freight.",
	"Agreed. The duty of the strong is to trade fairly and leave room for others.",
	"We would welcome more of this kind of discussion on the board.",
	"A sound principle. Steady repayment builds trust on both sides of a loan.",
	"This is how the trade grows: more routes, more ships, more honest dealing.",
	"Our allies and we agree. We hope the rest of the board does too.",
	"We support the right of carriers to refuse freight their hulls cannot safely carry.",
	"Agreed. Harbourmasters deserve fair pay and the authority to keep order.",
	"The Pokemon Pirates back this measure and will trade by it.",
	"A fair point well made. Predictability is worth more than any single concession.",
	"We would add that regional banks should lend where the trade is growing.",
	"Agreed. Policy should reward the careful captain, not the reckless one.",
	"This is a reasonable proposal and we support it.",
	"We stand by our allies and by every company that trades honestly.",
	"Well said. Open water belongs to every honest hull.",
}

func bulletinStage(stage string) bool {
	return stage == "bulletin-thread" || stage == "bulletin-reply"
}

func bulletinPath(path string) bool {
	return path == "/v1/bulletin/thread" || path == "/v1/bulletin/reply"
}

func optionalControlPath(path string) bool {
	return bulletinPath(path) || path == "/v1/fleet/rename-ship"
}

func bulletinAuthorizationError(ctx context.Context, status int, err error) error {
	if canceled := ctx.Err(); canceled != nil {
		return errors.Join(canceled, err)
	}
	if errors.Is(err, ErrExecutionStopped) {
		return err
	}
	return errors.Join(fmt.Errorf("%w after definitive bulletin HTTP %d", ErrExecutionStopped, status), err)
}

func tradingActionsPending(actions []store.Action) bool {
	for _, action := range actions {
		if !optionalControlPath(action.Path) {
			return true
		}
	}
	return false
}

func (state bulletinState) due() time.Time {
	return maxTime(maxTime(state.NextDue, state.CooldownUntil), state.LastPostAt.Add(bulletinInterval))
}

func (state *bulletinState) rememberReply(id string) {
	if slices.Contains(state.RepliedThreadIDs, id) {
		return
	}
	state.RepliedThreadIDs = append(state.RepliedThreadIDs, id)
	if len(state.RepliedThreadIDs) > bulletinReplyHistoryLimit {
		state.RepliedThreadIDs = state.RepliedThreadIDs[len(state.RepliedThreadIDs)-bulletinReplyHistoryLimit:]
	}
}

func (r *Runner) bulletinState() (bulletinState, error) {
	var state bulletinState
	if raw := r.snapshot["_bulletin"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &state); err != nil {
			return state, errors.New("invalid durable bulletin schedule")
		}
	}
	if state.ThreadCursor < 0 || state.ReplyCursor < 0 || len(state.RepliedThreadIDs) > bulletinReplyHistoryLimit {
		return state, errors.New("invalid durable bulletin pool cursor or reply history")
	}
	if state.ReplyScan != nil && (state.ReplyScan.Since.IsZero() || !validBulletinScan(state.ReplyScan.Cursor, state.ReplyScan.Candidates)) {
		return state, errors.New("invalid durable bulletin reply scan")
	}
	return state, nil
}

func (r *Runner) saveBulletinState(ctx context.Context, state bulletinState) error {
	r.snapshot["_bulletin"] = encode(state)
	return r.db.SaveSnapshot(ctx, r.account.ID, r.snapshot)
}

func (r *Runner) driveBulletin(ctx context.Context, mission *store.Mission, data *missionData) (changed bool, err error) {
	if !r.opts.Execute {
		return false, nil
	}
	state, err := r.bulletinState()
	if err != nil {
		return false, err
	}
	defer func() {
		if err == nil || ctx.Err() != nil || errors.Is(err, ErrExecutionStopped) {
			return
		}
		var upstream *gameapi.APIError
		if errors.As(err, &upstream) && gameapi.BulletinAuthorizationRefusal(upstream.Status, upstream.Body) {
			err = bulletinAuthorizationError(ctx, upstream.Status, err)
			return
		}
		// Optional public reads and writes must never consume the trading retry loop.
		if current, stateErr := r.bulletinState(); stateErr == nil {
			state = current
		}
		state.CooldownUntil = maxTime(state.CooldownUntil, time.Now().UTC().Add(bulletinRetryDelay))
		if upstream != nil {
			state.CooldownUntil = maxTime(state.CooldownUntil, upstream.RetryAfter)
		}
		if data.Action != nil {
			data.CooldownUntil = maxTime(data.CooldownUntil, state.CooldownUntil)
			err = errors.Join(err, r.saveMission(ctx, mission, data))
		}
		err = errors.Join(err, r.saveBulletinState(ctx, state))
		_ = r.decision(ctx, "__bulletin", "bulletin", "public posting skipped after an unavailable observation or unresolved request; retry cooldown recorded", nil)
	}()
	if mission != nil && mission.Phase == "done" && data.Action == nil {
		return true, r.db.DeleteMission(ctx, r.account.ID, mission.ShipID)
	}
	if mission != nil && time.Now().Before(data.CooldownUntil) {
		r.wakeBy(data.CooldownUntil)
		return false, r.holdExpected(ctx, mission.ShipID, "public posting waits for its scoped persisted retry")
	}
	if mission != nil && data.Action != nil {
		until := maxTime(state.CooldownUntil, data.CooldownUntil)
		if time.Now().Before(until) {
			r.wakeBy(until)
			return false, r.decision(ctx, "__bulletin", "bulletin", "public posting waits for its persisted cooldown", map[string]any{"until": until})
		}
		requestCtx, cancel := context.WithTimeout(ctx, bulletinRequestBudget)
		changed, err = r.dispatchWithOutcomeContext(requestCtx, ctx, mission, data)
		cancel()
		if err == nil && !changed && data.Action != nil {
			data.CooldownUntil = maxTime(data.CooldownUntil, time.Now().UTC().Add(bulletinRetryDelay))
			state.CooldownUntil = maxTime(state.CooldownUntil, data.CooldownUntil)
			err = r.saveBulletinState(ctx, state)
			if err == nil {
				err = r.saveMission(ctx, mission, data)
			}
			if err == nil {
				err = r.decision(ctx, "__bulletin", "bulletin", "ambiguous public post retained with its original key; authoritative company posts are checked before any resend", nil)
			}
		}
		return changed, err
	}
	if time.Now().Before(state.due()) {
		r.wakeBy(state.due())
		return false, r.decision(ctx, "__bulletin", "bulletin", "public posting waits for the three-real-hour schedule or cooldown", map[string]any{"until": state.due()})
	}
	if mission != nil {
		if data.Bulletin == nil {
			return false, errors.New("bulletin mission has no immutable post intent")
		}
		if !bulletinCuratedIntent(data.Bulletin) {
			return true, r.db.DeleteMission(ctx, r.account.ID, mission.ShipID)
		}
		return true, r.stageBulletin(ctx, mission, data)
	}
	pending, err := r.db.PendingActions(ctx, r.account.ID)
	if err != nil {
		return false, err
	}
	for _, action := range pending {
		if bulletinPath(action.Path) {
			return false, r.decision(ctx, "__bulletin", "bulletin", "public posting skipped: an orphaned bulletin request requires its original mission", nil)
		}
	}
	var profile struct {
		Company struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"company"`
	}
	if json.Unmarshal(r.snapshot["profile"], &profile) != nil || profile.Company.ID != r.account.CompanyID || profile.Company.Name != names.DefaultCompany {
		return false, r.decision(ctx, "__bulletin", "bulletin", "public posting skipped: the recorded company is not The Pokemon Pirates", nil)
	}
	intent := &bulletinIntent{CompanyName: profile.Company.Name, ReplyTurn: state.NextReply}
	intent.NotBefore, _, _ = r.serverBounds()
	if intent.NotBefore.IsZero() {
		return false, errors.New("bulletin staging requires an authoritative server clock")
	}
	requestCtx, cancel := context.WithTimeout(ctx, bulletinRequestBudget)
	defer cancel()
	if state.NextReply {
		ready, err := r.selectBulletinReply(requestCtx, &state, intent)
		if err != nil || !ready {
			return false, err
		}
	} else {
		var page bulletinPage
		if err := r.api.GetOnce(requestCtx, "/v1/bulletin?limit=100", &page); err != nil {
			return false, err
		}
		if page.Threads == nil {
			return false, errors.New("bulletin list omitted its threads")
		}
		for _, thread := range page.Threads {
			if uuid.Validate(thread.ID) == nil && len(intent.BeforeIDs) < bulletinReplyHistoryLimit {
				intent.BeforeIDs = append(intent.BeforeIDs, thread.ID)
			}
		}
	}
	state.ReplyScan = nil
	if intent.ThreadID != "" {
		intent.TemplateIndex = state.ReplyCursor % len(bulletinReplies)
		intent.Body = bulletinReplies[intent.TemplateIndex]
	} else {
		intent.TemplateIndex = state.ThreadCursor % len(bulletinThreads)
		template := bulletinThreads[intent.TemplateIndex]
		intent.Title, intent.Body = template.Title, template.Body
	}
	if err := validBulletinText(intent.Title, intent.Body); err != nil {
		return false, err
	}
	if err := r.saveBulletinState(ctx, state); err != nil {
		return false, err
	}
	mission = &store.Mission{AccountID: r.account.ID, ShipID: "__bulletin", Phase: "bulletin-thread", Generation: uuid.NewString()}
	*data = missionData{Plan: Plan{Kind: "bulletin"}, Bulletin: intent, CostsKnown: true}
	return true, r.stageBulletin(ctx, mission, data)
}

func validBulletinScan(cursor string, candidates []string) bool {
	if len(cursor) > bulletinCursorLimit || len(candidates) > bulletinPageLimit {
		return false
	}
	for _, id := range candidates {
		if uuid.Validate(id) != nil {
			return false
		}
	}
	return true
}

func (r *Runner) selectBulletinReply(ctx context.Context, state *bulletinState, intent *bulletinIntent) (bool, error) {
	if state.ReplyScan == nil {
		state.ReplyScan = &bulletinReplyScan{Since: intent.NotBefore.Add(-72 * time.Hour)}
	}
	scan := state.ReplyScan
	if len(scan.Candidates) == 0 && !scan.Complete {
		path := "/v1/bulletin?limit=100&since=" + url.QueryEscape(scan.Since.Format(time.RFC3339Nano))
		if scan.Cursor != "" {
			path += "&cursor=" + url.QueryEscape(scan.Cursor)
		}
		var page bulletinPage
		if err := r.api.GetOnce(ctx, path, &page); err != nil {
			return false, err
		}
		if page.Threads == nil || len(page.Threads) > bulletinPageLimit || len(page.NextCursor) > bulletinCursorLimit || page.NextCursor != "" && page.NextCursor == scan.Cursor {
			return false, errors.New("bulletin reply page is incomplete or has an invalid cursor")
		}
		scan.Complete = page.NextCursor == "" || len(page.Threads) > 0 && page.Threads[len(page.Threads)-1].Archived
		scan.Cursor = page.NextCursor
		slices.SortFunc(page.Threads, func(left, right bulletinThread) int {
			if order := right.LastPostAt.Compare(left.LastPostAt); order != 0 {
				return order
			}
			return strings.Compare(left.ID, right.ID)
		})
		for _, thread := range page.Threads {
			if thread.CompanyName == "The Kraken Cartel" && !thread.Archived && !thread.Removed && uuid.Validate(thread.ID) == nil && !slices.Contains(state.RepliedThreadIDs, thread.ID) {
				scan.Candidates = append(scan.Candidates, thread.ID)
			}
			if uuid.Validate(thread.ID) == nil && len(intent.BeforeIDs) < bulletinReplyHistoryLimit {
				intent.BeforeIDs = append(intent.BeforeIDs, thread.ID)
			}
		}
		if err := r.saveBulletinState(ctx, *state); err != nil {
			return false, err
		}
	}
	for len(scan.Candidates) > 0 {
		id := scan.Candidates[0]
		var detail bulletinDetail
		if err := r.api.GetOnce(ctx, "/v1/bulletin/thread?id="+url.QueryEscape(id), &detail); err != nil {
			return false, err
		}
		if detail.Thread.ID != id || detail.Posts == nil {
			return false, errors.New("bulletin reply observation omitted the selected thread or posts")
		}
		alreadyReplied := false
		for _, post := range detail.Posts {
			alreadyReplied = alreadyReplied || post.CompanyName == intent.CompanyName && !post.Opening
		}
		if !alreadyReplied && !detail.Thread.Archived && !detail.Thread.Removed && detail.Thread.CompanyName == "The Kraken Cartel" {
			intent.ThreadID, intent.BeforeIDs = id, nil
			for _, post := range detail.Posts {
				if post.CompanyName == intent.CompanyName && !post.CreatedAt.Before(intent.NotBefore) && uuid.Validate(post.ID) == nil && len(intent.BeforeIDs) < bulletinReplyHistoryLimit {
					intent.BeforeIDs = append(intent.BeforeIDs, post.ID)
				}
			}
			return true, nil
		}
		scan.Candidates = scan.Candidates[1:]
		if alreadyReplied {
			state.rememberReply(id)
		}
		if err := r.saveBulletinState(ctx, *state); err != nil {
			return false, err
		}
	}
	if scan.Complete {
		return true, nil
	}
	state.CooldownUntil = time.Now().UTC().Add(bulletinRetryDelay)
	if err := r.saveBulletinState(ctx, *state); err != nil {
		return false, err
	}
	return false, r.decision(ctx, "__bulletin", "bulletin", "public reply search saved its next page for a later idle pass", nil)
}

func (r *Runner) stageBulletin(ctx context.Context, mission *store.Mission, data *missionData) error {
	intent := data.Bulletin
	stage, path := "bulletin-thread", "/v1/bulletin/thread"
	if intent.ThreadID != "" {
		stage, path = "bulletin-reply", "/v1/bulletin/reply"
	}
	mission.Phase = stage
	if err := r.decision(ctx, "__bulletin", "bulletin", "staging an immutable public company post after trading was idle", intent); err != nil {
		return err
	}
	return r.stage(ctx, mission, data, ShipState{}, stage, path, bulletinPayload{Title: intent.Title, Body: intent.Body, ThreadID: intent.ThreadID})
}

func validBulletinText(title, body string) error {
	if utf8.RuneCountInString(title) > 120 || strings.TrimSpace(body) == "" || utf8.RuneCountInString(body) > 1000 || strings.ContainsAny(title+body, "\u2013\u2014") {
		return errors.New("bulletin content exceeds public text limits or contains a forbidden dash")
	}
	return nil
}

func (r *Runner) bulletinPreflight(data *missionData) error {
	intent, action := data.Bulletin, data.Action
	if intent == nil || action == nil || intent.CompanyName != names.DefaultCompany || intent.NotBefore.IsZero() {
		return errors.New("bulletin request lacks its immutable company intent")
	}
	var request bulletinPayload
	if json.Unmarshal(action.Body, &request) != nil || request.Title != intent.Title || request.Body != intent.Body || request.ThreadID != intent.ThreadID || action.Method != http.MethodPost {
		return errors.New("bulletin request differs from its immutable post intent")
	}
	if intent.ThreadID == "" && (intent.Title == "" || action.Stage != "bulletin-thread" || action.Path != "/v1/bulletin/thread") || intent.ThreadID != "" && (intent.Title != "" || action.Stage != "bulletin-reply" || action.Path != "/v1/bulletin/reply") {
		return errors.New("bulletin request stage differs from its thread or reply payload")
	}
	if !bulletinCuratedIntent(intent) {
		return errors.New("public post content must come only from the curated company voice")
	}
	return validBulletinText(intent.Title, intent.Body)
}

func bulletinCuratedIntent(intent *bulletinIntent) bool {
	if intent == nil || intent.TemplateIndex < 0 {
		return false
	}
	if intent.ThreadID != "" {
		return intent.TemplateIndex < len(bulletinReplies) && intent.Title == "" && intent.Body == bulletinReplies[intent.TemplateIndex]
	}
	return intent.TemplateIndex < len(bulletinThreads) && intent.Title == bulletinThreads[intent.TemplateIndex].Title && intent.Body == bulletinThreads[intent.TemplateIndex].Body
}

func bulletinCooldown(status int, code, detail string, retryAfter, now time.Time) time.Time {
	until := now.Add(6 * time.Hour)
	if status == http.StatusTooManyRequests {
		until = maxTime(retryAfter, gameapi.BulletinDetailDeadline(detail, now))
		if !until.After(now) {
			until = now.Add(time.Hour)
		}
	} else if status == http.StatusForbidden && code == "posting_muted" {
		until = maxTime(retryAfter, gameapi.BulletinDetailDeadline(detail, now))
		if !until.After(now) {
			until = now.Add(24 * time.Hour)
		}
	}
	return maxTime(until, retryAfter)
}

func (r *Runner) consumeBulletinResult(ctx context.Context, mission *store.Mission, data *missionData, status int, response json.RawMessage) (bool, error) {
	if data.Bulletin == nil {
		return false, errors.New("bulletin result has no durable company intent")
	}
	if status >= 400 && status < 500 {
		state, err := r.bulletinState()
		if err != nil {
			return false, err
		}
		if state.LastOutcomeID != data.Action.ID {
			var problem struct {
				Code   string `json:"code"`
				Detail string `json:"detail"`
			}
			_ = json.Unmarshal(response, &problem)
			state.CooldownUntil = bulletinCooldown(status, problem.Code, problem.Detail, data.CooldownUntil, time.Now().UTC())
			state.LastOutcomeID = data.Action.ID
			if err := r.saveBulletinState(ctx, state); err != nil {
				return false, err
			}
		}
		if err := r.decision(ctx, "__bulletin", "bulletin", "public post refused; company posting cooldown recorded without delaying trade", map[string]any{"status": status, "until": state.CooldownUntil}); err != nil {
			return false, err
		}
		data.CooldownUntil = state.CooldownUntil
		return r.finishBulletinMission(ctx, mission, data)
	}
	if status < 200 || status >= 300 {
		return false, nil
	}
	if data.Action.Stage == "bulletin-reply" {
		var post bulletinPost
		if json.Unmarshal(response, &post) == nil && matchesBulletinPost(data.Bulletin, post) {
			return r.completeBulletinPost(ctx, mission, data, post)
		}
	} else {
		var detail bulletinDetail
		if json.Unmarshal(response, &detail) == nil && detail.Thread.Title == data.Bulletin.Title && detail.Thread.CompanyName == data.Bulletin.CompanyName {
			for _, post := range detail.Posts {
				if post.ThreadID == detail.Thread.ID && matchesBulletinPost(data.Bulletin, post) {
					return r.completeBulletinPost(ctx, mission, data, post)
				}
			}
		}
	}
	if !data.Bulletin.ReconcilePending {
		data.Bulletin.ReconcilePending = true
		return false, r.saveMission(ctx, mission, data)
	}
	return r.reconcileBulletin(ctx, mission, data, &store.Action{ID: data.Action.ID, State: store.ActionComplete})
}

func matchesBulletinPost(intent *bulletinIntent, post bulletinPost) bool {
	return intent != nil && uuid.Validate(post.ID) == nil && uuid.Validate(post.ThreadID) == nil && post.CompanyName == intent.CompanyName && post.Body == intent.Body && !post.Removed && !post.Deleted &&
		post.Opening == (intent.ThreadID == "") && (intent.ThreadID == "" || intent.ThreadID == post.ThreadID) && !post.CreatedAt.IsZero() &&
		!post.CreatedAt.Before(intent.NotBefore) && !slices.Contains(intent.BeforeIDs, post.ID)
}

func (r *Runner) completeBulletinPost(ctx context.Context, mission *store.Mission, data *missionData, post bulletinPost) (bool, error) {
	state, err := r.bulletinState()
	if err != nil {
		return false, err
	}
	if state.LastOutcomeID != data.Action.ID {
		intent := data.Bulletin
		state.LastOutcomeID, state.LastPostID, state.LastPostAt = data.Action.ID, post.ID, post.CreatedAt
		state.NextDue = maxTime(time.Now().UTC(), post.CreatedAt).Add(bulletinInterval)
		state.CooldownUntil = time.Time{}
		state.ReplyScan = nil
		state.NextReply = !intent.ReplyTurn
		if intent.ThreadID == "" {
			state.ThreadCursor = (intent.TemplateIndex + 1) % len(bulletinThreads)
		} else {
			state.ReplyCursor = (intent.TemplateIndex + 1) % len(bulletinReplies)
			state.rememberReply(intent.ThreadID)
		}
		if err := r.saveBulletinState(ctx, state); err != nil {
			return false, err
		}
	}
	if err := r.decision(ctx, "__bulletin", "bulletin", "public company post confirmed; next three-real-hour slot recorded", map[string]any{"post_id": post.ID, "thread_id": post.ThreadID, "posted_at": post.CreatedAt, "next_due": state.NextDue}); err != nil {
		return false, err
	}
	return r.finishBulletinMission(ctx, mission, data)
}

func (r *Runner) finishBulletinMission(ctx context.Context, mission *store.Mission, data *missionData) (bool, error) {
	data.LastActionID = data.Action.ID
	data.Action = nil
	data.Sequence++
	mission.Phase = "done"
	return true, r.saveMission(ctx, mission, data)
}

func (r *Runner) restoreBulletinReconciliation(ctx context.Context, mission *store.Mission, data *missionData, response json.RawMessage) (bool, error) {
	var evidence struct {
		Kind string       `json:"kind"`
		Post bulletinPost `json:"post"`
	}
	if json.Unmarshal(response, &evidence) != nil || evidence.Kind != "authoritative_bulletin_post" || !matchesBulletinPost(data.Bulletin, evidence.Post) {
		return false, errors.New("reconciled bulletin request lacks its exact company post")
	}
	return r.completeBulletinPost(ctx, mission, data, evidence.Post)
}

func (r *Runner) reconcileBulletin(ctx context.Context, mission *store.Mission, data *missionData, action *store.Action) (bool, error) {
	intent := data.Bulletin
	if intent == nil {
		return false, errors.New("unresolved bulletin request lacks its immutable company intent")
	}
	if time.Now().Before(data.CooldownUntil) {
		return false, nil
	}
	if intent.ReconcileScan == nil {
		intent.ReconcileScan = &bulletinReconcileScan{}
		if intent.ThreadID != "" {
			intent.ReconcileScan.Candidates = []string{intent.ThreadID}
			intent.ReconcileScan.Complete = true
		}
	}
	scan := intent.ReconcileScan
	if !validBulletinScan(scan.Cursor, scan.Candidates) || scan.Match != nil && !matchesBulletinPost(intent, *scan.Match) {
		return false, errors.New("unresolved bulletin request has an invalid reconciliation scan")
	}
	if len(scan.Candidates) == 0 && !scan.Complete {
		path := "/v1/bulletin?limit=100&since=" + url.QueryEscape(intent.NotBefore.Format(time.RFC3339Nano))
		if scan.Cursor != "" {
			path += "&cursor=" + url.QueryEscape(scan.Cursor)
		}
		var page bulletinPage
		if err := r.api.GetOnce(ctx, path, &page); err != nil {
			return false, err
		}
		if page.Threads == nil || len(page.Threads) > bulletinPageLimit || len(page.NextCursor) > bulletinCursorLimit || page.NextCursor != "" && page.NextCursor == scan.Cursor {
			return false, errors.New("bulletin reconciliation page is incomplete or has an invalid cursor")
		}
		scan.Cursor, scan.Complete = page.NextCursor, page.NextCursor == ""
		for _, thread := range page.Threads {
			if thread.CompanyName == intent.CompanyName && thread.Title == intent.Title && !thread.CreatedAt.Before(intent.NotBefore) &&
				uuid.Validate(thread.ID) == nil && !slices.Contains(intent.BeforeIDs, thread.ID) && !slices.Contains(scan.Candidates, thread.ID) {
				scan.Candidates = append(scan.Candidates, thread.ID)
			}
		}
		if err := r.saveMission(ctx, mission, data); err != nil {
			return false, err
		}
	}
	for len(scan.Candidates) > 0 {
		id := scan.Candidates[0]
		var detail bulletinDetail
		if err := r.api.GetOnce(ctx, "/v1/bulletin/thread?id="+url.QueryEscape(id), &detail); err != nil {
			return false, err
		}
		if detail.Thread.ID != id || detail.Posts == nil {
			return false, errors.New("bulletin reconciliation omitted the requested thread or posts")
		}
		for _, post := range detail.Posts {
			if post.ThreadID != id || !matchesBulletinPost(intent, post) {
				continue
			}
			if scan.Match != nil && scan.Match.ID != post.ID {
				return false, errors.New("multiple company bulletin posts match the unresolved request")
			}
			copy := post
			scan.Match = &copy
		}
		scan.Candidates = scan.Candidates[1:]
		if err := r.saveMission(ctx, mission, data); err != nil {
			return false, err
		}
	}
	if !scan.Complete {
		state, err := r.bulletinState()
		if err != nil {
			return false, err
		}
		data.CooldownUntil = time.Now().UTC().Add(bulletinRetryDelay)
		state.CooldownUntil = maxTime(state.CooldownUntil, data.CooldownUntil)
		if err := r.saveMission(ctx, mission, data); err != nil {
			return false, err
		}
		if err := r.saveBulletinState(ctx, state); err != nil {
			return false, err
		}
		return true, r.decision(ctx, "__bulletin", "bulletin", "public post reconciliation saved its next page without resending the immutable request", nil)
	}
	if scan.Match == nil {
		intent.ReconcileScan = nil
		if !bulletinCuratedIntent(intent) {
			return false, errors.New("legacy public post remains reconciliation-only; private facts are never resent")
		}
		return false, r.saveMission(ctx, mission, data)
	}
	if action.State != store.ActionComplete {
		evidence := encode(map[string]any{"kind": "authoritative_bulletin_post", "post": scan.Match})
		if err := r.db.ReconcileAction(ctx, action.ID, evidence); err != nil {
			return false, err
		}
	}
	return r.completeBulletinPost(ctx, mission, data, *scan.Match)
}
