#!/bin/sh
# End-to-end test for euchre-web (web-server mode). Zero LLM calls: the agent
# is cmd/fakeagent (same as script/e2e.sh) and the browser/South role is driven
# with curl. The server runs on a random port with a throwaway proxenos stream.
#
# Scenario A (worked example, seed 7 — same hand as script/e2e.sh scenario A):
#   fakeagent claims north and plays JD TS AH AS TC; south is driven via the
#   HTTP API (script/e2e/south-hand1.txt: bid pass, KD KS TH 9S QS). Two
#   parallel SSE subscribers must both observe the full snapshot sequence
#   (bid1 → play → hand end → game over after the agent's tip + resign).
#   The stream must carry exactly the TUI's event vocabulary, with north's
#   hand disclosed and no action.rejected.
# Scenario B (no agent at all): the built-in bots play west/north/east; north
#   orders up (dealer south) so south's dealer discard is exercised over
#   HTTP; south's whole hand is driven via curl; /api/new must deal a fresh
#   game; POST validation errors must return 409 with a message; --token must
#   gate the API.
set -eu
cd "$(dirname "$0")/.."

PROX=${PROXENOS:-proxenos}
if ! command -v "$PROX" >/dev/null 2>&1; then
	for cand in "$HOME/.local/bin/proxenos" ../proxenos/proxenos ./proxenos; do
		if [ -x "$cand" ]; then PROX="$cand"; break; fi
	done
fi
if ! command -v "$PROX" >/dev/null 2>&1 && [ ! -x "$PROX" ]; then
	echo "e2e-web: no proxenos binary found." >&2
	echo "e2e-web: set PROXENOS=/path/to/proxenos, or clone https://github.com/dshumphr/proxenos" >&2
	echo "e2e-web: and build it (go build -o ~/.local/bin/proxenos .) so it is on PATH." >&2
	exit 1
fi
echo "e2e-web: proxenos binary: $PROX"

TMP=$(mktemp -d)
PORT=$(( (RANDOM % 20000) + 20000 ))
BASE="http://127.0.0.1:$PORT"
TOK="webtok"
Q="t=$TOK"
STREAM_A=euchre-web-e2e-a
STREAM_B=euchre-web-e2e-b

cleanup() {
	[ -n "${APP_PID:-}" ] && kill "$APP_PID" 2>/dev/null || true
	[ -n "${FA_PID:-}" ] && kill "$FA_PID" 2>/dev/null || true
	rm -rf "$TMP"
	"$PROX" rm "$STREAM_A" >/dev/null 2>&1 || true
	"$PROX" rm "$STREAM_B" >/dev/null 2>&1 || true
}
trap cleanup EXIT

go build -o "$TMP/euchre-web" ./cmd/euchre-web
go build -o "$TMP/fakeagent" ./cmd/fakeagent

# ── scenario A: agent north + HTTP south, SSE watchers ─────────────────────
"$PROX" rm "$STREAM_A" >/dev/null 2>&1 || true

"$TMP/euchre-web" --stream "$STREAM_A" --seed 7 --addr "127.0.0.1:$PORT" \
	--token "$TOK" --agent-timeout 5s --proxenos "$PROX" >"$TMP/app-a.log" 2>&1 &
APP_PID=$!
for _ in $(seq 1 100); do
	[ "$(curl -s -o /dev/null -w "%{http_code}" "$BASE/api/state?$Q")" = "200" ] && break
	sleep 0.1
done

# two independent SSE subscribers (multiple tabs must all work)
curl -sN --max-time 60 "$BASE/api/events?$Q" >"$TMP/sse-a1.log" 2>/dev/null &
SSE1=$!
curl -sN --max-time 60 "$BASE/api/events?$Q" >"$TMP/sse-a2.log" 2>/dev/null &
SSE2=$!
sleep 0.5

"$TMP/fakeagent" --stream "$STREAM_A" --script script/e2e/north-hand1.txt --proxenos "$PROX" \
	>"$TMP/agent-a.log" 2>&1 &
FA_PID=$!

