// Command fakeagent is a deterministic stand-in for the omp harness agent in
// end-to-end tests: it reads the euchre stream file directly, claims seats,
// and answers the claimed seats' bids and plays from a fixed script — zero
// LLM calls. Script lines (consumed in order):
//
//	claim               send euchre.claim_seat {seat:"north"} on game.started
//	claim <s[,s…]>      send euchre.claim_seat {seats:[…]} on game.started
//	                    (setup lines "claim"/"release"/"hand" are consumed in
//	                    sequence when game.started arrives)
//	release <s[,s…]>    send euchre.claim_seat {seats:[…],release:true}
//	hand <seat>         send euchre.hand_request {seat}
//	bid <decision>      euchre.bid when a claimed seat's turn to bid ("pass",
//	                    "order_up", "H", …, optional "alone")
//	play <card>         euchre.play when a claimed seat must play a card (also
//	                    used for a claimed dealer's discard)
//	tip <text…>         euchre.tip, sent when the next hand.summary arrives
//	resign              euchre.resign {} immediately on the next event
//
// Decisions are consumed strictly in play order, so a multi-seat script lists
// the claimed seats' bids and cards in the exact order the seats act.
//
// When the script is exhausted the agent goes silent (which exercises the
// app's timeout fallback if the game continues).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"euchre-demo/internal/prox"
)

// seatOrder matches the engine's clockwise seats S→W→N→E.
var seatOrder = map[string]int{"south": 0, "west": 1, "north": 2, "east": 3}
var seatName = map[int]string{0: "south", 1: "west", 2: "north", 3: "east"}

func nextSeat(s int) int { return (s + 1) % 4 }

type agent struct {
	client  *prox.Client
	script  []string
	pos     int
	logf    func(string, ...any)
	claimed map[string]bool // seats this agent has claimed per its script

	// game-state shadow (turn tracking only)
	turnDown   string // top card / turned-down card from bid.prompt
	dealer     string
	aloneSeat  string // seat going alone, "" if none
	actor      int    // seat whose card play is awaited
	tracking   bool   // trick-play tracking active
	postedPlay bool   // sent a play; wait for the app to echo card.played
}

func main() {
	stream := flag.String("stream", "euchre", "proxenos stream name")
	scriptPath := flag.String("script", "", "north-seat script file")
	bin := flag.String("proxenos", "proxenos", "path to the proxenos binary")
	flag.Parse()

	data, err := os.ReadFile(*scriptPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakeagent:", err)
		os.Exit(1)
	}
	var script []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			script = append(script, line)
		}
	}

	a := &agent{client: prox.NewClientAs(*bin, *stream, "agent:fakeagent"), script: script, claimed: map[string]bool{}, logf: func(f string, args ...any) { fmt.Println("fakeagent· " + fmt.Sprintf(f, args...)) }}

	var path string
	for i := 0; i < 100; i++ {
		p, err := a.client.ResolvePath()
		if err == nil {
			path = p
			break
		}
		time.Sleep(150 * time.Millisecond) // the app creates the stream shortly
	}
	if path == "" {
		fmt.Fprintln(os.Stderr, "fakeagent: stream never appeared")
		os.Exit(1)
	}
	prox.StartPoll(path, 0, func(ev prox.Event, _ bool) { a.onEvent(ev) }, a.logf)

	// The app exits after game.over; nothing keeps us alive then — poll until
	// the game ends or a generous deadline passes.
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(250 * time.Millisecond)
	}
	fmt.Fprintln(os.Stderr, "fakeagent: deadline reached without game.over")
	os.Exit(3)
}

func (a *agent) peekLine() (string, bool) {
	if a.pos >= len(a.script) {
		return "", false
	}
	return a.script[a.pos], true
}

func (a *agent) nextLine() (string, bool) {
	if a.pos >= len(a.script) {
		return "", false
	}
	line := a.script[a.pos]
	a.pos++
	return line, true
}

