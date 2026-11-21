package euchre

import (
	"fmt"
	"math/rand/v2"
	"sort"
)

// Seat is a player seat. Seats advance clockwise South → West → North → East.
type Seat int

const (
	South Seat = iota // the local user
	West              // built-in AI
	North             // the agent-partner (built-in AI until claimed)
	East              // built-in AI
)

var seatNames = [4]string{"south", "west", "north", "east"}

func (s Seat) String() string { return seatNames[s] }

// ParseSeat parses a seat name.
func ParseSeat(s string) (Seat, error) {
	for i, n := range seatNames {
		if s == n {
			return Seat(i), nil
		}
	}
	return 0, fmt.Errorf("bad seat %q", s)
}

// Next is the seat clockwise from s.
func (s Seat) Next() Seat { return (s + 1) % 4 }

// Partner is the seat across the table.
func (s Seat) Partner() Seat { return (s + 2) % 4 }

// Team is one of the two partnerships.
type Team int

const (
	NS Team = iota // South + North
	EW             // West + East
)

func (t Team) String() string {
	if t == NS {
		return "north-south"
	}
	return "east-west"
}

// TeamOf returns the team a seat belongs to.
func TeamOf(s Seat) Team {
	if s == South || s == North {
		return NS
	}
	return EW
}

// Phase is where the hand state machine sits.
type Phase int

const (
	PhaseBid1    Phase = iota // round 1: order up the top card or pass
	PhaseDiscard              // dealer picked up; must discard
	PhaseBid2                 // round 2: name a suit (not the turned-down one)
	PhasePlay                 // playing tricks
	PhaseHandEnd
	PhaseGameOver
)

// Call is a bidding decision: "order_up", "pass", or a suit code "C"/"D"/"H"/"S".
type Call string

const (
	CallOrderUp Call = "order_up"
	CallPass    Call = "pass"
)

// Bid records one player's bidding decision.
type Bid struct {
	Seat  Seat
	Call  Call
	Alone bool
}

// Played is one card put down in a trick.
type Played struct {
	Seat Seat
	Card Card
}

// TrickRecord is the log of one completed trick.
type TrickRecord struct {
	Lead   Seat
	Plays  []Played // in play order
	Winner Seat
}

// Game is the full state of one euchre match (to 10 points).
// The engine owns the rules; the app drives it turn by turn and renders it.
type Game struct {
	Score    [2]int // [NS, EW] match points
	Dealer   Seat   // dealer of the current (or last) hand
	HandNo   int    // 1-based
	Phase    Phase
	Over     bool
	Winner   Team
	rng      *rand.Rand
	hands    [4][]Card
	topCard  Card  // round-1 up card (turned down if round 2 starts)
	bidTurn  Seat  // whose bidding decision is awaited
	round    int   // 1 or 2
	bids     []Bid // this hand's bidding log
	turnDown Card  // card turned down after a swept round 1

	trump     Suit
	trumpSet  bool
	alone     bool
	aloneSeat Seat
	maker     Team

	trick     []Played // current trick, in play order
	lead      Seat     // leader of the current trick
	turn      Seat     // seat whose action is awaited
	trickNo   int      // 0-based trick index within the hand
	tricks    []TrickRecord
	tricksWon [2]int // [NS, EW] tricks this hand
}

// NewGame starts a match. seed drives the shuffle; firstDealer is the dealer of
// hand 1 (the dealer rotates clockwise each hand).
func NewGame(seed int64, firstDealer Seat) *Game {
	return &Game{rng: rand.New(rand.NewPCG(uint64(seed), 0x9E3779B97F4A7C15)), Dealer: firstDealer}
}

// Hand returns the cards held by a seat this hand.
func (g *Game) Hand(s Seat) []Card { return append([]Card(nil), g.hands[s]...) }

// TopCard is the round-1 up card.
func (g *Game) TopCard() Card { return g.topCard }

// TurnDown is the card turned down after all four passed round 1.
func (g *Game) TurnDown() Card { return g.turnDown }

// Trump is the declared trump suit for the hand.
func (g *Game) Trump() (Suit, bool) { return g.trump, g.trumpSet }

// Alone reports whether someone is going alone this hand (and who).
func (g *Game) Alone() (Seat, bool) { return g.aloneSeat, g.alone }

// Maker is the team that declared trump.
func (g *Game) Maker() Team { return g.maker }

// BidTurn is the seat whose bidding decision is awaited.
func (g *Game) BidTurn() Seat { return g.bidTurn }

// Turn is the seat whose action (discard or card) is awaited.
func (g *Game) Turn() Seat { return g.turn }

// TrickNo is the 0-based current trick index.
func (g *Game) TrickNo() int { return g.trickNo }

