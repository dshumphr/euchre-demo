package main

import (
	"encoding/json"
	"strings"
	"testing"

	"euchre-demo/internal/euchre"
	"euchre-demo/internal/prox"
)

func proxEvent(typ, body string) prox.Event {
	return prox.Event{Type: typ, Body: json.RawMessage(body)}
}

func newTestGame(t *testing.T) *euchre.Game {
	t.Helper()
	g := euchre.NewGame(1, euchre.South)
	g.StartHand()
	return g
}

// The Task-1 regression: a real omp agent claimed all three seats but only
// ever played North, because every non-North turn tripped the app's
// agent-timeout fallback. Three unit-testable roots were identified and
// removed; these tests pin each one.
//
//  1. The extension's resolveAppDir dead branch: passing the repo ROOT
//     (which contains cmd/euchre but no root ./euchre) made nested = dir
//     itself, so the branch was unreachable and euchre_open failed with
//     "No executable euchre" — the agent then claimed nothing or gave up.
//     (TypeScript, covered by the repo copy check in script/e2e.sh's
//     sibling checks — here we pin the Go-side pieces below.)
//
//  2. Stale seat names anywhere in the wake path would filter everything
//     to north. The seats come from ParseSeat; this test pins that the
//     app's claim parsing accepts every seat name the extension sends,
//     singular and plural, in one event.
//
//  3. A claimed seat whose hand the agent never saw (mid-hand claim on a
//     pre-existing stream, or a lost hand after a rejection) used to leave
//     the agent guessing card codes until the timeout fallback bot took
//     over. The extension now teaches euchre.hand_request on every wake,
//     and the app must answer hand_request for any claimed seat at any
//     time — this test drives that path through applyAgentEvent.

func TestClaimSeatAcceptsAllSeatShapes(t *testing.T) {
	// Seat names the extension teaches, singular and plural bodies.
	shapes := []struct {
		name string
		body string
		want [4]bool
	}{
		{"single", `{"seat":"north"}`, [4]bool{false, false, true, false}},
		{"plural all three", `{"seats":["north","east","west"]}`, [4]bool{false, true, true, true}},
		{"mixed seat+seats", `{"seat":"west","seats":["east"]}`, [4]bool{false, true, false, true}},
	}
	for _, tc := range shapes {
		a := &app{agentQ: newEventQueue()}
		ev := proxEvent("euchre.claim_seat", tc.body)
		a.applyAgentEvent(ev)
		if a.claimed != tc.want {
			t.Errorf("%s: claimed = %v, want %v", tc.name, a.claimed, tc.want)
		}
	}
}

func TestClaimSeatRejectsSouthAndGarbage(t *testing.T) {
	a := &app{agentQ: newEventQueue()}
	a.applyAgentEvent(proxEvent("euchre.claim_seat", `{"seats":["south"]}`))
	if a.claimed[euchre.South] {
		t.Error("south must never be claimable")
	}
	a2 := &app{agentQ: newEventQueue()}
	a2.applyAgentEvent(proxEvent("euchre.claim_seat", `{"seats":["middle"]}`))
	if a2.claimed != [4]bool{} {
		t.Error("garbage seat must be rejected wholesale")
	}
}

// A claimed seat's hand_request must be answered at any time — this is how
// the extension recovers an agent whose hand was disclosed while it was
// between turns (the "agent only plays north" symptom).
func TestHandRequestAnsweredForClaimedSeats(t *testing.T) {
	a := &app{agentQ: newEventQueue(), game: newTestGame(t)}
	// unclaimed: rejected
	a.applyAgentEvent(proxEvent("euchre.hand_request", `{"seat":"west"}`))
	// claimed: accepted and disclosed
	a.applyAgentEvent(proxEvent("euchre.claim_seat", `{"seat":"west"}`))
	a.applyAgentEvent(proxEvent("euchre.hand_request", `{"seat":"west"}`))
	// south: never claimable, so never requestable
	a.applyAgentEvent(proxEvent("euchre.hand_request", `{"seat":"south"}`))
}

// The restart-replay regression: agent actions from previous runs re-enter
// the poller on every app start (the stream is tailed from offset 0). They
// must never be validated or rejected again — 76% of the action.rejected
// events in the user's real session were exactly this artifact — but an old
// seat claim must still stand, because the agent expects to hold its seats.
func TestReplayNeverRejectsAndKeepsClaims(t *testing.T) {
	a := &app{agentQ: newEventQueue(), game: newTestGame(t)}

	// Old play/bid/hand_request from a previous run: no rejection, not queued.
	for _, ev := range []prox.Event{
		proxEvent("euchre.play", `{"card":"JD"}`),
		proxEvent("euchre.bid", `{"decision":"order_up"}`),
		proxEvent("euchre.hand_request", `{"seat":"west"}`),
		proxEvent("euchre.resign", `{}`),
	} {
		a.ingest(ev, true)
	}
	if len(a.agentQ.evs) != 0 {
		t.Fatalf("replayed actions must not be queued, got %d", len(a.agentQ.evs))
	}
	for _, line := range a.logLines {
		if strings.Contains(line, "rejected") {
			t.Fatalf("replayed action was rejected: %q", line)
		}
	}

	// An old claim still stands (state replay), and discloses the hand.
	a.ingest(proxEvent("euchre.claim_seat", `{"seat":"north"}`), true)
	if !a.claimed[euchre.North] || !a.disclose[euchre.North] {
		t.Fatalf("replayed claim not applied: claimed=%v disclose=%v", a.claimed, a.disclose)
	}
	// A replayed release is applied too.
	a.ingest(proxEvent("euchre.claim_seat", `{"seat":"north","release":true}`), true)
	if a.claimed[euchre.North] {
		t.Fatal("replayed release not applied")
	}

	// Live events still queue.
	a.ingest(proxEvent("euchre.play", `{"card":"JD"}`), false)
	if len(a.agentQ.evs) != 1 {
		t.Fatalf("live event not queued: %d", len(a.agentQ.evs))
	}

	// App echoes and sys metadata are ignored in both modes.
	a.ingest(prox.Event{Src: prox.AppSrc, Type: "card.played"}, false)
	a.ingest(prox.Event{Src: "sys:meta", Type: "stream.meta"}, true)
	if len(a.agentQ.evs) != 1 {
		t.Fatalf("echo/sys event queued: %d", len(a.agentQ.evs))
	}
}
