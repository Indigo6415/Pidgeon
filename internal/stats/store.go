// Package stats is the central, concurrency-safe store that HTTP handlers
// report into and the TUI / web progress feed read from.
//
// Hot-path counters (bytes written) are atomics so the counting writer never
// takes a lock per Write. Everything else (transfer registry, history, request
// log) is guarded by a single mutex that is only touched once per request and
// a few times per second by the sampler.
package stats

import (
	"context"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// HistorySeconds is how many 1-second throughput buckets are kept.
	HistorySeconds = 60
	// logSize is how many recent requests are retained.
	logSize = 200
	// finishedTTL is how long finished transfers stay visible (TUI + web).
	finishedTTL = 60 * time.Second
	// sampleEvery is the sampler period that drives per-transfer rates.
	sampleEvery = 250 * time.Millisecond
)

// Kind is what a transfer is delivering.
type Kind uint8

const (
	KindFile Kind = iota
	KindZip
	// KindUpload is a file arriving in the inbox: bytes flow from the
	// client to this machine, the opposite direction of the others.
	KindUpload
)

// State is the lifecycle of a transfer.
type State uint32

const (
	StateActive State = iota
	StateDone
	StateAborted
)

// String returns the wire / display name of the state.
func (s State) String() string {
	switch s {
	case StateActive:
		return "active"
	case StateDone:
		return "done"
	}
	return "aborted"
}

// TransferInfo describes a transfer when it begins.
type TransferInfo struct {
	DLID     string // client-generated download ID (web UI progress), may be ""
	Listener string // listener ID the request arrived through
	Client   string // remote IP
	Name     string // display name (path or zip file name)
	Kind     Kind
	Total    int64  // expected bytes, -1 if unknown
	Inline   bool   // opened for viewing (preview), not saved: not counted as a download
	Share    string // share link ID when the request came through /s/, else ""
}

// Transfer is one in-flight (or recently finished) download. Byte counts are
// atomics because the handler goroutine writes while the sampler reads.
type Transfer struct {
	TransferInfo
	ID      uint64
	Started time.Time

	total     atomic.Int64
	sent      atomic.Int64
	rateBits  atomic.Uint64 // math.Float64bits of bytes/sec (EWMA)
	state     atomic.Uint32
	cancelled atomic.Bool
	ended     atomic.Int64 // UnixNano

	lastSent int64 // sampler-only
}

// Add records n bytes written to the client.
func (t *Transfer) Add(n int) { t.sent.Add(int64(n)) }

// SetTotal records the expected size once it is known (e.g. Content-Length).
func (t *Transfer) SetTotal(n int64) { t.total.Store(n) }

// Sent returns the bytes delivered so far.
func (t *Transfer) Sent() int64 { return t.sent.Load() }

// Total returns the expected size, or -1 if unknown.
func (t *Transfer) Total() int64 { return t.total.Load() }

// Cancelled reports whether the operator asked to kill this transfer.
func (t *Transfer) Cancelled() bool { return t.cancelled.Load() }

// View is an immutable copy of a transfer for rendering.
type View struct {
	TransferInfo
	ID      uint64
	Started time.Time
	Ended   time.Time
	Total   int64
	Sent    int64
	Rate    float64
	State   State
}

// Progress returns 0..1, or -1 if the total is unknown.
func (v View) Progress() float64 {
	if v.Total <= 0 {
		return -1
	}
	return math.Min(1, float64(v.Sent)/float64(v.Total))
}

// view snapshots t.
func (t *Transfer) view() View {
	v := View{
		TransferInfo: t.TransferInfo,
		ID:           t.ID,
		Started:      t.Started,
		Total:        t.total.Load(),
		Sent:         t.sent.Load(),
		Rate:         math.Float64frombits(t.rateBits.Load()),
		State:        State(t.state.Load()),
	}
	if e := t.ended.Load(); e != 0 {
		v.Ended = time.Unix(0, e)
	}
	return v
}

