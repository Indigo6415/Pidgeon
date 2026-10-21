package stats

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Bandwidth control. Every byte of a download passes through
// CountingWriter.Write, so that is where downloads are paused and paced.
// Three token buckets can apply to one transfer: the global cap (--limit),
// the cap for the device it goes to (--client-limit), and a cap the
// operator set on that transfer alone. A write waits for all of them.

// Limiter is a token bucket: it lets through rate bytes per second on
// average, with a burst of a quarter of a second's worth. A rate of 0
// means unlimited. It is safe for concurrent use; concurrent writers share
// the rate roughly in the order they ask.
type Limiter struct {
	rate atomic.Int64 // bytes per second; 0 = unlimited

	mu     sync.Mutex
	tokens float64 // may go negative: a debt later writers wait out
	last   time.Time
}

// SetRate changes the rate (bytes/sec, 0 = unlimited). It takes effect on
// the next write.
func (l *Limiter) SetRate(bps int64) { l.rate.Store(max(0, bps)) }

// Rate returns the rate in bytes/sec, 0 when unlimited.
func (l *Limiter) Rate() int64 { return l.rate.Load() }

// reserve takes n bytes from the bucket and returns how long the caller
// must wait before sending them.
func (l *Limiter) reserve(n int) time.Duration {
	rate := float64(l.rate.Load())
	if rate <= 0 {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	burst := max(rate/4, 16<<10)
	// Refill for the time that passed, up to the burst size.
	if l.last.IsZero() {
		l.tokens = burst
	} else {
		l.tokens = min(burst, l.tokens+now.Sub(l.last).Seconds()*rate)
	}
	l.last = now
	l.tokens -= float64(n)
	if l.tokens >= 0 {
		return 0
	}
	return time.Duration(-l.tokens / rate * float64(time.Second))
}

// chunk is how much one paced write sends at a time: small enough that a
// slow rate still looks smooth, big enough not to waste syscalls.
func chunk(rate int64) int {
	return int(min(max(rate/20, 4<<10), 64<<10))
}

// pausePoll is how often a paused transfer checks whether it may go on.
const pausePoll = 100 * time.Millisecond

// pace blocks until n bytes of t may be sent: while the operator has the
// transfer paused, then for as long as its limiters require. It returns
// early with an error if the transfer is cancelled or ctx ends.
func (t *Transfer) pace(ctx context.Context, n int) error {
	done := ctxDone(ctx)
	// Paused: wait, checking now and then for resume or cancel.
	for t.paused.Load() {
		if t.cancelled.Load() {
			return ErrCancelled
		}
		select {
		case <-done:
			return ctx.Err()
		case <-time.After(pausePoll):
		}
	}
	var wait time.Duration
	// Every applicable bucket must allow the bytes; the slowest decides.
	for _, l := range t.limiters() {
		wait = max(wait, l.reserve(n))
	}
	// Sleep in short steps so a cancel or a raised limit is noticed.
	for wait > 0 {
		if t.cancelled.Load() {
			return ErrCancelled
		}
		step := min(wait, pausePoll)
		select {
		case <-done:
			return ctx.Err()
		case <-time.After(step):
		}
		wait -= step
	}
	return nil
}

// limiters returns the buckets that apply to t.
func (t *Transfer) limiters() []*Limiter {
	out := make([]*Limiter, 0, 3)
	if t.limit.Rate() > 0 {
		out = append(out, &t.limit)
	}
	// Uploads only answer to their own limit; the caps are about serving.
	if t.store == nil || t.Kind == KindUpload {
		return out
	}
	if t.store.global.Rate() > 0 {
		out = append(out, &t.store.global)
	}
	if c := t.client; c != nil && c.Rate() > 0 {
		out = append(out, c)
	}
	return out
}

// slowest returns the lowest rate among t's limiters, 0 if none applies.
func (t *Transfer) slowest() int64 {
	var r int64
	// The tightest cap decides the piece size.
	for _, l := range t.limiters() {
		if v := l.Rate(); r == 0 || v < r {
			r = v
		}
	}
	return r
}

// limited reports whether anything could hold t back right now.
func (t *Transfer) limited() bool {
	// Checked on every write, so it avoids building the limiter list.
	if t.paused.Load() || t.limit.Rate() > 0 {
		return true
	}
	if t.store == nil || t.Kind == KindUpload {
		return false
	}
	return t.store.global.Rate() > 0 || (t.client != nil && t.client.Rate() > 0)
}

// ctxDone is ctx.Done(), or nil (blocks forever) for a nil context.
func ctxDone(ctx context.Context) <-chan struct{} {
	if ctx == nil {
		return nil
	}
	return ctx.Done()
}

// EffectiveLimit is the tightest cap on a transfer (its own, its device's
// or the global one) in bytes/sec, 0 when nothing caps it.
func (s *Store) EffectiveLimit(v View) int64 {
	r := v.Limit
	if v.Kind == KindUpload {
		return r
	}
	// The lowest non-zero cap wins.
	for _, c := range []int64{s.global.Rate(), s.clientRate.Load()} {
		if c > 0 && (r == 0 || c < r) {
			r = c
		}
	}
	return r
}

// SetLimit caps the total rate of all downloads (bytes/sec, 0 = no cap).
func (s *Store) SetLimit(bps int64) { s.global.SetRate(bps) }

// Limit returns the cap on all downloads, 0 when there is none.
func (s *Store) Limit() int64 { return s.global.Rate() }

// SetClientLimit caps each device's downloads (bytes/sec, 0 = no cap).
func (s *Store) SetClientLimit(bps int64) {
	s.clientRate.Store(max(0, bps))
	s.mu.Lock()
	defer s.mu.Unlock()
	// Devices already seen switch to the new rate at once.
	for _, l := range s.clients {
		l.SetRate(bps)
	}
}

// ClientLimit returns the per-device cap, 0 when there is none.
func (s *Store) ClientLimit() int64 { return s.clientRate.Load() }

// clientLimiterLocked returns the shared bucket for one device. s.mu must be held.
func (s *Store) clientLimiterLocked(ip string) *Limiter {
	l := s.clients[ip]
	if l == nil {
		l = &Limiter{}
		l.SetRate(s.clientRate.Load())
		s.clients[ip] = l
	}
	return l
}

// Pause holds an active transfer: its writes block until Resume. The
// connection stays open, so the browser just sees the download stall.
func (s *Store) Pause(id uint64) bool { return s.setPaused(id, true) }

// Resume lets a paused transfer continue.
func (s *Store) Resume(id uint64) bool { return s.setPaused(id, false) }

// setPaused flips the pause flag of an active transfer.
func (s *Store) setPaused(id uint64, on bool) bool {
	s.mu.Lock()
	t, ok := s.transfers[id]
	s.mu.Unlock()
	// Only active transfers can be paused or resumed.
	if !ok || State(t.state.Load()) != StateActive {
		return false
	}
	t.paused.Store(on)
	return true
}

// SetTransferLimit caps one active transfer (bytes/sec, 0 = no own cap).
func (s *Store) SetTransferLimit(id uint64, bps int64) bool {
	s.mu.Lock()
	t, ok := s.transfers[id]
	s.mu.Unlock()
	if !ok || State(t.state.Load()) != StateActive {
		return false
	}
	t.limit.SetRate(bps)
	return true
}
