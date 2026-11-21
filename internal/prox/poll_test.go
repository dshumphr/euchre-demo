package prox

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPollerDeliversIncrementalAppends(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	rec := func(typ, id string) string {
		return "\n{\"v\":1,\"id\":\"" + id + "\",\"ts\":\"t\",\"src\":\"x\",\"type\":\"" + typ + "\",\"body\":{}}"
	}
	// start empty, like a stream that doesn't exist yet content-wise
	got := make(chan Event, 16)
	StartPoll(path, 0, func(ev Event, _ bool) { got <- ev }, nil)

	// stream appears with a first record (proxenos writes "\n"+JSON, no trailing newline)
	f.WriteString(rec("a", "1"))
	time.Sleep(400 * time.Millisecond)
	select {
	case ev := <-got:
		if ev.Type != "a" {
			t.Fatalf("got %s", ev.Type)
		}
	default:
		t.Fatal("first record not delivered")
	}

	// append more records over time
	f.WriteString(rec("b", "2"))
	time.Sleep(400 * time.Millisecond)
	select {
	case ev := <-got:
		if ev.Type != "b" {
			t.Fatalf("got %s", ev.Type)
		}
	default:
		t.Fatal("second record not delivered")
	}

	f.WriteString(rec("c", "3") + rec("d", "4"))
	time.Sleep(400 * time.Millisecond)
	for _, want := range []string{"c", "d"} {
		select {
		case ev := <-got:
			if ev.Type != want {
				t.Fatalf("got %s, want %s", ev.Type, want)
			}
		default:
			t.Fatalf("%s not delivered", want)
		}
	}
}

func TestPollerTornTailHeldThenDelivered(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	rec := func(typ, id string) string {
		return "\n{\"v\":1,\"id\":\"" + id + "\",\"ts\":\"t\",\"src\":\"x\",\"type\":\"" + typ + "\",\"body\":{}}"
	}
	got := make(chan Event, 16)
	StartPoll(path, 0, func(ev Event, _ bool) { got <- ev }, nil)

	// a complete-but-unterminated record is delivered in the steady state
	f.WriteString(rec("a", "1"))
	time.Sleep(400 * time.Millisecond)
	select {
	case ev := <-got:
		if ev.Type != "a" {
			t.Fatalf("got %s", ev.Type)
		}
	default:
		t.Fatal("unterminated complete record not delivered")
	}

	// a torn prefix is held: nothing delivered, offset not advanced past it
	f.WriteString("\n{\"v\":1,\"id\":\"2\",\"ts\":\"t\",\"src\":\"x\",\"type\":\"to")
	time.Sleep(400 * time.Millisecond)
	select {
	case ev := <-got:
		t.Fatalf("torn record delivered: %s", ev.Type)
	default:
	}

	// when the rest of the record lands, it is delivered exactly once
	// (file has `"type":"to`; completing it to `torry` needs no extra quote)
	f.WriteString("rry\",\"body\":{}}")
	time.Sleep(400 * time.Millisecond)
	select {
	case ev := <-got:
		if ev.Type != "torry" {
			t.Fatalf("got %s", ev.Type)
		}
	default:
		t.Fatal("completed record not delivered after torn tail resolved")
	}

	// a malformed terminated line is skipped silently
	f.WriteString("\nnot json at all" + rec("b", "5"))
	time.Sleep(400 * time.Millisecond)
	select {
	case ev := <-got:
		if ev.Type != "b" {
			t.Fatalf("got %s", ev.Type)
		}
	default:
		t.Fatal("record after malformed line not delivered")
	}
}

func TestPollerDedupesOnEventID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	rec := func(typ, id string) string {
		return "\n{\"v\":1,\"id\":\"" + id + "\",\"ts\":\"t\",\"src\":\"x\",\"type\":\"" + typ + "\",\"body\":{}}"
	}
	got := make(chan Event, 16)
	StartPoll(path, 0, func(ev Event, _ bool) { got <- ev }, nil)

	f.WriteString(rec("a", "same-id") + rec("a", "same-id"))
	time.Sleep(400 * time.Millisecond)
	select {
	case ev := <-got:
		_ = ev
	default:
		t.Fatal("first record not delivered")
	}
	select {
	case ev := <-got:
		t.Fatalf("duplicate delivered: %s", ev.Type)
	default:
	}
}

// The restart-replay regression: a poller started on an existing stream must
// flag events below the startup snapshot as replay, and events at/after it
// as live — the app replays old events for state only and never re-validates
// agent actions from previous runs. Mid-session re-baselines (file replaced)
// stay live: once past the snapshot, nothing is re-flagged.
func TestPollerFlagsPreStartEventsAsReplay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	rec := func(typ, id string) string {
		return "\n{\"v\":1,\"id\":\"" + id + "\",\"ts\":\"t\",\"src\":\"x\",\"type\":\"" + typ + "\",\"body\":{}}"
	}
	// A previous run's events (an old play and an old claim).
	old := rec("euchre.play", "old-play") + rec("euchre.claim_seat", "old-claim")
	f.WriteString(old)
	f.Sync()
	cutoff, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	type delivered struct {
		typ    string
		replay bool
	}
	got := make(chan delivered, 16)
	StartPoll(path, cutoff.Size(), func(ev Event, replay bool) { got <- delivered{ev.Type, replay} }, nil)

	time.Sleep(400 * time.Millisecond)
	for _, want := range []delivered{{"euchre.play", true}, {"euchre.claim_seat", true}} {
		select {
		case d := <-got:
			if d != want {
				t.Fatalf("pre-start event: got %+v, want %+v", d, want)
			}
		default:
			t.Fatalf("pre-start event %s not delivered", want.typ)
		}
	}

	// Live events at/after the snapshot are not flagged.
	f.WriteString(rec("euchre.play", "new-play"))
	time.Sleep(400 * time.Millisecond)
	select {
	case d := <-got:
		if d != (delivered{"euchre.play", false}) {
			t.Fatalf("post-start event flagged replay: %+v", d)
		}
	default:
		t.Fatal("post-start event not delivered")
	}

	// File replaced mid-session (re-baseline): everything in the new file is
	// live, replay mode must not resurrect.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(rec("euchre.bid", "fresh-1")+rec("euchre.bid", "fresh-2")), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	for _, want := range []delivered{{"euchre.bid", false}, {"euchre.bid", false}} {
		select {
		case d := <-got:
			if d != want {
				t.Fatalf("post-replacement event: got %+v, want %+v", d, want)
			}
		default:
			t.Fatalf("post-replacement event %s not delivered", want.typ)
		}
	}
}
