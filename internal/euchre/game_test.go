package euchre

import (
	"strings"
	"testing"
)

func mustCard(t *testing.T, s string) Card {
	t.Helper()
	c, err := ParseCard(s)
	if err != nil {
		t.Fatalf("ParseCard(%q): %v", s, err)
	}
	return c
}

// playTrick plays the given cards in turn order and returns the recorded winner.
func playTrick(t *testing.T, g *Game, cards []string) Seat {
	t.Helper()
	for _, cs := range cards {
		c := mustCard(t, cs)
		if err := g.Play(g.Turn(), c); err != nil {
			t.Fatalf("Play(%s, %s): %v", g.Turn(), cs, err)
		}
	}
	return g.Tricks()[len(g.Tricks())-1].Winner
}

func seatOf(t *testing.T, s string) Seat {
	t.Helper()
	v, err := ParseSeat(s)
	if err != nil {
		t.Fatalf("ParseSeat(%q): %v", s, err)
	}
	return v
}

// trickGame builds a game in PhasePlay with a known deal. Turn order is
// South → West → North → East (clockwise).
func trickGame(t *testing.T, hands map[string][]string, trumpSuit Suit, maker Team) *Game {
	g := &Game{Dealer: North, trump: trumpSuit, trumpSet: true, Phase: PhasePlay, maker: maker}
	for seatStr, cards := range hands {
		s := seatOf(t, seatStr)
		for _, cs := range cards {
			g.hands[s] = append(g.hands[s], mustCard(t, cs))
		}
	}
	return g
}

func TestBowerRanking(t *testing.T) {
	trump := Hearts
	// Within trump: JH (right) > JD (left) > AH > KH > QH.
	hand := []Card{
		{Queen, Hearts}, {Jack, Diamonds}, {Ace, Hearts}, {King, Hearts}, {Jack, Hearts},
	}
	// Insertion sort with lessCard must yield the canonical order.
	got := append([]Card(nil), hand...)
	for i := 1; i < len(got); i++ {
		for j := i; j > 0 && LessCard(got[j], got[j-1], trump); j-- {
			got[j], got[j-1] = got[j-1], got[j]
		}
	}
	want := []string{"JH", "JD", "AH", "KH", "QH"}
	for i := range want {
		if got[i].String() != want[i] {
			t.Fatalf("trump order[%d] = %s, want %s (full %v)", i, got[i], want[i], got)
		}
	}
}

func TestEffectiveSuit(t *testing.T) {
	if EffectiveSuit(Card{Jack, Diamonds}, Hearts) != Hearts {
		t.Fatal("JD must be trump when hearts is trump")
	}
	if EffectiveSuit(Card{Jack, Diamonds}, Spades) != Diamonds {
		t.Fatal("JD stays a diamond under spades")
	}
	if EffectiveSuit(Card{Jack, Clubs}, Hearts) != Clubs {
		t.Fatal("JC is never trump under hearts")
	}
	if EffectiveSuit(Card{Ace, Diamonds}, Hearts) != Diamonds {
		t.Fatal("AD is a diamond under hearts")
	}
	if EffectiveSuit(Card{Jack, Spades}, Hearts) != Spades {
		t.Fatal("JS is a spade under hearts")
	}
}

func TestTrickWinnerBowerBeatsAll(t *testing.T) {
	// Hearts trump. West leads AD; North plays JD (left bower = trump); East
	// plays 9C; South follows AH. The left bower must win.
	g := trickGame(t, map[string][]string{
		"west": {"AD", "9C"}, "north": {"JD"}, "east": {"9C"}, "south": {"AH"},
	}, Hearts, EW)
	g.lead, g.turn = West, West
	if w := playTrick(t, g, []string{"AD", "JD", "9C", "AH"}); w != North {
		t.Fatalf("left bower should win: got %s, want north", w)
	}
}