func (a *agent) onEvent(ev prox.Event) {
	if ev.Src == "agent:fakeagent" {
		return // our own sends, echoed back
	}
	a.logf("recv %s (src %s)", ev.Type, ev.Src)
	if line, ok := a.peekLine(); ok && line == "resign" {
		a.pos++
		a.send("euchre.resign", map[string]any{})
		return
	}
	switch ev.Type {
	case "game.started":
		a.consumeSetup()
	case "bid.prompt":
		var body struct {
			TopCard string `json:"topCard"`
			Dealer  string `json:"dealer"`
			Round   int    `json:"round"`
			Turn    string `json:"turn"`
		}
		if ev.DecodeBody(&body) != nil {
			return
		}
		a.dealer = body.Dealer
		a.turnDown = body.TopCard
		a.tracking = false
		a.aloneSeat = ""
		if a.claimed[body.Turn] {
			a.sendBid()
		}
	case "bid.made":
		var body struct {
			Seat     string `json:"seat"`
			Decision string `json:"decision"`
			Alone    bool   `json:"alone"`
		}
		if ev.DecodeBody(&body) != nil {
			return
		}
		if body.Alone {
			a.aloneSeat = body.Seat
		}
		if body.Decision == "order_up" {
			// The dealer picks up and discards. No card.played is emitted for
			// a discard, so if the dealer is a claimed seat the script
			// provides the discard (as a "play" line), and trick tracking
			// resumes on the next trick.started.
			if a.claimed[a.dealer] {
				if line, ok := a.nextPlayLine(); ok {
					a.send("euchre.play", map[string]any{"card": line})
					a.postedPlay = true
				}
			}
		}
	case "trick.started":
		var body struct {
			Leader string `json:"leader"`
		}
		if ev.DecodeBody(&body) != nil {
			return
		}
		a.tracking = true
		a.postedPlay = false
		a.actor = seatOrder[body.Leader]
		a.skipSatOut()
		a.maybePlay()
	case "card.played":
		var body struct {
			Seat string `json:"seat"`
		}
		if ev.DecodeBody(&body) != nil {
			return
		}
		if a.claimed[body.Seat] {
			a.postedPlay = false
		}
		if !a.tracking {
			return
		}
		a.actor = nextSeat(seatOrder[body.Seat])
		a.skipSatOut()
		a.maybePlay()
	case "hand.summary":
		if line, ok := a.peekLine(); ok && strings.HasPrefix(line, "tip ") {
			a.pos++
			a.send("euchre.tip", map[string]any{"text": strings.TrimPrefix(line, "tip ")})
		}
	case "game.over":
		os.Exit(0)
	}
}

// skipSatOut advances past the partner of a lone bidder.
func (a *agent) skipSatOut() {
	if a.aloneSeat != "" && seatName[a.actor] == a.aloneSeat {
		a.actor = nextSeat(a.actor)
	}
}

func (a *agent) maybePlay() {
	if a.postedPlay || !a.claimed[seatName[a.actor]] {
		return
	}
	line, ok := a.nextPlayLine()
	if !ok {
		return
	}
	a.send("euchre.play", map[string]any{"card": line})
	a.postedPlay = true
}

// consumeSetup consumes the leading setup lines (claim/release/hand) that are
// sent once, when the game starts. The first non-setup line is left in place.
func (a *agent) consumeSetup() {
	for {
		line, ok := a.peekLine()
		if !ok {
			return
		}
		switch {
		case line == "claim":
			a.pos++
			a.claimed["north"] = true
			a.send("euchre.claim_seat", map[string]any{"seat": "north"})
		case strings.HasPrefix(line, "claim "):
			a.pos++
			seats := splitSeats(line, "claim ")
			for _, s := range seats {
				a.claimed[s] = true
			}
			a.send("euchre.claim_seat", map[string]any{"seats": seats})
		case strings.HasPrefix(line, "release "):
			a.pos++
			seats := splitSeats(line, "release ")
			for _, s := range seats {
				delete(a.claimed, s)
			}
			a.send("euchre.claim_seat", map[string]any{"seats": seats, "release": true})
		case strings.HasPrefix(line, "hand "):
			a.pos++
			a.send("euchre.hand_request", map[string]any{"seat": strings.TrimSpace(strings.TrimPrefix(line, "hand "))})
		default:
			return
		}
	}
}

// splitSeats parses a comma-separated seat list after the given prefix.
func splitSeats(line, prefix string) []string {
	var seats []string
	for _, f := range strings.Split(strings.TrimPrefix(line, prefix), ",") {
		if s := strings.TrimSpace(f); s != "" {
			seats = append(seats, s)
		}
	}
	return seats
}

// nextPlayLine consumes the next "play <card>" script line — only if it is
// the very next line (non-play lines are left for their own handlers).
func (a *agent) nextPlayLine() (string, bool) {
	if line, ok := a.peekLine(); ok && strings.HasPrefix(line, "play ") {
		a.pos++
		return strings.TrimSpace(strings.TrimPrefix(line, "play ")), true
	}
	return "", false
}

func (a *agent) sendBid() {
	line, ok := a.peekLine()
	if !ok || !strings.HasPrefix(line, "bid ") {
		if ok {
			a.logf("script: expected bid line, got %q", line)
		}
		return
	}
	a.pos++
	f := strings.Fields(line)
	body := map[string]any{"decision": f[1]}
	for _, w := range f[2:] {
		if w == "alone" {
			body["goingAlone"] = true
		}
	}
	a.send("euchre.bid", body)
}

func (a *agent) send(typ string, body any) {
	if _, err := a.client.Send(typ, body); err != nil {
		a.logf("send %s failed: %v", typ, err)
		return
	}
	b, _ := json.Marshal(body)
	a.logf("sent %s %s", typ, b)
}