// Trick is the current trick's played cards so far.
func (g *Game) Trick() []Played { return append([]Played(nil), g.trick...) }

// Tricks returns the log of completed tricks this hand.
func (g *Game) Tricks() []TrickRecord { return append([]TrickRecord(nil), g.tricks...) }

// Bids returns this hand's bidding log.
func (g *Game) Bids() []Bid { return append([]Bid(nil), g.bids...) }

// TricksWonNS/EW report trick counts for the current hand.
func (g *Game) TricksWon(t Team) int { return g.tricksWon[t] }

// Round is the current bidding round (1 or 2).
func (g *Game) Round() int { return g.round }

// LegalPlays returns the cards seat may legally play right now: all cards when
// void in the led suit, otherwise only cards that follow the led suit.
func (g *Game) LegalPlays(s Seat) []Card {
	if g.Phase != PhasePlay || g.turn != s || len(g.trick) == 0 {
		return g.Hand(s)
	}
	led := EffectiveSuit(g.trick[0].Card, g.trump)
	var follow, all []Card
	for _, c := range g.hands[s] {
		all = append(all, c)
		if EffectiveSuit(c, g.trump) == led {
			follow = append(follow, c)
		}
	}
	if len(follow) == 0 {
		return all
	}
	return follow
}

// CanFollow reports whether seat holds any card of the led suit.
func (g *Game) CanFollow(s Seat) bool {
	led := EffectiveSuit(g.trick[0].Card, g.trump)
	for _, c := range g.hands[s] {
		if EffectiveSuit(c, g.trump) == led {
			return true
		}
	}
	return false
}

// activeSeats returns seats participating this hand (partner sits out on a loner).
func (g *Game) activeSeats() [4]bool {
	var a [4]bool
	for i := range a {
		a[i] = true
	}
	if g.alone {
		a[g.aloneSeat.Partner()] = false
	}
	return a
}

// StartHand shuffles and deals a fresh hand and opens round-1 bidding with the
// player left of the dealer.
func (g *Game) StartHand() {
	g.HandNo++
	g.Phase = PhaseBid1
	g.round = 1
	g.bids = nil
	g.trick = nil
	g.tricks = nil
	g.trickNo = 0
	g.tricksWon = [2]int{}
	g.trumpSet = false
	g.alone = false

	deck := Deck()
	g.rng.Shuffle(len(deck), func(i, j int) { deck[i], deck[j] = deck[j], deck[i] })
	for s := range 4 {
		// Copy each hand out of the deck's backing array: hands must never
		// alias shared storage, or a later in-place append (dealer pickup)
		// would overwrite the next seat's cards.
		g.hands[s] = append([]Card(nil), deck[s*5:s*5+5]...)
	}
	g.topCard = deck[20]
	g.bidTurn = g.Dealer.Next()
	g.turn = g.bidTurn
}

// sweptRound1 reopens bidding as round 2 after all four players passed.
func (g *Game) sweptRound1() {
	g.Phase = PhaseBid2
	g.round = 2
	g.turnDown = g.topCard
	g.bidTurn = g.Dealer.Next()
	g.turn = g.bidTurn
}

// ValidateBid checks a bid without applying it; returns a human-readable reason
// when illegal.
func (g *Game) ValidateBid(seat Seat, call Call, alone bool) error {
	if g.Phase != PhaseBid1 && g.Phase != PhaseBid2 {
		return fmt.Errorf("not bidding (phase %v)", g.Phase)
	}
	if seat != g.bidTurn {
		return fmt.Errorf("not %s's turn to bid (waiting on %s)", seat, g.bidTurn)
	}
	switch call {
	case CallPass:
		if alone {
			return fmt.Errorf("cannot go alone and pass")
		}
	case CallOrderUp:
		if g.Phase != PhaseBid1 {
			return fmt.Errorf("order_up is round 1 only")
		}
	default:
		su, err := ParseSuit(string(call))
		if err != nil {
			return fmt.Errorf("bad decision %q", call)
		}
		if g.Phase != PhaseBid2 {
			return fmt.Errorf("calling a suit is round 2 only")
		}
		if su == g.topCard.Suit {
			return fmt.Errorf("cannot call the turned-down suit (%s)", su)
		}
	}
	return nil
}

