package main

import (
	"testing"

	"euchre-demo/internal/euchre"
)

func newTestApp(cfg config) *app {
	return newApp(cfg)
}

// ── state serializer ──────────────────────────────────────────────────────

// Seed 7, dealer south (the e2e worked example): round 1 sweeps, west calls
// clubs in round 2; south's hand is KD KS TH 9S QS.
func TestSnapshotBid1(t *testing.T) {
	a := newTestApp(config{})
	a.game = euchre.NewGame(7, euchre.South)
	a.game.StartHand()

	st := a.snapshot()
	if st.Hand != 1 {
		t.Errorf("hand = %d, want 1", st.Hand)
	}
	if st.Phase != "bid1" {
		t.Errorf("phase = %q, want bid1", st.Phase)
	}
	if st.Dealer != "south" {
		t.Errorf("dealer = %q, want south", st.Dealer)
	}
	if st.Turn == nil || *st.Turn != "west" {
		t.Errorf("turn = %v, want west", st.Turn)
	}
	if !st.Bidding.Active || st.Bidding.PromptTo == nil || *st.Bidding.PromptTo != "west" {
		t.Errorf("bidding = %+v, want active promptTo west", st.Bidding)
	}
	if st.BidRound == nil || *st.BidRound != 1 {
		t.Errorf("bidRound = %v, want 1", st.BidRound)
	}
	if st.TopCard == nil || *st.TopCard != "9♦" {
		t.Errorf("topCard = %v, want 9♦", st.TopCard)
	}
	if st.Trump != nil {
		t.Errorf("trump = %v, want nil before a bid", st.Trump)
	}
	if st.Maker != nil || st.Alone != nil {
		t.Errorf("maker/alone = %v/%v, want nil", st.Maker, st.Alone)
	}
	if st.GameOver != nil {
		t.Errorf("gameOver = %+v, want nil", st.GameOver)
	}
	if len(st.YourHand) != 5 {
		t.Errorf("yourHand = %v, want 5 cards", st.YourHand)
	}
	if len(st.Legal) != 0 {
		t.Errorf("legal = %v, want empty outside south's play", st.Legal)
	}
	if st.Tricks.NS != 0 || st.Tricks.EW != 0 || st.Score.NS != 0 || st.Score.EW != 0 {
		t.Errorf("counts = %+v/%+v, want zeros", st.Tricks, st.Score)
	}
	// seatInfo turn flags: only west is awaited
	if !st.SeatInfo["west"].IsTurn {
		t.Error("seatInfo[west].isTurn = false, want true")
	}
	for _, s := range []string{"south", "north", "east"} {
		if st.SeatInfo[s].IsTurn {
			t.Errorf("seatInfo[%s].isTurn = true, want false", s)
		}
		if st.SeatInfo[s].CardsLeft != 5 {
			t.Errorf("seatInfo[%s].cardsLeft = %d, want 5", s, st.SeatInfo[s].CardsLeft)
		}
		if st.SeatInfo[s].LastBid != nil {
			t.Errorf("seatInfo[%s].lastBid = %v, want nil", s, st.SeatInfo[s].LastBid)
		}
	}
	if !st.SeatInfo["south"].IsDealer {
		t.Error("seatInfo[south].isDealer = false, want true (dealer is south)")
	}
	for _, s := range []string{"north", "east", "west"} {
		if st.SeatInfo[s].IsDealer {
			t.Errorf("seatInfo[%s].isDealer = true, want false", s)
		}
	}
	for _, s := range []string{"north", "east", "west"} {
		if st.SeatInfo[s].Claimed {
			t.Errorf("seatInfo[%s].claimed = true, want false", s)
		}
	}
}

