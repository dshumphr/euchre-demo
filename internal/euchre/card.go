// Package euchre implements the rules engine for standard 4-player euchre:
// 24 cards (9-A), two bidding rounds, bowers, going alone, tricks to 10 points.
package euchre

import (
	"fmt"
	"strings"
)

// Suit is one of the four suits.
type Suit int

const (
	Clubs Suit = iota
	Diamonds
	Hearts
	Spades
)

func (s Suit) String() string {
	switch s {
	case Clubs:
		return "clubs"
	case Diamonds:
		return "diamonds"
	case Hearts:
		return "hearts"
	case Spades:
		return "spades"
	}
	return "?"
}

// Code is the one-character wire code: C, D, H or S.
func (s Suit) Code() string { return string("CDHS"[int(s)]) }

// Glyph is the display symbol: ♣ ♦ ♠ ♥.
func (s Suit) Glyph() string { return [4]string{"♣", "♦", "♥", "♠"}[int(s)] }

// ParseSuit parses a one-character suit code.
func ParseSuit(s string) (Suit, error) {
	switch strings.ToUpper(s) {
	case "C":
		return Clubs, nil
	case "D":
		return Diamonds, nil
	case "H":
		return Hearts, nil
	case "S":
		return Spades, nil
	}
	return 0, fmt.Errorf("bad suit %q", s)
}

// Rank is a card rank, 9 through Ace.
type Rank int

const (
	Nine Rank = iota
	Ten
	Jack
	Queen
	King
	Ace
)

func (r Rank) String() string {
	switch r {
	case Nine:
		return "9"
	case Ten:
		return "10"
	case Jack:
		return "J"
	case Queen:
		return "Q"
	case King:
		return "K"
	case Ace:
		return "A"
	}
	return "?"
}

// Code is the one-character wire code for the rank: 9 T J Q K A (ten is T).
func (r Rank) Code() string { return string("9TJQKA"[int(r)]) }

// Card is a single card. Wire format is rank+suit code, e.g. "9C", "TS", "JH".
type Card struct {
	Rank Rank
	Suit Suit
}

func (c Card) String() string { return c.Rank.Code() + c.Suit.Code() }

// Glyph is the display form, e.g. "A♥".
func (c Card) Glyph() string { return c.Rank.String() + c.Suit.Glyph() }

// ParseCard parses a two-character card code like "9C", "TD", "JH".
func ParseCard(s string) (Card, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if len(s) != 2 {
		return Card{}, fmt.Errorf("bad card %q", s)
	}
	var r Rank
	switch s[0] {
	case '9':
		r = Nine
	case 'T':
		r = Ten
	case 'J':
		r = Jack
	case 'Q':
		r = Queen
	case 'K':
		r = King
	case 'A':
		r = Ace
	default:
		return Card{}, fmt.Errorf("bad rank in %q", s)
	}
	su, err := ParseSuit(string(s[1]))
	if err != nil {
		return Card{}, fmt.Errorf("bad suit in %q", s)
	}
	return Card{Rank: r, Suit: su}, nil
}

// Cards is a hand or trick as a card slice.
type Cards []Card

// Codes renders cards as wire codes.
func (cs Cards) Codes() []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.String()
	}
	return out
}

func (cs Cards) String() string { return strings.Join(cs.Codes(), " ") }

// EffectiveSuit maps a card to its suit for trick purposes: the jack of the
// same color as trump (the left bower) counts as trump.
func EffectiveSuit(c Card, trump Suit) Suit {
	if c.Rank == Jack && SameColor(c.Suit, trump) {
		return trump
	}
	return c.Suit
}

// SameColor reports whether two suits share a color (hearts/diamonds, clubs/spades).
func SameColor(a, b Suit) bool {
	switch a {
	case Clubs, Spades:
		return b == Clubs || b == Spades
	default:
		return b == Diamonds || b == Hearts
	}
}

// IsRightBower reports whether c is the jack of trump.
func IsRightBower(c Card, trump Suit) bool {
	return c.Rank == Jack && c.Suit == trump
}

// IsLeftBower reports whether c is the jack of the same-color suit.
func IsLeftBower(c Card, trump Suit) bool {
	return c.Rank == Jack && c.Suit != trump && SameColor(c.Suit, trump)
}

// Deck returns the 24-card euchre deck in a fixed order.
func Deck() Cards {
	var d Cards
	for s := Clubs; s <= Spades; s++ {
		for r := Nine; r <= Ace; r++ {
			d = append(d, Card{Rank: r, Suit: s})
		}
	}
	return d
}
