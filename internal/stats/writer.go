package stats

import (
	"context"
	"errors"
	"hash"
	"io"
	"net/http"
	"strconv"
)

// ErrCancelled is returned from Write once the operator cancels a transfer.
// Returning an error makes io.Copy (and zip writers) stop immediately.
var ErrCancelled = errors.New("transfer cancelled by server operator")

// ResponseRecorder wraps every response to capture status and byte count for
// the request log and the global throughput counter.
type ResponseRecorder struct {
	http.ResponseWriter
	store  *Store
	Status int
	Bytes  int64
}

// NewRecorder wraps w; Status defaults to 200 for handlers that never call
// WriteHeader explicitly.
func NewRecorder(w http.ResponseWriter, s *Store) *ResponseRecorder {
	return &ResponseRecorder{ResponseWriter: w, store: s, Status: http.StatusOK}
}

// WriteHeader records the status code.
func (r *ResponseRecorder) WriteHeader(code int) {
	r.Status = code
	r.ResponseWriter.WriteHeader(code)
}

// Write counts bytes as they reach the connection's buffer.
func (r *ResponseRecorder) Write(p []byte) (int, error) {
	n, err := r.ResponseWriter.Write(p)
	r.Bytes += int64(n)
	r.store.AddBytes(n)
	return n, err
}

// Flush forwards to the underlying writer (needed for SSE and streamed ZIPs).
func (r *ResponseRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the real writer.
func (r *ResponseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// CountingWriter attributes written bytes to one Transfer. It deliberately
// does not implement io.ReaderFrom: that would let net/http use sendfile(2)
// and bypass Write, making per-connection throughput invisible.
type CountingWriter struct {
	http.ResponseWriter
	T           *Transfer
	Ctx         context.Context // the request's context: ends a pause or a wait when the client leaves
	Hash        hash.Hash       // optional: every byte sent is also hashed (for delivery receipts)
	wroteHeader bool
	Status      int
}

// NewCountingWriter wraps w so every Write is credited to t.
func NewCountingWriter(w http.ResponseWriter, t *Transfer) *CountingWriter {
	return &CountingWriter{ResponseWriter: w, T: t, Status: http.StatusOK}
}

// WriteHeader captures the status and, if the handler set Content-Length
// (http.ServeContent does, accounting for Range), the transfer's total.
func (w *CountingWriter) WriteHeader(code int) {
	// Only the first WriteHeader counts; net/http ignores later ones too.
	if !w.wroteHeader {
		w.wroteHeader = true
		w.Status = code
		if cl, err := strconv.ParseInt(w.Header().Get("Content-Length"), 10, 64); err == nil {
			w.T.SetTotal(cl)
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

// Write sends p and credits the bytes, failing fast if cancelled.
func (w *CountingWriter) Write(p []byte) (int, error) {
	// Check before writing so a cancel takes effect within one buffer.
	if w.T.Cancelled() {
		return 0, ErrCancelled
	}
	// Implicit 200: make sure Content-Length is picked up as the total.
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	// Fast path: nothing is holding this transfer back.
	if !w.T.limited() {
		n, err := w.ResponseWriter.Write(p)
		w.T.Add(n)
		w.hash(p[:n])
		return n, err
	}
	// Paused or capped: send in small paced pieces so the rate is smooth
	// and a pause takes effect promptly.
	written := 0
	for written < len(p) {
		size := len(p) - written
		if rate := w.T.slowest(); rate > 0 {
			size = min(size, chunk(rate))
		}
		if err := w.T.pace(w.Ctx, size); err != nil {
			return written, err
		}
		n, err := w.ResponseWriter.Write(p[written : written+size])
		w.T.Add(n)
		w.hash(p[written : written+n])
		written += n
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

// hash feeds bytes that reached the client into the optional hash.
func (w *CountingWriter) hash(b []byte) {
	if w.Hash != nil {
		w.Hash.Write(b)
	}
}

// Flush forwards to the underlying writer.
func (w *CountingWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the real writer.
func (w *CountingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// CountingReader credits bytes read from a request body (an upload) to a
// transfer and to the store's received total, failing fast if cancelled.
type CountingReader struct {
	R     io.Reader
	T     *Transfer
	Store *Store
}

// Read reads from the body and counts what arrived.
func (r *CountingReader) Read(p []byte) (int, error) {
	// A cancel from the dashboard stops the upload at the next read.
	if r.T.Cancelled() {
		return 0, ErrCancelled
	}
	// Paused or capped from the dashboard: read in paced pieces.
	if r.T.limited() {
		if rate := r.T.slowest(); rate > 0 {
			p = p[:min(len(p), chunk(rate))]
		}
		if err := r.T.pace(context.Background(), len(p)); err != nil {
			return 0, err
		}
	}
	n, err := r.R.Read(p)
	r.T.Add(n)
	r.Store.AddReceived(n)
	return n, err
}
