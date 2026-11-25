package euchre

import "testing"

func TestParseSuit(t *testing.T) {
	// Codes and full names, any case, with surrounding whitespace.
	cases := map[string]Suit{
		"C": Clubs, "c": Clubs, " CLUBS ": Clubs, "Clubs": Clubs,
		"D": Diamonds, "d": Diamonds, " Diamonds ": Diamonds, "DIAMONDS": Diamonds,
		"H": Hearts, "h": Hearts, " hearts": Hearts, "HeArTs": Hearts,
		"S": Spades, "s": Spades, "spades ": Spades, "SPADES": Spades,
	}
	for in, want := range cases {
		got, err := ParseSuit(in)
		if err != nil {
			t.Errorf("ParseSuit(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseSuit(%q) = %s, want %s", in, got, want)
		}
	}
	for _, bad := range []string{"", "banana", "club", "CD", "♣", "H1"} {
		if _, err := ParseSuit(bad); err == nil {
			t.Errorf("ParseSuit(%q) accepted, want error", bad)
		}
	}
}