# drive South over HTTP following the scripted moves
python3 - "$BASE" "$TOK" script/e2e/south-hand1.txt <<'EOF'
import json, sys, time, urllib.request

base, token, moves_path = sys.argv[1], sys.argv[2], sys.argv[3]
qs = "t=" + token
moves = [l.strip() for l in open(moves_path) if l.strip() and not l.startswith("#")]

def state():
    with urllib.request.urlopen(base + "/api/state?" + qs, timeout=5) as r:
        return json.load(r)

def post(path, body):
    req = urllib.request.Request(base + path + "?" + qs, data=json.dumps(body).encode(),
                                 headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=5) as r:
            return r.status, json.load(r)
    except urllib.error.HTTPError as e:
        return e.code, json.load(e)

mi = 0
game_over = False
for it in range(240):
    s = state()
    if s.get("gameOver"):
        game_over = True
        break
    phase = s["phase"]
    turn = s.get("turn")
    if s["bidding"]["active"] and s["bidding"].get("promptTo") == "south":
        assert mi < len(moves), "moves exhausted at a south bid"
        verb, arg = moves[mi].split(" ", 1)
        assert verb == "bid", f"want bid move, got {moves[mi]}"
        mi += 1
        code, body = post("/api/bid", {"decision": arg})
        assert code == 200, f"bid {arg}: {code} {body}"
    elif phase == "discard" and turn == "south":
        assert mi < len(moves), "moves exhausted at a south discard"
        verb, arg = moves[mi].split(" ", 1)
        assert verb == "play", f"want play move, got {moves[mi]}"
        mi += 1
        code, body = post("/api/discard", {"card": arg})
        assert code == 200, f"discard {arg}: {code} {body}"
    elif phase == "play" and turn == "south":
        assert mi < len(moves), "moves exhausted at a south play"
        verb, arg = moves[mi].split(" ", 1)
        assert verb == "play", f"want play move, got {moves[mi]}"
        mi += 1
        code, body = post("/api/play", {"card": arg})
        assert code == 200, f"play {arg}: {code} {body}"
    time.sleep(0.15)
else:
    raise SystemExit("south driver: game never ended")

assert mi == len(moves), f"unused moves: {moves[mi:]}"
print("e2e-web: scenario A south driver done (game over reached, all moves used)")
EOF

# POST validation errors must be 409 with a message (checked at game over:
# /api/new still works, everything else must refuse)
CODE=$(curl -s -o "$TMP/err1.json" -w "%{http_code}" -X POST "$BASE/api/bid?$Q" -d '{"decision":"pass"}')
[ "$CODE" = "409" ] || { echo "e2e-web: bid at game over: $CODE"; exit 1; }
grep -q '"error"' "$TMP/err1.json" || { echo "e2e-web: 409 body lacks error"; exit 1; }
CODE=$(curl -s -o "$TMP/err2.json" -w "%{http_code}" -X POST "$BASE/api/bid?$Q" -d '{"decision":"banana"}')
[ "$CODE" = "409" ] || { echo "e2e-web: bad decision: $CODE"; exit 1; }

# auth: wrong/missing token must 401
CODE=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/api/state")
[ "$CODE" = "401" ] || { echo "e2e-web: unauthenticated /api/state: $CODE"; exit 1; }

# /api/new deals a fresh game on the same stream
NEW=$(curl -s -X POST "$BASE/api/new?$Q")
echo "$NEW" | python3 -c '
import json, sys
s = json.load(sys.stdin)
assert s["hand"] == 1 and s["phase"] == "bid1", s["phase"]
assert s["tricks"] == {"ns": 0, "ew": 0}, s["tricks"]
assert s["gameOver"] is None
assert all(i["lastBid"] is None for i in s["seatInfo"].values()), s["seatInfo"]
assert len(s["yourHand"]) == 5, s["yourHand"]
print("e2e-web: /api/new deals a fresh game")'

wait "$FA_PID" 2>/dev/null || true
FA_RC=$?
kill "$APP_PID" 2>/dev/null || true
wait "$APP_PID" 2>/dev/null || true
wait "$SSE1" "$SSE2" 2>/dev/null || true

