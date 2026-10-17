package main

import (
	"context"
	"sync"
	"time"
)

// sessions turns the stream of things that happen in each conversation
// into agent turns. kojo refuses a second concurrent turn for a session
// key, and a busy channel can produce a burst of messages and reactions
// within a second, so events are not forwarded one by one:
//
//   - everything that arrives within window of the first event is
//     collected into one batch;
//   - a batch for an idle conversation becomes one turn;
//   - a batch for a conversation whose turn is still running is steered
//     into that turn as one follow-up, or — when that is not possible —
//     queued, and the whole queue becomes the next single turn.
type sessions struct {
	window time.Duration
	limit  int // events held per conversation, waiting and queued

	// run executes one turn and returns when it has ended.
	run func(ctx context.Context, key string, batch []*incoming)
	// steer hands a batch to the running turn. nil means it was taken
	// (or may have been: the batch must not be run again).
	steer func(key string, batch []*incoming) error

	mu sync.Mutex
	m  map[string]*chatSession
}

// chatSession is the state of one conversation. It stays in the map
// while a turn runs, a timer is armed, a steer is in flight, or events
// are held.
type chatSession struct {
	cancel context.CancelFunc // non-nil while a turn runs
	// userStop cancels the running turn as a `!stop` (kojo is told the
	// user stopped it).
	userStop context.CancelFunc
	pending  []*incoming // collecting until the timer fires
	queue    []*incoming // waiting for the running turn to end
	timer    *time.Timer
	// timerGen tells a timer callback that lost the race against a stop
	// that it is no longer the armed one.
	timerGen int
	// steering is how many events are on their way into the running
	// turn. One batch is steered at a time so that batches the turn
	// refuses queue in the order they arrived.
	steering int
	// due: the window of pending closed while a steer was in flight;
	// it is flushed the moment that steer returns.
	due bool
	// stops counts stop calls; a batch whose steer was in flight across
	// one is dropped instead of being run.
	stops int
	// stopping is set by stop until the cancelled turn has ended.
	// Batches that arrive meanwhile queue instead of being steered into
	// a turn that is on its way out and would swallow them.
	stopping bool
}

func newSessions(window time.Duration, limit int) *sessions {
	return &sessions{window: window, limit: limit, m: map[string]*chatSession{}}
}

// add files an event under its conversation. It reports false when the
// conversation already holds as many events as it may.
func (s *sessions) add(key string, in *incoming) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.m[key]
	if sess == nil {
		sess = &chatSession{}
		s.m[key] = sess
	}
	if len(sess.pending)+len(sess.queue)+sess.steering >= s.limit {
		return false
	}
	sess.pending = append(sess.pending, in)
	if sess.timer == nil && !sess.due {
		s.armLocked(key, sess, s.window)
	}
	return true
}

func (s *sessions) armLocked(key string, sess *chatSession, after time.Duration) {
	sess.timerGen++
	gen := sess.timerGen
	sess.timer = time.AfterFunc(after, func() { s.flush(key, sess, gen) })
}

// releaseLocked forgets a conversation nothing is happening in.
func (s *sessions) releaseLocked(key string, sess *chatSession) {
	if sess.cancel == nil && sess.timer == nil && sess.steering == 0 &&
		len(sess.pending) == 0 && len(sess.queue) == 0 && s.m[key] == sess {
		delete(s.m, key)
	}
}

// stop cancels the conversation's turn and drops everything held for
// it. It reports whether there was anything to stop.
func (s *sessions) stop(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.m[key]
	if sess == nil {
		return false
	}
	had := sess.cancel != nil || len(sess.pending)+len(sess.queue)+sess.steering > 0
	sess.pending, sess.queue = nil, nil
	sess.stops++
	sess.due = false
	if sess.timer != nil {
		sess.timer.Stop()
		sess.timer = nil
		sess.timerGen++
	}
	if sess.cancel != nil {
		sess.userStop()
		sess.stopping = true
	}
	s.releaseLocked(key, sess)
	return had
}

// beginLocked marks a turn as running. The cancel is installed in the
// same critical section that takes the batch, so a stop always finds
// either the events still held or a turn it can cancel.
func (s *sessions) beginLocked(sess *chatSession) context.Context {
	ctx, cancel := context.WithCancelCause(context.Background())
	sess.cancel = func() { cancel(nil) }
	sess.userStop = func() { cancel(errStoppedByUser) }
	return ctx
}

// flush runs when a conversation's collection window closes.
func (s *sessions) flush(key string, sess *chatSession, gen int) {
	s.mu.Lock()
	if sess.timerGen != gen {
		s.mu.Unlock()
		return
	}
	sess.timer = nil
	if sess.steering > 0 {
		// Wait for the steer in flight; it flushes again when it is done.
		sess.due = true
		s.mu.Unlock()
		return
	}
	batch := sess.pending
	sess.pending = nil
	switch {
	case len(batch) == 0:
		s.releaseLocked(key, sess)
		s.mu.Unlock()
		return
	case sess.cancel == nil:
		ctx := s.beginLocked(sess)
		s.mu.Unlock()
		s.loop(ctx, key, sess, batch)
		return
	case sess.stopping || len(sess.queue) > 0:
		// Behind a batch the turn already refused, a steer would jump
		// the queue.
		sess.queue = append(sess.queue, batch...)
		s.mu.Unlock()
		return
	}
	sess.steering = len(batch)
	stops := sess.stops
	s.mu.Unlock()

	err := s.steer(key, batch)

	s.mu.Lock()
	sess.steering = 0
	if sess.due {
		sess.due = false
		s.armLocked(key, sess, 0)
	}
	switch {
	case err == nil || sess.stops != stops:
		// Taken by the turn, or stopped along with it.
		s.releaseLocked(key, sess)
		s.mu.Unlock()
	case sess.cancel != nil:
		sess.queue = append(sess.queue, batch...)
		s.mu.Unlock()
	default:
		// The turn ended before it could take the batch.
		ctx := s.beginLocked(sess)
		s.mu.Unlock()
		s.loop(ctx, key, sess, batch)
	}
}

// loop runs batch as a turn, then whatever queued up behind it as the
// next one, until nothing is queued.
func (s *sessions) loop(ctx context.Context, key string, sess *chatSession, batch []*incoming) {
	for batch != nil {
		s.run(ctx, key, batch)

		s.mu.Lock()
		sess.cancel()
		sess.cancel = nil
		sess.stopping = false
		if len(sess.queue) > 0 {
			batch = sess.queue
			sess.queue = nil
			ctx = s.beginLocked(sess)
		} else {
			batch = nil
			s.releaseLocked(key, sess)
		}
		s.mu.Unlock()
	}
}
