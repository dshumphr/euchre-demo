# euchre-demo ↔ omp extension

This is the **harness-side glue** for the conversation-first flow. It is what
makes the user just say **"let's play euchre"** in their omp chat: the agent
calls `euchre_open`, the game opens in a Tern block beside the conversation,
and the agent joins the game. It is a single TypeScript extension
(`proxenos-euchre.ts`) that omp loads; euchre itself only speaks the proxenos
stream, so it stays usable without any agent.

Adapted from bibtui's proven `proxenos-bibtui.ts` pattern.

## What it does

1. **Registers `euchre_open`** — params: `dir`, `stream` (default `euchre`),
   `web` (attach to the browser game — see *Web mode* below),
   `launch` (`auto` | `none` | `detached`), `block`, optional `wake` override
   (`summary`, `over`).
2. **Spawns euchre in a Tern block** beside the conversation:
   `tern split <pane> right --cwd <dir> --keep-open -- ./euchre --stream <stream>`.
   The tool result teaches the agent to claim seats first:
   `proxenos send <stream> euchre.claim_seat '{"seat":"north"}' --as agent:me`.
3. **Wakes the agent only when a CLAIMED seat must act** — the watcher tracks
   the claimed-seat set from the `euchre.claim_seat` events on the stream and
   filters by event BODY, not type (the app emits prompts for all four seats):

   | Event | Wake? |
   |---|---|
   | `bid.prompt` with `body.turn` claimed | **WAKE** — send `euchre.bid` |
   | `bid.made` order_up while the dealer is claimed | **WAKE** — discard (`euchre.play`) |
   | `trick.started` with `body.leader` claimed | **WAKE** — lead the trick |
   | `card.played` whose next actor (S→W→N→E, skipping a lone bidder's sitting-out partner) is claimed | **WAKE** — play a card |
   | `action.rejected` carrying `body.cards` | **WAKE** — the app attaches the seat's CURRENT hand; resend from that list, no `euchre.hand_request` round-trip |
   | `hand.summary` | **ALWAYS WAKE** — coach with `euchre.tip` |
   | `game.over` | **WAKE** — report the result |
   | `deal.your_hand` for a claimed seat while NO wake is pending | **WAKE** — out-of-band disclosure (mid-game claim): the hand is delivered inline |
   | unclaimed seats' prompts/plays, `deal.your_hand` during a wake sequence, `trick.won` | silent context |

   Every wake names the seat that must act, so claiming several seats still
   yields one unambiguous wake per decision.
4. **Teaches the full seat vocabulary**:
   - `euchre.claim_seat {"seat":"north"}` or
     `euchre.claim_seat {"seats":["north","east","west"]}` — additive,
     idempotent, any time; add `"release":true` to hand seats back to their
     bots. South is the human's seat and cannot be claimed.
   - `euchre.hand_request {"seat":"east"}` — re-emits that claimed seat's
     `deal.your_hand`.
   - `euchre.bid`, `euchre.play`, `euchre.tip` — per the app's contract.
5. **Cleans up** on session shutdown (its own Tern block + watcher only).

## Web mode

Call `euchre_open` with **`web: true`** to attach to the **browser-based
game** served by the separate `euchre-web` server (`cmd/euchre-web`, default
port 8090). Use this when the user is playing euchre in their browser rather
than the terminal TUI. In web mode the extension:

- watches the **`euchre-web`** stream instead of `euchre` (still overridable
  via `stream`);
- **never spawns anything** — no Tern split, no detached pty; the `launch`
  param is ignored. The board lives in the user's browser, at the URL the
  server printed at startup;
- everything else is identical: same watcher, same wake policy, same
  claimed-seat tracking, same taught reply commands.

Leave `web` unset for the normal terminal game.

## Typical setups

- *"let's play euchre"* — the agent claims **North** and is the user's
  partner and post-hand coach while bots play East/West.
- *"you play my partner AND both opponents"* — the agent sends one
  `euchre.claim_seat` with `{"seats":["north","east","west"]}` and plays all
  three seats against the user. Each seat's hand is disclosed to it as
  `deal.your_hand {seat, cards}` at the next decision point.

## Install

```sh
cp omp-extension/proxenos-euchre.ts ~/.omp/agent/extensions/
```

Then start a **new** omp session — extensions load at session start; a
brand-new file is not picked up by `/reload-plugins` (verified against omp
18.22.2).

Requirements: `proxenos` and `tern` on `PATH`; euchre built at `dir`.
`euchre_open`'s `dir` may be the repo root (binary at the root), the repo's
`cmd/euchre` directory, or — if you pass some unrelated directory — it falls
back to a built `~/.local/bin/euchre`.

## Play

Tell your agent: **"let's play euchre"** (one seat — the agent claims North
and is your partner), or **"you play my partner AND both opponents"** (the
agent claims North, East and West in ONE `euchre.claim_seat` and plays all
three seats against you). The agent can also claim or release seats mid-hand
— a newly claimed seat's current hand is disclosed at the next decision
point. A rejected play or bid carries that seat's CURRENT hand in its
`cards` list, so the agent self-corrects without any round-trip. Its
coaching tip appears in the tips panel after every hand.

If the agent ever seems stuck on one seat, remind it to send
`euchre.claim_seat` (idempotent) for the missing seats; wake messages teach
the rejection-cards recovery on every prompt.

Two restart/wake behaviors are pinned by the watcher's design: the app
never re-validates agent actions from previous runs (a restart used to
re-reject old plays en masse), and trick-state tracking counts EVERY
`card.played` — including echoes of the agent's own sends — before the
own-send filter, so the seat after the agent's play (classically East, with
the agent holding West and North) is always woken.