// LogEntry is one completed HTTP request.
type LogEntry struct {
	Time     time.Time
	Client   string
	Method   string
	Path     string
	Status   int
	Bytes    int64
	Duration time.Duration
	Listener string
}

// Snapshot is everything the dashboard needs, copied under one lock.
type Snapshot struct {
	Started     time.Time
	Uptime      time.Duration
	Requests    int64
	Downloads   int64 // completed transfers
	Uploads     int64 // completed uploads into the inbox
	BytesServed int64
	BytesRecv   int64 // bytes uploaded into the inbox
	OpenConns   int64
	PeakConns   int64
	Rate        float64   // aggregate bytes/sec right now (smoothed)
	PeakRate    float64   // highest 1-second bucket seen
	History     []float64 // bytes/sec per second, oldest first, len HistorySeconds
	Transfers   []View    // active first (oldest first), then recently finished
	Log         []LogEntry
	Hottest     string
	HottestHits int64
	// ActiveByListener counts active transfers per listener ID.
	ActiveByListener map[string]int
}

// Store aggregates all server activity.
type Store struct {
	started time.Time

	requests    atomic.Int64
	downloads   atomic.Int64
	uploads     atomic.Int64
	bytesServed atomic.Int64
	bytesRecv   atomic.Int64
	openConns   atomic.Int64
	peakConns   atomic.Int64
	nextID      atomic.Uint64

	mu        sync.Mutex
	transfers map[uint64]*Transfer
	hits      map[string]int64
	log       []LogEntry // ring buffer
	logHead   int        // next write position
	logLen    int
	history   [HistorySeconds]float64 // ring of per-second throughput
	histHead  int
	peakRate  float64
	rate      float64 // smoothed aggregate rate

	// sampler-only bookkeeping
	lastBytes  int64
	bucket     int64
	bucketFrom time.Time

	onLog func(LogEntry) // optional; headless mode prints requests
}

// New creates an empty store. Call Run to start the rate sampler.
func New() *Store {
	now := time.Now()
	return &Store{
		started:    now,
		transfers:  map[uint64]*Transfer{},
		hits:       map[string]int64{},
		log:        make([]LogEntry, logSize),
		bucketFrom: now,
	}
}

// OnLog registers a callback invoked (synchronously) for each logged request.
// It must be set before the server starts handling requests.
func (s *Store) OnLog(fn func(LogEntry)) { s.onLog = fn }

// ConnOpened implements listeners.ConnHooks.
func (s *Store) ConnOpened() {
	n := s.openConns.Add(1)
	// Lock-free max: retry until we either lose to a bigger value or win.
	for {
		peak := s.peakConns.Load()
		if n <= peak || s.peakConns.CompareAndSwap(peak, n) {
			return
		}
	}
}

// ConnClosed implements listeners.ConnHooks.
func (s *Store) ConnClosed() { s.openConns.Add(-1) }

// CountRequest increments the request counter.
func (s *Store) CountRequest() { s.requests.Add(1) }

// AddBytes records bytes written to any client (all responses, not only
// tracked transfers), feeding the aggregate rate and total.
func (s *Store) AddBytes(n int) { s.bytesServed.Add(int64(n)) }

// AddReceived records bytes uploaded by a client into the inbox.
func (s *Store) AddReceived(n int) { s.bytesRecv.Add(int64(n)) }

// BytesServed returns the total bytes sent to clients so far.
func (s *Store) BytesServed() int64 { return s.bytesServed.Load() }

// Begin registers a new transfer.
func (s *Store) Begin(info TransferInfo) *Transfer {
	t := &Transfer{TransferInfo: info, ID: s.nextID.Add(1), Started: time.Now()}
	t.total.Store(info.Total)
	s.mu.Lock()
	s.transfers[t.ID] = t
	s.mu.Unlock()
	return t
}

// Outcome is how a transfer ended.
type Outcome int

const (
	// Completed: every byte of the file/archive was delivered. Counts as a
	// download in the totals and the "hottest file" stat.
	Completed Outcome = iota
	// PartialDone: a Range request (e.g. video seeking) finished, but it
	// didn't deliver the whole file, so it isn't counted as a download.
	PartialDone
	// Aborted: the client went away, the operator cancelled, or an error hit.
	Aborted
	// Discarded: not a real transfer (304 Not Modified, 404, ...); drop it.
	Discarded
)

