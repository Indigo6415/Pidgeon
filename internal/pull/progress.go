package pull

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/term"

	"github.com/Indigo6415/pidgeon/internal/humanize"
)

// progress shows one live line per running download plus a summary line on
// a terminal, redrawn in place; elsewhere it prints one line per finished
// file. Finished files always leave a permanent line above the live block.
type progress struct {
	mu    sync.Mutex
	w     io.Writer
	tty   bool
	width int
	drawn int // lines of the live block currently on screen
	tasks []*task
	start time.Time

	filesDone, filesTotal atomic.Int64
	bytesDone, bytesTotal atomic.Int64
	listing               atomic.Bool // still discovering the tree
}

// task is one file being downloaded.
type task struct {
	name    string
	size    int64
	done    atomic.Int64
	started time.Time
}

// maxLive bounds the live lines (the rest are summarised).
const maxLive = 6

// newProgress writes to w, live when w is a terminal.
func newProgress(w io.Writer) *progress {
	p := &progress{w: w, start: time.Now(), width: 100}
	if f, ok := w.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		p.tty = true
		if cols, _, err := term.GetSize(int(f.Fd())); err == nil && cols > 20 {
			p.width = cols
		}
	}
	p.listing.Store(true)
	return p
}

// run redraws the live block a few times a second until ctx ends.
func (p *progress) run(ctx context.Context) {
	if !p.tty {
		return
	}
	tk := time.NewTicker(150 * time.Millisecond)
	defer tk.Stop()
	// Redraw until the transfer is over.
	for {
		select {
		case <-ctx.Done():
			p.mu.Lock()
			p.clear()
			p.mu.Unlock()
			return
		case <-tk.C:
			p.mu.Lock()
			p.redraw()
			p.mu.Unlock()
		}
	}
}

// begin registers a running download.
func (p *progress) begin(name string, size int64) *task {
	t := &task{name: name, size: size, started: time.Now()}
	p.mu.Lock()
	p.tasks = append(p.tasks, t)
	p.mu.Unlock()
	return t
}

// end removes a download from the live block.
func (p *progress) end(t *task) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, x := range p.tasks {
		if x == t {
			p.tasks = append(p.tasks[:i], p.tasks[i+1:]...)
			break
		}
	}
}

// add counts bytes received for t.
func (p *progress) add(t *task, n int) {
	t.done.Add(int64(n))
	p.bytesDone.Add(int64(n))
}

// log prints a permanent line above the live block.
func (p *progress) log(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clear()
	fmt.Fprintf(p.w, format+"\n", args...)
	if p.tty {
		p.redraw()
	}
}

// clear erases the live block. Callers hold mu.
func (p *progress) clear() {
	if p.drawn == 0 {
		return
	}
	// Move up over the block, clearing each line.
	fmt.Fprintf(p.w, "\x1b[%dA", p.drawn)
	for range p.drawn {
		fmt.Fprint(p.w, "\x1b[2K\n")
	}
	fmt.Fprintf(p.w, "\x1b[%dA", p.drawn)
	p.drawn = 0
}

// redraw replaces the live block. Callers hold mu.
func (p *progress) redraw() {
	p.clear()
	var lines []string
	for i, t := range p.tasks {
		if i == maxLive {
			lines = append(lines, fmt.Sprintf("  … and %d more", len(p.tasks)-maxLive))
			break
		}
		done := t.done.Load()
		pct := "   "
		if t.size > 0 {
			pct = fmt.Sprintf("%3d%%", done*100/t.size)
		}
		rate := float64(done) / max(0.001, time.Since(t.started).Seconds())
		tail := fmt.Sprintf("  %s  %s", pct, humanize.Rate(rate))
		lines = append(lines, "  "+fitLeft(t.name, p.width-len(tail)-3)+tail)
	}
	lines = append(lines, p.summary())
	for _, l := range lines {
		fmt.Fprintln(p.w, fit(l, p.width-1))
	}
	p.drawn = len(lines)
}

// summary is the totals line.
func (p *progress) summary() string {
	total := fmt.Sprint(p.filesTotal.Load())
	if p.listing.Load() {
		total += "+"
	}
	el := time.Since(p.start).Seconds()
	return fmt.Sprintf("  %d/%s files · %s of %s · %s",
		p.filesDone.Load(), total, humanize.Bytes(p.bytesDone.Load()), humanize.Bytes(p.bytesTotal.Load()),
		humanize.Rate(float64(p.bytesDone.Load())/max(0.001, el)))
}

// fit cuts s to at most n runes.
func fit(s string, n int) string {
	r := []rune(s)
	if n <= 1 || len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// fitLeft cuts the start of s (keeping the file name) and pads it to n.
func fitLeft(s string, n int) string {
	r := []rune(s)
	if n < 4 {
		return ""
	}
	if len(r) > n {
		r = append([]rune("…"), r[len(r)-n+1:]...)
	}
	return string(r) + strings.Repeat(" ", n-len(r))
}
