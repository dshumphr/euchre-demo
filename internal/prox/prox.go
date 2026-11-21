// Package prox is the proxenos middleware glue for the euchre app:
// outbound events go through the proxenos CLI, inbound events come from
// polling the stream file directly (the NDJSON log is the API).
package prox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// AppSrc is this app's fixed proxenos identity.
const AppSrc = "app:euchre"

// Event mirrors proxenos's wire envelope.
type Event struct {
	V    int             `json:"v"`
	ID   string          `json:"id"`
	TS   string          `json:"ts"`
	Src  string          `json:"src"`
	Type string          `json:"type"`
	Body json.RawMessage `json:"body"`
}

// DecodeBody unmarshals an event body into v.
func (e Event) DecodeBody(v any) error {
	return json.Unmarshal(e.Body, v)
}

// Client shells out to the proxenos binary for stream setup and sends.
type Client struct {
	bin    string
	stream string
	src    string
}

// NewClient wraps the given proxenos binary and stream name, sending as
// app:euchre.
func NewClient(bin, stream string) *Client {
	return &Client{bin: bin, stream: stream, src: AppSrc}
}

// NewClientAs is NewClient with an explicit sending identity (used by the
// fake agent, which must not borrow the app's src).
func NewClientAs(bin, stream, src string) *Client {
	return &Client{bin: bin, stream: stream, src: src}
}

