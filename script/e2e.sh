#!/bin/sh
# End-to-end test for euchre-demo. Zero LLM calls: the "agent" is
# cmd/fakeagent, a deterministic script-reader that talks to the proxenos
# stream exactly like a real harness agent would (claim north seat, bid, play,
# coach via euchre.tip, resign).
#
# Scenario A (worked example): one fully scripted hand, seed 7, dealer south.
#   Round 1 sweeps; west calls clubs in round 2 (JS is the left bower).
#   Tricks: S W N E E  →  NS 2 / EW 3, makers (EW) score 1, then the agent
#   tips and resigns. The expected values below were hand-verified against the
#   rules engine (bower ranking, follow-suit, scoring) — see README.
# Scenario B (timeout fallback): the agent claims North then goes silent; the
#   app must finish the whole game with the built-in bot playing North.
# Scenario C (all three seats): the agent claims north+east+west in one
#   euchre.claim_seat, gets each seat's hand disclosed (deal.your_hand), plus
#   an east hand via euchre.hand_request, and plays every claimed seat's bid
#   and card for a full hand (seed 11, dealer south — card order derived from
#   the rules engine: west/north/east pass, South's bot orders up clubs and
#   discards KD, tricks W-N-S-N-S for NS 4 / EW 1), then tips and resigns
#   (NS concedes: the agent holds North).
set -eu
cd "$(dirname "$0")/.."

PROX=${PROXENOS:-proxenos}
if ! command -v "$PROX" >/dev/null 2>&1; then
	for cand in "$HOME/.local/bin/proxenos" ../proxenos/proxenos ./proxenos; do
		if [ -x "$cand" ]; then PROX="$cand"; break; fi
	done
fi
if ! command -v "$PROX" >/dev/null 2>&1 && [ ! -x "$PROX" ]; then
	echo "e2e: no proxenos binary found." >&2
	echo "e2e: set PROXENOS=/path/to/proxenos, or clone https://github.com/dshumphr/proxenos" >&2
	echo "e2e: and build it (go build -o ~/.local/bin/proxenos .) so it is on PATH." >&2
	exit 1
fi
echo "e2e: proxenos binary: $PROX"

TMP=$(mktemp -d)
cleanup() {
	rm -rf "$TMP"
	"$PROX" rm euchre-e2e-a >/dev/null 2>&1 || true
	"$PROX" rm euchre-e2e-b >/dev/null 2>&1 || true
}
trap cleanup EXIT

go build -o "$TMP/euchre" ./cmd/euchre
go build -o "$TMP/fakeagent" ./cmd/fakeagent

STREAM_A=euchre-e2e-a
"$PROX" rm "$STREAM_A" >/dev/null 2>&1 || true

# ── scenario A: scripted hand + coaching tip + resign ──────────────────────
"$TMP/fakeagent" --stream "$STREAM_A" --script script/e2e/north-hand1.txt --proxenos "$PROX" >"$TMP/agent-a.log" 2>&1 &
FA=$!
sleep 0.4 # let the fake agent attach before the game starts
"$TMP/euchre" --stream "$STREAM_A" --seed 7 --moves script/e2e/south-hand1.txt --agent-timeout 2s --proxenos "$PROX" >"$TMP/app-a.log" 2>&1 &
APP=$!
wait "$APP"
APP_RC=$?
# the fake agent exits on game.over
wait "$FA"
FA_RC=$?

"$PROX" tail "$STREAM_A" >"$TMP/stream-a.ndjson"

python3 - "$TMP/stream-a.ndjson" <<'EOF'
import json, sys

events = [json.loads(l) for l in open(sys.argv[1]) if l.strip()]
by_type = {}
for e in events:
    by_type.setdefault(e["type"], []).append(e)

def body(t):
    return [e["body"] for e in by_type.get(t, [])]

rej = body("action.rejected")
assert not rej, f"unexpected action.rejected: {rej}"

starts = body("game.started")
assert len(starts) == 1
assert starts[0]["dealer"] == "south" and starts[0]["youAre"] == "south"
# South's hand must never be on the stream: it is visible in the TUI, and
# publishing it let an agent play South's cards for a claimed seat.
assert "yourHand" not in starts[0], starts[0]

