/**
 * proxenos-euchre — omp extension for the conversation-first euchre flow.
 *
 * The user tells their agent "let's play euchre"; the agent calls euchre_open
 * and this extension launches ./euchre in a Tern split beside the
 * conversation, watches the proxenos stream file, and injects the events the
 * agent must ACT on into the live session (pi.sendUserMessage). Everything
 * else arrives as silent hidden context on the agent's next turn.
 *
 * The user plays South; the agent can claim ANY of the other three seats —
 * North (the classic partner setup), East/West (the opponents), or all three
 * at once ("you play my partner AND both opponents"). Claims are additive and
 * idempotent per seat; euchre.claim_seat also releases seats back to the
 * built-in bots with {"seats":[…],"release":true}. The app discloses each
 * claimed seat's hand as deal.your_hand {seat, cards}, and re-emits it on
 * euchre.hand_request {seat}. Unclaimed seats are played by built-in bots;
 * claimed seats fall back to a bot after the app's --agent-timeout.
 *
 * Wake policy (filter by event BODY, not just type — the app emits prompts
 * for all four seats): the watcher tracks the claimed-seat set from the
 * euchre.claim_seat events flowing on the stream (its own agent's sends
 * included) and wakes only when a CLAIMED seat must act:
 *   - bid.prompt    with body.turn claimed                 → WAKE (bid)
 *   - bid.made      order_up while the dealer is claimed    → WAKE (discard)
 *   - trick.started with body.leader claimed               → WAKE (lead the trick)
 *   - card.played   whose next actor (S→W→N→E, skipping a lone bidder's
 *                   sitting-out partner) is claimed        → WAKE — play a card
 *                   (trick state — trickPlays, and the next-actor seat — is
 *                   tracked for EVERY card.played in handleRecord BEFORE the
 *                   own-send filter, so the seat after the agent's own play
 *                   is always woken)
 *   - action.rejected carrying body.cards                   → WAKE — the
 *                   app attaches the seat's CURRENT hand; the agent
 *                   self-corrects from the cards list, no hand_request
 *                   round-trip
 *   - hand.summary                                           → ALWAYS WAKE (coach with euchre.tip)
 *   - game.over                                              → WAKE (report the result)
 *   - everything else (game.started — which carries no hand since South's
 *     cards stay in the TUI — unclaimed seats'
 *     prompts and plays, trick.won, stream.contract/meta)  → silent context
 * Exception: deal.your_hand for a claimed seat arriving OUTSIDE a wake
 * sequence (no bid/trick/play currently awaited — a mid-game claim, or a
 * hand_request answered while nothing is pending) wakes with the hand inline,
 * so a seat claimed mid-game is never silently bot-played. Each wake names
 * the seat that must act, so multi-seat claims produce one unambiguous wake
 * per decision.
 *
 * Reply vocabulary taught to the agent:
 *   proxenos send <stream> euchre.claim_seat '{"seat":"north"}' --as agent:me
 *   proxenos send <stream> euchre.claim_seat '{"seats":["north","east","west"]}' --as agent:me
 *   proxenos send <stream> euchre.bid '{"decision":"order_up"}' --as agent:me
 *   proxenos send <stream> euchre.play '{"card":"JD"}' --as agent:me
 *   proxenos send <stream> euchre.hand_request '{"seat":"east"}' --as agent:me
 *   proxenos send <stream> euchre.tip '{"text":"…"}' --as agent:me
 *
 * The app stays harness-agnostic: this file is the only omp-specific glue.
 *
 * Web mode (euchre_open with web:true): attach to the browser-based game
 * served by the separately running euchre-web server (stream euchre-web,
 * default port 8090) — same watcher, wake policy and reply commands, but
 * NOTHING is spawned; the board lives in the user's browser.
 *
 * Install: copy to ~/.omp/agent/extensions/ (extensions load at session
 * start — a brand-new file is not picked up by /reload-plugins), or run
 * `omp -e /path/to/proxenos-euchre.ts`.
 */
import type { ExtensionAPI } from "@oh-my-pi/pi-coding-agent";
import { execFile, spawn } from "node:child_process";
import { promisify } from "node:util";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

const execFileP = promisify(execFile);

interface Envelope {
	v: number;
	id: string;
	ts: string;
	src: string;
	type: string;
	body: unknown;
	reply_to?: string;
}

interface EuchreBody {
	dealer?: string;
	round?: number;
	topCard?: string;
	turn?: string;
	leader?: string;
	seat?: string;
	seats?: string[];
	release?: boolean;
	card?: string;
	trickNo?: number;
	trump?: string;
	decision?: string;
	alone?: boolean;
	winner?: string;
	scoreNSE?: number[];
	tricksNSE?: number[];
	action?: string;
	reason?: string;
	cards?: string[];
}

