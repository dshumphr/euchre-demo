package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ── test harness ──────────────────────────────────────────────────────────

// testServer builds an app with a deterministic game and the loop goroutine
// running. All interaction is black-box: HTTP requests + published snapshots
// (the loop owns the game and drives the bots itself).
//
// Deterministic anchors (dealer south, hand 1):
//   - seed 11: west/north/east all pass → south is prompted in bid1; south's
//     order_up → south (dealer) must discard — a stable waiting state.
//   - seed 7: north orders up diamonds → south must discard immediately;
//     afterwards west leads and south's first play turn is a stable wait.
func testServer(t *testing.T, seed int64) *app {
	t.Helper()
	a := newApp(config{seed: seed})
	go a.loop()
	waitFor(t, a, func(s state) bool { return s.Phase != "" })
	return a
}

func waitFor(t *testing.T, a *app, pred func(state) bool) state {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second) // bots take ~500ms per decision
	for time.Now().Before(deadline) {
		var s state
		if err := json.Unmarshal(a.hub.last(), &s); err == nil && pred(s) {
			return s
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("state condition not reached in time; last: %s", a.hub.last())
	return state{}
}

func get(a *app, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	rec := httptest.NewRecorder()
	a.routes().ServeHTTP(rec, req)
	return rec
}

func post(a *app, path string, body any) *httptest.ResponseRecorder {
	tbuf, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", path, bytes.NewReader(tbuf))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.routes().ServeHTTP(rec, req)
	return rec
}

func decodeState(t *testing.T, body []byte) state {
	t.Helper()
	var s state
	if err := json.Unmarshal(body, &s); err != nil {
		t.Fatalf("snapshot decode: %v (%s)", err, body)
	}
	return s
}

func currentState(t *testing.T, a *app) state {
	t.Helper()
	return decodeState(t, get(a, "/api/state").Body.Bytes())
}

// ── bid handler ───────────────────────────────────────────────────────────

func TestBidHappyAndErrorPaths(t *testing.T) {
	a := testServer(t, 11) // bots pass → south is prompted in bid1

	st := waitFor(t, a, func(s state) bool {
		return s.Bidding.Active && s.Bidding.PromptTo != nil && *s.Bidding.PromptTo == "south"
	})
	if st.BidRound == nil || *st.BidRound != 1 {
		t.Fatalf("bidRound = %v, want 1", st.BidRound)
	}

	rec := post(a, "/api/bid", map[string]any{"decision": "pass"})
	if rec.Code != 200 {
		t.Fatalf("south bid pass: code %d (%s)", rec.Code, rec.Body)
	}
	st = decodeState(t, rec.Body.Bytes()) // POST replies carry the fresh snapshot
	if st.Phase != "bid2" {
		t.Fatalf("phase after south pass = %q, want bid2", st.Phase)
	}
	if st.SeatInfo["south"].LastBid == nil || *st.SeatInfo["south"].LastBid != "pass" {
		t.Errorf("south lastBid = %v, want pass", st.SeatInfo["south"].LastBid)
	}

	// The game moves on (west calls a suit); once south's play turn is a
	// stable wait, bidding must be rejected with a message.
	waitFor(t, a, func(s state) bool {
		return s.Phase == "play" && s.Turn != nil && *s.Turn == "south"
	})
	for _, bad := range []string{"banana", "order_up", "hearts", ""} {
		rec = post(a, "/api/bid", map[string]any{"decision": bad})
		if rec.Code != 409 {
			t.Errorf("bad decision %q: code %d, want 409 (%s)", bad, rec.Code, rec.Body)
		}
		var errBody map[string]string
		json.Unmarshal(rec.Body.Bytes(), &errBody)
		if errBody["error"] == "" {
			t.Errorf("409 body missing error field: %s", rec.Body)
		}
	}

	// malformed JSON body: 400
	rec = post(a, "/api/bid", "not-json-object")
	if rec.Code != 400 {
		t.Fatalf("malformed body: code %d, want 400", rec.Code)
	}
}

// ── play handler ──────────────────────────────────────────────────────────

func TestPlayHappyAndErrorPaths(t *testing.T) {
	a := testServer(t, 7) // north orders up → south discards → trick play

	// Not south's action during the discard phase: 409.
	if rec := post(a, "/api/play", map[string]any{"card": "K♦"}); rec.Code != 409 {
		t.Fatalf("play during discard: code %d, want 409 (%s)", rec.Code, rec.Body)
	}

	// Complete the discard so trick play opens; south's first play turn
	// (west leads, north/east bots follow) is a stable wait.
	dsc := waitFor(t, a, func(s state) bool {
		return s.Phase == "discard" && s.Turn != nil && *s.Turn == "south" && len(s.Legal) > 0
	})
	if rec := post(a, "/api/discard", map[string]any{"card": dsc.Legal[0]}); rec.Code != 200 {
		t.Fatalf("discard: %d (%s)", rec.Code, rec.Body)
	}
	st := waitFor(t, a, func(s state) bool {
		return s.Phase == "play" && s.Turn != nil && *s.Turn == "south" && len(s.Legal) > 0
	})
	card := st.Legal[0]

	// A card not in south's hand: 409.
	if rec := post(a, "/api/play", map[string]any{"card": "2♣"}); rec.Code != 409 {
		t.Fatalf("phantom card: code %d, want 409 (%s)", rec.Code, rec.Body)
	}

	rec := post(a, "/api/play", map[string]any{"card": card})
	if rec.Code != 200 {
		t.Fatalf("legal play %s: code %d (%s)", card, rec.Code, rec.Body)
	}
	after := decodeState(t, rec.Body.Bytes())
	if after.Tricks.NS+after.Tricks.EW != 1 {
		t.Errorf("tricks after south's play = %+v, want one trick complete", after.Tricks)
	}

	// An immediate replay of the same card is no longer south's action: 409.
	if rec := post(a, "/api/play", map[string]any{"card": card}); rec.Code != 409 {
		t.Fatalf("replayed card: code %d, want 409 (%s)", rec.Code, rec.Body)
	}
}

// ── discard handler ───────────────────────────────────────────────────────

func TestDiscardHandler(t *testing.T) {
	a := testServer(t, 7) // north orders up diamonds; south (dealer) discards

	st := waitFor(t, a, func(s state) bool {
		return s.Phase == "discard" && s.Turn != nil && *s.Turn == "south"
	})
	if st.Trump == nil || *st.Trump != "♦" {
		t.Errorf("trump = %v, want ♦ (north ordered up)", st.Trump)
	}
	if st.Maker == nil || *st.Maker != "north" {
		t.Errorf("maker = %v, want north", st.Maker)
	}
	if len(st.YourHand) != 6 || len(st.Legal) != 6 {
		t.Fatalf("yourHand/legal = %d/%d, want 6/6 (dealer picked up)", len(st.YourHand), len(st.Legal))
	}

	// Bidding is not awaited during the discard phase: 409.
	if rec := post(a, "/api/bid", map[string]any{"decision": "pass"}); rec.Code != 409 {
		t.Fatalf("bid during discard: code %d, want 409", rec.Code)
	}
	// A card not in the 6-card hand: 409.
	if rec := post(a, "/api/discard", map[string]any{"card": "AH"}); rec.Code != 409 {
		t.Fatalf("phantom discard: code %d, want 409 (%s)", rec.Code, rec.Body)
	}

	rec := post(a, "/api/discard", map[string]any{"card": st.Legal[0]})
	if rec.Code != 200 {
		t.Fatalf("discard: code %d (%s)", rec.Code, rec.Body)
	}
	st = decodeState(t, rec.Body.Bytes())
	if st.Phase != "play" {
		t.Errorf("phase after discard = %q, want play", st.Phase)
	}
	if len(st.YourHand) != 5 {
		t.Errorf("yourHand = %d, want 5", len(st.YourHand))
	}
}

// ── /api/new ──────────────────────────────────────────────────────────────

func TestNewGame(t *testing.T) {
	a := testServer(t, 7)

	// Mid-hand (discard phase): /api/new must yield a fresh deal.
	waitFor(t, a, func(s state) bool { return s.Phase == "discard" })
	rec := post(a, "/api/new", nil)
	if rec.Code != 200 {
		t.Fatalf("new: code %d (%s)", rec.Code, rec.Body)
	}
	st := decodeState(t, rec.Body.Bytes())
	if st.Phase != "bid1" {
		t.Fatalf("phase after /api/new = %q, want bid1", st.Phase)
	}
	if st.Hand != 1 {
		t.Errorf("hand after /api/new = %d, want 1", st.Hand)
	}
	for seat, b := range st.SeatInfo {
		if b.LastBid != nil {
			t.Errorf("seat %s lastBid = %v after new game, want nil", seat, b.LastBid)
		}
		if b.CardsLeft != 5 {
			t.Errorf("seat %s cardsLeft = %d after deal, want 5", seat, b.CardsLeft)
		}
	}
	if st.Tricks.NS != 0 || st.Tricks.EW != 0 {
		t.Errorf("tricks after /api/new = %+v, want zeros", st.Tricks)
	}
}

// ── auth ──────────────────────────────────────────────────────────────────

func TestTokenAuth(t *testing.T) {
	a := newApp(config{token: "s3cret"}) // no loop: /api/state answers 503 once auth passes
	handler := a.routes()

	for _, qs := range []string{"", "?t=wrong"} {
		rec := get(a, "/api/state"+qs)
		if rec.Code != 401 {
			t.Errorf("token check %q: code %d, want 401", qs, rec.Code)
		}
	}
	// Valid token → auth passes; the "state not ready" 503 proves it.
	rec := get(a, "/api/state?t=s3cret")
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "not ready") {
		t.Errorf("valid token: code %d (%s), want 503 not-ready (auth passed)", rec.Code, rec.Body)
	}
	// POST endpoints are gated too.
	req := httptest.NewRequest("POST", "/api/bid?t=s3cret", bytes.NewReader([]byte(`{}`)))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code == 401 {
		t.Error("POST with valid token was rejected as unauthenticated")
	}
	req = httptest.NewRequest("POST", "/api/new", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("POST /api/new without token: code %d, want 401", rec.Code)
	}
}

