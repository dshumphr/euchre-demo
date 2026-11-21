package euchre

import (
	"fmt"
	"sort"
)

// The built-in heuristic AI. Deterministic: same game state always yields the
// same decision. It plays East/West always and North until the agent claims
// that seat.

// AIBid decides a bidding action for seat.
func AIBid(g *Game, seat Seat) (Call, bool, error) {
	if g.Phase != PhaseBid1 && g.Phase != PhaseBid2 {
		return "", false, fmt.Errorf("not bidding")
	}
	if g.round == 1 {
		st := handStrength(g.Hand(seat), g.topCard.Suit)
		if st >= 3.0 {
			return CallOrderUp, st >= 4.5, nil
		}
		return CallPass, false, nil
	}
	// Round 2: pick the strongest remaining suit, preferring Next (the suit of
	// the same color as the turned-down card).
	best, bestSt := Suit(-1), 0.0
	for _, su := range suitOrderFor(g.topCard.Suit) {
		if su == g.topCard.Suit {
			continue
		}
		st := handStrength(g.Hand(seat), su)
		if st > bestSt {
			best, bestSt = su, st
		}
	}
	if best >= 0 && bestSt >= 2.5 {
		return Call(best.Code()), bestSt >= 4.5, nil
	}
	return CallPass, false, nil
}

// suitOrderFor ranks candidate suits: Next first, then greens, then reverse-next.
func suitOrderFor(down Suit) []Suit {
	switch down {
	case Clubs:
		return []Suit{Spades, Hearts, Diamonds}
	case Spades:
		return []Suit{Clubs, Diamonds, Hearts}
	case Diamonds:
		return []Suit{Hearts, Spades, Clubs}
	default: // Hearts
		return []Suit{Diamonds, Clubs, Spades}
	}
}

// handStrength scores a hand for a given trump suit: bowers and trump honors
// dominate, off-suit aces add a little.
func handStrength(hand []Card, trump Suit) float64 {
	st := 0.0
	for _, c := range hand {
		switch {
		case IsRightBower(c, trump):
			st += 2.0
		case IsLeftBower(c, trump):
			st += 1.5
		case EffectiveSuit(c, trump) == trump:
			switch c.Rank {
			case Ace:
				st += 1.0
			case King:
				st += 0.75
			default:
				st += 0.5
			}
		case c.Rank == Ace:
			st += 0.5
		}
	}
	return st
}

// AIDiscard picks the dealer's discard: the lowest non-trump card, from the
// shortest suit; pure trump hands discard the lowest trump.
func AIDiscard(g *Game, seat Seat) (Card, error) {
	if g.Phase != PhaseDiscard {
		return Card{}, fmt.Errorf("no discard awaited")
	}
	hand := g.Hand(seat)
	if len(hand) != 6 {
		return Card{}, fmt.Errorf("dealer holds %d cards, want 6", len(hand))
	}
	trump, _ := g.Trump()
	var best Card
	bestScore := 1e18
	for _, c := range hand {
		score := discardScore(c, trump, hand)
		if score < bestScore || (score == bestScore && c.String() < best.String()) {
			best, bestScore = c, score
		}
	}
	return best, nil
}

func discardScore(c Card, trump Suit, hand []Card) float64 {
	if EffectiveSuit(c, trump) == trump {
		// Keep bowers and high trump; throw low filler trump last.
		return 100 + float64(c.Rank)
	}
	// Prefer dumping from short suits (fewer cards of that suit held).
	n := 0
	for _, o := range hand {
		if o.Suit == c.Suit {
			n++
		}
	}
	return float64(n)*10 + float64(c.Rank)
}

// AIPlay chooses the card to play for seat.
func AIPlay(g *Game, seat Seat) (Card, error) {
	legal := g.LegalPlays(seat)
	if len(legal) == 0 {
		return Card{}, fmt.Errorf("no legal plays for %s", seat)
	}
	if len(g.trick) == 0 {
		return aiLead(g, seat, legal), nil
	}
	return aiFollow(g, seat, legal), nil
}

// aiLead picks a lead card: boss trump early, otherwise top of a long off-suit.
func aiLead(g *Game, seat Seat, legal []Card) Card {
	trump, _ := g.Trump()
	// Lead the right bower if we also hold the left (near-certain winner).
	for _, c := range legal {
		if IsRightBower(c, trump) {
			for _, o := range legal {
				if IsLeftBower(o, trump) {
					return c
				}
			}
		}
	}
	// Otherwise lead high from the longest non-trump suit.
	best, bestScore := legal[0], -1.0
	counts := map[Suit]int{}
	for _, c := range legal {
		if EffectiveSuit(c, trump) != trump {
			counts[c.Suit]++
		}
	}
	for _, c := range legal {
		score := float64(c.Rank)
		if EffectiveSuit(c, trump) != trump {
			score += float64(counts[c.Suit]) * 3
		} else {
			score -= 10 // avoid leading low trump without the bowers
		}
		if score > bestScore || (score == bestScore && c.String() < best.String()) {
			best, bestScore = c, score
		}
	}
	return best
}

// aiFollow picks a card when following: win cheaply if the trick isn't already
// the partner's, else shed the lowest card.
func aiFollow(g *Game, seat Seat, legal []Card) Card {
	trump, _ := g.Trump()
	played := g.trick
	wouldWin := trickWinnerOf(played, trump)
	partnerWinning := TeamOf(wouldWin) == TeamOf(seat)

	sort.Slice(legal, func(i, j int) bool {
		a, b := EffectiveSuit(legal[i], trump), EffectiveSuit(legal[j], trump)
		if a != b {
			if a == trump {
				return false
			}
			if b == trump {
				return true
			}
			return a < b
		}
		return legal[i].Rank < legal[j].Rank
	})

	if !partnerWinning {
		// Cheapest card that takes the trick as it stands.
		for _, c := range legal {
			if trickWinnerOf(append(append([]Played(nil), played...), Played{Seat: seat, Card: c}), trump) == seat {
				return c
			}
		}
	}
	// Can't (or won't) take it: shed the lowest legal card.
	return legal[0]
}

// trickWinnerOf picks the winner of a partially (or fully) played trick under
// the given trump, using the same ranking as the engine.
func trickWinnerOf(trick []Played, trump Suit) Seat {
	best := trick[0]
	for _, p := range trick[1:] {
		ps, bs := EffectiveSuit(p.Card, trump), EffectiveSuit(best.Card, trump)
		switch {
		case ps == bs:
			if trickPower(p.Card, trump) > trickPower(best.Card, trump) {
				best = p
			}
		case ps == trump:
			best = p
		}
	}
	return best.Seat
}