interface EuchreParams {
	dir?: string;
	stream?: string;
	web?: boolean;
	launch?: "auto" | "none" | "detached";
	block?: string;
	wake?: { summary?: boolean; over?: boolean };
}

interface WakeConfig {
	/** Always wake on hand.summary (the coaching hook). */
	summary: boolean;
	/** Wake on game.over. */
	over: boolean;
}

/** Seats act clockwise in this order: South → West → North → East. */
const SEAT_ORDER = ["south", "west", "north", "east"];

function seatAfter(seat: string): string {
	const i = SEAT_ORDER.indexOf(seat);
	return i === -1 ? seat : SEAT_ORDER[(i + 1) % 4];
}

/**
 * The seat that acts after `actor` in the current trick: clockwise, skipping
 * the sitting-out partner when someone is going alone. Used by both
 * trick.started (who plays after the leader) and card.played (who plays
 * after this card), so the skip logic must stay in lockstep.
 */
function nextActor(aloneSeat: string | undefined, actor: string): string {
	let next = seatAfter(actor);
	if (aloneSeat && next === seatPartnerOf(aloneSeat)) {
		next = seatAfter(next);
	}
	return next;
}

/** The seat across the table from `seat` (a lone bidder's partner sits out). */
function seatPartnerOf(seat: string): string {
	return seatAfter(seatAfter(seat));
}

/** The seat a wake sequence is delivering for (null = not seat-scoped). */
function wakeSeat(type: string, b: EuchreBody): string | null {
	switch (type) {
		case "bid.prompt":
			return b.turn ?? null;
		case "bid.made":
		case "trick.started":
			// discard goes to the dealer; trick lead to the leader
			return (type === "trick.started" ? b.leader : b.seat) ?? null;
		case "card.played":
			return b.seat ?? null;
		default:
			return null;
	}
}

interface Watcher {
	stream: string;
	filePath: string;
	offset: number;
	pendingCarry: string;
	timer: NodeJS.Timeout;
	seen: Set<string>;
	spawnedPid?: number;
	/** tern block id when the app was spawned via the tern CLI (shutdown kills it). */
	ternBlockId?: string | null;
	cfg: WakeConfig;
	/** Seats the agent has claimed (per the euchre.claim_seat events on the
	 *  stream — claims and releases from any agent src update the set). */
	claimed: Set<string>;
	/** dealer seat from the most recent bid.prompt — an order_up bid.made
	 *  while the dealer is claimed means the discard phase is its. */
	lastBidPromptDealer: string | undefined;
	/** who is going alone this hand (from bid.made alone:true), for the
	 *  sit-out skip in nextActor. */
	aloneSeat: string | undefined;
	/** leader of the current trick — card.played events compute the next
	 *  actor from the played card's seat; this guards against card.played
	 *  events seen outside a live trick (e.g. a watcher attached mid-trick).
	 *  Set by trick.started, cleared by hand.summary — NEVER cleared by
	 *  card.played, or the seat after the second play would lose its wake. */
	trickLeader: string | undefined;
	/** plays made in the current trick (EVERY card.played counts, tracked in
	 *  handleRecord before the own-send filter — mirroring claim_seat — so
	 *  the next-actor computation never strands on a filtered event). When
	 *  this reaches the active-seat count the trick is over and what follows
	 *  is trick.won, not another turn. */
	trickPlays: number;
	/** Seat a wake sequence is currently delivering for (set when a
	 *  bid/trick/play wake is issued, cleared by hand.summary). deal.your_hand
	 *  events arriving while a wake is pending are normal decision-point
	 *  disclosures; arriving with this undefined they are out-of-band (a
	 *  mid-game claim) and deserve their own wake. */
	pendingWakeSeat: string | undefined;
	/** Context lines awaiting the next turn (hidden, zero-cost delivery). */
	digest: string[];
	digestTimer: NodeJS.Timeout | undefined;
}

const POLL_MS = 500;
const STREAM_WAIT_MS = 10_000;
const DIGEST_FLUSH_MS = 2_000;
const DIGEST_MAX_LINES = 24;
const DIGEST_LINE_MAX = 140;
const APP_BIN = "euchre";
const DEFAULT_STREAM = "euchre";
/** Stream used by the browser-based euchre-web server (cmd/euchre-web). */
const DEFAULT_STREAM_WEB = "euchre-web";

/**
 * Multiplexer omp can spawn into, detected from the session environment.
 * null = none recognized — the launch chain then tries the tern CLI before
 * falling back to a detached pty.
 */
function detectMultiplexer(): "herdr" | "tmux" | "zellij" | "cmux" | "orca" | null {
	const e = process.env;
	if (e.HERDR_ENV) return "herdr";
	if (e.TMUX) return "tmux";
	if (e.ZELLIJ) return "zellij";
	if (e.CMUX_WORKSPACE_ID || e.CMUX_SURFACE_ID) return "cmux";
	if (e.ORCA_PANE_KEY) return "orca";
	return null;
}

