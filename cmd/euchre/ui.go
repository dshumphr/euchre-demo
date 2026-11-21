package main

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"euchre-demo/internal/euchre"
)

// ── ANSI color helpers ─────────────────────────────────────────────────────

// ANSI SGR escape sequences. The renderer only runs on a terminal (draw()
// returns early otherwise), so raw sequences are safe; all output ends with
// Reset so nothing bleeds across lines.
const (
	ansiReset  = "\033[0m"
	ansiBold   = "\033[1m"
	ansiDim    = "\033[2m"
	ansiRed    = "\033[31m"
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
	ansiBlue   = "\033[34m"
	ansiWhite  = "\033[97m"
	ansiCyan   = "\033[36m"
)

// paint wraps s in the given SGR codes (each already ends with an implicit
// reset when the caller appends ansiReset).
func paint(code, s string) string {
	if s == "" {
		return ""
	}
	return code + s + ansiReset
}

// cardPaint colors a card glyph by suit: red for hearts/diamonds,
// white/bold for clubs/spades.
func cardPaint(c euchre.Card, trump euchre.Suit) string {
	s := c.Glyph()
	switch c.Suit {
	case euchre.Hearts, euchre.Diamonds:
		return paint(ansiRed, s)
	default:
		return paint(ansiWhite+ansiBold, s)
	}
}

// seatPaint styles a seat label: the seat whose turn it is gets a bright
// cyan pointer, claimed seats stand out, bot seats stay dim.
func seatPaint(s euchre.Seat, a *app) string {
	label := seatTitle[s]
	var code string
	switch {
	case s == euchre.South:
		code = ansiBold
	case a.claimed[s]:
		code = ansiGreen + ansiBold
	default:
		code = ansiDim
	}
	if a.isSeatTurn(s) {
		code += ansiCyan
	}
	return paint(code, label)
}

// ── ANSI-aware line fitting ────────────────────────────────────────────────

// skipAnsi returns the index just past the escape sequence starting at
// s[i] (which must be ESC). Handles CSI (…final byte) and OSC (…BEL/ST).
func skipAnsi(s string, i int) int {
	if i+1 >= len(s) {
		return len(s)
	}
	switch s[i+1] {
	case '[': // CSI: consume through the final byte (0x40–0x7E)
		for j := i + 2; j < len(s); j++ {
			if s[j] >= 0x40 && s[j] <= 0x7e {
				return j + 1
			}
		}
		return len(s)
	case ']': // OSC: consume through BEL or ESC \
		for j := i + 2; j < len(s); j++ {
			if s[j] == 0x07 {
				return j + 1
			}
			if s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\' {
				return j + 2
			}
		}
		return len(s)
	default:
		return i + 2
	}
}

// visibleLen counts printable cells in s, ignoring ANSI escape sequences.
// Every glyph the renderer emits (cards, suits, ★ ◀ · …) is single-width.
func visibleLen(s string) int {
	n := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			i = skipAnsi(s, i)
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		n++
		i += size
	}
	return n
}