# exactly one hand, fully summarised
sums = body("hand.summary")
assert len(sums) == 1, f"want 1 hand.summary, got {len(sums)}"
h = sums[0]
assert h["dealer"] == "south"
assert h["tricksNSE"] == [2, 3], h["tricksNSE"]
assert h["scoreNSE"] == [0, 1], h["scoreNSE"]
assert h["goingAlone"] is None
assert h["bids"] == [
    {"seat": "west", "decision": "pass", "alone": False},
    {"seat": "north", "decision": "pass", "alone": False},
    {"seat": "east", "decision": "pass", "alone": False},
    {"seat": "south", "decision": "pass", "alone": False},
    {"seat": "west", "decision": "C", "alone": False},
], h["bids"]
assert h["tricks"] == [
    {"lead": "west", "plays": [["west", "QD"], ["north", "JD"], ["east", "TD"], ["south", "KD"]], "winner": "south"},
    {"lead": "south", "plays": [["south", "KS"], ["west", "JS"], ["north", "TS"], ["east", "JH"]], "winner": "west"},
    {"lead": "west", "plays": [["west", "QH"], ["north", "AH"], ["east", "KH"], ["south", "TH"]], "winner": "north"},
    {"lead": "north", "plays": [["north", "AS"], ["east", "JC"], ["south", "9S"], ["west", "QC"]], "winner": "east"},
    {"lead": "east", "plays": [["east", "AC"], ["south", "QS"], ["west", "KC"], ["north", "TC"]], "winner": "east"},
], h["tricks"]

# every hand.summary trick split sums to five and score deltas match tricks
for h in body("hand.summary"):
    assert h["tricksNSE"][0] + h["tricksNSE"][1] == 5

# north played exactly the fake agent's scripted cards, in order
north_played = [e["body"]["card"] for e in by_type["card.played"] if e["body"]["seat"] == "north"]
assert north_played == ["JD", "TS", "AH", "AS", "TC"], north_played

agent_plays = [e["body"]["card"] for e in by_type["euchre.play"]]
assert agent_plays == ["JD", "TS", "AH", "AS", "TC"], agent_plays

# the agent claimed the seat and coached after the summary
claims = body("euchre.claim_seat")
assert claims == [{"seat": "north"}]
tips = body("euchre.tip")
assert len(tips) == 1 and "KD was boss" in tips[0]["text"], tips

# every trick got a trick.won with running bookkeeping
assert len(body("trick.won")) == 5

overs = body("game.over")
assert overs == [{"winner": "east-west"}], overs  # the agent resigned: NS concedes
print("e2e: scenario A (worked example) assertions passed")
EOF
echo "e2e: scenario A exit codes: app=$APP_RC fakeagent=$FA_RC"
[ "$APP_RC" -eq 0 ] || { echo "e2e: app exit $APP_RC"; exit 1; }
[ "$FA_RC" -eq 0 ] || { echo "e2e: fakeagent exit $FA_RC"; exit 1; }

# ── scenario B: agent claims then goes silent — the game must never stall ──
STREAM_B=euchre-e2e-b
"$PROX" rm "$STREAM_B" >/dev/null 2>&1 || true

"$TMP/fakeagent" --stream "$STREAM_B" --script script/e2e/north-silent.txt --proxenos "$PROX" >"$TMP/agent-b.log" 2>&1 &
FA=$!
sleep 0.4
"$TMP/euchre" --stream "$STREAM_B" --seed 7 --moves script/e2e/south-auto.txt --agent-timeout 300ms --proxenos "$PROX" >"$TMP/app-b.log" 2>&1 &
APP=$!
wait "$APP"
APP_RC=$?
wait "$FA"
FA_RC=$?

"$PROX" tail "$STREAM_B" >"$TMP/stream-b.ndjson"

python3 - "$TMP/stream-b.ndjson" <<'EOF'
import json, sys

events = [json.loads(l) for l in open(sys.argv[1]) if l.strip()]
by_type = {}
for e in events:
    by_type.setdefault(e["type"], []).append(e)

def body(t):
    return [e["body"] for e in by_type.get(t, [])]

assert not body("action.rejected"), body("action.rejected")