/**
 * Spawn argv in a new Tern block and return the block id. `tern split PANE
 * right --cwd DIR --keep-open -- ARGV...` puts the command in a block beside
 * the calling pane. argv goes straight to execFile: no shell, no quoting.
 * Failures fall through to a new tab, then to the detached-pty path.
 */
async function spawnInTern(args: string[], cwd: string, target?: string): Promise<string | null> {
	// The extension runs in omp's shared kernel, which has no TERN_PANE, so the
	// agent passes its own pane id (from `echo $TERN_PANE`); @focused is the
	// last resort.
	const pane = target || process.env.TERN_PANE || "@focused";
	try {
		const { stdout } = await execFileP(
			"tern",
			["split", "--json", pane, "right", "--cwd", cwd, "--keep-open", "--", ...args],
			{ timeout: 20_000 },
		);
		const created = JSON.parse(stdout) as { block?: number | string };
		if (created?.block !== undefined) return String(created.block);
	} catch (err) {
		console.error(`[proxenos-euchre] tern split failed, trying a new tab: ${String(err)}`);
	}
	try {
		const { stdout } = await execFileP(
			"tern",
			["new", "tab", "--json", "--cwd", cwd, "--keep-open", "--", ...args],
			{ timeout: 20_000 },
		);
		const created = JSON.parse(stdout) as { block?: number | string };
		return created?.block !== undefined ? String(created.block) : null;
	} catch (err) {
		console.error(`[proxenos-euchre] tern spawn failed: ${String(err)}`);
		return null;
	}
}

/**
 * Resolve the app directory: it must contain an executable `euchre` file.
 * Accepts the repo root (binary at the root), the repo's cmd/euchre directory,
 * or — when the caller passed some unrelated directory — a built binary in
 * ~/.local/bin. Returns null when none holds, so callers fail loudly instead
 * of spawning `./euchre` into "Is a directory" (exit 126).
 */
function resolveAppDir(dir: string): string | null {
	const isExec = (p: string): boolean => {
		try {
			const st = fs.statSync(p);
			return st.isFile() && (st.mode & 0o111) !== 0;
		} catch {
			return false;
		}
	};
	if (isExec(path.join(dir, APP_BIN))) return dir;
	const nested = path.join(dir, "cmd", "euchre");
	if (isExec(nested)) return nested;
	// Fall back to a built binary next to the extension (install hint), or any
	// euchre on PATH: `go build -o euchre ./cmd/euchre` in the repo.
	if (isExec(path.join(os.homedir(), ".local", "bin", APP_BIN)))
		return path.join(os.homedir(), ".local", "bin");
	return null;
}

/** Is the app instance this watcher launched still alive? */
async function appAlive(w: Watcher): Promise<boolean> {
	if (w.spawnedPid) {
		try {
			process.kill(w.spawnedPid, 0);
			return true;
		} catch {
			return false;
		}
	}
	if (w.ternBlockId) {
		try {
			const { stdout } = await execFileP("tern", ["ls", "--json"], { timeout: 5000 });
			return stdout.includes(w.ternBlockId);
		} catch {
			return false;
		}
	}
	return false;
}

function sleep(ms: number): Promise<void> {
	const { promise, resolve } = Promise.withResolvers<void>();
	setTimeout(resolve, ms);
	return promise;
}

function streamsDir(): string {
	return path.join(
		process.env.XDG_DATA_HOME ?? path.join(os.homedir(), ".local", "share"),
		"proxenos",
		"streams",
	);
}

