package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"euchre-demo/internal/euchre"
	webassets "euchre-demo"
)

// mustWebSub returns the embedded frontend filesystem rooted at web/.
func mustWebSub() fs.FS {
	sub, err := fs.Sub(webassets.Web, "web")
	if err != nil {
		panic("embedded web/ missing: " + err.Error())
	}
	return sub
}

// ── frontend ──────────────────────────────────────────────────────────────

// The frontend (repo-root web/: index.html, style.css, app.js) is embedded
// verbatim by the webassets package at the module root — go:embed patterns
// are package-relative, so the embed lives one level above web/. Nothing
// copies or rewrites those files.

// ── snapshot hub (SSE fan-out) ────────────────────────────────────────────

// subscriber carries the newest snapshot for one SSE connection. Publishing
// never blocks on a slow client: the latest snapshot is overwritten in place
// and the reader drains it on notify — every snapshot is full state, so
// skipping intermediates is lossless for the client.
type subscriber struct {
	mu     sync.Mutex
	latest []byte
	notify chan struct{}
}

func (s *subscriber) push(b []byte) {
	s.mu.Lock()
	s.latest = b
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *subscriber) drain() []byte {
	s.mu.Lock()
	b := s.latest
	s.mu.Unlock()
	return b
}

type hub struct {
	mu     sync.Mutex
	subs   map[*subscriber]bool
	latest []byte
}

func newHub() *hub {
	return &hub{subs: map[*subscriber]bool{}}
}

func (h *hub) publish(b []byte) {
	h.mu.Lock()
	h.latest = b
	subs := make([]*subscriber, 0, len(h.subs))
	for s := range h.subs {
		subs = append(subs, s)
	}
	h.mu.Unlock()
	for _, s := range subs {
		s.push(b)
	}
}

func (h *hub) last() []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.latest
}

func (h *hub) add() *subscriber {
	s := &subscriber{notify: make(chan struct{}, 1)}
	h.mu.Lock()
	h.subs[s] = true
	h.mu.Unlock()
	return s
}

func (h *hub) remove(s *subscriber) {
	h.mu.Lock()
	delete(h.subs, s)
	h.mu.Unlock()
}

// ── HTTP requests into the game loop ──────────────────────────────────────

// req is one South decision (or a new-game request) handed to the loop
// goroutine, which owns the game. The loop replies with either an error
// message (surfaced as 409) or the fresh state snapshot.
type req struct {
	kind  string // "bid" | "play" | "discard" | "new"
	call  euchre.Call
	alone bool
	card  euchre.Card

	reply chan reqReply
}

type reqReply struct {
	err  string
	snap []byte
}

func (a *app) replyOK(r *req) {
	r.reply <- reqReply{snap: a.hub.last()}
}

func (a *app) replyErr(r *req, msg string) {
	r.reply <- reqReply{err: msg}
}

// ── server ────────────────────────────────────────────────────────────────

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServer(http.FS(mustWebSub())))
	mux.HandleFunc("GET /api/state", a.auth(a.handleState))
	mux.HandleFunc("GET /api/events", a.auth(a.handleEvents))
	mux.HandleFunc("POST /api/bid", a.auth(a.handleBid))
	mux.HandleFunc("POST /api/play", a.auth(a.handlePlay))
	mux.HandleFunc("POST /api/discard", a.auth(a.handleDiscard))
	mux.HandleFunc("POST /api/new", a.auth(a.handleNew))
	return mux
}

func (a *app) serve() error {
	ln, err := net.Listen("tcp", a.cfg.addr)
	if err != nil {
		return err
	}
	suffix := ""
	if a.cfg.token != "" {
		suffix = "?t=" + a.cfg.token
	}
	fmt.Printf("euchre-web: serving on http://localhost:%d/%s\n", ln.Addr().(*net.TCPAddr).Port, suffix)
	for _, ip := range lanIPs() {
		fmt.Printf("euchre-web: serving on http://%s:%d/%s\n", ip, ln.Addr().(*net.TCPAddr).Port, suffix)
	}
	return http.Serve(ln, a.routes())
}