# the agent did nothing but claim
agent_events = [e for e in events if e["src"] == "agent:fakeagent"]
assert all(e["type"] == "euchre.claim_seat" for e in agent_events), agent_events

# the game ran to ten points with the built-in bot filling in for North
overs = body("game.over")
assert len(overs) == 1, overs
sums = body("hand.summary")
assert len(sums) >= 2, f"want several hands, got {len(sums)}"
last = [0, 0]
for h in sums:
    assert h["tricksNSE"][0] + h["tricksNSE"][1] == 5, h
    cur = h["scoreNSE"]
    assert cur[0] >= last[0] and cur[1] >= last[1], (last, cur)
    last = cur
assert last[0] >= 10 or last[1] >= 10, last
# North's cards were played by the fallback bot after the claim
north_cards = [e for e in by_type["card.played"] if e["body"]["seat"] == "north"]
assert len(north_cards) >= 5, len(north_cards)
print(f"e2e: scenario B (timeout fallback) assertions passed — {len(sums)} hands, final score {last}")
EOF
echo "e2e: scenario B exit codes: app=$APP_RC fakeagent=$FA_RC"
[ "$APP_RC" -eq 0 ] || { echo "e2e: app exit $APP_RC"; exit 1; }
[ "$FA_RC" -eq 0 ] || { echo "e2e: fakeagent exit $FA_RC"; exit 1; }

# ── scenario C: the agent claims and plays all three non-human seats ───────
STREAM_C=euchre-e2e-c
"$PROX" rm "$STREAM_C" >/dev/null 2>&1 || true

"$TMP/fakeagent" --stream "$STREAM_C" --script script/e2e/all-seats.txt --proxenos "$PROX" >"$TMP/agent-c.log" 2>&1 &
FA=$!
sleep 0.4
"$TMP/euchre" --stream "$STREAM_C" --seed 11 --moves script/e2e/south-auto.txt --agent-timeout 2s --proxenos "$PROX" >"$TMP/app-c.log" 2>&1 &
APP=$!
wait "$APP"
APP_RC=$?
wait "$FA"
FA_RC=$?

"$PROX" tail "$STREAM_C" >"$TMP/stream-c.ndjson"

python3 - "$TMP/stream-c.ndjson" <<'EOF'
import json, sys

events = [json.loads(l) for l in open(sys.argv[1]) if l.strip()]
by_type = {}
for e in events:
    by_type.setdefault(e["type"], []).append(e)

def body(t):
    return [e["body"] for e in by_type.get(t, [])]

rej = body("action.rejected")
assert not rej, f"unexpected action.rejected: {rej}"

CLAIMED = {"north", "east", "west"}

# one additive multi-seat claim, plus one hand_request refresh for east
claims = body("euchre.claim_seat")
assert claims == [{"seats": ["north", "east", "west"]}], claims
assert body("euchre.hand_request") == [{"seat": "east"}], body("euchre.hand_request")

# every claimed seat got its hand disclosed; east twice for hand 1 (claim
# disclosure + hand_request refresh). A third disclosure may follow: after
# hand.summary the app deals hand 2 and re-discloses claimed seats before the
# scripted resign (sent on the next event) can land — that hand-2 deal is
# asserted only via game bookkeeping below.
hands = body("deal.your_hand")
per_seat = {}
for h in hands:
    assert h["seat"] in {"south"} | CLAIMED, h
    assert len(h["cards"]) == 5 and len(set(h["cards"])) == 5, h
    per_seat.setdefault(h["seat"], []).append(h["cards"])
assert set(per_seat) == {"south"} | CLAIMED, per_seat.keys()
assert len(per_seat["east"]) >= 2 and per_seat["east"][0] == per_seat["east"][1], per_seat["east"]

# the four hands plus the top card partition the 24-card deck (per-seat sets,
# sort-independent)
expected = {
    "south": {"TC", "TS", "KD", "AC", "JS"},
    "west": {"JH", "AS", "9C", "JD", "TH"},
    "north": {"AD", "9H", "QD", "KS", "JC"},
    "east": {"TD", "QS", "9S", "9D", "QH"},
}
for seat, cards in expected.items():
    got = set(per_seat[seat][0])
    assert got == cards, (seat, got, cards)