func TestTrickWinnerNoTrumpPlayed(t *testing.T) {
	// Hearts trump, diamonds led: high diamond wins; trump QH would win but
	// East discards a club instead.
	g := trickGame(t, map[string][]string{
		"west": {"KD"}, "north": {"AD"}, "east": {"QC"}, "south": {"9D"},
	}, Hearts, EW)
	g.lead, g.turn = West, West
	if w := playTrick(t, g, []string{"KD", "AD", "QC", "9D"}); w != North {
		t.Fatalf("AD should win plain diamond trick: got %s", w)
	}
}

func TestTrickWinnerLowTrumpBeatsLedAce(t *testing.T) {
	// Spades trump, hearts led: 9S beats AH; JC (left bower) beats 9S.
	g := trickGame(t, map[string][]string{
		"west": {"AH"}, "north": {"9S"}, "east": {"JC"}, "south": {"KC"},
	}, Spades, EW)
	g.lead, g.turn = West, West
	if w := playTrick(t, g, []string{"AH", "9S", "JC", "KC"}); w != East {
		t.Fatalf("left bower should win: got %s", w)
	}
}

func TestRightBowerBeatsLeftBower(t *testing.T) {
	// Hearts trump: JH beats JD no matter the order.
	g := trickGame(t, map[string][]string{
		"west": {"JD"}, "north": {"9C"}, "east": {"JH"}, "south": {"9D"},
	}, Hearts, EW)
	g.lead, g.turn = West, West
	if w := playTrick(t, g, []string{"JD", "9C", "JH", "9D"}); w != East {
		t.Fatalf("right bower should beat left: got %s", w)
	}
}

func TestMustFollowSuit(t *testing.T) {
	// Hearts trump. West leads AD; South is void in diamonds → may discard.
	g := trickGame(t, map[string][]string{
		"west": {"AD"}, "north": {"KD"}, "east": {"QD"}, "south": {"9C", "JH"},
	}, Hearts, EW)
	g.lead, g.turn = West, West
	if err := g.Play(West, mustCard(t, "AD")); err != nil {
		t.Fatalf("lead: %v", err)
	}
	if err := g.Play(North, mustCard(t, "KD")); err != nil {
		t.Fatalf("north follow: %v", err)
	}
	if err := g.Play(East, mustCard(t, "QD")); err != nil {
		t.Fatalf("east follow: %v", err)
	}
	legal := g.LegalPlays(South)
	if len(legal) != 2 {
		t.Fatalf("void in led suit: want 2 legal plays, got %v", legal)
	}
	if err := g.Play(South, mustCard(t, "JH")); err != nil {
		t.Fatalf("discard when void: %v", err)
	}
	// Trump discard wins the trick.
	if w := g.Tricks()[0].Winner; w != South {
		t.Fatalf("trump discard should win: got %s", w)
	}
}

func TestRenegeRejected(t *testing.T) {
	// Hearts trump. West leads AD. South holds a diamond → must follow.
	g := trickGame(t, map[string][]string{
		"west": {"AD"}, "north": {"KD"}, "east": {"QD"}, "south": {"9D", "JH"},
	}, Hearts, EW)
	g.lead, g.turn = West, West
	if err := g.Play(West, mustCard(t, "AD")); err != nil {
		t.Fatalf("lead: %v", err)
	}
	if err := g.Play(North, mustCard(t, "KD")); err != nil {
		t.Fatalf("north: %v", err)
	}
	if err := g.Play(East, mustCard(t, "QD")); err != nil {
		t.Fatalf("east: %v", err)
	}
	err := g.Play(South, mustCard(t, "JH"))
	if err == nil {
		t.Fatal("renege (JH while holding 9D on a diamond lead) must be rejected")
	}
	if g.Turn() != South {
		t.Fatalf("turn changed after rejection: %s", g.Turn())
	}
	if err := g.Play(South, mustCard(t, "9D")); err != nil {
		t.Fatalf("legal follow rejected: %v", err)
	}
}

