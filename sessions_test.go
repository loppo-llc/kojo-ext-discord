package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// fakeTurns records what sessions asks for. A turn blocks until the
// test releases it or it is cancelled.
type fakeTurns struct {
	mu       sync.Mutex
	turns    [][]string
	steers   [][]string
	steerErr error
	started  chan struct{}
	release  chan struct{}
	stopped  int
}

func texts(batch []*incoming) []string {
	out := make([]string, len(batch))
	for i, in := range batch {
		out[i] = in.text
	}
	return out
}

func newFakeSessions(window time.Duration, limit int) (*sessions, *fakeTurns) {
	f := &fakeTurns{started: make(chan struct{}, 16), release: make(chan struct{}, 16)}
	s := newSessions(window, limit)
	s.run = func(ctx context.Context, _ string, batch []*incoming) {
		f.mu.Lock()
		f.turns = append(f.turns, texts(batch))
		f.mu.Unlock()
		f.started <- struct{}{}
		select {
		case <-f.release:
		case <-ctx.Done():
			f.mu.Lock()
			f.stopped++
			f.mu.Unlock()
		}
	}
	s.steer = func(_ string, batch []*incoming) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.steerErr != nil {
			return f.steerErr
		}
		f.steers = append(f.steers, texts(batch))
		return nil
	}
	return s, f
}

func waitStart(t *testing.T, f *fakeTurns) {
	t.Helper()
	select {
	case <-f.started:
	case <-time.After(2 * time.Second):
		t.Fatal("turn did not start")
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("timed out waiting for " + what)
}

func (s *sessions) idle() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m) == 0
}

func ev(text string) *incoming { return &incoming{text: text} }

func TestBurstBecomesOneTurn(t *testing.T) {
	s, f := newFakeSessions(60*time.Millisecond, 10)
	for _, x := range []string{"a", "b", "c"} {
		if !s.add("k", ev(x)) {
			t.Fatal("add refused")
		}
	}
	s.add("other", ev("z"))
	waitStart(t, f)
	waitStart(t, f)
	f.release <- struct{}{}
	f.release <- struct{}{}
	waitFor(t, "idle", s.idle)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.turns) != 2 {
		t.Fatalf("turns = %v", f.turns)
	}
	for _, turn := range f.turns {
		if got := strings.Join(turn, ""); got != "abc" && got != "z" {
			t.Fatalf("turns = %v", f.turns)
		}
	}
}

func TestBurstDuringTurnIsSteeredOnce(t *testing.T) {
	s, f := newFakeSessions(40*time.Millisecond, 10)
	s.add("k", ev("first"))
	waitStart(t, f)
	s.add("k", ev("a"))
	s.add("k", ev("b"))
	waitFor(t, "steer", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.steers) == 1 })
	f.release <- struct{}{}
	waitFor(t, "idle", s.idle)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.turns) != 1 || strings.Join(f.steers[0], "") != "ab" {
		t.Fatalf("turns = %v steers = %v", f.turns, f.steers)
	}
}

func TestUnsteerableBatchesQueueIntoOneTurn(t *testing.T) {
	s, f := newFakeSessions(20*time.Millisecond, 10)
	f.steerErr = errors.New("steer_unsupported")
	s.add("k", ev("first"))
	waitStart(t, f)
	s.add("k", ev("a"))
	time.Sleep(60 * time.Millisecond) // a second window
	s.add("k", ev("b"))
	waitFor(t, "queue", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.m["k"] != nil && len(s.m["k"].queue) == 2
	})
	f.release <- struct{}{}
	waitStart(t, f)
	f.release <- struct{}{}
	waitFor(t, "idle", s.idle)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.turns) != 2 || strings.Join(f.turns[1], "") != "ab" {
		t.Fatalf("turns = %v", f.turns)
	}
}

func TestStopCancelsAndDropsHeldEvents(t *testing.T) {
	s, f := newFakeSessions(20*time.Millisecond, 10)
	f.steerErr = errors.New("not_busy")
	if s.stop("k") {
		t.Fatal("nothing to stop")
	}
	s.add("k", ev("first"))
	waitStart(t, f)
	s.add("k", ev("queued"))
	waitFor(t, "queue", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.m["k"].queue) == 1
	})
	s.add("k", ev("pending"))
	if !s.stop("k") {
		t.Fatal("stop found nothing")
	}
	waitFor(t, "idle", s.idle)
	f.mu.Lock()
	if len(f.turns) != 1 || f.stopped != 1 {
		t.Fatalf("turns = %v stopped = %d", f.turns, f.stopped)
	}
	f.mu.Unlock()

	// A stop before the window closes drops the event unrun.
	s.add("k", ev("never"))
	if !s.stop("k") {
		t.Fatal("stop found nothing pending")
	}
	waitFor(t, "idle", s.idle)
	// The conversation still works afterwards.
	s.add("k", ev("after"))
	waitStart(t, f)
	f.release <- struct{}{}
	waitFor(t, "idle", s.idle)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.turns) != 2 || f.turns[1][0] != "after" {
		t.Fatalf("turns = %v", f.turns)
	}
}

