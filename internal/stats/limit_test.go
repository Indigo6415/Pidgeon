package stats

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLimiterPacesWrites(t *testing.T) {
	st := New()
	tr := st.Begin(TransferInfo{Client: "10.0.0.9", Name: "/x", Total: 64 << 10})
	if !st.SetTransferLimit(tr.ID, 64<<10) {
		t.Fatal("limit not set on an active transfer")
	}
	cw := NewCountingWriter(httptest.NewRecorder(), tr)
	start := time.Now()
	// 16 KiB of burst, then 48 KiB at 64 KiB/s: about 0.75s.
	if _, err := cw.Write(make([]byte, 64<<10)); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el < 600*time.Millisecond || el > 2*time.Second {
		t.Fatalf("64 KiB at 64 KiB/s took %v", el)
	}
	if tr.Sent() != 64<<10 {
		t.Fatalf("sent %d", tr.Sent())
	}
}

func TestClientAndGlobalCapsApply(t *testing.T) {
	st := New()
	st.SetClientLimit(1 << 20)
	tr := st.Begin(TransferInfo{Client: "10.0.0.9", Name: "/x"})
	if got := st.EffectiveLimit(tr.view()); got != 1<<20 {
		t.Fatalf("effective limit %d", got)
	}
	st.SetLimit(256 << 10)
	if got := st.EffectiveLimit(tr.view()); got != 256<<10 {
		t.Fatalf("the global cap should win when lower: %d", got)
	}
	up := st.Begin(TransferInfo{Client: "10.0.0.9", Name: "/u", Kind: KindUpload})
	if up.limited() {
		t.Fatal("uploads aren't subject to the download caps")
	}
	snap := st.Snapshot()
	if snap.Limit != 256<<10 || snap.ClientLimit != 1<<20 {
		t.Fatalf("snapshot caps %d / %d", snap.Limit, snap.ClientLimit)
	}
}

func TestPauseHoldsWritesUntilResumeOrCancel(t *testing.T) {
	st := New()
	tr := st.Begin(TransferInfo{Client: "10.0.0.9", Name: "/x"})
	st.Pause(tr.ID)
	cw := NewCountingWriter(httptest.NewRecorder(), tr)
	done := make(chan error, 1)
	go func() { _, err := cw.Write([]byte("hello")); done <- err }()
	select {
	case <-done:
		t.Fatal("a paused transfer wrote")
	case <-time.After(250 * time.Millisecond):
	}
	if v := st.Snapshot().Transfers[0]; !v.Paused {
		t.Fatal("snapshot doesn't show the pause")
	}
	st.Resume(tr.ID)
	if err := <-done; err != nil || tr.Sent() != 5 {
		t.Fatalf("after resume: err %v, sent %d", err, tr.Sent())
	}

	// A cancel ends a pause; so does the client going away.
	st.Pause(tr.ID)
	go func() { _, err := cw.Write([]byte("x")); done <- err }()
	st.Cancel(tr.ID)
	if err := <-done; err != ErrCancelled {
		t.Fatalf("cancel while paused: %v", err)
	}
	tr2 := st.Begin(TransferInfo{Client: "10.0.0.9", Name: "/y"})
	st.Pause(tr2.ID)
	ctx, cancel := context.WithCancel(context.Background())
	cw2 := NewCountingWriter(httptest.NewRecorder(), tr2)
	cw2.Ctx = ctx
	go func() { _, err := cw2.Write([]byte("x")); done <- err }()
	cancel()
	if err := <-done; err == nil {
		t.Fatal("a paused write should end when the client leaves")
	}
}