// The wrong-seat regression: the app checks follow-suit before in-hand, so a
// card the seat does not hold — but whose suit coincides with the led suit —
// was reported as a phantom renege instead of the truthful "not in hand"
// (the agent then hunted for a suit bug while playing from the wrong hand).
func TestNotInHandReportedBeforeFollowSuit(t *testing.T) {
	// Hearts trump. West leads AD; South holds diamonds but the played card
	// is NOT one of them (South's hand came from the wrong seat).
	g := trickGame(t, map[string][]string{
		"west": {"AD"}, "north": {"KD"}, "east": {"QD"}, "south": {"9D", "JH"},
	}, Hearts, EW)
	g.lead, g.turn = West, West
	if err := g.Play(West, mustCard(t, "AD")); err != nil {
		t.Fatalf("lead: %v", err)
	}
	// North plays second: TD is a diamond (the led suit) but NOT in north's
	// hand — the old check order reported a phantom renege for exactly this.
	err := g.ValidatePlay(North, mustCard(t, "TD"))
	if err == nil {
		t.Fatal("card not in hand must be rejected")
	}
	if !strings.Contains(err.Error(), "not in north's hand") {
		t.Fatalf("want truthful not-in-hand reason, got %q", err)
	}
	if strings.Contains(err.Error(), "renegade") {
		t.Fatalf("phantom renege for a not-in-hand card: %q", err)
	}
	// An in-hand card that breaks follow-suit still reports the renege.
	if err := g.Play(North, mustCard(t, "KD")); err != nil {
		t.Fatalf("north: %v", err)
	}
	if err := g.Play(East, mustCard(t, "QD")); err != nil {
		t.Fatalf("east: %v", err)
	}
	err = g.ValidatePlay(South, mustCard(t, "JH"))
	if err == nil || !strings.Contains(err.Error(), "renegade") {
		t.Fatalf("in-hand follow-suit violation must report renege, got %q", err)
	}
}

func TestLeftBowerIsNotFollowingLedPlainSuit(t *testing.T) {
	// Diamonds led, hearts trump. South holds only JD (left bower = trump):
	// void in diamonds, so any card is legal.
	g := trickGame(t, map[string][]string{
		"west": {"AD"}, "north": {"KD"}, "east": {"QD"}, "south": {"JD"},
	}, Hearts, EW)
	g.lead, g.turn = West, West
	if err := g.Play(West, mustCard(t, "AD")); err != nil {
		t.Fatalf("lead: %v", err)
	}
	if g.CanFollow(South) {
		t.Fatal("JD (left bower) must not count as following a diamond lead")
	}
	if err := g.Play(North, mustCard(t, "KD")); err != nil {
		t.Fatalf("north: %v", err)
	}
	if err := g.Play(East, mustCard(t, "QD")); err != nil {
		t.Fatalf("east: %v", err)
	}
	if err := g.Play(South, mustCard(t, "JD")); err != nil {
		t.Fatalf("left bower played on diamond lead: %v", err)
	}
	if w := g.Tricks()[0].Winner; w != South {
		t.Fatalf("left bower (only trump) should win: got %s", w)
	}
}

