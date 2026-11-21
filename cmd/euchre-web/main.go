// Command euchre-web is the headless web-server mode of the euchre app: the
// browser is the South player's UI over HTTP, while the proxenos stream side
// behaves exactly like the terminal app (cmd/euchre). South is the human via
// the HTTP API; a built-in heuristic AI plays the other seats until an agent
// claims them over the stream (claim_seat / bid / play / hand_request / tip /
// resign all work as in the TUI), with the same per-seat --agent-timeout
// fallback. The stream contract, event vocabulary and replay-safe startup are
// identical to the TUI; the contract is embedded verbatim from cmd/euchre.
//
// HTTP surface:
//
//	GET  /             the embedded frontend (web/)
//	GET  /api/events   SSE: full state snapshot on connect, then after every
//	                   state change; heartbeat comment every 15s
//	POST /api/bid      {"decision":"order_up"|"pass"|"<suit>","goingAlone":bool}
//	POST /api/play     {"card":"K♠"}   (glyph or code form accepted)
//	POST /api/discard  {"card":"…"}
//	POST /api/new      start a fresh game on the same stream
//
// Every successful POST returns the fresh state snapshot; validation errors
// return 409 {"error":"…"}. With --token set, every endpoint requires ?t=.
package main

import (
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"euchre-demo/internal/euchre"
	"euchre-demo/internal/prox"
)

//go:embed proxenos-contract.json
var contractJSON []byte

type config struct {
	stream       string
	addr         string
	token        string
	seed         int64
	agentTimeout time.Duration
	proxenosBin  string
}

func main() {
	cfg := config{}
	flag.StringVar(&cfg.stream, "stream", "euchre-web", "proxenos stream name")
	flag.StringVar(&cfg.addr, "addr", "0.0.0.0:8090", "HTTP listen address")
	flag.StringVar(&cfg.token, "token", "", "require ?t=<token> on every endpoint when set")
	flag.Int64Var(&cfg.seed, "seed", 1, "deterministic shuffle seed for game 1 (later games are seeded from the clock)")
	flag.DurationVar(&cfg.agentTimeout, "agent-timeout", 30*time.Second, "how long to wait for the agent on a claimed seat's turn before the built-in bot fills in")
	flag.StringVar(&cfg.proxenosBin, "proxenos", "proxenos", "path to the proxenos binary")
	flag.Parse()

	if err := run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "euchre-web:", err)
		os.Exit(1)
	}
}

// ── app state ─────────────────────────────────────────────────────────────

type app struct {
	cfg    config
	game   *euchre.Game // owned by the loop goroutine only
	client *prox.Client
	proxOK bool

	hub *hub // published snapshots (SSE + POST replies)

	agentC chan prox.Event // inbound live agent events (from the poller)
	pending []prox.Event   // bid/play deferred by control drains (loop goroutine only)
	reqCh  chan *req       // inbound HTTP decisions

	waitKind string // "bid"|"play"|"discard" while the loop awaits South (loop goroutine only)

	mu       sync.Mutex // guards claimed, disclose, tips, logLines (poller + loop)
	claimed  [4]bool    // seats the agent has claimed (south is never claimable)
	disclose [4]bool    // newly claimed seats whose hand is disclosed at the next decision point
	tips     []string
	logLines []string

	matchNo        int
	summarizedHand int
	doneCh         chan struct{} // closed when the loop goroutine exits
}

func newApp(cfg config) *app {
	return &app{
		cfg:    cfg,
		hub:    newHub(),
		agentC: make(chan prox.Event, 256),
		reqCh:  make(chan *req, 64),
		doneCh: make(chan struct{}),
	}
}

// Sentinel control-flow errors for the decision paths.
var (
	errRestart = errors.New("new game requested")
	errOver    = errors.New("game is over")
)