// auth enforces --token via the ?t= query parameter on every endpoint (SSE
// included, since EventSource cannot set headers).
func (a *app) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.cfg.token != "" && r.URL.Query().Get("t") != a.cfg.token {
			a.jsonErr(w, http.StatusUnauthorized, "invalid or missing token (use ?t=…)")
			return
		}
		next(w, r)
	}
}

func (a *app) jsonErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// submit hands a request to the loop and writes the reply (fresh snapshot on
// success, 409 + {"error"} on validation failure).
func (a *app) submit(w http.ResponseWriter, r *req) {
	select {
	case a.reqCh <- r:
	case <-a.doneCh:
		a.jsonErr(w, http.StatusServiceUnavailable, "server is shutting down")
		return
	}
	select {
	case rep := <-r.reply:
		if rep.err != "" {
			a.jsonErr(w, http.StatusConflict, rep.err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(rep.snap)
	case <-a.doneCh:
		a.jsonErr(w, http.StatusServiceUnavailable, "server is shutting down")
	}
}

// southAwaiting reports whether the latest published snapshot shows South's
// action of the given kind awaited. Request-time gate: decisions are only
// accepted while it is South's turn (the loop re-validates against the
// engine, so a snapshot that went stale between check and apply still fails
// cleanly).
func (a *app) southAwaiting(kind string) bool {
	snap := a.hub.last()
	if snap == nil {
		return false
	}
	var s state
	if err := json.Unmarshal(snap, &s); err != nil {
		return false
	}
	isSouthTurn := s.Turn != nil && *s.Turn == "south"
	switch kind {
	case "bid":
		return s.Bidding.Active && s.Bidding.PromptTo != nil && *s.Bidding.PromptTo == "south"
	case "play":
		return s.Phase == "play" && isSouthTurn
	case "discard":
		return s.Phase == "discard" && isSouthTurn
	}
	return false
}

func notTurnErr(kind string) string {
	return fmt.Sprintf("not south's turn to %s right now", kind)
}

func (a *app) handleBid(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Decision   string `json:"decision"`
		GoingAlone bool   `json:"goingAlone"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		a.jsonErr(w, http.StatusBadRequest, "bad JSON body: "+err.Error())
		return
	}
	call, err := parseBidDecision(body.Decision)
	if err != nil {
		a.jsonErr(w, http.StatusConflict, err.Error())
		return
	}
	alone := body.GoingAlone
	if call == euchre.CallPass {
		alone = false
	}
	if !a.southAwaiting("bid") {
		a.jsonErr(w, http.StatusConflict, notTurnErr("bid"))
		return
	}
	a.submit(w, &req{kind: "bid", call: call, alone: alone, reply: make(chan reqReply, 1)})
}

func (a *app) handlePlay(w http.ResponseWriter, r *http.Request) {
	card, ok := a.readCard(w, r, "play")
	if !ok {
		return
	}
	a.submit(w, &req{kind: "play", card: card, reply: make(chan reqReply, 1)})
}

func (a *app) handleDiscard(w http.ResponseWriter, r *http.Request) {
	card, ok := a.readCard(w, r, "discard")
	if !ok {
		return
	}
	a.submit(w, &req{kind: "discard", card: card, reply: make(chan reqReply, 1)})
}

func (a *app) readCard(w http.ResponseWriter, r *http.Request, kind string) (euchre.Card, bool) {
	var body struct {
		Card string `json:"card"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		a.jsonErr(w, http.StatusBadRequest, "bad JSON body: "+err.Error())
		return euchre.Card{}, false
	}
	card, err := parseCardAny(body.Card)
	if err != nil {
		a.jsonErr(w, http.StatusConflict, err.Error())
		return euchre.Card{}, false
	}
	if !a.southAwaiting(kind) {
		a.jsonErr(w, http.StatusConflict, notTurnErr(kind))
		return euchre.Card{}, false
	}
	return card, true
}

func (a *app) handleNew(w http.ResponseWriter, r *http.Request) {
	a.submit(w, &req{kind: "new", reply: make(chan reqReply, 1)})
}

// handleState serves the latest published snapshot (polling convenience; the
// SSE stream stays the primary channel).
func (a *app) handleState(w http.ResponseWriter, r *http.Request) {
	snap := a.hub.last()
	if snap == nil {
		a.jsonErr(w, http.StatusServiceUnavailable, "game state not ready")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(snap)
}

// ── SSE ───────────────────────────────────────────────────────────────────

func (a *app) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		a.jsonErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	// The loop publishes the first snapshot during startup; wait briefly for
	// it so the connect always carries a full state snapshot.
	snap := a.hub.last()
	for i := 0; i < 100 && snap == nil; i++ {
		time.Sleep(20 * time.Millisecond)
		snap = a.hub.last()
	}
	if snap == nil {
		a.jsonErr(w, http.StatusServiceUnavailable, "game state not ready")
		return
	}

	s := a.hub.add()
	defer a.hub.remove(s)

	fmt.Fprintf(w, "data: %s\n\n", snap)
	fl.Flush()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-s.notify:
			if b := s.drain(); b != nil {
				fmt.Fprintf(w, "data: %s\n\n", b)
				fl.Flush()
			}
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case <-r.Context().Done():
			return
		case <-a.doneCh:
			return
		}
	}
}

