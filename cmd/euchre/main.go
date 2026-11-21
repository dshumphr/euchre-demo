// Command euchre is a terminal euchre game. The user plays South; a built-in
// heuristic AI plays the other seats until an agent claims them through
// proxenos. Any or all of North, East and West can be claimed — one seat (the
// classic "agent partner in North" setup) or all three (the agent plays
// against the user across the table). Claims are additive and idempotent per
// seat, can arrive any time (they take effect at the next decision point),
// and can be released back to the bots. Every claimed seat's hand is
// disclosed (deal.your_hand), and each claimed seat falls back to the
// built-in bot after --agent-timeout so the game never stalls.
//
// The proxenos integration follows proxenos docs/app-integration.md: the
// stream is created idempotently with the embedded contract, outbound events
// go through `proxenos send --as app:euchre`, and inbound agent actions come
// from polling the stream file directly (see internal/prox).
package main

import (
	"bufio"
	_ "embed"
	"encoding/json"
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
	seed         int64
	movesPath    string
	agentTimeout time.Duration
	proxenosBin  string
}

func main() {
	cfg := config{}
	flag.StringVar(&cfg.stream, "stream", "euchre", "proxenos stream name")
	flag.Int64Var(&cfg.seed, "seed", 1, "deterministic shuffle seed")
	flag.StringVar(&cfg.movesPath, "moves", "", "scripted South moves file, one action per line (for e2e / non-interactive runs); \"auto\" lets the built-in bot play South")
	flag.DurationVar(&cfg.agentTimeout, "agent-timeout", 30*time.Second, "how long to wait for the agent on a claimed seat's turn before the built-in bot fills in")
	flag.StringVar(&cfg.proxenosBin, "proxenos", "proxenos", "path to the proxenos binary")
	flag.Parse()

	if err := run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "euchre:", err)
		os.Exit(1)
	}
}

// ── app state ─────────────────────────────────────────────────────────────

type app struct {
	cfg      config
	game     *euchre.Game
	client   *prox.Client
	proxOK   bool
	claimed  [4]bool  // seats the agent has claimed (south is never claimable)
	disclose [4]bool  // newly claimed seats whose hand is disclosed at the next decision point
	tips     []string
	logLines []string
	interact bool // stdin is a terminal

	agentQ    *eventQueue // inbound agent events
	stdinCh   chan string // user input lines (interactive only)
	moves     []string    // scripted South moves (moves mode)
	moveIdx   int
	movesAuto bool // moves file hit its "auto" sentinel: bot plays South

	summarizedHand int // last hand a hand.summary was emitted for
}

func run(cfg config) error {
	a := &app{cfg: cfg, agentQ: newEventQueue()}

	a.interact = stdinIsTerminal()

	if a.interact {
		a.stdinCh = make(chan string)
		go func() {
			sc := bufio.NewScanner(os.Stdin)
			for sc.Scan() {
				a.stdinCh <- sc.Text()
			}
			// stdin closed: no more input; block forever so selects don't
			// spin — the game simply runs on without user input.
			select {}
		}()
	} else if cfg.movesPath != "" {
		data, err := os.ReadFile(cfg.movesPath)
		if err != nil {
			return fmt.Errorf("read moves: %w", err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			a.moves = append(a.moves, line)
		}
	}

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
			// or rejected again (re-judging them caused the restart replay
			// storm of spurious action.rejected events).
			var startOffset int64
			if st, err := os.Stat(path); err == nil {
				startOffset = st.Size()
			}
			prox.StartPoll(path, startOffset, a.ingest, a.logf)
		}
	}

	a.game = euchre.NewGame(cfg.seed, euchre.South)
	a.game.StartHand()
	// South's hand is deliberately NOT on the stream: it is visible in the
	// TUI, and publishing it let an agent play South's cards for a claimed
	// seat. Claimed seats get their hands via deal.your_hand.
	a.emit("game.started", map[string]any{
		"dealer": a.game.Dealer.String(),
		"youAre": "south",
	})
	a.emitDeal()
	a.logf("new game — you are South, dealer %s", a.game.Dealer)

	return a.loop()
}

// ── inbound agent event queue ─────────────────────────────────────────────

// eventQueue is a small ordered queue of inbound agent events. The app pops
// events only when it can act on them, and can push one back (drainPending
// must not swallow a bid/play that arrived while the app was still deciding
// whether the agent has the seat).
type eventQueue struct {
	mu     sync.Mutex
	evs    []prox.Event
	notify chan struct{}
}