all_cards = [c for cards in per_seat.values() for c in cards[0]]
assert len(set(all_cards)) == 20, sorted(set(all_cards))

# bidding: the claimed seats' passes arrive in seat order, South's bot orders up
bids_made = [(b["seat"], b["decision"], b.get("alone", False)) for b in body("bid.made")]
assert bids_made == [
    ("west", "pass", False),
    ("north", "pass", False),
    ("east", "pass", False),
    ("south", "order_up", False),
], bids_made
agent_bids = [e["body"]["decision"] for e in by_type["euchre.bid"]]
assert agent_bids == ["pass", "pass", "pass"], agent_bids

# every claimed seat's played cards match the script, in order
expected_plays = {
    "west": ["AS", "JH", "9C", "JD", "TH"],
    "north": ["KS", "9H", "JC", "AD", "QD"],
    "east": ["9S", "QH", "9D", "TD", "QS"],
    "south": ["TS", "TC", "AC", "JS", "KC"],  # the built-in bot's seat
}
for seat, cards in expected_plays.items():
    played = [e["body"]["card"] for e in by_type["card.played"] if e["body"]["seat"] == seat]
    assert played == cards, (seat, played, cards)

# the agent sent exactly the claimed seats' 15 cards, in acting order
agent_plays = [e["body"]["card"] for e in by_type["euchre.play"]]
assert agent_plays == [
    "AS", "KS", "9S",   # trick 0: west, north, east
    "JH", "9H", "QH",   # trick 1
    "9C", "JC", "9D",   # trick 2
    "AD", "TD", "JD",   # trick 3: north leads
    "TH", "QD", "QS",   # trick 4
], agent_plays

# one completed hand, fully summarised with correct bookkeeping
sums = body("hand.summary")
assert len(sums) == 1, f"want 1 hand.summary, got {len(sums)}"
h = sums[0]
assert h["dealer"] == "south" and h["goingAlone"] is None
assert h["tricksNSE"] == [4, 1], h["tricksNSE"]
assert h["scoreNSE"] == [1, 0], h["scoreNSE"]
assert h["bids"] == [
    {"seat": "west", "decision": "pass", "alone": False},
    {"seat": "north", "decision": "pass", "alone": False},
    {"seat": "east", "decision": "pass", "alone": False},
    {"seat": "south", "decision": "order_up", "alone": False},
], h["bids"]
assert h["tricks"] == [
    {"lead": "west", "plays": [["west", "AS"], ["north", "KS"], ["east", "9S"], ["south", "TS"]], "winner": "west"},
    {"lead": "west", "plays": [["west", "JH"], ["north", "9H"], ["east", "QH"], ["south", "TC"]], "winner": "south"},
    {"lead": "south", "plays": [["south", "AC"], ["west", "9C"], ["north", "JC"], ["east", "9D"]], "winner": "north"},
    {"lead": "north", "plays": [["north", "AD"], ["east", "TD"], ["south", "JS"], ["west", "JD"]], "winner": "south"},
    {"lead": "south", "plays": [["south", "KC"], ["west", "TH"], ["north", "QD"], ["east", "QS"]], "winner": "south"},
], h["tricks"]

# trick bookkeeping: five trick.won with running tricksNSE
tw = [(e["body"]["seat"], e["body"]["pointsNSE"]) for e in by_type["trick.won"]]
assert tw == [
    ("west", [0, 1]), ("south", [1, 1]), ("north", [2, 1]), ("south", [3, 1]), ("south", [4, 1])
], tw

tips = body("euchre.tip")
assert len(tips) == 1 and "right bower" in tips[0]["text"], tips

# the agent resigned; it holds North, so North-South concedes
overs = body("game.over")
assert overs == [{"winner": "east-west"}], overs
print("e2e: scenario C (all three seats claimed and played) assertions passed")
EOF
echo "e2e: scenario C exit codes: app=$APP_RC fakeagent=$FA_RC"
[ "$APP_RC" -eq 0 ] || { echo "e2e: app exit $APP_RC"; exit 1; }
[ "$FA_RC" -eq 0 ] || { echo "e2e: fakeagent exit $FA_RC"; exit 1; }

echo "e2e: ALL GREEN (zero LLM calls)"