func TestSnapshotBid2AndPlay(t *testing.T) {
	a := newTestApp(config{})
	g := euchre.NewGame(7, euchre.South)
	a.game = g
	g.StartHand()

	// round 1: everyone passes (west, north, east bots; south via engine call)
	for _, s := range []euchre.Seat{euchre.West, euchre.North, euchre.East, euchre.South} {
		if err := g.Bid(s, euchre.CallPass, false); err != nil {
			t.Fatalf("bid pass %s: %v", s, err)
		}
	}
	st := a.snapshot()
	if st.Phase != "bid2" {
		t.Fatalf("phase = %q, want bid2", st.Phase)
	}
	if st.BidRound == nil || *st.BidRound != 2 {
		t.Errorf("bidRound = %v, want 2", st.BidRound)
	}
	if st.TopCard == nil || *st.TopCard != "9♦" {
		t.Errorf("topCard = %v, want the turned-down 9♦", st.TopCard)
	}

	// west calls clubs in round 2 → trick play, west leads
	if err := g.Bid(euchre.West, euchre.Call("C"), false); err != nil {
		t.Fatalf("bid C: %v", err)
	}
	st = a.snapshot()
	if st.Phase != "play" {
		t.Fatalf("phase = %q, want play", st.Phase)
	}
	if st.Trump == nil || *st.Trump != "♣" {
		t.Errorf("trump = %v, want ♣", st.Trump)
	}
	if st.Maker == nil || *st.Maker != "west" {
		t.Errorf("maker = %v, want west", st.Maker)
	}
	if st.Bidding.Active {
		t.Error("bidding.active = true, want false during play")
	}
	if st.Leader == nil || *st.Leader != "west" {
		t.Errorf("leader = %v, want west", st.Leader)
	}
	for _, s := range []string{"north", "east", "south", "west"} {
		if st.Trick[s] != nil {
			t.Errorf("trick[%s] = %v, want nil on an empty trick", s, st.Trick[s])
		}
	}
	if st.SeatInfo["south"].LastBid == nil || *st.SeatInfo["south"].LastBid != "pass" {
		t.Errorf("south lastBid = %v, want pass", st.SeatInfo["south"].LastBid)
	}

	// three bot plays; then it is south's turn and only K♦ follows the Q♦ lead
	for range 3 {
		seat := g.Turn()
		c, err := euchre.AIPlay(g, seat)
		if err != nil {
			t.Fatalf("AIPlay %s: %v", seat, err)
		}
		if err := g.Play(seat, c); err != nil {
			t.Fatalf("play %s: %v", seat, err)
		}
	}
	st = a.snapshot()
	if st.Turn == nil || *st.Turn != "south" {
		t.Fatalf("turn = %v, want south", st.Turn)
	}
	if !st.SeatInfo["south"].IsTurn {
		t.Error("seatInfo[south].isTurn = false, want true")
	}
	if len(st.Legal) != 1 || st.Legal[0] != "K♦" {
		t.Errorf("legal = %v, want [K♦] (must follow the Q♦ lead)", st.Legal)
	}

	// south's play completes the trick: south takes it and leads next
	if err := g.Play(euchre.South, euchre.Card{Rank: euchre.King, Suit: euchre.Diamonds}); err != nil {
		t.Fatalf("play KD: %v", err)
	}
	st = a.snapshot()
	if st.Tricks.NS != 1 {
		t.Errorf("tricks.ns = %d, want 1", st.Tricks.NS)
	}
	if st.Leader == nil || *st.Leader != "south" {
		t.Errorf("leader = %v, want south (trick winner leads)", st.Leader)
	}
	for _, s := range []string{"north", "east", "south", "west"} {
		if st.Trick[s] != nil {
			t.Errorf("trick[%s] = %v, want nil after the trick", s, st.Trick[s])
		}
	}
}

// Seed 11, dealer south: west/north/east pass, south orders up clubs and must
// discard (6-card hand).
func TestSnapshotDiscardPhase(t *testing.T) {
	a := newTestApp(config{})
	g := euchre.NewGame(11, euchre.South)
	a.game = g
	g.StartHand()

	for _, s := range []euchre.Seat{euchre.West, euchre.North, euchre.East} {
		call, _, err := euchre.AIBid(g, s)
		if err != nil {
			t.Fatalf("AIBid %s: %v", s, err)
		}
		if call != euchre.CallPass {
			t.Fatalf("AIBid %s = %v, want pass for seed 11 hand 1", s, call)
		}
		if err := g.Bid(s, call, false); err != nil {
			t.Fatalf("bid %s: %v", s, err)
		}
	}
	if err := g.Bid(euchre.South, euchre.CallOrderUp, false); err != nil {
		t.Fatalf("order up: %v", err)
	}

	st := a.snapshot()
	if st.Phase != "discard" {
		t.Fatalf("phase = %q, want discard", st.Phase)
	}
	if st.Turn == nil || *st.Turn != "south" {
		t.Errorf("turn = %v, want south (dealer discards)", st.Turn)
	}
	if st.Trump == nil || *st.Trump != "♣" {
		t.Errorf("trump = %v, want ♣", st.Trump)
	}
	if st.Maker == nil || *st.Maker != "south" {
		t.Errorf("maker = %v, want south", st.Maker)
	}
	if len(st.YourHand) != 6 {
		t.Errorf("yourHand = %d cards, want 6 (dealer picked up)", len(st.YourHand))
	}
	if len(st.Legal) != 6 {
		t.Errorf("legal = %d cards, want 6 (any discard)", len(st.Legal))
	}
	if st.BidRound != nil {
		t.Errorf("bidRound = %v, want nil outside bidding", st.BidRound)
	}

	card, err := euchre.AIDiscard(g, euchre.South)
	if err != nil {
		t.Fatalf("AIDiscard: %v", err)
	}
	if err := g.Discard(euchre.South, card); err != nil {
		t.Fatalf("discard: %v", err)
	}
	st = a.snapshot()
	if st.Phase != "play" {
		t.Errorf("phase = %q, want play after discard", st.Phase)
	}
	if st.Leader == nil || *st.Leader != "west" {
		t.Errorf("leader = %v, want west (left of dealer)", st.Leader)
	}
	if len(st.YourHand) != 5 || len(st.Legal) != 0 {
		t.Errorf("yourHand/legal = %d/%d, want 5/0 (not south's turn)", len(st.YourHand), len(st.Legal))
	}
}