func TestBiddingRound1OrderUp(t *testing.T) {
	g := NewGame(1, South)
	g.StartHand()
	g.hands[North] = []Card{mustCard(t, "JH"), mustCard(t, "JD"), mustCard(t, "AH"), mustCard(t, "KH"), mustCard(t, "9H")}
	g.hands[South] = []Card{mustCard(t, "9C"), mustCard(t, "TC"), mustCard(t, "JC"), mustCard(t, "QC"), mustCard(t, "KC")}
	g.hands[West] = []Card{mustCard(t, "AD"), mustCard(t, "TD"), mustCard(t, "QD"), mustCard(t, "9D"), mustCard(t, "AC")}
	g.hands[East] = []Card{mustCard(t, "AS"), mustCard(t, "TS"), mustCard(t, "QS"), mustCard(t, "9S"), mustCard(t, "KS")}
	g.topCard = mustCard(t, "TH")

	if g.BidTurn() != West {
		t.Fatalf("bid turn after South deals should be west, got %s", g.BidTurn())
	}
	if err := g.Bid(West, CallPass, false); err != nil {
		t.Fatalf("west pass: %v", err)
	}
	if err := g.Bid(North, CallOrderUp, false); err != nil {
		t.Fatalf("north order up: %v", err)
	}
	if tr, ok := g.Trump(); !ok || tr != Hearts {
		t.Fatalf("trump = %v ok=%v, want hearts", tr, ok)
	}
	if g.Phase != PhaseDiscard {
		t.Fatalf("phase = %v, want discard", g.Phase)
	}
	if len(g.DiscardHand()) != 6 {
		t.Fatalf("dealer hand after pickup = %d cards, want 6", len(g.DiscardHand()))
	}
	if err := g.Discard(South, mustCard(t, "9C")); err != nil {
		t.Fatalf("discard: %v", err)
	}
	if g.Phase != PhasePlay {
		t.Fatalf("phase after discard = %v, want play", g.Phase)
	}
	if g.Turn() != West {
		t.Fatalf("first lead should be west (left of dealer), got %s", g.Turn())
	}
	// The bidder picks up nothing — the dealer does. North keeps 5 cards.
	if n := len(g.Hand(North)); n != 5 {
		t.Fatalf("north holds %d cards, want 5", n)
	}
	// South (dealer) is down to 5 after the discard.
	if n := len(g.Hand(South)); n != 5 {
		t.Fatalf("south holds %d cards, want 5", n)
	}
}

func TestDealerPicksUpTopCard(t *testing.T) {
	// Dealer is the bidder this time: the top card joins the dealer's hand.
	g := NewGame(2, West)
	g.StartHand()
	for s := range 4 {
		g.hands[s] = []Card{mustCard(t, "9C"), mustCard(t, "TD"), mustCard(t, "JH"), mustCard(t, "QS"), mustCard(t, "AC")}
	}
	g.topCard = mustCard(t, "AH")
	// West deals, so bidding starts at North; walk to West.
	for _, s := range []Seat{North, East, South} {
		if err := g.Bid(s, CallPass, false); err != nil {
			t.Fatalf("pass by %s: %v", s, err)
		}
	}
	if err := g.Bid(West, CallOrderUp, false); err != nil {
		t.Fatalf("dealer order up: %v", err)
	}
	if len(g.DiscardHand()) != 6 {
		t.Fatalf("dealer should hold 6 after pickup, got %d", len(g.DiscardHand()))
	}
	found := false
	for _, c := range g.DiscardHand() {
		if c == mustCard(t, "AH") {
			found = true
		}
	}
	if !found {
		t.Fatal("top card AH must be in dealer's hand after pickup")
	}
	if err := g.Discard(West, mustCard(t, "9C")); err != nil {
		t.Fatalf("discard: %v", err)
	}
	if len(g.Hand(West)) != 5 {
		t.Fatalf("dealer holds %d after discard, want 5", len(g.Hand(West)))
	}
	if tr, _ := g.Trump(); tr != Hearts {
		t.Fatalf("trump = %s, want hearts", tr)
	}
}

func TestBiddingWrongTurnAndSuit(t *testing.T) {
	g := NewGame(1, South)
	g.StartHand()
	for s := range 4 {
		g.hands[s] = []Card{mustCard(t, "9C"), mustCard(t, "TD"), mustCard(t, "JH"), mustCard(t, "QS"), mustCard(t, "AC")}
	}
	g.topCard = mustCard(t, "AH")

	if err := g.Bid(South, CallOrderUp, false); err == nil {
		t.Fatal("South bidding out of turn must fail")
	}
	for _, s := range []Seat{West, North, East, South} {
		if err := g.Bid(s, CallPass, false); err != nil {
			t.Fatalf("pass by %s: %v", s, err)
		}
	}
	if g.Round() != 2 || g.Phase != PhaseBid2 {
		t.Fatalf("round=%d phase=%v, want 2/PhaseBid2 after sweep", g.Round(), g.Phase)
	}
	if err := g.Bid(West, Call("H"), false); err == nil {
		t.Fatal("calling the turned-down suit must fail")
	}
	if err := g.Bid(West, Call("S"), false); err != nil {
		t.Fatalf("round-2 call: %v", err)
	}
	if tr, _ := g.Trump(); tr != Spades {
		t.Fatalf("trump = %s, want spades", tr)
	}
}