// End marks t finished with the given outcome.
func (s *Store) End(t *Transfer, o Outcome) {
	// Non-transfers vanish without a trace in the dashboard.
	if o == Discarded {
		t.state.Store(uint32(StateAborted))
		s.mu.Lock()
		delete(s.transfers, t.ID)
		s.mu.Unlock()
		return
	}
	st := StateAborted
	if o == Completed || o == PartialDone {
		st = StateDone
	}
	// Only the first End wins; guards against double-reporting.
	if !t.state.CompareAndSwap(uint32(StateActive), uint32(st)) {
		return
	}
	t.ended.Store(time.Now().UnixNano())
	t.rateBits.Store(0)
	// Uploads have their own counter; they are not downloads.
	if o == Completed && t.Kind == KindUpload {
		s.uploads.Add(1)
		return
	}
	// Whole-file downloads feed the counters and the "hottest file" stat;
	// previews (opened inline) don't count as downloads.
	if o == Completed && !t.Inline {
		s.downloads.Add(1)
		s.mu.Lock()
		s.hits[t.Name]++
		s.mu.Unlock()
	}
}

// Cancel asks an active transfer to stop; its writer fails the next Write.
func (s *Store) Cancel(id uint64) bool {
	s.mu.Lock()
	t, ok := s.transfers[id]
	s.mu.Unlock()
	// Only active transfers can be cancelled.
	if !ok || State(t.state.Load()) != StateActive {
		return false
	}
	t.cancelled.Store(true)
	return true
}

// CancelClient cancels every active transfer of one client IP and returns
// how many were cancelled.
func (s *Store) CancelClient(ip string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	// Flag each of the client's active transfers; their writers stop next Write.
	for _, t := range s.transfers {
		if t.Client == ip && State(t.state.Load()) == StateActive {
			t.cancelled.Store(true)
			n++
		}
	}
	return n
}

// ActiveDownloads counts in-flight transfers to clients (uploads excluded).
func (s *Store) ActiveDownloads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	// Only outgoing transfers that haven't ended yet.
	for _, t := range s.transfers {
		if t.Kind != KindUpload && State(t.state.Load()) == StateActive {
			n++
		}
	}
	return n
}

// Log appends a request to the ring buffer.
func (s *Store) Log(e LogEntry) {
	s.mu.Lock()
	s.log[s.logHead] = e
	s.logHead = (s.logHead + 1) % logSize
	// Grow until full; afterwards the oldest entry is overwritten.
	if s.logLen < logSize {
		s.logLen++
	}
	s.mu.Unlock()
	if s.onLog != nil {
		s.onLog(e)
	}
}

// Run samples rates until ctx is cancelled. It must be running for rates,
// history and transfer expiry to update.
func (s *Store) Run(ctx context.Context) {
	tk := time.NewTicker(sampleEvery)
	defer tk.Stop()
	last := time.Now()
	// One sample per tick; dt is measured, not assumed, to stay accurate
	// when the process is briefly descheduled.
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tk.C:
			s.sample(now, now.Sub(last).Seconds())
			last = now
		}
	}
}