// ── input parsing ─────────────────────────────────────────────────────────

// parseBidDecision parses a /api/bid decision: "order_up", "pass", or a suit
// (code letter C/D/H/S, full name clubs/diamonds/hearts/spades, or glyph
// ♣♦♥♠, case-insensitive).
func parseBidDecision(s string) (euchre.Call, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "order_up", "order-up", "orderup", "up", "o", "y":
		return euchre.CallOrderUp, nil
	case "pass", "p":
		return euchre.CallPass, nil
	}
	su, err := parseSuitAny(s)
	if err != nil {
		return "", fmt.Errorf("bad decision %q (order_up, pass, or a suit)", s)
	}
	return euchre.Call(su.Code()), nil
}

// parseSuitAny parses a suit code letter or glyph.
func parseSuitAny(s string) (euchre.Suit, error) {
	trimmed := strings.TrimSpace(s)
	if runes := []rune(trimmed); len(runes) == 1 {
		switch runes[0] {
		case '♣':
			return euchre.Clubs, nil
		case '♦':
			return euchre.Diamonds, nil
		case '♥':
			return euchre.Hearts, nil
		case '♠':
			return euchre.Spades, nil
		}
	}
	return euchre.ParseSuit(trimmed)
}

// parseCardAny parses a card in glyph form ("K♠", "10♥") or wire-code form
// ("KS", "TD").
func parseCardAny(s string) (euchre.Card, error) {
	s = strings.TrimSpace(s)
	if c, err := euchre.ParseCard(s); err == nil {
		return c, nil
	}
	runes := []rune(s)
	if len(runes) == 2 || len(runes) == 3 { // rank (2 for "10") + suit glyph
		su, err := parseSuitAny(string(runes[len(runes)-1]))
		if err != nil {
			return euchre.Card{}, fmt.Errorf("bad card %q", s)
		}
		rank := strings.ToUpper(string(runes[:len(runes)-1]))
		if rank == "10" {
			rank = "T"
		}
		return euchre.ParseCard(rank + su.Code())
	}
	return euchre.Card{}, fmt.Errorf("bad card %q (use a card glyph like K♠ or a code like KS)", s)
}

// lanIPs returns non-loopback IPv4 addresses of this host for the startup
// banner.
func lanIPs() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, addr := range addrs {
		if ipn, ok := addr.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() {
			out = append(out, ipn.IP.String())
		}
	}
	return out
}