// Bid applies a bidding decision. Ordering up moves the hand to the discard
// phase (dealer picks up); calling a suit (round 2) opens play. A swept round 2
// redeals with the next dealer.
func (g *Game) Bid(seat Seat, call Call, alone bool) error {
	if err := g.ValidateBid(seat, call, alone); err != nil {
		return err
	}
	g.bids = append(g.bids, Bid{Seat: seat, Call: call, Alone: alone})

	switch call {
	case CallPass:
		// Count passes this round; four in a round moves the bidding along.
		passes := 0
		for _, b := range g.roundBids() {
			if b.Call == CallPass {
				passes++
			}
		}
		if passes == 4 {
			if g.round == 1 {
				g.sweptRound1()
			} else {
				// All passed twice: redeal, dealer rotates clockwise.
				g.Dealer = g.Dealer.Next()
				g.StartHand()
			}
		} else {
			g.bidTurn = g.bidTurn.Next()
			g.turn = g.bidTurn
		}
		return nil

	case CallOrderUp:
		g.trump = g.topCard.Suit
		g.setMaker(seat, alone)
		// Dealer picks the up card up; a discard follows. Copy before
		// appending — the hand must own its storage.
		pickup := append([]Card(nil), g.hands[g.Dealer]...)
		g.hands[g.Dealer] = append(pickup, g.topCard)
		g.Phase = PhaseDiscard
		g.turn = g.Dealer
		return nil

	default:
		su, _ := ParseSuit(string(call))
		g.trump = su
		g.setMaker(seat, alone)
		g.beginPlay()
		return nil
	}
}

// roundBids returns the bidding decisions made in the current round.
func (g *Game) roundBids() []Bid {
	if g.round == 2 && len(g.bids) >= 4 {
		return g.bids[4:]
	}
	if g.round == 1 {
		return g.bids
	}
	return nil
}

func (g *Game) setMaker(bidder Seat, alone bool) {
	g.maker = TeamOf(bidder)
	g.trumpSet = true
	g.alone = alone
	if alone {
		g.aloneSeat = bidder
	}
}

// beginPlay opens trick play: player left of the dealer leads.
func (g *Game) beginPlay() {
	g.Phase = PhasePlay
	g.lead = g.Dealer.Next()
	if g.alone && g.lead == g.aloneSeat.Partner() {
		g.lead = g.lead.Next()
	}
	g.turn = g.lead
}

// ValidateDiscard checks a dealer discard without applying it.
func (g *Game) ValidateDiscard(seat Seat, card Card) error {
	if g.Phase != PhaseDiscard {
		return fmt.Errorf("no discard awaited (phase %v)", g.Phase)
	}
	if seat != g.Dealer {
		return fmt.Errorf("only the dealer (%s) discards", g.Dealer)
	}
	for _, c := range g.hands[seat] {
		if c == card {
			return nil
		}
	}
	return fmt.Errorf("card %s not in hand", card)
}

// Discard applies the dealer's discard after picking up the top card and opens
// trick play.
func (g *Game) Discard(seat Seat, card Card) error {
	if err := g.ValidateDiscard(seat, card); err != nil {
		return err
	}
	hand := g.hands[seat]
	for i, c := range hand {
		if c == card {
			g.hands[seat] = append(hand[:i:i], hand[i+1:]...)
			break
		}
	}
	g.beginPlay()
	return nil
}

// DiscardHand returns the dealer's 6-card hand during the discard phase.
func (g *Game) DiscardHand() []Card {
	sorted := append([]Card(nil), g.hands[g.Dealer]...)
	sort.Slice(sorted, func(i, j int) bool { return LessCard(sorted[i], sorted[j], g.trump) })
	return sorted
}

// ValidatePlay checks a card play without applying it. The in-hand check
// comes BEFORE the follow-suit check: reporting a phantom renege for a card
// the seat doesn't even hold (because the played suit coincides with the led
// suit) sends the agent hunting for a suit error when the real problem is a
// wrong hand.
func (g *Game) ValidatePlay(seat Seat, card Card) error {
	if g.Phase != PhasePlay {
		return fmt.Errorf("not playing tricks (phase %v)", g.Phase)
	}
	if seat != g.turn {
		return fmt.Errorf("not %s's turn (waiting on %s)", seat, g.turn)
	}
	if g.alone && seat == g.aloneSeat.Partner() {
		return fmt.Errorf("%s sits out this hand", seat)
	}
	inHand := false
	for _, c := range g.hands[seat] {
		if c == card {
			inHand = true
			break
		}
	}
	if !inHand {
		return fmt.Errorf("card %s not in %s's hand", card, seat)
	}
	for _, c := range g.LegalPlays(seat) {
		if c == card {
			return nil
		}
	}
	if err := g.CanFollowErr(seat); err != nil {
		return err
	}
	// Unreachable in practice: a held card that is not a legal play always
	// trips CanFollowErr (LegalPlays narrows to follow cards exactly when
	// the seat holds led-suit cards).
	return fmt.Errorf("card %s is not a legal play", card)
}