// ── SSE ───────────────────────────────────────────────────────────────────

func sseCollect(t *testing.T, a *app) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/events", nil)
	rec := httptest.NewRecorder() // implements Flusher
	go a.routes().ServeHTTP(rec, req)
	return rec
}

func sseWaitFor(t *testing.T, rec *httptest.ResponseRecorder, substr, what string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(rec.Body.String(), substr) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("SSE never carried %s; body so far: %.600s", what, rec.Body.String())
}

func TestSSEEmitsOnChange(t *testing.T) {
	a := testServer(t, 11)

	rec := sseCollect(t, a)
	sseWaitFor(t, rec, `"phase":"bid1"`, "initial bid1 snapshot")

	// A stable south decision point: order_up → the discard phase persists
	// until south discards, so its snapshot frame is guaranteed observable.
	waitFor(t, a, func(s state) bool {
		return s.Bidding.Active && s.Bidding.PromptTo != nil && *s.Bidding.PromptTo == "south"
	})
	if rec2 := post(a, "/api/bid", map[string]any{"decision": "order_up"}); rec2.Code != 200 {
		t.Fatalf("order_up: %d (%s)", rec2.Code, rec2.Body)
	}
	sseWaitFor(t, rec, `"phase":"discard"`, "discard snapshot after south's bid")
	if n := strings.Count(rec.Body.String(), "data: "); n < 2 {
		t.Errorf("want ≥2 SSE frames (initial + change), got %d", n)
	}
	// frames are full snapshots, not deltas
	if !strings.Contains(rec.Body.String(), `"seatInfo"`) || !strings.Contains(rec.Body.String(), `"yourHand"`) {
		t.Errorf("SSE frame missing full-state fields: %.200s", rec.Body.String())
	}
}