export default function proxenosEuchreExtension(pi: ExtensionAPI) {
	const z = pi.zod;
	const watchers = new Map<string, Watcher>();

	/** Compact one-line summary for the silent-context digest. */
	function eventLine(e: Envelope): string {
		const body = JSON.stringify(e.body ?? {});
		return `${e.type} ${body.length > DIGEST_LINE_MAX ? body.slice(0, DIGEST_LINE_MAX) + "…" : body}`;
	}

	/** Free tier: queue a context line, flushed (debounced) as hidden nextTurn context. */
	function queueDigest(w: Watcher, line: string): void {
		w.digest.push(line);
		if (w.digest.length > DIGEST_MAX_LINES) {
			w.digest.splice(0, w.digest.length - DIGEST_MAX_LINES);
		}
		if (w.digestTimer) return;
		w.digestTimer = setTimeout(() => {
			w.digestTimer = undefined;
			if (w.digest.length === 0) return;
			const lines = w.digest.splice(0, w.digest.length).join("\n");
			pi.sendMessage(
				`[euchre ${w.stream}] recent app activity (context only — no reply needed unless the user asks):\n${lines}`,
				{ deliverAs: "nextTurn" },
			);
		}, DIGEST_FLUSH_MS);
	}

	/**
	 * The body filter: return the wake message when this event must start a
	 * turn, null when it is silent context. The watcher tracks which seats
	 * the agent has claimed; a wake fires only when a CLAIMED seat must act.
	 * Seats act in order S→W→N→E, so a claimed seat's play turn follows the
	 * trick leader or the previous card. An order_up bid.made while a claimed
	 * seat is dealer means the discard phase is its (bid.prompt is not
	 * re-emitted for the discard). hand.summary always wakes (the coaching
	 * hook).
	 */
	function wakeMessage(w: Watcher, e: Envelope): string | null {
		const b = (e.body ?? {}) as EuchreBody;
		switch (e.type) {
			case "bid.prompt":
				w.lastBidPromptDealer = b.dealer;
				if (!b.turn || !w.claimed.has(b.turn)) return null;
				return (
					`[euchre ${w.stream}] Bidding — it is ${b.turn}'s turn to bid (you play ${b.turn})` +
					(b.topCard ? ` (round ${b.round ?? "?"}, top card ${b.topCard}${b.dealer ? `, dealer ${b.dealer}` : ""})` : "") +
					`. Send exactly one:\n` +
					`  proxenos send ${w.stream} euchre.bid '{"decision":"order_up"}' --as agent:me   (round 1: take the top card's suit)\n` +
					`  proxenos send ${w.stream} euchre.bid '{"decision":"pass"}' --as agent:me\n` +
					`  proxenos send ${w.stream} euchre.bid '{"decision":"H"}' --as agent:me   (round 2 only: C/D/H/S, not the turned-down suit; add "goingAlone":true to go alone)\n` +
					`No ${b.turn} hand yet? proxenos send ${w.stream} euchre.hand_request '{"seat":"${b.turn}"}' --as agent:me.\n` +
					`If ${b.turn} is the dealer and someone ordered up, discard with euchre.play next.`
				);
			case "bid.made": {
				if (b.alone) w.aloneSeat = b.seat;
				// An order_up always sends the dealer to the discard phase —
				// including when the dealer ordered up their own top card.
				const dealer = w.lastBidPromptDealer;
				if (b.decision === "order_up" && dealer && w.claimed.has(dealer)) {
					return (
						`[euchre ${w.stream}] ${b.seat ?? "someone"} ordered up and YOU (${dealer}) are the dealer: ` +
						`pick up and discard — send proxenos send ${w.stream} euchre.play '{"card":"XH"}' --as agent:me with one card from ${dealer}'s six-card hand.`
					);
				}
				return null;
			}
			case "trick.started":
				w.trickLeader = b.leader;
				w.trickPlays = 0;
				if (!b.leader || !w.claimed.has(b.leader)) return null;
				return (
					`[euchre ${w.stream}] New trick — YOU lead as ${b.leader}${b.trump ? ` (trump ${b.trump})` : ""}. ` +
					`Send proxenos send ${w.stream} euchre.play '{"card":"JD"}' --as agent:me with a card from ${b.leader}'s hand ` +
					`(no hand in context? proxenos send ${w.stream} euchre.hand_request '{"seat":"${b.leader}"}' --as agent:me). ` +
					`Follow the led suit when you can; the jack of the trump's color is the left bower.`
				);
			case "card.played": {
				// Who acts after this card? Clockwise from the PLAYER of this
				// card (b.seat — not the trick leader, or only the first play
				// of each trick would ever wake the following seat: the East
				// never woken while the agent held West and North). A live
				// trick is guarded by trickLeader, set by trick.started.
				if (!w.trickLeader) return null;
				// The fourth play (third on a loner) is followed by trick.won,
				// not another turn.
				const need = w.aloneSeat ? 3 : 4;
				if (w.trickPlays >= need) return null;
				const next = nextActor(w.aloneSeat, b.seat ?? w.trickLeader);
				if (next === b.seat || !w.claimed.has(next)) return null;
				return (
					`[euchre ${w.stream}] ${b.seat ?? "they"} played ${b.card ?? "?"} — it is ${next}'s turn to play (you play ${next})` +
					(b.trump ? ` (trump ${b.trump})` : "") +
					`. Send proxenos send ${w.stream} euchre.play '{"card":"JD"}' --as agent:me with a legal card from ${next}'s hand ` +
					`(follow suit if you can; if a send is rejected, the rejection carries that seat's current cards — resend from that list).`
				);
			}
			case "deal.your_hand": {
				// A claimed seat's hand arriving OUTSIDE a wake sequence (no bid,
				// trick or play is currently awaited — e.g. the agent claimed the
				// seat mid-game or re-joined a live session) must not vanish into
				// the digest: without a nudge the agent never reads its cards and
				// the seat stays bot-played. Wake with the hand inline.
				const waiting = w.pendingWakeSeat !== undefined;
				if (waiting) return null; // normal disclosure: the wake sequence delivers it
				if (!b.seat || !w.claimed.has(b.seat)) return null;
				const cards = (b.cards ?? []).join(" ");
				return (
					`[euchre ${w.stream}] You play ${b.seat} — its current hand: ${cards}. ` +
					`It is not your turn yet; wait for a wake naming ${b.seat}, then send ` +
					`proxenos send ${w.stream} euchre.play '{"card":"JD"}' --as agent:me with one of these cards.`
				);
			}
			case "action.rejected": {
				// A rejected play/bid for a claimed seat carries that seat's
				// CURRENT hand (body.cards) — wake so the agent corrects
				// immediately from that list; the old euchre.hand_request
				// recovery round-trip never happened reliably and is gone.
				if (!b.cards || b.cards.length === 0) return null;
				return (
					`[euchre ${w.stream}] Your ${b.action ?? "action"} was rejected: ${b.reason ?? "?"}. ` +
					`Use the cards list in this rejection — current hand: ${b.cards.join(" ")}. ` +
					`Send proxenos send ${w.stream} euchre.play '{"card":"<one of those cards>"}' --as agent:me ` +
					`(or euchre.bid for a rejected bid) when it is that seat's turn again.`
				);
			}
			case "hand.summary":
				w.aloneSeat = undefined;
				w.trickLeader = undefined;
				w.trickPlays = 0;
				w.pendingWakeSeat = undefined;
				if (!w.cfg.summary) return null;
				return (
					`[euchre ${w.stream}] Hand over — score NSE ${JSON.stringify(b.scoreNSE ?? "?")}, tricks ${JSON.stringify(b.tricksNSE ?? "?")}. ` +
					`Coach the user (your South partner) with ONE short tip:\n` +
					`  proxenos send ${w.stream} euchre.tip '{"text":"…"}' --as agent:me\n` +
					`One or two sentences on what the bidding or a key play says, or what to try next hand. ` +
					`It renders in the app's tips panel. Still holding every seat you meant to claim? ` +
					`Missing one — send euchre.claim_seat again (it is idempotent).`
				);
			case "game.over":
				if (!w.cfg.over) return null;
				return (
					`[euchre ${w.stream}] Game over — winner: ${b.winner ?? "?"}. ` +
					`Tell the user here in chat how the match went and offer a rematch ` +
					`(the app must be relaunched for a new game).`
				);
			default:
				return null;
		}
	}

	function handleRecord(w: Watcher, raw: string): void {
		const line = raw.trim();
		if (line === "") return;
		let e: Envelope;
		try {
			e = JSON.parse(line) as Envelope;
		} catch {
			return; // torn tail — the offset logic keeps the bytes for next poll
		}
		if (!e || typeof e.type !== "string") return;
		if (w.seen.has(e.id)) return;
		w.seen.add(e.id);
		if (w.seen.size > 1024) {
			const it = w.seen.values();
			for (let i = 0; i < 256; i++) {
				const v = it.next().value;
				if (v === undefined) break;
				w.seen.delete(v);
			}
		}
		// Claim/release events drive the claimed-seat set no matter who sent
		// them (the app applies any agent's claim). Tracked BEFORE the
		// own-send filter below so the agent's echoed claims still land here.
		if (e.type === "euchre.claim_seat") {
			const b = (e.body ?? {}) as EuchreBody;
			for (const s of b.seats ?? (b.seat ? [b.seat] : [])) {
				if (b.release) w.claimed.delete(s);
				else w.claimed.add(s);
			}
		}
		// Trick-state tracking must also see EVERY card.played, including
		// echoes of the agent's own sends (the app emits card.played for an
		// accepted claimed-seat play — the same pattern as claim_seat above).
		// wakeMessage computes the next actor from that state; filtering
		// first would strand the following seat without a wake.
		if (e.type === "card.played") {
			w.trickPlays++;
		}
		// The agent's own sends echo onto the stream; never react to those.
		if (e.src.startsWith("agent:")) return;

		const wake = wakeMessage(w, e);
		if (wake !== null) {
			pi.logger.debug("px-euchre: wake", { stream: w.stream, type: e.type, id: e.id });
			const seat = wakeSeat(e.type, (e.body ?? {}) as EuchreBody);
			if (seat !== null) {
				w.pendingWakeSeat = seat;
			}
			pi.sendUserMessage(wake);
			return;
		}
		// Everything else: free tier. No turn, hidden context on the next one.
		queueDigest(w, eventLine(e));
	}

	function startWatcher(stream: string, filePath: string, spawnedPid: number | undefined, cfg: WakeConfig, startOffset: number): Watcher {
		const existing = watchers.get(stream);
		if (existing) {
			// Reconfigure in place — a second euchre_open call is a settings
			// update, never a duplicate watcher or a second launch.
			existing.cfg = cfg;
			return existing;
		}
		const w: Watcher = {
			stream,
			filePath,
			offset: startOffset,
			pendingCarry: "",
			timer: null as never,
			seen: new Set(),
			spawnedPid,
			cfg,
			claimed: new Set(),
			lastBidPromptDealer: undefined,
			aloneSeat: undefined,
			trickLeader: undefined,
			trickPlays: 0,
			pendingWakeSeat: undefined,
			digest: [],
			digestTimer: undefined,
		};
		watchers.set(stream, w);

		w.timer = setInterval(() => {
			let fd: number;
			try {
				fd = fs.openSync(filePath, "r");
			} catch {
				return; // not created yet / transient
			}
			try {
				const size = fs.fstatSync(fd).size;
				if (size < w.offset) {
					// replaced or truncated (compact / new generation): re-baseline at end
					w.offset = size;
					w.pendingCarry = "";
					return;
				}
				if (size === w.offset) return;
				const buf = Buffer.alloc(size - w.offset);
				fs.readSync(fd, buf, 0, buf.length, w.offset);
				const chunk = w.pendingCarry + buf.toString("utf8");
				w.pendingCarry = "";
				// leading-\n framing: records are \n-separated; the file never ends
				// with a newline in steady state, so the final segment may be the
				// newest complete record (EOF rule) or a torn prefix.
				let start = 0;
				for (;;) {
					const nl = chunk.indexOf("\n", start);
					if (nl === -1) break;
					handleRecord(w, chunk.slice(start, nl));
					start = nl + 1;
				}
				const tail = chunk.slice(start);
				if (tail === "") {
					w.offset = size;
					return;
				}
				try {
					JSON.parse(tail); // complete record, unterminated — deliverable now
					handleRecord(w, tail);
					w.offset = size;
				} catch {
					// torn prefix: keep offset at the tail start so nothing is skipped
					w.pendingCarry = tail;
					w.offset = size - Buffer.byteLength(tail);
				}
			} finally {
				fs.closeSync(fd);
			}
		}, POLL_MS);
		return w;
	}

	pi.registerTool({
		name: "euchre_open",
		label: "Open euchre",
		description:
			"Launch the euchre terminal card game attached to a shared proxenos event stream, " +
			"then watch the stream so you can play. Call this when the user says things like " +
			"\"let's play euchre\". The user plays South; claim the seats the user asked you to " +
			"play — ANY of North, East, West, or ALL THREE at once — with euchre.claim_seat: " +
			"one seat {\"seat\":\"north\"} (the classic partner) or all three in ONE send " +
			"{\"seats\":[\"north\",\"east\",\"west\"]} (\"you play my partner AND both opponents\"). " +
			"CLAIM SEATS BEFORE ANYTHING ELSE — the game starts bidding immediately, and only a " +
			"claimed seat's prompts wake you. Claims are additive and idempotent; pass " +
			"{\"seats\":[…],\"release\":true} to hand seats back to the bots. Each claimed seat's " +
			"hand is disclosed as deal.your_hand {seat, cards} (refresh with euchre.hand_request " +
			"{\"seat\":\"east\"}). The game has no chat box: the user talks to you here, in this " +
			"session. " +
			"WAKE POLICY: the watcher tracks your claimed seats; you are woken ONLY when one " +
			"of them must act — a bid.prompt naming it, a bid.made order_up while it is " +
			"dealer (discard), a trick.started it leads, a card.played whose next actor " +
			"(S→W→N→E, skipping a lone bidder's sitting-out partner) is a claimed seat — " +
			"ALWAYS on hand.summary (send a coaching euchre.tip), on game.over, and on an " +
			"action.rejected that carries body.cards (the seat's current hand — resend from " +
			"that list). Every wake " +
			"names the seat that must act; prompts and plays for unclaimed seats arrive " +
			"silently as hidden context on your next turn. " +
			"Use launch:'none' only if the user explicitly wants to run the game themselves — " +
			"the watcher still works; catch up with `proxenos tail <stream> --tail 60 --pretty`. " +
			"For the browser-based game, set web:true to attach to the browser-based game served by " +
			"the euchre-web server (already running separately); do not set it for the terminal game. " +
			"Calling this again reconfigures wake settings and relaunches if the app died — " +
			"never improvise tmux/screen commands for this.",
		approval: "exec",
		parameters: z.object({
			dir: z.string().optional().describe("Directory containing the euchre binary (default: current directory)"),
			stream: z.string().optional().describe(`Proxenos stream name (default: ${DEFAULT_STREAM}, or ${DEFAULT_STREAM_WEB} when web:true). Use a throwaway name for tests.`),
			web: z.boolean().optional().describe("Attach to the browser-based game instead of launching the TUI: watch stream " + DEFAULT_STREAM_WEB + " (unless overridden) and never spawn anything — the board lives in the user's browser, served by the already-running euchre-web server. Leave unset for the terminal game."),
			launch: z.enum(["auto", "none", "detached"]).optional().describe("'auto' (default) spawns the game in a detected multiplexer pane; 'detached' runs it headless in its own pty; 'none' only watches the stream. Ignored when web:true — web mode never spawns."),
			block: z.string().optional().describe("Tern block to split beside (the pane you are talking in). Get it from your shell: echo $TERN_PANE."),
			wake: z.object({
				summary: z.boolean().optional().describe("Always wake on hand.summary for the coaching tip (default true)"),
				over: z.boolean().optional().describe("Wake on game.over (default true)"),
			}).optional().describe("Override the wake policy"),
		}),
		async execute(_toolCallId, params: EuchreParams, _signal, _onUpdate, ctx) {
			const requestedDir = params.dir ?? process.cwd();
			const appDir = resolveAppDir(requestedDir);
			const dir = appDir ?? requestedDir;
			const stream = params.stream ?? (params.web ? DEFAULT_STREAM_WEB : DEFAULT_STREAM);
			const cfg = {
				summary: params.wake?.summary ?? true,
				over: params.wake?.over ?? true,
			};
			const args = ["./" + APP_BIN, "--stream", stream];

			// Snapshot before launching: the watcher starts at this offset so
			// everything the fresh app instance emits is seen, while stale events
			// from a previous match on the same stream are not replayed.
			const preSize = (() => {
				try {
					return fs.statSync(path.join(streamsDir(), `${stream}.jsonl`)).size;
				} catch {
					return 0;
				}
			})();

			// Reconfigure vs relaunch: an existing watcher is updated in place, but
			// "reconfigure" must never stand in for "the app is open". If the
			// caller wants the app open (launch:'auto') and the previously
			// launched instance is dead, relaunch against the same watcher.
			const existing = watchers.get(stream);
			if (existing) {
				const wantsLaunch = !params.web && (params.launch ?? "auto") === "auto";
				const alive = await appAlive(existing);
				if (!wantsLaunch || alive) {
					const state = alive
						? `euchre is running (pid ${existing.spawnedPid ?? "?"}${existing.ternBlockId ? `, tern block ${existing.ternBlockId}` : ""})`
						: params.web
							? "web mode — the board is in the user's browser (euchre-web server, already running)"
							: "watch-only mode — the user runs euchre themselves";
					startWatcher(stream, "", undefined, cfg, preSize);
					return {
						content: [
							{
								type: "text",
								text:
									`Watcher for stream "${stream}" reconfigured: ` +
									`wake=bid.prompt/discard/trick/card.played for any claimed seat/summary=${cfg.summary}/over=${cfg.over}. ${state}. ` +
									"No new app instance was launched.",
							},
						],
						details: { stream, reconfigured: true, appAlive: alive },
					};
				}
				// Dead app, caller wants it open: clean stale handles and relaunch.
				existing.spawnedPid = undefined;
				existing.ternBlockId = null;
			}

			let launchedVia: string | null = null;
			let spawnedPid: number | undefined;
			let ternBlockId: string | null = null;
			const launchMode = params.web ? "none" : (params.launch ?? "auto");
			if (launchMode !== "none") {
				// Validate before creating any block/session — a wrong dir would
				// spawn a doomed block ("Is a directory", exit 126).
				if (!appDir) {
					return {
						content: [
							{
								type: "text",
								text: `No executable euchre in ${requestedDir} (nor ${path.join(requestedDir, APP_BIN)}). ` +
									`Pass dir=<repo root containing the euchre binary>, e.g. dir=~/euchre-demo. ` +
									`Nothing was launched.`,
							},
						],
						details: { stream, launched: false, dir: requestedDir },
					};
				}
				if (launchMode === "auto") {
					const mux = detectMultiplexer();
					if (mux && ctx.ui?.openTerminal) {
						try {
							const req = { multiplexer: mux, placement: "pane", command: args, cwd: dir } as Parameters<
								NonNullable<typeof ctx.ui.openTerminal>
							>[0];
							await ctx.ui.openTerminal(req);
							launchedVia = `${mux} pane`;
						} catch (err) {
							pi.logger.debug("proxenos-euchre: openTerminal failed, falling back", { err: String(err) });
						}
					}
					if (!launchedVia) {
						ternBlockId = await spawnInTern(args, dir, params.block);
						if (ternBlockId !== null) launchedVia = "tern block";
					}
				}
				if (!launchedVia) {
					// Headless: the game gets its own pty via script(1) so the TUI
					// doesn't die on a closed stdout. Output goes nowhere — the
					// watcher still works, only South's typing needs a real pane.
					const child = spawn("script", ["-qec", args.join(" "), "/dev/null"], {
						cwd: dir,
						detached: true,
						stdio: "ignore",
					});
					child.unref();
					spawnedPid = child.pid;
					launchedVia = "detached pty";
				}
			}

			const filePath = path.join(streamsDir(), `${stream}.jsonl`);
			let found = false;
			for (let i = 0; i < STREAM_WAIT_MS / 250; i++) {
				if (fs.existsSync(filePath) && (launchMode === "none" || fs.statSync(filePath).size > preSize)) {
					found = true;
					break;
				}
				await sleep(250);
			}
			if (!found) {
				const userCmd = `cd ${dir} && ./${APP_BIN} --stream ${stream}`;
				return {
					content: [
						{
							type: "text",
							text: launchMode !== "none"
								? `euchre was launched but stream "${stream}" never appeared at ${filePath}. ` +
									`Check that ./${APP_BIN} exists in ${dir} and proxenos is on PATH.`
								: params.web
									? `No stream "${stream}" yet — the euchre-web server has not created it ` +
										`(is the server running? It defaults to port 8090 and prints its URL at startup). ` +
										`Nothing was spawned; call euchre_open again with web:true once the server is up.`
									: `No stream "${stream}" yet. Ask the user to run: ${userCmd} ` +
										`(in their own split/pane), then call euchre_open again with launch:'none'.`,
						},
					],
					details: { stream, launched: false },
				};
			}

			// Start at the pre-launch size so the fresh instance's opening events
			// (game.started, first deal) are all seen, while stale events from a
			// previous match on the same stream are not replayed.
			const w = startWatcher(stream, filePath, spawnedPid, cfg, preSize);
			w.ternBlockId = ternBlockId;

			const opening = params.web
				? `WEB MODE — the euchre board is in the user's BROWSER, served by the euchre-web server ` +
					`(default port 8090; the server printed its exact URL at startup — do not guess an IP or port). ` +
					`Do NOT open the terminal TUI and do not spawn anything: the game is ALREADY running on ` +
					`stream "${stream}". Claim seats and play.`
				: launchedVia
				? `euchre is open (${launchedVia}) on stream "${stream}".` +
					(launchedVia === "tern block"
						? ` It is a split beside this conversation in the same tern tab` +
							(ternBlockId ? ` (block ${ternBlockId}; close it with \`tern close ${ternBlockId}\`).` : ".")
						: "")
				: `Watching stream "${stream}" — waiting for the user to open euchre in their own pane (${args.join(" ")}).`;
			return {
				content: [
					{
						type: "text",
						text:
							opening + "\n" +
							`The user plays South; you play EVERY seat you claimed. Claim seats now, ` +
							`before the next wake (additive, idempotent, any time):\n` +
							`  proxenos send ${stream} euchre.claim_seat '{"seat":"north"}' --as agent:me   (one seat — the classic partner)\n` +
							`  proxenos send ${stream} euchre.claim_seat '{"seats":["north","east","west"]}' --as agent:me   (ALL three in ONE send — you play partner AND both opponents)\n` +
							`  proxenos send ${stream} euchre.claim_seat '{"seats":["west"],"release":true}' --as agent:me   (hand a seat back to its bot)\n` +
							`Mid-hand claims work too: each newly claimed seat's CURRENT hand arrives as ` +
							`deal.your_hand {seat, cards} at the next decision point. If you claimed a seat ` +
							`but have not seen its hand yet (or lost track after a renege/rejection), ask for ` +
							`it again:\n` +
							`  proxenos send ${stream} euchre.hand_request '{"seat":"east"}' --as agent:me   (re-emits east's deal.your_hand)\n` +
							`You are woken ONLY when a claimed seat must act, and the wake names the seat. Act when woken:\n` +
							`  proxenos send ${stream} euchre.bid '{"decision":"order_up"}' --as agent:me   (round 1; "pass"; or a suit letter C/D/H/S in round 2, optional "goingAlone":true)\n` +
							`  proxenos send ${stream} euchre.play '{"card":"JD"}' --as agent:me   (a card from THAT seat's hand, or its dealer discard)\n` +
							`  proxenos send ${stream} euchre.tip '{"text":"…"}' --as agent:me   (after EVERY hand.summary — one or two coaching sentences for your South partner)\n` +
							`If euchre.play or euchre.bid is rejected, the rejection carries that seat's CURRENT ` +
							`hand in body.cards — resend using one of those cards, no euchre.hand_request needed. ` +
							`If you go quiet past the app's agent-timeout, bots fill in for your claimed seats until you wake up.\n` +
							`Full vocabulary: proxenos tail ${stream} --tail 30 --pretty`,
					},
				],
				details: { stream, launchedVia, filePath },
			};
		},
	});

	pi.on("session_shutdown", async () => {
		for (const w of watchers.values()) {
			clearInterval(w.timer);
			clearTimeout(w.digestTimer);
			if (w.spawnedPid) {
				try {
					process.kill(-w.spawnedPid, "SIGTERM"); // script(1) made it a group leader
				} catch {
					/* already gone */
				}
			}
			if (w.ternBlockId) {
				// Only kill blocks the extension spawned, never user-created ones.
				try {
					await execFileP("tern", ["close", w.ternBlockId], { timeout: 5000 });
				} catch {
					/* already gone or tern absent */
				}
			}
		}
		watchers.clear();
	});
}