// truncVis fits s to n visible cells without ever splitting an escape
// sequence; the tail is dropped whole. If anything was cut, a reset is
// appended so a truncated color run can't bleed into the next line.
func truncVis(s string, n int) string {
	if visibleLen(s) <= n {
		return s
	}
	var b strings.Builder
	w := 0
	for i := 0; i < len(s) && w < n; {
		if s[i] == 0x1b {
			j := skipAnsi(s, i)
			b.WriteString(s[i:j])
			i = j
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		b.WriteRune(r)
		w++
		i += size
	}
	b.WriteString(ansiReset)
	return b.String()
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

var seatTitle = [4]string{"SOUTH (you)", "WEST", "NORTH", "EAST"}

// isSeatTurn reports whether s is the seat the game is waiting on (bid,
// discard or play).
func (a *app) isSeatTurn(s euchre.Seat) bool {
	g := a.game
	switch g.Phase {
	case euchre.PhaseBid1, euchre.PhaseBid2:
		return g.BidTurn() == s
	case euchre.PhaseDiscard:
		return g.Dealer == s
	default:
		return !g.Over && g.Phase == euchre.PhasePlay && g.Turn() == s
	}
}

// ── table rendering ─────────────────────────────────────────────────────────

// draw renders the full table. Layout (real-table seating):
//
//	                 NORTH
//	                  9♠
//	     WEST  ┌──────────────┐  EAST
//	     T♥    │   W   N      │  J♦
//	           │      S       │
//	           └──────────────┘
//	                 YOU
//
// with the trick card rendered NEXT TO the seat that played it, a status bar
// (hand, dealer, trump, bid state, tricks NS/EW, score NS/EW), and South's
// hand at the bottom — legal plays marked, illegal dimmed, bidding prompts
// inline.
//
// The layout is width-aware: the terminal size is fetched at draw time
// (TIOCGWINSZ, default 80) and every line is clipped to width-1, so nothing
// ever wraps or drifts. The trick box shrinks with the available width; west
// and east cards always sit against the box edges; below 75 columns the tips
// panel and log tail compact and bid indicators drop to single symbols.
func (a *app) draw() {
	if !a.interact {
		return // non-interactive runs log one line per action instead
	}
	termW := terminalWidth()
	lo := layout{w: termW - 1, compact: termW < 75}
	g := a.game

	// current trick card per seat (public info only)
	trick := g.Trick()
	cardAt := make(map[euchre.Seat]euchre.Card, 4)
	for _, p := range trick {
		cardAt[p.Seat] = p.Card
	}

	lines := []string{a.statusLine(lo)}
	lines = append(lines, a.tableLines(cardAt, lo)...)
	lines = append(lines, "", a.handRow(lo))

	// tips panel (3 rows wide layout, 2 compact)
	if len(a.tips) > 0 {
		lines = append(lines, a.tipsLines(lo)...)
	}

	// event log tail (6 rows wide layout, 3 compact)
	lines = append(lines, "")
	nLog := 6
	if lo.compact {
		nLog = 3
	}
	start := len(a.logLines) - nLog
	if start < 0 {
		start = 0
	}
	for _, l := range a.logLines[start:] {
		lines = append(lines, "  · "+l)
	}

	// hard guarantee: no line reaches the last column, so no wraps, no
	// scroll drift, no garbled ANSI.
	for i, l := range lines {
		lines[i] = truncVis(l, lo.w)
	}
	fmt.Print("\033[H\033[2J" + strings.Join(lines, "\n") + "\n")
}

// layout carries the per-draw width budget to the render helpers.
type layout struct {
	w       int  // max visible cells per line (terminal width - 1)
	compact bool // terminal narrower than 75 columns
}

// statusLine builds the always-visible bookkeeping bar: hand, dealer,
// trump, bid indicator, tricks NS/EW, score NS/EW. Compact mode shortens
// the labels so the bar fits ~55 columns without losing the score.
func (a *app) statusLine(lo layout) string {
	g := a.game
	var s string
	if lo.compact {
		s += paint(ansiBold, fmt.Sprintf("hand %d", g.HandNo))
		s += "  dlr " + paint(ansiYellow, g.Dealer.String())
	} else {
		s += paint(ansiBold, fmt.Sprintf("EUCHRE — hand %d", g.HandNo))
		s += "  dealer " + paint(ansiYellow, g.Dealer.String())
	}

	if tr, ok := g.Trump(); ok {
		s += "  " + trumpBadge(tr)
		if by := paint(ansiDim, " (by "+a.makerSeat()+")"); visibleLen(s)+visibleLen(by) <= lo.w {
			s += by
		}
		if seat, alone := g.Alone(); alone {
			if al := paint(ansiRed+ansiBold, "  ★ "+seat.String()+" ALONE"); visibleLen(s)+visibleLen(al) <= lo.w {
				s += al
			}
		}
	} else if lo.compact {
		s += "  " + paint(ansiDim, "bid r"+fmt.Sprint(g.Round()))
	} else {
		s += "  " + paint(ansiDim, "bidding round "+fmt.Sprint(g.Round()))
	}

	tw := tricksNSE(g)
	if lo.compact {
		s += "  tr " + paint(ansiGreen, fmt.Sprintf("NS%d", tw[0])) +
			paint(ansiRed, fmt.Sprintf("/EW%d", tw[1]))
		s += "  sc " + paint(ansiGreen+ansiBold, fmt.Sprintf("NS%d", g.Score[euchre.NS])) +
			paint(ansiRed+ansiBold, fmt.Sprintf("/EW%d", g.Score[euchre.EW]))
		return s
	}
	s += "  tricks " + paint(ansiGreen, fmt.Sprintf("NS %d", tw[0])) +
		" / " + paint(ansiRed, fmt.Sprintf("EW %d", tw[1]))
	s += "  score " + paint(ansiGreen+ansiBold, fmt.Sprintf("NS %d", g.Score[euchre.NS])) +
		" / " + paint(ansiRed+ansiBold, fmt.Sprintf("EW %d", g.Score[euchre.EW]))
	if to10 := paint(ansiDim, " (to 10)"); visibleLen(s)+visibleLen(to10) <= lo.w {
		s += to10
	}
	return s
}

// trumpBadge renders the trump suit glyph, highlighted.
func trumpBadge(tr euchre.Suit) string {
	return paint(ansiYellow+ansiBold, "★"+tr.Glyph())
}

// makerSeat finds the seat that declared trump (the bidder whose call set
// the maker team, from the hand's bid log).
func (a *app) makerSeat() string {
	g := a.game
	for _, bd := range g.Bids() {
		if bd.Call != euchre.CallPass && euchre.TeamOf(bd.Seat) == g.Maker() {
			return bd.Seat.String()
		}
	}
	return "?"
}

// seatRow renders one seat line: label, played card this trick, cards left,
// claim/bid indicator, turn pointer. No fixed padding — the caller places
// the row relative to the trick box.
func (a *app) seatRow(s euchre.Seat, cardAt map[euchre.Seat]euchre.Card, lo layout) string {
	g := a.game
	var b strings.Builder
	b.WriteString(seatPaint(s, a))

	// played card this trick
	if c, ok := cardAt[s]; ok {
		b.WriteString("  " + cardPaint(c, displayTrump(g)))
	} else {
		b.WriteString("   " + paint(ansiDim, "·"))
	}

	// cards left (public count)
	n := len(g.Hand(s))
	if g.Phase == euchre.PhaseDiscard && s == g.Dealer {
		n = len(g.DiscardHand())
	}
	b.WriteString(paint(ansiDim, fmt.Sprintf("  · %d card", n)))
	if n != 1 {
		b.WriteString("s")
	}

	// claim marker + bid indicator
	if s != euchre.South {
		if a.claimed[s] {
			b.WriteString(paint(ansiGreen+ansiBold, " [agent]"))
		} else {
			b.WriteString(paint(ansiDim, " [bot]"))
		}
	}
	if bd, ok := a.bidFor(s); ok {
		b.WriteString("  " + paint(ansiCyan, bidTag(bd, lo.compact)))
	}
	if a.isSeatTurn(s) && !g.Over {
		b.WriteString(paint(ansiCyan+ansiBold, "  ◀ turn"))
	}
	return b.String()
}

// bidTag renders a seat's bid: full text normally, a single symbol when the
// terminal is narrow (↑ order-up, – pass, suit glyph, ! = alone).
func bidTag(bd euchre.Bid, compact bool) string {
	if !compact {
		return describeBid(bd.Call, bd.Alone)
	}
	switch bd.Call {
	case euchre.CallOrderUp:
		if bd.Alone {
			return "↑!"
		}
		return "↑"
	case euchre.CallPass:
		return "–"
	default:
		if bd.Alone {
			return string(bd.Call) + "!"
		}
		return string(bd.Call)
	}
}

// bidFor returns the hand's bid for s (order_up/pass/suit call).
func (a *app) bidFor(s euchre.Seat) (euchre.Bid, bool) {
	for _, bd := range a.game.Bids() {
		if bd.Seat == s {
			return bd, true
		}
	}
	return euchre.Bid{}, false
}

// tableLines renders the trick area: north's row on top, west's card against
// the box's left edge and east's against its right edge, north's card at the
// top inside the box, south's at the bottom, south's row below. The box
// shrinks with the terminal width (inner = clamp(width-40, 16, 32)) and the
// whole table is centered; padding is computed from the actual box width, so
// cards hug the box edges at every width.
func (a *app) tableLines(cardAt map[euchre.Seat]euchre.Card, lo layout) []string {
	g := a.game
	tr := displayTrump(g)
	fmtCard := func(c euchre.Card, ok bool) string {
		if !ok {
			return paint(ansiDim, "·")
		}
		return cardPaint(c, tr)
	}
	nc, nok := cardAt[euchre.North]
	wc, wok := cardAt[euchre.West]
	ec, eok := cardAt[euchre.East]
	sc, sok := cardAt[euchre.South]

	innerW := clamp(lo.w-40, 16, 32)
	boxW := innerW + 2

	westSeg := seatPaint(euchre.West, a) + " " + fmtCard(wc, wok)
	eastSeg := fmtCard(ec, eok) + " " + seatPaint(euchre.East, a)
	wv := visibleLen(westSeg)
	ev := visibleLen(eastSeg)

	// table block: margin + west card + gap + box + gap + east card
	tableW := 2 + wv + 1 + boxW + 1 + ev
	left := (lo.w - tableW) / 2
	if left < 2 {
		left = 2
	}
	pad := strings.Repeat(" ", left)
	// column where the box starts: west's card touches the left edge
	boxCol := left + 2 + wv + 1
	boxPad := strings.Repeat(" ", boxCol)

	center := func(s string) string { // center a card glyph inside the box
		vw := visibleLen(s)
		l := (innerW - vw) / 2
		return strings.Repeat(" ", l) + s + strings.Repeat(" ", innerW-vw-l)
	}
	// center a seat row on the box's center column, clamped to the screen
	seatLine := func(row string) string {
		vw := visibleLen(row)
		col := boxCol + boxW/2 - vw/2
		if col < 2 {
			col = 2
		}
		if col+vw > lo.w {
			col = lo.w - vw
		}
		if col < 2 {
			col = 2
		}
		return strings.Repeat(" ", col) + row
	}

	h := strings.Repeat("─", innerW)
	boxRow := func(mid string) string { return boxPad + "│" + mid + "│" }

	return []string{
		seatLine(a.seatRow(euchre.North, cardAt, lo)),
		pad + westSeg + " ┌" + h + "┐ " + eastSeg,
		boxRow(center(fmtCard(nc, nok))),
		boxRow(strings.Repeat(" ", innerW)),
		boxRow(center(fmtCard(sc, sok))),
		boxPad + "└" + h + "┘",
		seatLine(a.seatRow(euchre.South, cardAt, lo)),
	}
}

// tipsLines renders the tips panel: 3 rows wide layout, 2 compact, border
// width following the terminal.
func (a *app) tipsLines(lo layout) []string {
	n := 3
	if lo.compact {
		n = 2
	}
	start := len(a.tips) - n
	if start < 0 {
		start = 0
	}
	pw := clamp(lo.w-4, 16, 47)
	lines := []string{
		"",
		"  ┌─ tips " + strings.Repeat("─", pw-11) + "┐",
	}
	for _, t := range a.tips[start:] {
		lines = append(lines, "  │ "+truncVis(t, pw-5))
	}
	lines = append(lines, "  └"+strings.Repeat("─", pw-4)+"┘")
	return lines
}

// handRow renders South's hand: sorted, legal plays marked with their pick
// number, illegal cards dimmed. During bidding, the prompt is inline.
// Compact mode shortens the prompt and separator spacing so five cards fit
// even at ~55 columns.
func (a *app) handRow(lo layout) string {
	g := a.game
	switch g.Phase {
	case euchre.PhaseBid1:
		if lo.compact {
			return "  " + paint(ansiBold, "Your bid") +
				paint(ansiDim, " (top ") + cardPaint(g.TopCard(), euchre.Clubs) +
				paint(ansiDim, "): [o]rder up / [p]ass")
		}
		return "  " + paint(ansiBold, "Your bid") +
			paint(ansiDim, " — top card ") + cardPaint(g.TopCard(), euchre.Clubs) +
			paint(ansiDim, ": [o]rder up / [p]ass / <suit> alone")
	case euchre.PhaseBid2:
		if lo.compact {
			return "  " + paint(ansiBold, "Your bid") +
				paint(ansiDim, " (turned ") + cardPaint(g.TurnDown(), euchre.Clubs) +
				paint(ansiDim, "): trump C/D/H/S or [p]ass")
		}
		return "  " + paint(ansiBold, "Your bid") +
			paint(ansiDim, " — turned down ") + cardPaint(g.TurnDown(), euchre.Clubs) +
			paint(ansiDim, ": name trump (C/D/H/S) or [p]ass")
	default:
		legal := map[euchre.Card]bool{}
		if g.Phase == euchre.PhasePlay && g.Turn() == euchre.South {
			for _, c := range g.LegalPlays(euchre.South) {
				legal[c] = true
			}
		} else if g.Phase == euchre.PhaseDiscard && g.Dealer == euchre.South {
			for _, c := range g.DiscardHand() {
				legal[c] = true
			}
		}
		hand := euchre.SortHand(g.Hand(euchre.South), displayTrump(g))
		if g.Phase == euchre.PhaseDiscard && g.Dealer == euchre.South {
			hand = euchre.SortHand(g.DiscardHand(), displayTrump(g))
		}
		if len(hand) == 0 {
			return ""
		}
		var parts []string
		num := 1
		for _, c := range hand {
			p := cardPaint(c, displayTrump(g))
			if len(legal) == 0 || legal[c] {
				p = fmt.Sprintf("%d:%s", num, p)
				num++
			} else {
				p = " " + paint(ansiDim, c.Glyph())
			}
			parts = append(parts, p)
		}
		sep := "   "
		if lo.compact {
			sep = " "
		}
		verb := "Your play"
		if g.Phase == euchre.PhaseDiscard {
			verb = "Your discard"
		}
		if lo.compact {
			return "  " + paint(ansiBold, verb+":") + " " + strings.Join(parts, sep)
		}
		return "  " + paint(ansiBold, verb) + paint(ansiDim, " — number or code:") + "  " + strings.Join(parts, sep)
	}
}