func run(cfg config) error {
	a := newApp(cfg)

	// proxenos: middleware trouble never blocks the game (the bibtui rule).
	if _, err := exec.LookPath(cfg.proxenosBin); err != nil {
		a.logf("proxenos not on PATH (%v) — playing without the agent partner", err)
	} else {
		a.client = prox.NewClient(cfg.proxenosBin, cfg.stream)
		if err := a.client.EnsureStream(contractJSON); err != nil {
			a.logf("stream setup failed (%v) — playing without the agent partner", err)
		} else if path, err := a.client.ResolvePath(); err != nil {
			a.logf("stream path resolve failed (%v) — playing without the agent partner", err)
		} else {
			a.proxOK = true
			// Snapshot the stream size at startup: events wholly below this
			// offset predate this process. The poller delivers them flagged as
			// replay and the app applies them for STATE only (claims) — old
			// bids/plays/hand_requests from previous runs are never validated
			// or rejected again (same as the TUI).
			var startOffset int64
			if st, err := os.Stat(path); err == nil {
				startOffset = st.Size()
			}
			prox.StartPoll(path, startOffset, a.ingest, a.logf)
		}
	}

	go a.loop()

	return a.serve()
}

// ── inbound agent events ──────────────────────────────────────────────────

// ingest classifies one inbound stream event (poller goroutine). Replayed
// events (wholly below this process's startup snapshot) only update state —
// they are never validated or rejected; live events go to the loop.
func (a *app) ingest(ev prox.Event, replay bool) {
	if ev.Src == prox.AppSrc || strings.HasPrefix(ev.Src, "sys:") {
		return // our own emits echoed back, and proxenos metadata
	}
	if replay {
		a.applyReplay(ev)
		return
	}
	select {
	case a.agentC <- ev:
	default:
		a.logf("agent event dropped (queue full): %s", ev.Type)
	}
}

// applyReplay applies an event from before this process started for STATE
// only. Seat claims still stand (the agent expects to hold them — an old
// claim must survive a restart); old bids, plays, hand requests, tips and
// resigns belong to a previous run and are silently skipped.
func (a *app) applyReplay(ev prox.Event) {
	switch ev.Type {
	case "euchre.claim_seat":
		seats, release, err := parseClaim(ev.Body)
		if err != nil {
			a.logf("replay: skipping malformed %s from a previous run", ev.Type)
			return
		}
		a.mu.Lock()
		var msgs []string
		if release {
			for _, s := range seats {
				a.claimed[s] = false
			}
			a.mu.Unlock()
			return
		}
		for _, s := range seats {
			if s == euchre.South {
				continue // never claimable; nothing to apply
			}
			if !a.claimed[s] {
				a.claimed[s] = true
				a.disclose[s] = true
				msgs = append(msgs, fmt.Sprintf("replay: %s seat claim from a previous run still stands", s))
			}
		}
		a.mu.Unlock()
		for _, m := range msgs {
			a.logf("%s", m)
		}
	default:
		a.logf("replay: ignoring %s from a previous run (state only)", ev.Type)
	}
}