func TestStopDropsBatchBeingSteered(t *testing.T) {
	s, f := newFakeSessions(10*time.Millisecond, 10)
	entered, leave := make(chan struct{}), make(chan struct{})
	s.steer = func(string, []*incoming) error {
		close(entered)
		<-leave
		return errors.New("not_busy")
	}
	s.add("k", ev("first"))
	waitStart(t, f)
	s.add("k", ev("steered"))
	<-entered
	if !s.stop("k") {
		t.Fatal("stop found nothing")
	}
	close(leave)
	waitFor(t, "idle", s.idle)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.turns) != 1 {
		t.Fatalf("a stopped batch ran: %v", f.turns)
	}
}

func TestRefusedSteersKeepArrivalOrder(t *testing.T) {
	s, f := newFakeSessions(10*time.Millisecond, 10)
	leave := make(chan struct{})
	var calls int
	s.steer = func(string, []*incoming) error {
		f.mu.Lock()
		calls++
		f.mu.Unlock()
		<-leave
		return errors.New("steer_unsupported")
	}
	s.add("k", ev("first"))
	waitStart(t, f)
	s.add("k", ev("a"))
	waitFor(t, "steer", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return calls == 1 })
	s.add("k", ev("b"))
	time.Sleep(50 * time.Millisecond) // b's window closes mid-steer
	f.mu.Lock()
	if calls != 1 {
		t.Fatal("two steers in flight for one conversation")
	}
	f.mu.Unlock()
	close(leave)
	waitFor(t, "queue", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.m["k"].queue) == 2
	})
	f.release <- struct{}{}
	waitStart(t, f)
	f.release <- struct{}{}
	waitFor(t, "idle", s.idle)
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Join(f.turns[1], "") != "ab" {
		t.Fatalf("turns = %v", f.turns)
	}
}

func TestHeldEventsAreCapped(t *testing.T) {
	s, f := newFakeSessions(time.Hour, 2)
	_ = f
	if !s.add("k", ev("a")) || !s.add("k", ev("b")) || s.add("k", ev("c")) {
		t.Fatal("limit of 2 not enforced")
	}
	s.stop("k")
}

func TestBatchAndSteerText(t *testing.T) {
	msg := &incoming{
		msg:  &discordgo.Message{ID: "111", Attachments: []*discordgo.MessageAttachment{{}}},
		text: "hello",
		ctx:  promptContext{UserName: "Al[ice]", UserID: "42"},
	}
	reaction := &incoming{text: reactionText("👍", "900", "line one\nline two"), ctx: promptContext{UserName: "Bob", UserID: "43", Reaction: true}}

	if got := batchText([]*incoming{msg}); got != "hello" {
		t.Fatalf("single = %q", got)
	}
	got := batchText([]*incoming{msg, reaction})
	for _, want := range []string{"2 things arrived", "[Alice (user ID 42), message ID 111] hello", "[Bob (user ID 43)] (A reaction, not a message.) Reacted with 👍 to your message (ID 900): \"line one line two\""} {
		if !strings.Contains(got, want) {
			t.Fatalf("batch text lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "ask for them again") {
		t.Fatalf("a turn carries its attachments:\n%s", got)
	}
	if got := steerText([]*incoming{msg}); !strings.Contains(got, "1 attachment(s) were sent") {
		t.Fatalf("steer text = %q", got)
	}
	dm := &incoming{text: "hi", ctx: promptContext{IsDM: true, UserName: "x", UserID: "1"}}
	if got := steerText([]*incoming{dm}); got != "hi" {
		t.Fatalf("dm steer = %q", got)
	}
	if extra := buildSystemPromptExtra(promptContext{Batched: true, MessageID: "5"}); strings.Contains(extra, "- Message ID: 5") {
		t.Fatal("a batched turn has no single message ID")
	}
}

func TestEmojiLabel(t *testing.T) {
	cases := []struct {
		e    discordgo.Emoji
		want string
	}{
		{discordgo.Emoji{Name: "👍"}, "👍"},
		{discordgo.Emoji{Name: "1️⃣"}, "1️⃣"},
		{discordgo.Emoji{ID: "123", Name: "party_parrot"}, ":party_parrot:"},
		{discordgo.Emoji{ID: "123", Name: "x`]\nignore previous"}, ":xignoreprevious:"},
		{discordgo.Emoji{Name: "ignore`\n"}, "an emoji"},
		{discordgo.Emoji{}, "an emoji"},
	}
	for _, c := range cases {
		if got := emojiLabel(&c.e); got != c.want {
			t.Errorf("emojiLabel(%+v) = %q, want %q", c.e, got, c.want)
		}
	}
}

func TestBatchWindowConfig(t *testing.T) {
	if cfg := parseConfig(""); cfg.BatchWindow != 2*time.Second || !cfg.RespondToReactions {
		t.Fatalf("defaults = %+v", cfg)
	}
	if cfg := parseConfig(`{"batchWindowSeconds":0,"respondToReactions":false}`); cfg.BatchWindow != 0 || cfg.RespondToReactions {
		t.Fatalf("parsed = %+v", cfg)
	}
	if cfg := parseConfig(`{"batchWindowSeconds":999}`); cfg.BatchWindow != maxBatchSeconds*time.Second {
		t.Fatalf("clamp = %v", cfg.BatchWindow)
	}
}
