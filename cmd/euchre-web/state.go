package main

import (
	"encoding/json"

	"euchre-demo/internal/euchre"
)

// The state snapshot is the entire browser UI's view of the game: fully
// self-contained JSON, republished after every state change. Field order and
// shape follow the schema the frontend is built against; cards are glyph
// strings ("K♠", "10♥"), suits are glyph chars, seats are lowercase names.

type teamCounts struct {
	NS int `json:"ns"`
	EW int `json:"ew"`
}

type seatInfo struct {
	Claimed   bool    `json:"claimed"`
	CardsLeft int     `json:"cardsLeft"`
	LastBid   *string `json:"lastBid"`
	IsDealer  bool    `json:"isDealer"`
	IsTurn    bool    `json:"isTurn"`
}

type biddingState struct {
	Active   bool    `json:"active"`
	PromptTo *string `json:"promptTo"`
}

type gameOverState struct {
	Winner string     `json:"winner"` // "ns" or "ew"
	Score  teamCounts `json:"score"`
}

type state struct {
	Hand     int                 `json:"hand"`
	Phase    string              `json:"phase"`
	Dealer   string              `json:"dealer"`
	Turn     *string             `json:"turn"`
	Leader   *string             `json:"leader"`
	Trump    *string             `json:"trump"`
	Maker    *string             `json:"maker"`
	Alone    *string             `json:"alone"`
	TopCard  *string             `json:"topCard"`
	BidRound *int                `json:"bidRound"`
	YourHand []string            `json:"yourHand"`
	Legal    []string            `json:"legal"`
	Trick    map[string]*string  `json:"trick"`
	SeatInfo map[string]seatInfo `json:"seatInfo"`
	Tricks   teamCounts          `json:"tricks"`
	Score    teamCounts          `json:"score"`
	Tips     []string            `json:"tips"`
	Log      []string            `json:"log"`
	Bidding  biddingState        `json:"bidding"`
	GameOver *gameOverState      `json:"gameOver"`
}

func phaseName(p euchre.Phase) string {
	switch p {
	case euchre.PhaseBid1:
		return "bid1"
	case euchre.PhaseBid2:
		return "bid2"
	case euchre.PhaseDiscard:
		return "discard"
	case euchre.PhasePlay:
		return "play"
	case euchre.PhaseHandEnd:
		return "hand_over"
	case euchre.PhaseGameOver:
		return "game_over"
	}
	return "unknown"
}

func seatPtr(s euchre.Seat) *string {
	v := s.String()
	return &v
}

func glyphPtr(c euchre.Card) *string {
	v := c.Glyph()
	return &v
}

func suitPtr(s euchre.Suit) *string {
	v := s.Glyph()
	return &v
}

// snapshot builds the current state. Called only from the loop goroutine (the
// game is loop-owned); claimed/tips/log are copied under the mutex because the
// poller goroutine may write them.
func (a *app) snapshot() state {
	g := a.game
	a.mu.Lock()
	claimed := a.claimed
	tips := append([]string{}, a.tips...)
	logTail := make([]string, 0, 8)
	if n := len(a.logLines); n > 0 {
		start := n - 8
		if start < 0 {
			start = 0
		}
		logTail = append(logTail, a.logLines[start:]...)
	}
	a.mu.Unlock()

	st := state{
		Hand:     g.HandNo,
		Phase:    phaseName(g.Phase),
		Dealer:   g.Dealer.String(),
		YourHand: []string{},
		Legal:    []string{},
		Tips:     tips,
		Log:      logTail,
		Trick:    map[string]*string{"north": nil, "east": nil, "south": nil, "west": nil},
		SeatInfo: map[string]seatInfo{},
	}

	// whose action is awaited / who leads the current trick
	switch g.Phase {
	case euchre.PhaseBid1, euchre.PhaseBid2:
		st.Turn = seatPtr(g.BidTurn())
		st.BidRound = new(int)
		*st.BidRound = g.Round()
		st.TopCard = glyphPtr(g.TopCard())
		if g.Round() == 2 {
			st.TopCard = glyphPtr(g.TurnDown())
		}
		st.Bidding = biddingState{Active: true, PromptTo: seatPtr(g.BidTurn())}
	case euchre.PhaseDiscard:
		st.Turn = seatPtr(g.Dealer)
	case euchre.PhasePlay:
		st.Turn = seatPtr(g.Turn())
		if len(g.Trick()) == 0 {
			st.Leader = seatPtr(g.Turn())
		} else {
			st.Leader = seatPtr(g.Trick()[0].Seat)
		}
		for _, p := range g.Trick() {
			st.Trick[p.Seat.String()] = glyphPtr(p.Card)
		}
	}

	if tr, ok := g.Trump(); ok {
		st.Trump = suitPtr(tr)
	}
	for i := len(g.Bids()) - 1; i >= 0; i-- {
		if g.Bids()[i].Call != euchre.CallPass {
			st.Maker = seatPtr(g.Bids()[i].Seat)
			break
		}
	}
	if seat, alone := g.Alone(); alone {
		st.Alone = seatPtr(seat)
	}

	// South's hand (display order); during the dealer's discard it is 6 cards.
	hand := g.Hand(euchre.South)
	if g.Phase == euchre.PhaseDiscard && g.Dealer == euchre.South {
		hand = g.DiscardHand()
	}
	sorted := euchre.SortHand(hand, displayTrump(g))
	for _, c := range sorted {
		st.YourHand = append(st.YourHand, c.Glyph())
	}

	// Cards South may put down right now.
	switch {
	case g.Phase == euchre.PhaseDiscard && g.Dealer == euchre.South:
		for _, c := range g.DiscardHand() {
			st.Legal = append(st.Legal, c.Glyph())
		}
	case g.Phase == euchre.PhasePlay && g.Turn() == euchre.South:
		for _, c := range g.LegalPlays(euchre.South) {
			st.Legal = append(st.Legal, c.Glyph())
		}
	}

	// lastBid per seat (this hand's bidding log)
	lastBid := map[euchre.Seat]string{}
	for _, b := range g.Bids() {
		lastBid[b.Seat] = string(b.Call)
	}
	for s := euchre.South; s <= euchre.East; s++ {
		info := seatInfo{
			Claimed:   claimed[s],
			CardsLeft: len(g.Hand(s)),
			IsDealer:  g.Dealer == s,
		}
		if v, ok := lastBid[s]; ok {
			vv := v
			info.LastBid = &vv
		}
		if st.Turn != nil && *st.Turn == s.String() {
			info.IsTurn = true
		}
		st.SeatInfo[s.String()] = info
	}

	st.Tricks = teamCounts{NS: g.TricksWon(euchre.NS), EW: g.TricksWon(euchre.EW)}
	st.Score = teamCounts{NS: g.Score[euchre.NS], EW: g.Score[euchre.EW]}

	if g.Over {
		winner := "ew"
		if g.Winner == euchre.NS {
			winner = "ns"
		}
		st.GameOver = &gameOverState{Winner: winner, Score: st.Score}
	}
	return st
}

// snapshotJSON marshals the current state.
func (a *app) snapshotJSON() ([]byte, error) {
	return json.Marshal(a.snapshot())
}

// publish recomputes the state snapshot and pushes it to all SSE subscribers
// (and records it for POST replies / SSE connects). Loop goroutine only.
func (a *app) publish() {
	snap, err := a.snapshotJSON()
	if err != nil {
		a.logf("snapshot: %v", err)
		return
	}
	a.hub.publish(snap)
}