// CanFollowErr returns a renege-specific error when the seat must follow suit
// but the played card doesn't.
func (g *Game) CanFollowErr(seat Seat) error {
	if len(g.trick) == 0 {
		return nil
	}
	led := EffectiveSuit(g.trick[0].Card, g.trump)
	for _, c := range g.hands[seat] {
		if EffectiveSuit(c, g.trump) == led {
			return fmt.Errorf("must follow %s (renegade)", led)
		}
	}
	return nil
}

// Play applies a card play, completing the trick (and the hand) as needed.
func (g *Game) Play(seat Seat, card Card) error {
	if err := g.ValidatePlay(seat, card); err != nil {
		return err
	}
	g.removeCard(seat, card)
	g.trick = append(g.trick, Played{Seat: seat, Card: card})

	// Whose turn is next among active seats?
	g.turn = g.turn.Next()
	if g.alone && g.turn == g.aloneSeat.Partner() {
		g.turn = g.turn.Next()
	}

	// Trick complete when every active seat has played (3 on a loner, else 4).
	need := 4
	if g.alone {
		need = 3
	}
	if len(g.trick) < need {
		return nil
	}

	winner := g.trickWinner(g.trick)
	g.tricks = append(g.tricks, TrickRecord{Lead: g.lead, Plays: append([]Played(nil), g.trick...), Winner: winner})
	g.tricksWon[TeamOf(winner)]++
	g.trick = nil
	g.trickNo++
	if g.trickNo == 5 {
		g.scoreHand()
	} else {
		g.lead = winner
		g.turn = winner
	}
	return nil
}

func (g *Game) removeCard(seat Seat, card Card) {
	hand := g.hands[seat]
	for i, c := range hand {
		if c == card {
			g.hands[seat] = append(hand[:i:i], hand[i+1:]...)
			return
		}
	}
}

// trickWinner picks the trick winner: highest trump wins; otherwise the
// highest card of the led suit.
func (g *Game) trickWinner(trick []Played) Seat {
	best := trick[0]
	for _, p := range trick[1:] {
		ps := EffectiveSuit(p.Card, g.trump)
		bs := EffectiveSuit(best.Card, g.trump)
		switch {
		case ps == bs:
			if trickPower(p.Card, g.trump) > trickPower(best.Card, g.trump) {
				best = p
			}
		case ps == g.trump:
			best = p // trump beats any non-trump
		}
	}
	return best.Seat
}

// scoreHand converts trick counts into match points:
//
//	makers 3 or 4 tricks: 1 point
//	makers all 5 tricks (march): 2 points — but only 1 for a lone march
//	defenders 3+ tricks (euchre): 2 points
func (g *Game) scoreHand() {
	mt := g.tricksWon[g.maker]
	if mt >= 3 {
		pts := 1
		if mt == 5 {
			if g.alone {
				pts = 1 // a lone march scores only one point
			} else {
				pts = 2
			}
		}
		g.Score[g.maker] += pts
	} else {
		g.Score[1-int(g.maker)] += 2 // euchred
	}
	g.Phase = PhaseHandEnd
	if g.Score[NS] >= 10 || g.Score[EW] >= 10 {
		g.Phase = PhaseGameOver
		g.Over = true
		if g.Score[NS] >= 10 {
			g.Winner = NS
		} else {
			g.Winner = EW
		}
	}
}

// ResignAs concedes the match for team t — the other team wins. Idempotent:
// once over, the result stands. The phase is left as-is — the hand was cut
// short, so there is no hand.summary for it.
func (g *Game) ResignAs(t Team) {
	if g.Over {
		return
	}
	g.Over = true
	if t == NS {
		g.Winner = EW
	} else {
		g.Winner = NS
	}
}

// trickPower ranks a card for trick-taking under trump: right bower > left
// bower > A > K > Q > J > 10 > 9 of trump; within a plain suit, A high.
func trickPower(c Card, trump Suit) int {
	switch {
	case IsRightBower(c, trump):
		return 100
	case IsLeftBower(c, trump):
		return 99
	default:
		return int(c.Rank)
	}
}

// LessCard orders cards for display: trump high (right bower first), then by
// suit, then by power.
func LessCard(a, b Card, trump Suit) bool {
	as, bs := EffectiveSuit(a, trump), EffectiveSuit(b, trump)
	if as != bs {
		if as == trump {
			return true
		}
		if bs == trump {
			return false
		}
		return as < bs
	}
	return trickPower(a, trump) > trickPower(b, trump)
}

// SortHand returns the hand in display order: trump first (bower high), then
// by suit, high cards first.
func SortHand(hand []Card, trump Suit) []Card {
	out := append([]Card(nil), hand...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && LessCard(out[j], out[j-1], trump); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
