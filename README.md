# euchre-demo

Terminal euchre where your partner — your opponents, or all three at once —
and your post-hand coach can be an AI agent (omp harness), connected through
the [proxenos](https://github.com/dshumphr/proxenos) middleware.

You sit **South**. Built-in heuristic bots play the other seats — until an
agent claims them via `euchre.claim_seat` on the `euchre` stream. Any of
**North** (your partner), **East** and **West** (your opponents) can be
claimed, in any combination, at any time: one seat (the classic "agent
partner" setup) or all three at once (your agent plays against you across the
table). From then on the agent bids and plays every claimed seat's cards each
turn, sees each claimed seat's hand (`deal.your_hand`), and after every hand
it coaches you with a `euchre.tip` rendered in the tips panel.

Standard rules: 24 cards (9–A), two bidding rounds, right/left bowers, going
alone, first team to 10 points. Scoring follows the classic rule set: makers
3–4 tricks = 1 point, all five (march) = 2, lone march = 1, euchre = 2.

## 60-second demo

```sh
# one-time: build the middleware and the game
git clone https://github.com/dshumphr/proxenos
(cd proxenos && go build -o ~/.local/bin/proxenos .)   # somewhere on PATH
git clone https://github.com/dshumphr/euchre-demo
(cd euchre-demo && go build -o euchre ./cmd/euchre)

# terminal 1 — watch the stream the game and the agent share
proxenos tail euchre -f

# terminal 2 — play (proxenos integration is optional: without it, the game
# still runs and the bot plays all three other seats)
./euchre
```

Bidding: `o` orders up the top card (round 1), `p` passes, `C/D/H/S` names
trump in round 2, add ` alone` to go alone. Cards: type the number shown next
to a legal card or its code (`JH`, `10D`→`TD`).

## Web mode (browser as South)

```sh
go build -o euchre-web ./cmd/euchre-web
./euchre-web --stream euchre-web --addr 0.0.0.0:8090 [--token s3cret]
```

Same game semantics as the TUI (bots fill the other seats until an agent
claims them over the proxenos stream; same contract, same per-seat
`--agent-timeout` fallback, same replay-safe startup), but South plays in the
browser: the embedded frontend (repo-root `web/`) renders the table, and the
HTTP API drives South:

- `GET /api/events` — SSE; a full self-contained state snapshot on connect and
  after every state change (heartbeat every 15s; multiple tabs fine)
- `GET /api/state` — the same snapshot, for polling
- `POST /api/bid` `{"decision":"order_up"|"pass"|"<suit>","goingAlone":bool}`
- `POST /api/play` / `POST /api/discard` `{"card":"K♠"}` (glyphs or codes)
- `POST /api/new` — fresh game on the same stream

POSTs return the fresh snapshot; illegal moves get `409 {"error":"…"}`. With
`--token`, every API endpoint (SSE included) requires `?t=<token>`.

### Running the web server

```sh
./euchre-web --stream euchre-web --addr 0.0.0.0:8090
# background variant:
nohup ./euchre-web --stream euchre-web --addr 0.0.0.0:8090 > /tmp/euchre-web-serve.log 2>&1 &
```

- Locally: http://127.0.0.1:8090 — on your LAN, `http://<your-host>:8090`
  (the server prints the URL, including the machine's LAN IP, at startup)
- Health check: `curl -X POST http://127.0.0.1:8090/api/new` (note: POST, not
  GET) — expect `200` with a game-state JSON snapshot
- Stop: `pkill -f 'euchre-web --stream'`
- Clean up the proxenos stream so a relaunch starts fresh:
  `proxenos rm euchre-web` (ignore the error if absent)
- The omp extension attaches with `euchre_open` `web:true`

## The table

The board renders as a real table: NORTH at the top, WEST left, YOU (SOUTH)
at the bottom, EAST right, with the current trick's cards sitting next to the
seat that played them. The layout is width-adaptive: the terminal size is
read at draw time, the trick box shrinks with available width, west/east
cards always sit against the box edges, and every line is clipped to the
terminal width so nothing wraps — tested from 55 to 130 columns (below 75
the tips panel and log tail compact and bid indicators drop to single
symbols). Color carries meaning — hearts/diamonds render red,
clubs/spades white/bold, trump is marked ★, the seat whose turn it is gets a
`◀ turn` pointer, claimed seats show `[agent]`, bot seats `[bot]`, bots'
labels are dimmed:

```
EUCHRE — hand 1  dealer south  trump ★♠ (by west)  tricks NS 2 / EW 1  score NS 3 / EW 2 (to 10)

                NORTH  10♣  · 4 cards [agent]  pass
      WEST  K♣  ┌────────────────┐  EAST  A♣
                │       10♣       │
                │                 │
                │        ·        │
                └────────────────┘
                SOUTH (you)  · 5 cards  ◀ turn

  Your play — number or code:  1:K♠   2:Q♠   3:9♠   4:K♦   5:10♥
```

The status bar always shows hand number, dealer, trump (+ who called it and
who is going alone), tricks won this hand per team (NS vs EW), and the match
score for both teams. Only public information appears around the table
(played cards and cards-left counts) — face-down hands are never revealed;
your own hand is at the bottom with legal plays numbered and illegal ones
dimmed. Bidding prompts render inline in the same row, and the tips panel +
event log stay at the foot of the screen.

## Requirements & limitations

This demo has two external dependencies, and being honest about them is part
of the design:

- **Middleware: [proxenos](https://github.com/dshumphr/proxenos)** — the
  append-only stream the game and the agent share. It is a hard prerequisite
  for the agent flow (without it the game still runs; bots fill every seat).
- **Harness: omp + Tern (closed beta)** — the only harness wired up today, via
  the extension in `omp-extension/`. No other harnesses are wired up yet.

The app itself is harness-agnostic: the **stream contract** (the NDJSON event
vocabulary in `cmd/euchre/proxenos-contract.json`) is the only interface. Any
harness that can read a file and run a shell command can drive a seat — see
the raw-CLI fallback below.

## Agent setup — one prompt

Prerequisites: **omp**, **Tern**, and [proxenos](https://github.com/dshumphr/proxenos)
built and on `PATH`. Paste this into a fresh omp session and the agent does
the whole setup:

> Set up the euchre-demo project for me, end to end:
> 1. Clone https://github.com/dshumphr/proxenos and build it to
>    `~/.local/bin/proxenos` (`go build -o ~/.local/bin/proxenos .`), then
>    verify `proxenos --help` works (add `~/.local/bin` to PATH if not).
> 2. Clone https://github.com/dshumphr/euchre-demo, and from the repo root
>    build both binaries: `go build -o euchre ./cmd/euchre` and
>    `go build -o euchre-web ./cmd/euchre-web`.
> 3. Typecheck the omp extension — install `typescript` and
>    `@oh-my-pi/pi-coding-agent` as dev dependencies in the repo (that package
>    supplies the `ExtensionAPI` types), run `tsc --noEmit` on
>    `omp-extension/proxenos-euchre.ts`, then copy that file to
>    `~/.omp/agent/extensions/`.
> 4. Verify: `go build ./...` and `go test ./...` pass, and `./script/e2e.sh`
>    plus `./script/e2e-web.sh` run green (they use the proxenos binary from
>    step 1; override with `PROXENOS=/path/to/proxenos` if it is not on PATH).
> When everything is green, tell me to start a NEW omp session — extensions
> load at session start, and `/reload-plugins` does not pick up brand-new
> files. In the new session, "let's play euchre" starts a game.

## Play with your agent (omp — the extension)

Tell your agent in the omp chat: **"let's play euchre"** — or **"you play my
partner AND both opponents"**. If the `proxenos-euchre` extension is installed
(`omp-extension/proxenos-euchre.ts`, copied to `~/.omp/agent/extensions/`),
the agent calls `euchre_open`: the game opens in a Tern block beside the
conversation on stream `euchre`, the agent claims the seats it was asked to
play (one `euchre.claim_seat`, either `{"seat":"north"}` or
`{"seats":["north","east","west"]}` — additive, idempotent, any time; add
`"release":true` to hand seats back to their bots), and the extension watches
the stream and wakes the agent only when a **claimed** seat must act — a
`bid.prompt` naming it, a discard when it is dealer and someone ordered up, a
trick it leads, a play whose next actor (S→W→N→E, skipping a lone bidder's
sitting-out partner) is claimed — and **always** on `hand.summary` (the
coaching hook — the agent sends `euchre.tip`). Every wake names the seat that
must act; everything else arrives as silent context on its next turn. Each
claimed seat's hand is disclosed to the agent as `deal.your_hand {seat,
cards}` at the next decision point (`euchre.hand_request {"seat":…}` re-emits
it on demand).

No paste-prompts, no manual `proxenos tail` polling. See
[`omp-extension/README.md`](omp-extension/README.md).

### Raw-CLI fallback (no extension)

Paste this into the harness chat:

> Attach to the proxenos stream `euchre` and join my euchre game. Read the
> stream.contract on that stream first. Claim the seats I asked you to play by
> sending `euchre.claim_seat` — either `{"seat":"north"}` (my partner) or
> `{"seats":["north","east","west"]}` (all three) — then watch the stream:
> each claimed seat's hand arrives as `deal.your_hand`. Whenever a claimed
> seat's turn to bid comes up send `euchre.bid` (order_up/pass in round 1, a
> suit letter in round 2), and whenever a claimed seat must play a card send
> `euchre.play` with a card from that seat's hand. After every `hand.summary`
> event, send `euchre.tip` with one or two sentences coaching me on the hand.
> I'll ask you questions here in chat.

The contract's `instructions` field teaches the agent the same protocol; the
harness needs no app-specific code — one append-only NDJSON log is the API.

If the agent goes quiet for over 30 seconds mid-hand (flag
`--agent-timeout`, default `30s`), the built-in bot plays any claimed seat it
is waiting on, so the game never stalls; when the agent wakes up it is back
in the seat for the next turn. This fallback is also exercised end-to-end by
`script/e2e.sh`.

## Contract excerpt

The full contract lives at `cmd/euchre/proxenos-contract.json`. It is written
to the stream on first run (creation is idempotent; probe with
`proxenos tail euchre --tail 1`, create with `proxenos new euchre --contract
…`); on an existing stream the app appends a newer `stream.contract` at every
start (last one wins), so streams from older app versions pick up the current
vocabulary.

```json
"emits":   { "game.started":  {"dealer":"south","youAre":"south"},
             "bid.prompt": {"topCard":"TH","dealer":"south","round":1,"turn":"north"},
             "deal.your_hand": {"seat":"north","cards":["JH","JD","AH","KS","9D"]},
             "card.played": {"seat":"north","card":"JD","trickNo":0},
             "hand.summary": {"scoreNSE":[3,1],"tricksNSE":[3,2],"dealer":"south",
                              "goingAlone":null,"bids":[…],"tricks":[…] } },
"accepts": { "euchre.bid":      {"decision":"order_up","goingAlone":false},
             "euchre.play":     {"card":"JD"},
             "euchre.claim_seat": {"seats":["north","east","west"]},
             "euchre.hand_request": {"seat":"east"},
             "euchre.tip":      {"text":"Your AH was boss — leading it…"},
             "euchre.resign":   {} }
```

`euchre.claim_seat` also takes `{"seat":"north"}` and a `"release":true`
variant that hands seats back to their bots (see the contract's
`instructions`). `game.started` carries no hand: South's cards are visible
in the TUI and never published on the stream — only claimed seats' hands are
disclosed, as `deal.your_hand`. `deal.your_hand` is emitted for South (the
user) and for every claimed seat at the next decision point after a deal.
`hand.summary` is the coaching hook: per-hand bidding decisions and every
trick's plays, emitted after each hand. Illegal agent moves come back as
`action.rejected` with the truthful rules reason (`card KC not in north's
hand` is reported before any follow-suit complaint …); a rejected play or
bid for a claimed seat also carries that seat's CURRENT hand as `cards`, so
the agent self-corrects from the rejection without a `euchre.hand_request`
round-trip.

## Restart replay

The stream file outlives the app (it is the whole history of every match
played on that stream), and the poller re-reads it from offset 0 on every
start. On startup the app snapshots the stream size and the poller flags
events wholly below that offset as `replay`: they are applied for STATE only
— seat claims still stand, because the agent expects to hold them — but old
bids, plays, hand requests and resigns from previous runs are never
validated or rejected again. (Re-judging them used to produce a storm of
spurious `action.rejected` events on every restart; mid-session behavior is
unchanged.)

## Repo layout

```
cmd/euchre         the terminal app (flags: --stream --seed --moves
                   --agent-timeout --proxenos)
cmd/euchre-web     the web-server mode (flags: --stream --addr --token --seed
                   --agent-timeout --proxenos); embeds web/ via webassets.go
cmd/fakeagent      deterministic script-driven stand-in for the harness agent
internal/euchre    rules engine + heuristic AI (fully unit-tested)
internal/prox      proxenos glue: idempotent stream setup, CLI sends,
                   file poller with the documented reader rules
script/e2e.sh      fake-agent end-to-end test, zero LLM calls
script/e2e-web.sh  web-mode end-to-end test (fake agent + curl South, SSE
                   snapshot assertions), zero LLM calls
script/dealgen     prints the deal for a seed (hand-crafting test scripts)
```

## Tests

```sh
go test ./...           # rules engine, web serializer, HTTP handlers
./script/e2e.sh         # three scenarios, deterministic, no LLM
./script/e2e-web.sh     # web mode: agent north + HTTP south, SSE, /api/new
```

The e2e worked example (seed 7, dealer South): round 1 sweeps, West calls
clubs in round 2 (the jack of spades is the left bower of clubs), tricks go
S-W-N-E-E for NS 2 / EW 3, the makers score 1, the agent tips and resigns.
The all-seats scenario (seed 11, dealer South) has the fake agent claim
north+east+west in one `euchre.claim_seat`, collect each seat's
`deal.your_hand` (plus an east refresh via `euchre.hand_request`), play all
three seats' bids and cards to a completed hand (NS 4 / EW 1), then tip and
resign. All scenarios assert stream bookkeeping, not just exit codes.

The app couples only to proxenos (shell-out to `send`, poll of the stream
file); middleware trouble disables the agent partner and never blocks the
game. Reader rules per DESIGN.md: skip empty/malformed lines, deliver a final
unterminated record that parses, hold torn prefixes, re-baseline when the file
is replaced, dedupe on event id.