// EnsureStream idempotently makes sure the stream exists, creating it with the
// embedded contract if `proxenos tail <stream> --tail 1` fails. On an existing
// stream it appends the contract as a newer stream.contract event (per
// app-integration.md the last contract on the stream wins), so a stream from
// an older app version always picks up the current vocabulary.
func (c *Client) EnsureStream(contractJSON []byte) error {
	if cmd := exec.Command(c.bin, "tail", c.stream, "--tail", "1"); cmd.Run() == nil {
		return c.sendContractUpdate(contractJSON)
	}
	tmp, err := os.CreateTemp("", "euchre-contract-*.json")
	if err != nil {
		return fmt.Errorf("contract tempfile: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(contractJSON); err != nil {
		tmp.Close()
		return fmt.Errorf("contract write: %w", err)
	}
	tmp.Close()
	out, err := exec.Command(c.bin, "new", c.stream, "--contract", name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("proxenos new %s: %v (%s)", c.stream, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// sendContractUpdate appends the contract body as a stream.contract event,
// stamped fresh by the CLI (a newer contract replaces the older one).
func (c *Client) sendContractUpdate(contractJSON []byte) error {
	var record struct {
		Body json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(contractJSON, &record); err != nil {
		return fmt.Errorf("contract json: %w", err)
	}
	out, err := exec.Command(c.bin, "send", c.stream, "stream.contract", string(record.Body), "--as", c.src).CombinedOutput()
	if err != nil {
		return fmt.Errorf("contract update: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ResolvePath asks proxenos where the stream file lives (its config can move
// the streams directory, so the CLI is the authority).
func (c *Client) ResolvePath() (string, error) {
	out, err := exec.Command(c.bin, "ls", "--json").Output()
	if err != nil {
		return "", err
	}
	var infos []struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal(out, &infos); err != nil {
		return "", err
	}
	for _, si := range infos {
		if si.Name == c.stream {
			return si.Path, nil
		}
	}
	return "", fmt.Errorf("stream %q not found in proxenos ls", c.stream)
}

// Send appends one event as app:euchre and returns the stamped id.
func (c *Client) Send(typ string, body any) (string, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshal %s: %w", typ, err)
	}
	out, err := exec.Command(c.bin, "send", c.stream, typ, string(data), "--as", c.src).Output()
	if err != nil {
		return "", fmt.Errorf("send %s: %w (%s)", typ, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// ── inbound: file poll with the documented reader rules ──────────────────
//
//   - empty lines are skipped;
//   - a terminated line that fails to parse is skipped;
//   - a final unterminated segment is delivered when it parses as a complete
//     envelope, otherwise it is torn/in-flight: hold it (do not advance past);
//   - the poller re-baselines (offset 0) when the file is replaced, relying on
//     event-id dedupe to not re-deliver;
//   - events wholly below replayUntil are delivered with replay=true so the
//     consumer can apply them for state without re-validating them (a fresh
//     app start must not re-judge agent actions from previous runs).

const pollInterval = 150 * time.Millisecond

// Poller tails a stream file and hands parsed events to handle. The bool in
// the callback marks replayed pre-startup events (wholly below replayUntil).
type Poller struct {
	path        string
	offset      int64
	replayUntil int64 // deliver events starting below this offset as replay
	pastReplay  bool  // latched once the offset passes replayUntil; a
	// mid-session re-baseline (file replaced) never re-enters replay mode
	dev    uint64
	ino    uint64
	seen   map[string]bool
	handle func(Event, bool)
	logf   func(string, ...any)
}

// StartPoll runs a Poller in a background goroutine. Events that start below
// replayUntil (e.g. the stream size snapshotted just before this process
// began acting) are handed to handle with replay=true; pass 0 to mark
// nothing. There is no teardown; process exit ends it.
func StartPoll(path string, replayUntil int64, handle func(Event, bool), logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	p := &Poller{path: path, replayUntil: replayUntil, handle: handle, seen: map[string]bool{}, logf: logf}
	if st, err := os.Stat(path); err == nil {
		p.dev, p.ino = devIno(st)
	}
	go p.loop()
}

func (p *Poller) loop() {
	for range time.Tick(pollInterval) {
		p.tick()
	}
}

func (p *Poller) tick() {
	st, err := os.Stat(p.path)
	if err != nil {
		return // stream momentarily gone
	}
	dev, ino := devIno(st)
	if p.offset == 0 || dev != p.dev || ino != p.ino {
		// First look, or the file was replaced/compacted: re-baseline.
		p.dev, p.ino = dev, ino
		p.offset = 0
	}
	if st.Size() <= p.offset {
		return
	}
	f, err := os.Open(p.path)
	if err != nil {
		return
	}
	defer f.Close()
	buf := make([]byte, st.Size()-p.offset)
	n, err := f.ReadAt(buf, p.offset)
	if err != nil && n == 0 {
		return
	}
	buf = buf[:n]

	for len(buf) > 0 {
		nl := bytes.IndexByte(buf, '\n')
		if nl < 0 {
			// Unterminated tail: deliver only if it parses as a complete
			// envelope; otherwise hold it (offset does not advance).
			if ev, ok := parseEvent(bytes.TrimSpace(buf)); ok {
				p.deliver(ev, p.offset < p.replayUntil && !p.pastReplay)
				p.offset += int64(len(buf))
				p.latchReplay()
			}
			return
		}
		segStart := p.offset
		seg := bytes.TrimSpace(buf[:nl])
		buf = buf[nl+1:]
		if len(seg) == 0 {
			p.offset++ // empty line
			continue
		}
		if ev, ok := parseEvent(seg); ok {
			p.deliver(ev, segStart < p.replayUntil && !p.pastReplay)
		} else {
			p.logf("prox: skipping malformed terminated line")
		}
		p.offset += int64(len(seg)) + 1
	}
	p.latchReplay()
}

// latchReplay ends replay mode once the offset has passed the startup
// snapshot; mid-session re-baselines (file replaced) then stay live.
func (p *Poller) latchReplay() {
	if p.replayUntil > 0 && p.offset >= p.replayUntil {
		p.pastReplay = true
	}
}

func (p *Poller) deliver(ev Event, replay bool) {
	if ev.ID == "" || p.seen[ev.ID] {
		return // dedupe on event id
	}
	p.seen[ev.ID] = true
	p.handle(ev, replay)
}

func parseEvent(seg []byte) (Event, bool) {
	if len(seg) == 0 || seg[0] != '{' {
		return Event{}, false
	}
	var ev Event
	if err := json.Unmarshal(seg, &ev); err != nil || ev.Type == "" {
		return Event{}, false
	}
	return ev, true
}

func devIno(st os.FileInfo) (uint64, uint64) {
	s := st.Sys().(*syscall.Stat_t)
	return s.Dev, s.Ino
}