func newEventQueue() *eventQueue {
	return &eventQueue{notify: make(chan struct{}, 1)}
}

func (q *eventQueue) push(ev prox.Event) {
	q.mu.Lock()
	q.evs = append(q.evs, ev)
	q.mu.Unlock()
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

// pushFront puts an event back at the head of the queue.
func (q *eventQueue) pushFront(ev prox.Event) {
	q.mu.Lock()
	q.evs = append([]prox.Event{ev}, q.evs...)
	q.mu.Unlock()
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

// pop waits up to d for the next event.
func (q *eventQueue) pop(d time.Duration) (prox.Event, bool) {
	if d <= 0 {
		return prox.Event{}, false
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		q.mu.Lock()
		if len(q.evs) > 0 {
			ev := q.evs[0]
			q.evs = q.evs[1:]
			q.mu.Unlock()
			return ev, true
		}
		q.mu.Unlock()
		select {
		case <-q.notify:
			// loop: an event may be waiting
		case <-timer.C:
			return prox.Event{}, false
		}
	}
}

// ingest classifies one inbound stream event. Replayed events (wholly below
// this process's startup snapshot) only update state — they are never
// validated or rejected; live events queue for the main loop.
func (a *app) ingest(ev prox.Event, replay bool) {
	if ev.Src == prox.AppSrc || strings.HasPrefix(ev.Src, "sys:") {
		return // our own emits echoed back, and proxenos metadata
	}
	if replay {
		a.applyReplay(ev)
		return
	}
	a.agentQ.push(ev)
}

// applyReplay applies an event from before this process started for STATE
// only. Seat claims still stand (the agent expects to hold them — an old
// claim must survive a restart); old bids, plays, hand requests, tips and
// resigns belong to a previous run and are silently skipped: re-validating
// them re-rejected actions that had already been judged, and a stale resign
// would kill the fresh game.
func (a *app) applyReplay(ev prox.Event) {
	switch ev.Type {
	case "euchre.claim_seat":
		seats, release, err := parseClaim(ev.Body)
		if err != nil {
			a.logf("replay: skipping malformed %s from a previous run", ev.Type)
			return
		}
		if release {
			for _, s := range seats {
				a.claimed[s] = false
			}
			return
		}
		for _, s := range seats {
			if s == euchre.South {
				continue // never claimable; nothing to apply
			}
			if !a.claimed[s] {
				a.claimed[s] = true
				a.disclose[s] = true
				a.logf("replay: %s seat claim from a previous run still stands", s)
			}
		}
	default:
		a.logf("replay: ignoring %s from a previous run (state only)", ev.Type)
	}
}

// ── main loop ─────────────────────────────────────────────────────────────

func (a *app) loop() error {
	for !a.done() {
		a.flushDisclose()
		a.draw()
		switch a.game.Phase {
		case euchre.PhaseBid1, euchre.PhaseBid2:
			if err := a.doBid(); err != nil {
				return err
			}
		case euchre.PhaseDiscard:
			if err := a.doDiscard(); err != nil {
				return err
			}
		case euchre.PhasePlay:
			if err := a.doPlay(); err != nil {
				return err
			}
		case euchre.PhaseHandEnd, euchre.PhaseGameOver:
			a.finishHand()
		}
	}
	a.emit("game.over", map[string]any{"winner": a.game.Winner.String()})
	a.logf("GAME OVER — %s wins %d–%d", a.game.Winner, a.game.Score[euchre.NS], a.game.Score[euchre.EW])
	a.draw()
	return nil
}

// done reports whether the match is finished and fully reported. A hand won
// on its fifth trick goes straight to PhaseGameOver — the final hand.summary
// must still be emitted (the coaching hook) before the loop may end. A
// mid-hand resign ends the game without a summary for the partial hand.
func (a *app) done() bool {
	if !a.game.Over {
		return false
	}
	if a.game.Phase == euchre.PhaseGameOver {
		return a.summarizedHand == a.game.HandNo
	}
	return true
}

// doBid resolves one bidding decision.
func (a *app) doBid() error {
	g := a.game
	seat := g.BidTurn()
	a.emitBidPrompt()

	if seat == euchre.South {
		call, alone := a.southBid()
		if err := g.Bid(seat, call, alone); err != nil {
			return err
		}
		a.emitBidMade(seat, call, alone)
		return nil
	}

	if !a.claimed[seat] {
		a.drainPending(500 * time.Millisecond)
	}
	if a.claimed[seat] {
		call, alone, ok := a.waitAgentBid(seat)
		if ok {
			if err := g.ValidateBid(seat, call, alone); err != nil {
				a.rejectWithHand("euchre.bid", err.Error(), seat)
				a.logf("%s bid: agent bid illegal — built-in bot bids", seat)
			} else {
				if err := g.Bid(seat, call, alone); err != nil {
					return err
				}
				a.emitBidMade(seat, call, alone)
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
	switch {
	case seat == euchre.South:
		card = a.southCard("discard", g.DiscardHand())
		have = true
	case seat != euchre.South:
		if !a.claimed[seat] {
			a.drainPending(500 * time.Millisecond)
		}
		if !a.claimed[seat] {
			break
		}
		c, ok := a.waitAgentCard(seat)
		if !ok {
			a.logf("%s discard: agent quiet past timeout — built-in bot discards", seat)
		} else if err := g.ValidateDiscard(seat, c); err != nil {
			a.rejectWithHand("euchre.play", err.Error(), seat)
			a.logf("%s discard: agent card illegal — built-in bot discards", seat)
		} else {
			card, have = c, true
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
	switch {
	case seat == euchre.South:
		card = a.southCard("play", g.LegalPlays(seat))
		have = true
	default:
		if !a.claimed[seat] {
			a.drainPending(500 * time.Millisecond)
		}
		if !a.claimed[seat] {
			break
		}
		c, ok := a.waitAgentCard(seat)
		if !ok {
			a.logf("%s play: agent quiet past timeout — built-in bot plays", seat)
		} else if err := g.ValidatePlay(seat, c); err != nil {
			a.rejectWithHand("euchre.play", err.Error(), seat)
			a.logf("%s play: agent card illegal — built-in bot plays", seat)
		} else {
			card, have = c, true
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
		if a.interact {
			time.Sleep(600 * time.Millisecond)
		}
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
	if g.Over {
		return
	}
	g.Dealer = g.Dealer.Next()
	g.StartHand()
	a.emitDeal()
	a.logf("hand %d — dealer %s", g.HandNo, g.Dealer)
}

// flushDisclose emits deal.your_hand for seats claimed since the last
// decision point — a mid-hand claim discloses that seat's current hand here,
// before it must act.
func (a *app) flushDisclose() {
	for s := euchre.South; s <= euchre.East; s++ {
		if a.disclose[s] {
			a.disclose[s] = false
			a.emitHandFor(s)
		}
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
		if a.claimed[s] {
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

// ── user input (South) ────────────────────────────────────────────────────

// southBid prompts the user (or the moves file) for South's bidding decision.
func (a *app) southBid() (euchre.Call, bool) {
	g := a.game
	for {
		var line string
		if a.interact {
			if g.Round() == 1 {
				fmt.Print(truncVis(fmt.Sprintf("\nYour bid (top card %s): [o]rder up / [p]ass / <suit> alone: ", g.TopCard()), terminalWidth()-1))
			} else {
				fmt.Print(truncVis("\nYour bid — name trump (C/D/H/S) or [p]ass: ", terminalWidth()-1))
			}
			line = a.readLine()
		} else {
			line = a.nextMove("bid")
		}
		if line == "auto" {
			call, alone, err := euchre.AIBid(g, euchre.South)
			if err != nil {
				a.logf("auto bid: %v", err)
				continue
			}
			return call, alone
		}
		call, alone, err := parseBidLine(line, g.Round(), g.TopCard().Suit)
		if err != nil {
			a.logf("%v", err)
			continue
		}
		return call, alone
	}
}

func parseBidLine(line string, round int, topCard euchre.Suit) (euchre.Call, bool, error) {
	f := strings.Fields(strings.ToLower(strings.TrimSpace(line)))
	if len(f) == 0 {
		return "", false, fmt.Errorf("empty bid")
	}
	alone := false
	for _, w := range f[1:] {
		if w == "alone" {
			alone = true
		}
	}
	switch f[0] {
	case "o", "order", "order_up", "up", "y":
		if round != 1 {
			return "", false, fmt.Errorf("order_up is round 1 only")
		}
		return euchre.CallOrderUp, alone, nil
	case "p", "pass":
		return euchre.CallPass, false, nil
	default:
		su, err := euchre.ParseSuit(f[0])
		if err != nil {
			return "", false, fmt.Errorf("bid? %q (order up / pass / C-D-H-S)", f[0])
		}
		if round != 2 {
			return "", false, fmt.Errorf("calling a suit is round 2 only")
		}
		if su == topCard {
			return "", false, fmt.Errorf("cannot call the turned-down suit (%s)", su)
		}
		return euchre.Call(su.Code()), alone, nil
	}
}

// southCard prompts the user (or the moves file) for South's next card.
func (a *app) southCard(verb string, legal []euchre.Card) euchre.Card {
	for {
		var line string
		if a.interact {
			fmt.Print(truncVis(fmt.Sprintf("\nYour %s — legal cards:", verb), terminalWidth()-1))
			for i, c := range legal {
				fmt.Print(truncVis(fmt.Sprintf("\n  %d) %s", i+1, c), terminalWidth()-1))
			}
			fmt.Print(truncVis("\nnumber or card code (e.g. JH): ", terminalWidth()-1))
			line = a.readLine()
		} else {
			line = a.nextMove(verb)
		}
		if line == "auto" {
			if verb == "discard" {
				return a.autoDiscard()
			}
			return a.autoPlay()
		}
		card, err := parseCardPick(line, legal)
		if err != nil {
			a.logf("%v", err)
			continue
		}
		return card
	}
}

func (a *app) autoPlay() euchre.Card {
	c, err := euchre.AIPlay(a.game, euchre.South)
	if err != nil {
		fmt.Fprintf(os.Stderr, "euchre: auto play: %v\n", err)
		os.Exit(2)
	}
	return c
}

func (a *app) autoDiscard() euchre.Card {
	c, err := euchre.AIDiscard(a.game, euchre.South)
	if err != nil {
		fmt.Fprintf(os.Stderr, "euchre: auto discard: %v\n", err)
		os.Exit(2)
	}
	return c
}

func parseCardPick(line string, legal []euchre.Card) (euchre.Card, error) {
	line = strings.TrimSpace(strings.ToLower(line))
	if line == "" {
		return euchre.Card{}, fmt.Errorf("empty card")
	}
	// Users type "10H"; the wire code for ten is "T".
	line = strings.Replace(line, "10", "t", 1)
	if c, err := euchre.ParseCard(line); err == nil {
		for _, l := range legal {
			if l == c {
				return c, nil
			}
		}
		return euchre.Card{}, fmt.Errorf("%s is not a legal card right now", c)
	}
	var n int
	if _, err := fmt.Sscanf(line, "%d", &n); err == nil && n >= 1 && n <= len(legal) {
		return legal[n-1], nil
	}
	return euchre.Card{}, fmt.Errorf("card? %q (number or code like JH)", line)
}

// readLine reads one interactive input line, draining control events so tips
// and seat claims surface between prompts.
func (a *app) readLine() string {
	a.drainPending(50 * time.Millisecond)
	return <-a.stdinCh
}

// drainPending applies buffered control events (claims, hand requests, tips,
// resign) within a short window — used before a bot-seat decision so a
// just-arrived seat claim takes effect. Bids and plays are left queued for
// the real wait.
func (a *app) drainPending(d time.Duration) {
	deadline := time.Now().Add(d)
	for {
		ev, ok := a.agentQ.pop(time.Until(deadline))
		if !ok {
			return
		}
		switch ev.Type {
		case "euchre.claim_seat", "euchre.hand_request", "euchre.tip", "euchre.resign":
			a.applyAgentEvent(ev)
		case "euchre.bid", "euchre.play":
			a.agentQ.pushFront(ev)
			return
		default:
			a.dispatchOther(ev, "the game is deciding")
		}
	}
}

// nextMove pops the next scripted South move and returns its argument part.
// A file whose last line is a bare "auto" switches South to the built-in bot
// from that point on (used when the script doesn't cover the whole game).
func (a *app) nextMove(verb string) string {
	if a.moveIdx >= len(a.moves) {
		if a.movesAuto {
			return "auto"
		}
		fmt.Fprintf(os.Stderr, "euchre: moves file exhausted while waiting for a %q\n", verb)
		os.Exit(2)
	}
	line := a.moves[a.moveIdx]
	a.moveIdx++
	if line == "auto" {
		a.movesAuto = true
		return "auto"
	}
	f := strings.Fields(line)
	if f[0] != verb {
		fmt.Fprintf(os.Stderr, "euchre: moves: got %q, want %q\n", line, verb)
		os.Exit(2)
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), verb))
}

// ── waiting on the agent ──────────────────────────────────────────────────

// waitAgentBid waits up to the agent timeout for euchre.bid (claims, hand
// requests, tips and resign are applied along the way; other events are
// rejected or ignored). seat is the bidder whose hand rejections disclose.
func (a *app) waitAgentBid(seat euchre.Seat) (euchre.Call, bool, bool) {
	deadline := time.Now().Add(a.cfg.agentTimeout)
	for {
		if a.game.Over {
			return "", false, false
		}
		ev, ok := a.agentQ.pop(time.Until(deadline))
		if !ok {
			return "", false, false
		}
		switch ev.Type {
		case "euchre.bid":
			var body struct {
				Decision   string `json:"decision"`
				GoingAlone bool   `json:"goingAlone"`
			}
			if err := ev.DecodeBody(&body); err != nil {
				a.rejectWithHand(ev.Type, "bad body: "+err.Error(), seat)
				continue
			}
			return euchre.Call(strings.ToLower(body.Decision)), body.GoingAlone, true
		case "euchre.claim_seat", "euchre.hand_request", "euchre.tip", "euchre.resign":
			a.applyAgentEvent(ev)
		default:
			a.dispatchOther(ev, "a bid is awaited")
		}
	}
}

// waitAgentCard waits up to the agent timeout for euchre.play. seat is the
// player whose hand rejections disclose.
func (a *app) waitAgentCard(seat euchre.Seat) (euchre.Card, bool) {
	deadline := time.Now().Add(a.cfg.agentTimeout)
	for {
		if a.game.Over {
			return euchre.Card{}, false
		}
		ev, ok := a.agentQ.pop(time.Until(deadline))
		if !ok {
			return euchre.Card{}, false
		}
		switch ev.Type {
		case "euchre.play":
			var body struct {
				Card string `json:"card"`
			}
			if err := ev.DecodeBody(&body); err != nil {
				a.rejectWithHand(ev.Type, "bad body: "+err.Error(), seat)
				continue
			}
			c, err := euchre.ParseCard(body.Card)
			if err != nil {
				a.rejectWithHand(ev.Type, err.Error(), seat)
				continue
			}
			return c, true
		case "euchre.bid", "euchre.claim_seat", "euchre.hand_request", "euchre.tip", "euchre.resign":
			a.applyAgentEvent(ev)
		default:
			a.dispatchOther(ev, "a card is awaited")
		}
	}
}

// dispatchOther handles non-waited events: known euchre verbs are rejected
// loudly, anything else (harness context, unknown types) is ignored.
func (a *app) dispatchOther(ev prox.Event, awaiting string) {
	if strings.HasPrefix(ev.Type, "euchre.") {
		a.reject(ev.Type, "not actionable right now ("+awaiting+")")
	} else {
		a.logf("ignoring %s from %s", ev.Type, ev.Src)
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

// applyAgentEvent handles an agent event outside the wait paths: seat
// claims/releases, hand requests, tips, and resign.
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
		if release {
			for _, s := range seats {
				if a.claimed[s] {
					a.claimed[s] = false
					a.logf("agent released the %s seat — built-in bot takes over", s)
				}
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
				a.logf("agent claimed the %s seat", s)
			}
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
		if !a.claimed[s] {
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
		a.tips = append(a.tips, body.Text)
		a.logf("tip: %s", body.Text)
	case "euchre.resign":
		if ev.Src == prox.AppSrc {
			a.reject(ev.Type, "euchre.resign is not for the app")
			return
		}
		// The agent's primary team concedes: North-South if it holds North
		// (its classic partner seat), otherwise the East-West seats it holds.
		team := euchre.EW
		if a.claimed[euchre.North] {
			team = euchre.NS
		}
		a.game.ResignAs(team)
		a.logf("agent resigned — game over")
	default:
		if strings.HasPrefix(ev.Type, "euchre.") {
			a.reject(ev.Type, "not actionable right now")
		} else {
			a.logf("ignoring %s from %s", ev.Type, ev.Src)
		}
	}
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
// zero round-trips — the old euchre.hand_request recovery loop never
// happened reliably, and the app already knows the hand.
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
	a.logLines = append(a.logLines, fmt.Sprintf(format, args...))
	if len(a.logLines) > 200 {
		a.logLines = a.logLines[len(a.logLines)-200:]
	}
	if !a.interact {
		fmt.Println("· " + fmt.Sprintf(format, args...))
	}
}

// neutral display ordering before trump is declared
func displayTrump(g *euchre.Game) euchre.Suit {
	if tr, ok := g.Trump(); ok {
		return tr
	}
	return euchre.Clubs
}