func TestSSEMultipleSubscribers(t *testing.T) {
	a := testServer(t, 11)

	recs := []*httptest.ResponseRecorder{sseCollect(t, a), sseCollect(t, a)}
	for _, rec := range recs {
		sseWaitFor(t, rec, `"phase":"bid1"`, "initial snapshot")
	}
	waitFor(t, a, func(s state) bool {
		return s.Bidding.Active && s.Bidding.PromptTo != nil && *s.Bidding.PromptTo == "south"
	})
	if rec := post(a, "/api/bid", map[string]any{"decision": "order_up"}); rec.Code != 200 {
		t.Fatalf("order_up: %d (%s)", rec.Code, rec.Body)
	}
	for _, rec := range recs {
		sseWaitFor(t, rec, `"phase":"discard"`, "discard snapshot")
	}
}

// ── static frontend ───────────────────────────────────────────────────────

func TestStaticServesEmbeddedFrontend(t *testing.T) {
	a := newApp(config{})
	handler := a.routes()
	for _, path := range []string{"/", "/app.js", "/style.css"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Errorf("GET %s: code %d, want 200 (embedded web/)", path, rec.Code)
		}
	}
	// http.FileServer canonically redirects /index.html → ./
	req := httptest.NewRequest("GET", "/index.html", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 301 {
		t.Errorf("GET /index.html: code %d, want 301 (FileServer redirect)", rec.Code)
	}
	req = httptest.NewRequest("GET", "/missing.txt", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Errorf("GET /missing.txt: code %d, want 404", rec.Code)
	}
}

// ── turn gate consistency ─────────────────────────────────────────────────

func TestSouthAwaitingGate(t *testing.T) {
	a := testServer(t, 7)
	dsc := waitFor(t, a, func(s state) bool { return s.Phase == "discard" })
	if a.southAwaiting("bid") || a.southAwaiting("play") {
		t.Error("southAwaiting(bid/play) = true during discard, want false")
	}
	if !a.southAwaiting("discard") {
		t.Error("southAwaiting(discard) = false during discard, want true")
	}
	if rec := post(a, "/api/discard", map[string]any{"card": dsc.Legal[0]}); rec.Code != 200 {
		t.Fatalf("discard: %d (%s)", rec.Code, rec.Body)
	}
	waitFor(t, a, func(s state) bool {
		return s.Phase == "play" && s.Turn != nil && *s.Turn == "south"
	})
	if a.southAwaiting("discard") || a.southAwaiting("bid") {
		t.Error("southAwaiting(discard/bid) = true during play, want false")
	}
	if !a.southAwaiting("play") {
		t.Error("southAwaiting(play) = false on south's play turn, want true")
	}
}