# SSE subscribers must have seen the full snapshot sequence
python3 - "$TMP/sse-a1.log" "$TMP/sse-a2.log" <<'EOF'
import json, sys
for path in sys.argv[1:]:
    raw = open(path).read()
    frames = [json.loads(l[6:]) for l in raw.splitlines() if l.startswith("data: ")]
    assert len(frames) >= 4, f"{path}: only {len(frames)} frames"
    phases = [f["phase"] for f in frames]
    assert "bid1" in phases and "play" in phases, phases
    over = [f for f in frames if f.get("gameOver")]
    assert over and over[-1]["gameOver"]["winner"] == "ew", phases[-1]
    last = frames[-1]
    # every frame is a full self-contained snapshot
    for f in frames:
        for key in ("hand", "phase", "dealer", "turn", "yourHand", "legal",
                    "trick", "seatInfo", "tricks", "score", "tips", "log",
                    "bidding", "gameOver"):
            assert key in f, key
    # the score after the played hand (EW makers, 3 tricks) must appear
    assert any(f["score"] == {"ns": 0, "ew": 1} for f in frames), [f["score"] for f in frames]
print("e2e-web: SSE subscribers saw full snapshots through game over")
EOF

# the proxenos stream must match the TUI's event vocabulary
"$PROX" tail "$STREAM_A" >"$TMP/stream-a.ndjson"
python3 - "$TMP/stream-a.ndjson" <<'EOF'
import json, sys

events = [json.loads(l) for l in open(sys.argv[1]) if l.strip()]
types = [e["type"] for e in events]

allowed = {"stream.contract", "stream.meta", "game.started", "deal.your_hand", "bid.prompt",
           "bid.made", "trick.started", "card.played", "trick.won",
           "hand.summary", "game.over", "action.rejected",
           "euchre.claim_seat", "euchre.bid", "euchre.play", "euchre.tip",
           "euchre.resign", "euchre.hand_request"}
bad = [t for t in types if t not in allowed]
assert not bad, f"unexpected event types: {bad}"

def body(t):
    return [e["body"] for e in events if e["type"] == t]

assert not body("action.rejected"), body("action.rejected")

for t in ("game.started", "bid.prompt", "bid.made", "trick.started",
          "card.played", "trick.won", "hand.summary", "game.over",
          "euchre.claim_seat", "euchre.tip", "euchre.resign"):
    assert body(t), f"missing {t} on the stream"

# north's hand was disclosed to the agent (deal.your_hand)
north_hands = [b for b in body("deal.your_hand") if b["seat"] == "north"]
assert north_hands and len(north_hands[0]["cards"]) == 5, north_hands

# bids and plays recorded on the stream match the scripted game
north_plays = [b["card"] for b in body("card.played") if b["seat"] == "north"]
assert north_plays == ["JD", "TS", "AH", "AS", "TC"], north_plays
sums = body("hand.summary")
assert len(sums) == 1 and sums[0]["tricksNSE"] == [2, 3], sums
assert sums[0]["scoreNSE"] == [0, 1], sums

# the agent resigned after tipping; NS (its north seat) concedes
assert body("game.over") == [{"winner": "east-west"}], body("game.over")
print(f"e2e-web: scenario A stream vocabulary ok ({len(events)} events)")
EOF
echo "e2e-web: scenario A done"

# ── scenario B: no agent — bots play, south via HTTP, discard path ─────────
PORT=$(( PORT + 1 ))
BASE="http://127.0.0.1:$PORT"
"$PROX" rm "$STREAM_B" >/dev/null 2>&1 || true

"$TMP/euchre-web" --stream "$STREAM_B" --seed 7 --addr "127.0.0.1:$PORT" \
	--token "$TOK" --agent-timeout 300ms --proxenos "$PROX" >"$TMP/app-b.log" 2>&1 &
APP_PID=$!
for _ in $(seq 1 100); do
	[ "$(curl -s -o /dev/null -w "%{http_code}" "$BASE/api/state?$Q")" = "200" ] && break
	sleep 0.1
done

# north (a bot) orders up with dealer south → south must discard over HTTP
python3 - "$BASE" "$TOK" <<'EOF'
import json, sys, time, urllib.request