func TestSnapshotGameOver(t *testing.T) {
	a := newTestApp(config{})
	g := euchre.NewGame(7, euchre.South)
	a.game = g
	g.StartHand()

	g.ResignAs(euchre.NS) // NS concedes → EW wins
	st := a.snapshot()
	if st.GameOver == nil {
		t.Fatal("gameOver = nil, want set after resign")
	}
	if st.GameOver.Winner != "ew" {
		t.Errorf("gameOver.winner = %q, want ew", st.GameOver.Winner)
	}
	if st.GameOver.Score != (teamCounts{0, 0}) {
		t.Errorf("gameOver.score = %+v, want zeros", st.GameOver.Score)
	}
}

func TestSnapshotLogAndTipsCaps(t *testing.T) {
	a := newTestApp(config{})
	a.game = euchre.NewGame(7, euchre.South)
	a.game.StartHand()
	for i := range 12 {
		a.logf("line %d", i)
	}
	a.mu.Lock()
	a.tips = append(a.tips, "lead your boss trump…")
	a.mu.Unlock()
	st := a.snapshot()
	if len(st.Log) != 8 {
		t.Errorf("log = %d lines, want 8 (capped)", len(st.Log))
	}
	if st.Log[len(st.Log)-1] != "line 11" {
		t.Errorf("last log line = %q, want \"line 11\"", st.Log[len(st.Log)-1])
	}
	if len(st.Tips) != 1 || st.Tips[0] != "lead your boss trump…" {
		t.Errorf("tips = %v, want the one tip", st.Tips)
	}
}

// ── input parsing ─────────────────────────────────────────────────────────

func TestParseBidDecision(t *testing.T) {
	cases := []struct {
		in   string
		want euchre.Call
		ok   bool
	}{
		{"order_up", euchre.CallOrderUp, true},
		{"ORDER_UP", euchre.CallOrderUp, true},
		{"pass", euchre.CallPass, true},
		{"P", euchre.CallPass, true},
		{"C", euchre.Call("C"), true},
		{"h", euchre.Call("H"), true},
		{"♠", euchre.Call("S"), true},
		{"♦", euchre.Call("D"), true},
		{"banana", "", false},
		{"", "", false},
		{"hearts", euchre.Call("H"), true}, // full suit names are accepted
		{"CLUBS", euchre.Call("C"), true},
		{" spades ", euchre.Call("S"), true},
	}
	for _, c := range cases {
		got, err := parseBidDecision(c.in)
		if c.ok && (err != nil || got != c.want) {
			t.Errorf("parseBidDecision(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
		if !c.ok && err == nil {
			t.Errorf("parseBidDecision(%q) = %q, want error", c.in, got)
		}
	}
}

func TestParseCardAny(t *testing.T) {
	cases := []struct {
		in   string
		want euchre.Card
		ok   bool
	}{
		{"K♠", euchre.Card{Rank: euchre.King, Suit: euchre.Spades}, true},
		{"10♥", euchre.Card{Rank: euchre.Ten, Suit: euchre.Hearts}, true},
		{"TD", euchre.Card{Rank: euchre.Ten, Suit: euchre.Diamonds}, true},
		{"9c", euchre.Card{Rank: euchre.Nine, Suit: euchre.Clubs}, true},
		{"TH", euchre.Card{Rank: euchre.Ten, Suit: euchre.Hearts}, true},
		{"banana", euchre.Card{}, false},
		{"KX", euchre.Card{}, false},
		{"10", euchre.Card{}, false},
		{"", euchre.Card{}, false},
	}
	for _, c := range cases {
		got, err := parseCardAny(c.in)
		if c.ok && (err != nil || got != c.want) {
			t.Errorf("parseCardAny(%q) = %+v, %v; want %+v", c.in, got, err, c.want)
		}
		if !c.ok && err == nil {
			t.Errorf("parseCardAny(%q) = %+v, want error", c.in, got)
		}
	}
}