func TestBiddingSweptTwiceRedeals(t *testing.T) {
	g := NewGame(1, South)
	d0 := g.Dealer
	g.StartHand()
	for round := 0; round < 2; round++ {
		if round == 1 {
			g.topCard = mustCard(t, "AH")
		}
		for _, s := range []Seat{West, North, East, South} {
			if err := g.Bid(s, CallPass, false); err != nil {
				t.Fatalf("round %d pass by %s: %v", round+1, s, err)
			}
		}
	}
	if g.Dealer != d0.Next() {
		t.Fatalf("dealer should rotate after a full sweep, got %s", g.Dealer)
	}
	if g.Round() != 1 || g.Phase != PhaseBid1 {
		t.Fatalf("expected fresh hand: round=%d phase=%v", g.Round(), g.Phase)
	}
	if g.HandNo != 2 {
		t.Fatalf("handNo = %d, want 2", g.HandNo)
	}
}

// scoreHand applies the scoring rule to a finished trick count, bypassing
// trick play (scoring is what's under test).
func scoreHand(t *testing.T, maker Team, tricksNS, tricksEW int, lone bool) (nsPts, ewPts int) {
	t.Helper()
	g := &Game{trump: Hearts, trumpSet: true, maker: maker, alone: lone, aloneSeat: South}
	g.tricksWon[NS] = tricksNS
	g.tricksWon[EW] = tricksEW
	g.scoreHand()
	return g.Score[NS], g.Score[EW]
}

func TestScoringStandard(t *testing.T) {
	cases := []struct {
		name           string
		maker          Team
		ns, ew         int
		lone           bool
		wantNS, wantEW int
	}{
		{"makers 3 tricks", NS, 3, 2, false, 1, 0},
		{"makers 4 tricks", NS, 4, 1, false, 1, 0},
		{"march", NS, 5, 0, false, 2, 0},
		{"euchred", NS, 2, 3, false, 0, 2},
		{"lone march", NS, 5, 0, true, 1, 0},
		{"lone stopped at 4", NS, 4, 1, true, 1, 0},
		{"lone stopped at 3", NS, 3, 2, true, 1, 0},
		{"lone euchred", NS, 2, 3, true, 0, 2},
		{"EW makers march", EW, 0, 5, false, 0, 2},
		{"EW euchred", EW, 3, 2, false, 2, 0},
	}
	for _, tc := range cases {
		ns, ew := scoreHand(t, tc.maker, tc.ns, tc.ew, tc.lone)
		if ns != tc.wantNS || ew != tc.wantEW {
			t.Fatalf("%s: got NS=%d EW=%d, want NS=%d EW=%d", tc.name, ns, ew, tc.wantNS, tc.wantEW)
		}
	}
}

func TestGameToTenPoints(t *testing.T) {
	g := &Game{Dealer: North, trump: Hearts, trumpSet: true, maker: NS}
	g.Score[NS] = 9
	g.tricksWon[NS] = 3
	g.tricksWon[EW] = 2
	g.scoreHand()
	if !g.Over || g.Phase != PhaseGameOver {
		t.Fatal("game should be over at 10 points")
	}
	if g.Winner != NS {
		t.Fatalf("winner = %s, want north-south", g.Winner)
	}
}