base, token = sys.argv[1], sys.argv[2]
qs = "t=" + token

def state():
    with urllib.request.urlopen(base + "/api/state?" + qs, timeout=5) as r:
        return json.load(r)

def post(path, body):
    req = urllib.request.Request(base + path + "?" + qs, data=json.dumps(body).encode(),
                                 headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=5) as r:
            return r.status, json.load(r)
    except urllib.error.HTTPError as e:
        return e.code, json.load(e)

# wait for the dealer-discard decision point (north orders up diamonds)
for _ in range(200):
    s = state()
    if s["phase"] == "discard" and s.get("turn") == "south":
        break
    time.sleep(0.1)
else:
    raise SystemExit(f"never reached south's discard (phase={s['phase']})")
assert len(s["yourHand"]) == 6 and len(s["legal"]) == 6, (s["yourHand"], s["legal"])

# an illegal discard must 409 with a message
code, body = post("/api/discard", {"card": "AH"})
assert code == 409 and "error" in body, (code, body)

# discarding a real card opens trick play
code, body = post("/api/discard", {"card": s["legal"][0]})
assert code == 200, (code, body)
s = body
assert s["phase"] == "play" and len(s["yourHand"]) == 5, (s["phase"], s["yourHand"])
print("e2e-web: scenario B discard over HTTP ok")

# drive the whole hand: every south decision point gets the first legal card
# (the snapshot can jump straight from hand_over to hand 2's first bid, so the
# final trick count must be remembered from the last poll that still had it)
seen_tricks = 0
for _ in range(400):
    s = state()
    seen_tricks = max(seen_tricks, s["tricks"]["ns"] + s["tricks"]["ew"])
    if s["hand"] != 1 or s["phase"] in ("hand_over", "game_over") or \
       (s["tricks"]["ns"] + s["tricks"]["ew"]) == 5:
        break
    if s["bidding"]["active"] and s["bidding"].get("promptTo") == "south":
        code, body = post("/api/bid", {"decision": "pass"})
        assert code == 200, (code, body)
        seen_tricks = max(seen_tricks, body["tricks"]["ns"] + body["tricks"]["ew"])
    elif s["phase"] == "play" and s.get("turn") == "south":
        assert s["legal"], s
        code, body = post("/api/play", {"card": s["legal"][0]})
        assert code == 200, (code, body)
        seen_tricks = max(seen_tricks, body["tricks"]["ns"] + body["tricks"]["ew"])
    time.sleep(0.1)
else:
    raise SystemExit("hand never completed")

# The web app deals the next hand the instant a hand scores: the hand-end
# snapshot (full tricks) is published only briefly before hand 2's first
# snapshot (tricks reset), so a poll may or may not catch the 5-trick state.
# The full 5-trick proof lives in the stream's hand.summary, checked below.
assert seen_tricks >= 4, (seen_tricks, s["tricks"])
print(f"e2e-web: scenario B hand 1 fully driven via HTTP ({seen_tricks} tricks observed, hand scored)")
EOF

kill "$APP_PID" 2>/dev/null || true
wait "$APP_PID" 2>/dev/null || true

"$PROX" tail "$STREAM_B" >"$TMP/stream-b.ndjson"
python3 - "$TMP/stream-b.ndjson" <<'EOF'
import json, sys

events = [json.loads(l) for l in open(sys.argv[1]) if l.strip()]
agent_ev = [e for e in events if e["src"].startswith("agent:")]
assert not agent_ev, agent_ev
types = {e["type"] for e in events}
assert "bid.made" in types and "card.played" in types and "trick.won" in types, types
rej = [e for e in events if e["type"] == "action.rejected"]
assert not rej, rej
# hand 1 was fully played: its hand.summary must record all five tricks
sums = [e["body"] for e in events if e["type"] == "hand.summary"]
assert sums and sum(sums[0]["tricksNSE"]) == 5, [s["tricksNSE"] for s in sums]
print(f"e2e-web: scenario B stream ok ({len(events)} events, no agent, no rejections)")
EOF

echo "e2e-web: ALL GREEN"