// sample updates every rate and rolls the per-second history.
func (s *Store) sample(now time.Time, dt float64) {
	// Ignore degenerate intervals (clock jumps, first tick).
	if dt <= 0 {
		return
	}
	const alpha = 0.35 // EWMA weight: smooth but still responsive at 4 Hz

	total := s.bytesServed.Load()
	delta := total - s.lastBytes
	s.lastBytes = total

	s.mu.Lock()
	defer s.mu.Unlock()

	inst := float64(delta) / dt
	s.rate = alpha*inst + (1-alpha)*s.rate
	// Snap tiny residuals to zero so the UI shows idle as 0 B/s.
	if s.rate < 1 {
		s.rate = 0
	}

	// Accumulate into the current 1-second bucket; flush whole seconds.
	s.bucket += delta
	if el := now.Sub(s.bucketFrom); el >= time.Second {
		v := float64(s.bucket) / el.Seconds()
		s.history[s.histHead] = v
		s.histHead = (s.histHead + 1) % HistorySeconds
		s.peakRate = math.Max(s.peakRate, v)
		s.bucket = 0
		s.bucketFrom = now
	}

	// Per-transfer rates; also expire finished transfers.
	for id, t := range s.transfers {
		if State(t.state.Load()) != StateActive {
			if now.Sub(time.Unix(0, t.ended.Load())) > finishedTTL {
				delete(s.transfers, id)
			}
			continue
		}
		sent := t.sent.Load()
		inst := float64(sent-t.lastSent) / dt
		t.lastSent = sent
		prev := math.Float64frombits(t.rateBits.Load())
		r := alpha*inst + (1-alpha)*prev
		// A brand-new transfer starts at its instantaneous rate.
		if prev == 0 {
			r = inst
		}
		t.rateBits.Store(math.Float64bits(r))
	}
}

// Snapshot copies the current state for rendering.
func (s *Store) Snapshot() Snapshot {
	now := time.Now()
	snap := Snapshot{
		Started:          s.started,
		Uptime:           now.Sub(s.started),
		Requests:         s.requests.Load(),
		Downloads:        s.downloads.Load(),
		Uploads:          s.uploads.Load(),
		BytesServed:      s.bytesServed.Load(),
		BytesRecv:        s.bytesRecv.Load(),
		OpenConns:        max(0, s.openConns.Load()),
		PeakConns:        s.peakConns.Load(),
		ActiveByListener: map[string]int{},
	}

	s.mu.Lock()
	snap.Rate = s.rate
	snap.PeakRate = s.peakRate
	snap.History = make([]float64, HistorySeconds)
	// Unroll the history ring so index 0 is the oldest second.
	for i := range HistorySeconds {
		snap.History[i] = s.history[(s.histHead+i)%HistorySeconds]
	}
	snap.Transfers = make([]View, 0, len(s.transfers))
	// Copy every transfer and count active ones per listener.
	for _, t := range s.transfers {
		v := t.view()
		snap.Transfers = append(snap.Transfers, v)
		if v.State == StateActive {
			snap.ActiveByListener[v.Listener]++
		}
	}
	snap.Log = make([]LogEntry, s.logLen)
	// Newest first: walk backwards from the write head.
	for i := range s.logLen {
		snap.Log[i] = s.log[(s.logHead-1-i+logSize)%logSize]
	}
	// The hottest file is the one with the most completed downloads.
	for name, n := range s.hits {
		if n > snap.HottestHits || (n == snap.HottestHits && name < snap.Hottest) {
			snap.Hottest, snap.HottestHits = name, n
		}
	}
	s.mu.Unlock()

	sortViews(snap.Transfers)
	return snap
}

// TransfersByDLPrefix returns transfers whose download ID starts with prefix
// (the web client's ID), for the browser's progress feed.
func (s *Store) TransfersByDLPrefix(prefix string) []View {
	// An empty prefix would match every client's downloads; refuse it.
	if prefix == "" {
		return nil
	}
	s.mu.Lock()
	var out []View
	// Filter to this client's downloads only.
	for _, t := range s.transfers {
		if len(t.DLID) > len(prefix) && t.DLID[:len(prefix)] == prefix {
			out = append(out, t.view())
		}
	}
	s.mu.Unlock()
	sortViews(out)
	return out
}

// sortViews orders active transfers first (oldest first), then finished ones
// (most recently finished first).
func sortViews(v []View) {
	sort.Slice(v, func(i, j int) bool {
		a, b := v[i], v[j]
		// Active transfers always come before finished ones.
		if (a.State == StateActive) != (b.State == StateActive) {
			return a.State == StateActive
		}
		if a.State == StateActive {
			return a.ID < b.ID
		}
		return a.Ended.After(b.Ended)
	})
}