// parseClaim extracts the seats and release flag from a euchre.claim_seat
// body (singular "seat" and/or plural "seats").
func parseClaim(body []byte) (seats []euchre.Seat, release bool, err error) {
	var parsed struct {
		Seat    string   `json:"seat"`
		Seats   []string `json:"seats"`
		Release bool     `json:"release"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, false, err
	}
	names := parsed.Seats
	if parsed.Seat != "" {
		names = append([]string{parsed.Seat}, names...)
	}
	if len(names) == 0 {
		return nil, false, fmt.Errorf("no seats given")
	}
	out := make([]euchre.Seat, 0, len(names))
	for _, n := range names {
		s, err := euchre.ParseSeat(n)
		if err != nil {
			return nil, false, fmt.Errorf("bad seat %q", n)
		}
		out = append(out, s)
	}
	return out, parsed.Release, nil
}

// applyAgentEvent handles an agent event on the loop goroutine: seat
// claims/releases, hand requests, tips, and resign. (Verbatim semantics from
// the TUI app.)
func (a *app) applyAgentEvent(ev prox.Event) {
	switch ev.Type {
	case "euchre.claim_seat":
		seats, release, err := parseClaim(ev.Body)
		if err != nil {
			a.reject(ev.Type, `bad body: `+err.Error()+` (use {"seat":"north"} or {"seats":["north","east","west"]})`)
			return
		}
		for _, s := range seats {
			if s == euchre.South {
				a.reject(ev.Type, "the south seat is the human's")
				return
			}
		}
		a.mu.Lock()
		var msgs []string
		if release {
			for _, s := range seats {
				if a.claimed[s] {
					a.claimed[s] = false
					msgs = append(msgs, fmt.Sprintf("agent released the %s seat — built-in bot takes over", s))
				}
			}
			a.mu.Unlock()
			for _, m := range msgs {
				a.logf("%s", m)
			}
			return
		}
		// Claims are additive and idempotent per seat; a newly claimed seat
		// starts playing at the next decision point, with its hand disclosed
		// then.
		for _, s := range seats {
			if !a.claimed[s] {
				a.claimed[s] = true
				a.disclose[s] = true
				msgs = append(msgs, fmt.Sprintf("agent claimed the %s seat", s))
			}
		}
		a.mu.Unlock()
		for _, m := range msgs {
			a.logf("%s", m)
		}
	case "euchre.hand_request":
		var body struct {
			Seat string `json:"seat"`
		}
		if err := ev.DecodeBody(&body); err != nil {
			a.reject(ev.Type, "bad body: "+err.Error())
			return
		}
		s, err := euchre.ParseSeat(body.Seat)
		if err != nil {
			a.reject(ev.Type, fmt.Sprintf("bad seat %q", body.Seat))
			return
		}
		if !a.isClaimed(s) {
			a.reject(ev.Type, "hand_request is only for claimed seats")
			return
		}
		a.emitHandFor(s)
	case "euchre.tip":
		if ev.Src == prox.AppSrc {
			a.reject(ev.Type, "euchre.tip is agent-only")
			return
		}
		var body struct {
			Text string `json:"text"`
		}
		if err := ev.DecodeBody(&body); err != nil {
			a.reject(ev.Type, "bad body: "+err.Error())
			return
		}
		a.mu.Lock()
		a.tips = append(a.tips, body.Text)
		a.mu.Unlock()
		a.logf("tip: %s", body.Text)
	case "euchre.resign":
		if ev.Src == prox.AppSrc {
			a.reject(ev.Type, "euchre.resign is not for the app")
			return
		}
		// The agent's primary team concedes: North-South if it holds North
		// (its classic partner seat), otherwise the East-West seats it holds.
		team := euchre.EW
		if a.isClaimed(euchre.North) {
			team = euchre.NS
		}
		a.game.ResignAs(team)
		a.logf("agent resigned — game over")
	default:
		if len(ev.Type) >= 7 && ev.Type[:7] == "euchre." {
			a.reject(ev.Type, "not actionable right now")
		} else {
			a.logf("ignoring %s from %s", ev.Type, ev.Src)
		}
	}
}

func (a *app) isClaimed(s euchre.Seat) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.claimed[s]
}

// dispatchOther handles non-waited events: known euchre verbs are rejected
// loudly, anything else (harness context, unknown types) is ignored.
func (a *app) dispatchOther(ev prox.Event, awaiting string) {
	if len(ev.Type) >= 7 && ev.Type[:7] == "euchre." {
		a.reject(ev.Type, "not actionable right now ("+awaiting+")")
	} else {
		a.logf("ignoring %s from %s", ev.Type, ev.Src)
	}
}

// ── main loop ─────────────────────────────────────────────────────────────

// loop drives the match state machine forever: play a match to 10 points,
// then hold at game over until POST /api/new starts a fresh one.
func (a *app) loop() {
	defer close(a.doneCh)
	a.startMatch()
	for {
		err := a.playMatch()
		if err == errRestart {
			continue // startMatch already ran inside the wait
		}
		if err != nil && err != errOver {
			a.logf("loop: %v", err)
			return
		}
		// Match over (played out or resigned mid-hand).
		a.emit("game.over", map[string]any{"winner": a.game.Winner.String()})
		a.logf("GAME OVER — %s wins %d–%d", a.game.Winner, a.game.Score[euchre.NS], a.game.Score[euchre.EW])
		a.publish()
		a.waitNewGame()
	}
}

// matchDone reports whether the match is finished and fully reported. A hand
// won on its fifth trick goes straight to PhaseGameOver — the final
// hand.summary must still be emitted (the coaching hook) before the match may
// end. A mid-hand resign ends the game without a summary for the partial hand.
func (a *app) matchDone() bool {
	if !a.game.Over {
		return false
	}
	if a.game.Phase == euchre.PhaseGameOver {
		return a.summarizedHand == a.game.HandNo
	}
	return true
}

func (a *app) playMatch() error {
	for !a.matchDone() {
		if a.drainReqs() {
			return errRestart
		}
		a.flushDisclose()
		a.publish()
		var err error
		switch a.game.Phase {
		case euchre.PhaseBid1, euchre.PhaseBid2:
			err = a.doBid()
		case euchre.PhaseDiscard:
			err = a.doDiscard()
		case euchre.PhasePlay:
			err = a.doPlay()
		case euchre.PhaseHandEnd, euchre.PhaseGameOver:
			a.finishHand()
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// startMatch deals a fresh game on the same stream — the web equivalent of
// relaunching the TUI. Seat claims persist across games (a relaunched TUI
// re-applies them from replay).
func (a *app) startMatch() {
	a.matchNo++
	seed := a.cfg.seed
	if a.matchNo > 1 {
		seed = time.Now().UnixNano()
	}
	a.game = euchre.NewGame(seed, euchre.South)
	a.summarizedHand = 0
	a.pending = nil
	a.game.StartHand()
	// South's hand is deliberately NOT on the stream: it is visible in the
	// browser UI, and publishing it let an agent play South's cards for a
	// claimed seat. Claimed seats get their hands via deal.your_hand.
	a.emit("game.started", map[string]any{
		"dealer": a.game.Dealer.String(),
		"youAre": "south",
	})
	a.emitDeal()
	a.logf("new game — you are South, dealer %s", a.game.Dealer)
	a.publish()
}

// waitNewGame holds at game over until a /api/new arrives, applying agent
// control events (claims persist into the next game) along the way.
func (a *app) waitNewGame() {
	for {
		select {
		case ev := <-a.agentC:
			switch ev.Type {
			case "euchre.claim_seat", "euchre.hand_request", "euchre.tip", "euchre.resign":
				a.applyAgentEvent(ev)
				a.publish()
			default:
				a.dispatchOther(ev, "the game is over")
			}
		case r := <-a.reqCh:
			if r.kind == "new" {
				a.startMatch()
				a.replyOK(r)
				return
			}
			a.replyErr(r, "the game is over — POST /api/new to start a new game")
		}
	}
}

// drainReqs non-blockingly handles queued HTTP requests at a safe point of
// the loop. Reports whether a new game was started (the caller must unwind).
func (a *app) drainReqs() bool {
	for {
		select {
		case r := <-a.reqCh:
			if r.kind == "new" {
				a.startMatch()
				a.replyOK(r)
				return true
			}
			a.replyErr(r, fmt.Sprintf("not awaiting %q from south right now", r.kind))
		default:
			return false
		}
	}
}

// ── decisions ─────────────────────────────────────────────────────────────

// doBid resolves one bidding decision.
func (a *app) doBid() error {
	g := a.game
	seat := g.BidTurn()
	a.emitBidPrompt()

	if seat == euchre.South {
		for {
			r, err := a.waitSouth("bid")
			if err != nil {
				return err
			}
			if err := g.Bid(seat, r.call, r.alone); err != nil {
				a.replyErr(r, err.Error())
				continue
			}
			a.emitBidMade(seat, r.call, r.alone)
			a.publish()
			a.replyOK(r)
			return nil
		}
	}

	if !a.isClaimed(seat) {
		a.drainPending(500 * time.Millisecond)
	}
	if a.isClaimed(seat) {
		call, alone, ok, err := a.waitAgentBid(seat)
		if err != nil {
			return err
		}
		if ok {
			if err := g.ValidateBid(seat, call, alone); err != nil {
				a.rejectWithHand("euchre.bid", err.Error(), seat)
				a.logf("%s bid: agent bid illegal — built-in bot bids", seat)
			} else {
				if err := g.Bid(seat, call, alone); err != nil {
					return err
				}
				a.emitBidMade(seat, call, alone)
				a.publish()
				return nil
			}
		} else if !a.game.Over {
			a.logf("%s bid: agent quiet past timeout — built-in bot bids", seat)
		}
	}
	if a.game.Over {
		return nil
	}

	call, alone, err := euchre.AIBid(g, seat)
	if err != nil {
		return err
	}
	if err := g.Bid(seat, call, alone); err != nil {
		return err
	}
	a.emitBidMade(seat, call, alone)
	a.publish()
	return nil
}

func (a *app) emitBidPrompt() {
	g := a.game
	body := map[string]any{"dealer": g.Dealer.String(), "round": g.Round(), "turn": g.BidTurn().String()}
	if g.Round() == 1 {
		body["topCard"] = g.TopCard().String()
	} else {
		body["topCard"] = g.TurnDown().String()
	}
	a.emit("bid.prompt", body)
}

func (a *app) emitBidMade(seat euchre.Seat, call euchre.Call, alone bool) {
	body := map[string]any{"seat": seat.String(), "decision": string(call)}
	if alone {
		body["alone"] = true
	}
	if tr, ok := a.game.Trump(); ok {
		body["trump"] = tr.Code()
	}
	a.emit("bid.made", body)
	a.logf("%s bids %s", seat, describeBid(call, alone))
}

func describeBid(call euchre.Call, alone bool) string {
	switch call {
	case euchre.CallOrderUp:
		if alone {
			return "order up (alone)"
		}
		return "order up"
	case euchre.CallPass:
		return "pass"
	default:
		if alone {
			return string(call) + " (alone)"
		}
		return string(call)
	}
}

// doDiscard resolves the dealer's discard after an order-up.
func (a *app) doDiscard() error {
	g := a.game
	seat := g.Dealer

	var card euchre.Card
	have := false
	var accepted *req
	if seat == euchre.South {
		for {
			r, err := a.waitSouth("discard")
			if err != nil {
				return err
			}
			if err := g.ValidateDiscard(seat, r.card); err != nil {
				a.replyErr(r, err.Error())
				continue
			}
			card = r.card
			accepted = r
			have = true
			break
		}
	} else {
		if !a.isClaimed(seat) {
			a.drainPending(500 * time.Millisecond)
		}
		if a.isClaimed(seat) {
			c, ok, err := a.waitAgentCard(seat)
			if err != nil {
				return err
			}
			if !ok {
				a.logf("%s discard: agent quiet past timeout — built-in bot discards", seat)
			} else if err := g.ValidateDiscard(seat, c); err != nil {
				a.rejectWithHand("euchre.play", err.Error(), seat)
				a.logf("%s discard: agent card illegal — built-in bot discards", seat)
			} else {
				card, have = c, true
			}
		}
	}
	if !have && !a.game.Over {
		c, err := euchre.AIDiscard(g, seat)
		if err != nil {
			return err
		}
		card = c
	}
	if a.game.Over {
		return nil
	}
	if err := g.Discard(seat, card); err != nil {
		return err
	}
	who := seat.String()
	if seat == euchre.South {
		who = "you"
	}
	a.logf("%s discard%s %s", who, discardSuffix(seat), card)
	a.publish()
	if accepted != nil {
		a.replyOK(accepted)
	}
	return nil
}

func discardSuffix(seat euchre.Seat) string {
	if seat == euchre.South {
		return ""
	}
	return "s"
}

// doPlay resolves one card play.
func (a *app) doPlay() error {
	g := a.game
	seat := g.Turn()
	if len(g.Trick()) == 0 {
		trump, _ := g.Trump()
		a.emit("trick.started", map[string]any{"leader": seat.String(), "trump": trump.Code()})
	}
	before := g.TrickNo()

	var card euchre.Card
	have := false
	var accepted *req
	if seat == euchre.South {
		for {
			r, err := a.waitSouth("play")
			if err != nil {
				return err
			}
			if err := g.ValidatePlay(seat, r.card); err != nil {
				a.replyErr(r, err.Error())
				continue
			}
			card = r.card
			accepted = r
			have = true
			break
		}
	} else {
		if !a.isClaimed(seat) {
			a.drainPending(500 * time.Millisecond)
		}
		if a.isClaimed(seat) {
			c, ok, err := a.waitAgentCard(seat)
			if err != nil {
				return err
			}
			if !ok {
				a.logf("%s play: agent quiet past timeout — built-in bot plays", seat)
			} else if err := g.ValidatePlay(seat, c); err != nil {
				a.rejectWithHand("euchre.play", err.Error(), seat)
				a.logf("%s play: agent card illegal — built-in bot plays", seat)
			} else {
				card, have = c, true
			}
		}
	}
	if !have && !a.game.Over {
		c, err := euchre.AIPlay(g, seat)
		if err != nil {
			return err
		}
		card = c
	}
	if a.game.Over {
		return nil
	}
	if err := g.Play(seat, card); err != nil {
		return err
	}
	a.emit("card.played", map[string]any{"seat": seat.String(), "card": card.String(), "trickNo": before})
	a.logf("%s plays %s", seat, card)

	if g.TrickNo() != before { // the trick just completed
		last := g.Tricks()[len(g.Tricks())-1]
		a.emit("trick.won", map[string]any{"seat": last.Winner.String(), "pointsNSE": tricksNSE(g)})
		a.logf("%s takes the trick", last.Winner)
	}
	a.publish()
	if accepted != nil {
		a.replyOK(accepted)
	}
	return nil
}

func tricksNSE(g *euchre.Game) [2]int {
	return [2]int{g.TricksWon(euchre.NS), g.TricksWon(euchre.EW)}
}

// finishHand emits hand.summary and either deals the next hand or ends the game.
func (a *app) finishHand() {
	g := a.game
	a.summarizedHand = g.HandNo
	body := map[string]any{
		"scoreNSE":  [2]int{g.Score[euchre.NS], g.Score[euchre.EW]},
		"tricksNSE": tricksNSE(g),
		"dealer":    g.Dealer.String(),
		"bids":      bidsBody(g),
		"tricks":    tricksBody(g),
	}
	if seat, alone := g.Alone(); alone {
		body["goingAlone"] = map[string]any{"seat": seat.String()}
	} else {
		body["goingAlone"] = nil
	}
	a.emit("hand.summary", body)
	a.logf("hand %d done — tricks NS %d / EW %d — score NS %d / EW %d",
		g.HandNo, tricksNSE(g)[0], tricksNSE(g)[1], g.Score[euchre.NS], g.Score[euchre.EW])
	a.publish()
	if g.Over {
		return
	}
	g.Dealer = g.Dealer.Next()
	g.StartHand()
	a.emitDeal()
	a.logf("hand %d — dealer %s", g.HandNo, g.Dealer)
	a.publish()
}

// flushDisclose emits deal.your_hand for seats claimed since the last
// decision point — a mid-hand claim discloses that seat's current hand here,
// before it must act.
func (a *app) flushDisclose() {
	a.mu.Lock()
	var pending []euchre.Seat
	for s := euchre.South; s <= euchre.East; s++ {
		if a.disclose[s] {
			a.disclose[s] = false
			pending = append(pending, s)
		}
	}
	a.mu.Unlock()
	for _, s := range pending {
		a.emitHandFor(s)
	}
	if len(pending) > 0 {
		a.publish()
	}
}

// emitHandFor discloses one seat's current hand as deal.your_hand.
func (a *app) emitHandFor(s euchre.Seat) {
	hand := euchre.SortHand(a.game.Hand(s), displayTrump(a.game))
	a.emit("deal.your_hand", map[string]any{"seat": s.String(), "cards": euchre.Cards(hand).Codes()})
}

// emitDeal discloses South's hand plus every claimed seat's hand (each new
// hand re-discloses the claimed seats).
func (a *app) emitDeal() {
	a.emitHandFor(euchre.South)
	for s := euchre.West; s <= euchre.East; s++ {
		if a.isClaimed(s) {
			a.emitHandFor(s)
		}
	}
}

func bidsBody(g *euchre.Game) []map[string]any {
	var out []map[string]any
	for _, b := range g.Bids() {
		out = append(out, map[string]any{"seat": b.Seat.String(), "decision": string(b.Call), "alone": b.Alone})
	}
	return out
}

func tricksBody(g *euchre.Game) []map[string]any {
	var out []map[string]any
	for _, tr := range g.Tricks() {
		var plays []any
		for _, p := range tr.Plays {
			plays = append(plays, [2]string{p.Seat.String(), p.Card.String()})
		}
		out = append(out, map[string]any{"lead": tr.Lead.String(), "plays": plays, "winner": tr.Winner.String()})
	}
	return out
}

// ── waiting ───────────────────────────────────────────────────────────────

// waitSouth blocks until an HTTP request of the given kind arrives from the
// browser (or a new game is requested, or the game ends under us). Agent
// control events are applied along the way so claims, tips and resign land
// promptly. Only the loop goroutine calls this.
func (a *app) waitSouth(kind string) (*req, error) {
	a.waitKind = kind
	defer func() { a.waitKind = "" }()
	for {
		if a.game.Over {
			return nil, errOver
		}
		// Prioritize agent control events already queued.
		select {
		case ev := <-a.agentC:
			a.handleControlWhileWaiting(ev, kind)
			continue
		default:
		}
		select {
		case r := <-a.reqCh:
			if r.kind == "new" {
				a.startMatch()
				a.replyOK(r)
				return nil, errRestart
			}
			if r.kind != kind {
				a.replyErr(r, fmt.Sprintf("not waiting for %q now (waiting for %q)", r.kind, kind))
				continue
			}
			return r, nil
		case ev := <-a.agentC:
			a.handleControlWhileWaiting(ev, kind)
		}
	}
}

// handleControlWhileWaiting applies one agent event during a South wait.
func (a *app) handleControlWhileWaiting(ev prox.Event, kind string) {
	switch ev.Type {
	case "euchre.claim_seat", "euchre.hand_request", "euchre.tip", "euchre.resign":
		a.applyAgentEvent(ev)
		a.publish()
	default:
		a.dispatchOther(ev, "the human (south) is deciding")
	}
}

// drainPending applies buffered control events (claims, hand requests, tips,
// resign) within a short window — used before a bot-seat decision so a
// just-arrived seat claim takes effect. Bids and plays are deferred to the
// real wait. (TUI semantics.)
func (a *app) drainPending(d time.Duration) {
	deadline := time.Now().Add(d)
	for {
		var ev prox.Event
		select {
		case ev = <-a.agentC:
		case <-time.After(time.Until(deadline)):
			return
		}
		switch ev.Type {
		case "euchre.claim_seat", "euchre.hand_request", "euchre.tip", "euchre.resign":
			a.applyAgentEvent(ev)
			a.publish()
		case "euchre.bid", "euchre.play":
			a.pending = append(a.pending, ev)
			return
		default:
			a.dispatchOther(ev, "the game is deciding")
		}
	}
}

// takePending pops one deferred bid/play event.
func (a *app) takePending() (prox.Event, bool) {
	if len(a.pending) == 0 {
		return prox.Event{}, false
	}
	ev := a.pending[0]
	a.pending = a.pending[1:]
	return ev, true
}

// waitAgentBid waits up to the agent timeout for euchre.bid (claims, hand
// requests, tips and resign are applied along the way; other events are
// rejected or ignored). seat is the bidder whose hand rejections disclose.
// HTTP requests are answered (errors, or a new game) while waiting.
func (a *app) waitAgentBid(seat euchre.Seat) (euchre.Call, bool, bool, error) {
	timer := time.NewTimer(a.cfg.agentTimeout)
	defer timer.Stop()
	for {
		if a.game.Over {
			return "", false, false, nil
		}
		if ev, ok := a.takePending(); ok {
			if call, alone, done, err := a.agentBidEvent(ev, seat); done || err != nil {
				return call, alone, done, err
			}
			continue
		}
		select {
		case ev := <-a.agentC:
			if call, alone, done, err := a.agentBidEvent(ev, seat); done || err != nil {
				return call, alone, done, err
			}
		case r := <-a.reqCh:
			if a.handleReqDuringAgentWait(r) {
				return "", false, false, errRestart
			}
		case <-timer.C:
			return "", false, false, nil
		}
	}
}

// agentBidEvent processes one event inside waitAgentBid.
func (a *app) agentBidEvent(ev prox.Event, seat euchre.Seat) (call euchre.Call, alone, done bool, err error) {
	switch ev.Type {
	case "euchre.bid":
		var body struct {
			Decision   string `json:"decision"`
			GoingAlone bool   `json:"goingAlone"`
		}
		if err := ev.DecodeBody(&body); err != nil {
			a.rejectWithHand(ev.Type, "bad body: "+err.Error(), seat)
			return "", false, false, nil
		}
		return euchre.Call(strings.ToLower(body.Decision)), body.GoingAlone, true, nil
	case "euchre.claim_seat", "euchre.hand_request", "euchre.tip", "euchre.resign":
		a.applyAgentEvent(ev)
		a.publish()
		return "", false, false, nil
	default:
		a.dispatchOther(ev, "a bid is awaited")
		return "", false, false, nil
	}
}

// waitAgentCard waits up to the agent timeout for euchre.play. seat is the
// player whose hand rejections disclose.
func (a *app) waitAgentCard(seat euchre.Seat) (euchre.Card, bool, error) {
	timer := time.NewTimer(a.cfg.agentTimeout)
	defer timer.Stop()
	for {
		if a.game.Over {
			return euchre.Card{}, false, nil
		}
		if ev, ok := a.takePending(); ok {
			if c, done, err := a.agentCardEvent(ev, seat); done || err != nil {
				return c, done, err
			}
			continue
		}
		select {
		case ev := <-a.agentC:
			if c, done, err := a.agentCardEvent(ev, seat); done || err != nil {
				return c, done, err
			}
		case r := <-a.reqCh:
			if a.handleReqDuringAgentWait(r) {
				return euchre.Card{}, false, errRestart
			}
		case <-timer.C:
			return euchre.Card{}, false, nil
		}
	}
}

func (a *app) agentCardEvent(ev prox.Event, seat euchre.Seat) (c euchre.Card, done bool, err error) {
	switch ev.Type {
	case "euchre.play":
		var body struct {
			Card string `json:"card"`
		}
		if err := ev.DecodeBody(&body); err != nil {
			a.rejectWithHand(ev.Type, "bad body: "+err.Error(), seat)
			return euchre.Card{}, false, nil
		}
		c, err := euchre.ParseCard(body.Card)
		if err != nil {
			a.rejectWithHand(ev.Type, err.Error(), seat)
			return euchre.Card{}, false, nil
		}
		return c, true, nil
	case "euchre.bid", "euchre.claim_seat", "euchre.hand_request", "euchre.tip", "euchre.resign":
		a.applyAgentEvent(ev)
		a.publish()
		return euchre.Card{}, false, nil
	default:
		a.dispatchOther(ev, "a card is awaited")
		return euchre.Card{}, false, nil
	}
}

// handleReqDuringAgentWait answers an HTTP request that lands while a claimed
// agent seat is deciding. Reports whether a new game was started.
func (a *app) handleReqDuringAgentWait(r *req) bool {
	if r.kind == "new" {
		a.startMatch()
		a.replyOK(r)
		return true
	}
	a.replyErr(r, fmt.Sprintf("not waiting for %q from south right now (an agent seat is deciding)", r.kind))
	return false
}

// ── proxenos outbound ─────────────────────────────────────────────────────

func (a *app) emit(typ string, body any) {
	if !a.proxOK {
		return
	}
	if _, err := a.client.Send(typ, body); err != nil {
		a.logf("emit %s failed: %v", typ, err)
	}
}

func (a *app) reject(action, reason string) {
	a.emitRejected(action, reason, nil)
}

// rejectWithHand rejects a bid/play/discard for a claimed seat and attaches
// that seat's CURRENT hand (body.cards) so the agent can self-correct with
// zero round-trips.
func (a *app) rejectWithHand(action, reason string, seat euchre.Seat) {
	cards := euchre.Cards(euchre.SortHand(a.game.Hand(seat), displayTrump(a.game))).Codes()
	a.emitRejected(action, reason, cards)
}

func (a *app) emitRejected(action, reason string, cards []string) {
	a.logf("rejected %s: %s", action, reason)
	body := map[string]any{"action": action, "reason": reason}
	if len(cards) > 0 {
		body["cards"] = cards
	}
	a.emit("action.rejected", body)
}

func (a *app) logf(format string, args ...any) {
	a.mu.Lock()
	a.logLines = append(a.logLines, fmt.Sprintf(format, args...))
	if len(a.logLines) > 200 {
		a.logLines = a.logLines[len(a.logLines)-200:]
	}
	a.mu.Unlock()
	fmt.Println("· " + fmt.Sprintf(format, args...))
}

// neutral display ordering before trump is declared
func displayTrump(g *euchre.Game) euchre.Suit {
	if tr, ok := g.Trump(); ok {
		return tr
	}
	return euchre.Clubs
}