func TestGoingAlonePartnerSitsOut(t *testing.T) {
	// South orders up alone: North sits out; play order skips North.
	g := NewGame(7, South)
	g.StartHand()
	for s := range 4 {
		g.hands[s] = []Card{mustCard(t, "9C"), mustCard(t, "TD"), mustCard(t, "JH"), mustCard(t, "QS"), mustCard(t, "AC")}
	}
	g.hands[East] = []Card{mustCard(t, "9C"), mustCard(t, "9D"), mustCard(t, "JH"), mustCard(t, "QS"), mustCard(t, "AC")}
	g.hands[South] = []Card{mustCard(t, "AD"), mustCard(t, "JH"), mustCard(t, "QS"), mustCard(t, "AC"), mustCard(t, "TC")}
	g.topCard = mustCard(t, "AH")
	for _, s := range []Seat{West, North, East} {
		if err := g.Bid(s, CallPass, false); err != nil {
			t.Fatalf("pass: %v", err)
		}
	}
	if err := g.Bid(South, CallOrderUp, true); err != nil {
		t.Fatalf("south alone: %v", err)
	}
	if err := g.Discard(South, mustCard(t, "TC")); err != nil {
		t.Fatalf("discard: %v", err)
	}
	if g.Turn() != West {
		t.Fatalf("lead = %s, want west", g.Turn())
	}
	if err := g.Play(West, mustCard(t, "TD")); err != nil {
		t.Fatalf("west lead: %v", err)
	}
	if g.Turn() != East {
		t.Fatalf("after west lead, turn = %s, want east (north sits out)", g.Turn())
	}
	if err := g.Play(North, mustCard(t, "9C")); err == nil {
		t.Fatal("sitting-out partner playing must be rejected")
	}
	if err := g.Play(East, mustCard(t, "9D")); err != nil {
		t.Fatalf("east play: %v", err)
	}
	if g.Turn() != South {
		t.Fatalf("after east, turn = %s, want south", g.Turn())
	}
	// South must follow diamonds; AD wins the trick.
	if err := g.Play(South, mustCard(t, "AD")); err != nil {
		t.Fatalf("south play: %v", err)
	}
	if w := g.Tricks()[0].Winner; w != South {
		t.Fatalf("AD should win: got %s", w)
	}
}

func TestDeckAndParse(t *testing.T) {
	d := Deck()
	if len(d) != 24 {
		t.Fatalf("deck size %d", len(d))
	}
	seen := map[Card]bool{}
	for _, c := range d {
		if seen[c] {
			t.Fatalf("duplicate %s", c)
		}
		seen[c] = true
		p, err := ParseCard(c.String())
		if err != nil || p != c {
			t.Fatalf("roundtrip %s: %v %v", c, p, err)
		}
	}
	for _, bad := range []string{"8C", "JDQ", "9x", "", "10H"} {
		if _, err := ParseCard(bad); err == nil {
			t.Fatalf("ParseCard(%q) should fail", bad)
		}
	}
}

func TestOrderUpDoesNotCorruptOtherHands(t *testing.T) {
	// Regression: hands were slices of the shared deck backing array, so the
	// dealer's pickup append overwrote the next seat's first card (a duplicate
	// top card appeared in two hands and one card vanished from the game).
	g := NewGame(11, South)
	g.StartHand()
	before := map[Seat][]Card{West: g.Hand(West), North: g.Hand(North), East: g.Hand(East)}
	// South is the dealer; west/north/east pass, then south orders up.
	for _, s := range []Seat{West, North, East} {
		if err := g.Bid(s, CallPass, false); err != nil {
			t.Fatalf("pass by %s: %v", s, err)
		}
	}
	if err := g.Bid(South, CallOrderUp, false); err != nil {
		t.Fatalf("order up: %v", err)
	}
	seen := map[Card]Seat{}
	for _, s := range []Seat{South, West, North, East} {
		for _, c := range g.Hand(s) {
			if prev, dup := seen[c]; dup {
				t.Fatalf("card %s in both %s and %s hands after pickup", c, prev, s)
			}
			seen[c] = s
		}
	}
	for s, want := range before {
		got := g.Hand(s)
		if len(got) != len(want) {
			t.Fatalf("%s hand changed size after pickup: %v (want %v)", s, got, want)
		}
		for i, c := range want {
			if got[i] != c {
				t.Fatalf("%s hand corrupted by pickup: %v (want %v)", s, got, want)
			}
		}
	}
	if _, ok := seen[g.TopCard()]; !ok {
		t.Fatal("top card not held by any hand after pickup")
	}
}
