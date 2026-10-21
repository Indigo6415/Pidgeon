package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"image"
	_ "image/gif" // decoders, registered for image.Decode
	"image/jpeg"
	"image/png"
	"io"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/image/draw"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"

	"github.com/Indigo6415/pidgeon/internal/brand"
)

// Thumbnails for the grid view: GET /api/thumb?path=/a.jpg&w=256 returns
// the image scaled to fit a w×w box, turned upright per its EXIF
// orientation. Results are cached on disk (in the user cache directory,
// never in the served folder) and in memory, keyed by the file's path,
// size and modification time, so an edited photo gets a fresh thumbnail.
const (
	thumbMaxSource   = 96 << 20  // bigger files get the plain icon
	thumbMaxPixels   = 100 << 20 // decoded size limit (about 100 megapixels)
	thumbMinWidth    = 64
	thumbMaxWidth    = 512
	thumbMemBudget   = 24 << 20  // bytes of thumbnails kept in memory
	thumbDiskBudget  = 256 << 20 // the disk cache is trimmed to this at startup
	thumbExifWindow  = 128 << 10 // EXIF lives in the first 64 KB of a JPEG
	thumbJPEGQuality = 82
)

// thumbExts lists the formats thumbnails are made for.
var thumbExts = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true, ".bmp": true, ".tif": true, ".tiff": true}

// thumbCache generates and caches thumbnails. Generation is limited to a
// few at a time, and requests for the same thumbnail share one result.
type thumbCache struct {
	dir string        // disk cache directory; "" when there is none
	sem chan struct{} // slots for concurrent decodes

	mu       sync.Mutex
	mem      map[string]thumb
	memOrder []string
	memBytes int
	running  map[string]*thumbCall
}

// thumb is one encoded thumbnail.
type thumb struct {
	data  []byte
	ctype string
}

// thumbCall is a thumbnail being made; waiters block on done.
type thumbCall struct {
	done chan struct{}
	t    thumb
	err  error
}

// errNoThumb means the file can't be thumbnailed; the UI shows its icon.
var errNoThumb = errors.New("no thumbnail for this file")

// newThumbCache prepares the cache, using dir or else the user cache
// directory. Without a writable directory thumbnails are only kept in memory.
func newThumbCache(dir string) *thumbCache {
	c := &thumbCache{
		sem:     make(chan struct{}, min(4, max(1, runtime.NumCPU()/2))),
		mem:     map[string]thumb{},
		running: map[string]*thumbCall{},
	}
	if dir == "" {
		if base, err := os.UserCacheDir(); err == nil {
			dir = filepath.Join(base, brand.Command, "thumbs")
		}
	}
	if dir != "" && os.MkdirAll(dir, 0o700) == nil {
		c.dir = dir
		go trimDir(dir, thumbDiskBudget)
	}
	return c
}

// handleThumb serves one thumbnail.
func (s *Server) handleThumb(w http.ResponseWriter, r *http.Request) {
	if s.thumbs == nil {
		http.Error(w, "thumbnails are off", http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	name, err := s.resolve(q.Get("path"))
	if err != nil {
		httpError(w, err)
		return
	}
	if !thumbExts[strings.ToLower(path.Ext(name))] {
		http.Error(w, "not an image", http.StatusUnsupportedMediaType)
		return
	}
	width := thumbWidth(q.Get("w"))
	fi, err := s.stat(name)
	if err != nil {
		httpError(w, err)
		return
	}
	if !fi.Mode().IsRegular() || fi.Size() > thumbMaxSource {
		http.Error(w, "no thumbnail for this file", http.StatusUnsupportedMediaType)
		return
	}
	key := s.thumbKey(name, fi, width)
	etag := `"t` + key[:24] + `"`
	// The browser's copy is still right.
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	t, err := s.thumbs.get(r.Context(), key, func() (thumb, error) { return s.makeThumb(name, width) })
	if err != nil {
		if r.Context().Err() == nil {
			http.Error(w, "no thumbnail for this file", http.StatusUnsupportedMediaType)
		}
		return
	}
	h := w.Header()
	h.Set("Content-Type", t.ctype)
	h.Set("ETag", etag)
	// The URL carries the file's mtime, so a long private cache is safe.
	h.Set("Cache-Control", "private, max-age=604800")
	h.Set("Content-Length", strconv.Itoa(len(t.data)))
	w.Write(t.data)
}

// thumbWidth parses ?w=, rounded to a multiple of 64 within the limits so
// the cache isn't fragmented by odd sizes.
func thumbWidth(v string) int {
	n, err := strconv.Atoi(v)
	if err != nil {
		n = 256
	}
	n = (n + 63) / 64 * 64
	return min(thumbMaxWidth, max(thumbMinWidth, n))
}

// thumbKey identifies a thumbnail: which server tree, which version of the
// file, which size.
func (s *Server) thumbKey(name string, fi fs.FileInfo, width int) string {
	h := sha256.New()
	io.WriteString(h, strings.Join(s.root.Paths(), "\x00"))
	io.WriteString(h, "\x00"+hashKey(name, fi)+"\x00"+strconv.Itoa(width))
	return hex.EncodeToString(h.Sum(nil))
}

// get returns the cached thumbnail for key, or makes it with gen.
func (c *thumbCache) get(ctx context.Context, key string, gen func() (thumb, error)) (thumb, error) {
	c.mu.Lock()
	if t, ok := c.mem[key]; ok {
		c.mu.Unlock()
		return t, nil
	}
	call := c.running[key]
	if call == nil {
		call = &thumbCall{done: make(chan struct{})}
		c.running[key] = call
		go c.fill(key, call, gen)
	}
	c.mu.Unlock()
	// Wait for the shared result unless the client leaves first; the
	// thumbnail is still finished and cached for next time.
	select {
	case <-call.done:
		return call.t, call.err
	case <-ctx.Done():
		return thumb{}, ctx.Err()
	}
}

// fill makes (or loads from disk) one thumbnail and publishes it.
func (c *thumbCache) fill(key string, call *thumbCall, gen func() (thumb, error)) {
	defer close(call.done)
	t, ok := c.load(key)
	if !ok {
		c.sem <- struct{}{}
		t, call.err = gen()
		<-c.sem
		if call.err == nil {
			c.save(key, t)
		}
	}
	call.t = t
	c.mu.Lock()
	delete(c.running, key)
	if call.err == nil {
		c.rememberLocked(key, t)
	}
	c.mu.Unlock()
}

// load reads a thumbnail from the disk cache.
func (c *thumbCache) load(key string) (thumb, bool) {
	if c.dir == "" {
		return thumb{}, false
	}
	// Either format may have been stored.
	for _, f := range []struct{ ext, ctype string }{{".jpg", "image/jpeg"}, {".png", "image/png"}} {
		if data, err := os.ReadFile(filepath.Join(c.dir, key+f.ext)); err == nil {
			return thumb{data: data, ctype: f.ctype}, true
		}
	}
	return thumb{}, false
}

// save writes a thumbnail to the disk cache (atomically, via a rename).
func (c *thumbCache) save(key string, t thumb) {
	if c.dir == "" {
		return
	}
	ext := ".jpg"
	if t.ctype == "image/png" {
		ext = ".png"
	}
	tmp, err := os.CreateTemp(c.dir, "tmp-*")
	if err != nil {
		return
	}
	_, err = tmp.Write(t.data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), filepath.Join(c.dir, key+ext))
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
}

// rememberLocked keeps a thumbnail in memory, evicting the oldest ones
// beyond the memory budget.
func (c *thumbCache) rememberLocked(key string, t thumb) {
	if _, ok := c.mem[key]; ok {
		return
	}
	c.mem[key] = t
	c.memOrder = append(c.memOrder, key)
	c.memBytes += len(t.data)
	// Drop from the front until back under budget.
	for c.memBytes > thumbMemBudget && len(c.memOrder) > 1 {
		old := c.memOrder[0]
		c.memOrder = c.memOrder[1:]
		c.memBytes -= len(c.mem[old].data)
		delete(c.mem, old)
	}
}

// makeThumb decodes an image and scales it to fit a width×width box.
func (s *Server) makeThumb(name string, width int) (thumb, error) {
	f, _, err := s.openFile(name)
	if err != nil {
		return thumb{}, err
	}
	if f == nil {
		return thumb{}, errNoThumb
	}
	defer f.Close()

	// The orientation tag sits near the start of a JPEG.
	head := make([]byte, thumbExifWindow)
	n, _ := io.ReadFull(f, head)
	orientation := exifOrientation(head[:n])
	// Check the dimensions before committing memory to a decode.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return thumb{}, err
	}
	cfg, _, err := image.DecodeConfig(bufio.NewReader(f))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > thumbMaxPixels {
		return thumb{}, errNoThumb
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return thumb{}, err
	}
	img, _, err := image.Decode(bufio.NewReader(f))
	if err != nil {
		return thumb{}, errNoThumb
	}
	dst := orient(scaleToFit(img, width), orientation)

	var buf bytes.Buffer
	// Opaque images compress far better as JPEG; transparency needs PNG.
	if dst.Opaque() {
		err = jpeg.Encode(&buf, dst, &jpeg.Options{Quality: thumbJPEGQuality})
		return thumb{data: buf.Bytes(), ctype: "image/jpeg"}, err
	}
	err = (&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(&buf, dst)
	return thumb{data: buf.Bytes(), ctype: "image/png"}, err
}

// scaleToFit shrinks img to fit a box×box square, keeping its aspect
// ratio; small images keep their size. Big reductions take a quick pass to
// twice the target size first, then a sharp final pass.
func scaleToFit(img image.Image, box int) *image.RGBA {
	b := img.Bounds()
	scale := math.Min(1, float64(box)/float64(max(b.Dx(), b.Dy())))
	tw := max(1, int(math.Round(float64(b.Dx())*scale)))
	th := max(1, int(math.Round(float64(b.Dy())*scale)))
	if b.Dx() > 4*tw {
		mid := image.NewRGBA(image.Rect(0, 0, 2*tw, 2*th))
		draw.ApproxBiLinear.Scale(mid, mid.Bounds(), img, b, draw.Src, nil)
		img, b = mid, mid.Bounds()
	}
	dst := image.NewRGBA(image.Rect(0, 0, tw, th))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Src, nil)
	return dst
}

// orient turns an image upright according to its EXIF orientation (1-8).
func orient(src *image.RGBA, o int) *image.RGBA {
	if o < 2 || o > 8 {
		return src
	}
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	dw, dh := w, h
	// Orientations 5-8 swap width and height.
	if o >= 5 {
		dw, dh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	// For each output pixel, find the source pixel that lands there.
	for y := range dh {
		for x := range dw {
			var sx, sy int
			switch o {
			case 2:
				sx, sy = w-1-x, y
			case 3:
				sx, sy = w-1-x, h-1-y
			case 4:
				sx, sy = x, h-1-y
			case 5:
				sx, sy = y, x
			case 6:
				sx, sy = y, h-1-x
			case 7:
				sx, sy = w-1-y, h-1-x
			case 8:
				sx, sy = w-1-y, x
			}
			si, di := src.PixOffset(sx, sy), dst.PixOffset(x, y)
			copy(dst.Pix[di:di+4], src.Pix[si:si+4])
		}
	}
	return dst
}

// exifOrientation reads the orientation tag from the start of a JPEG, or
// returns 1 (upright) if there is none.
func exifOrientation(b []byte) int {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return 1
	}
	// Walk the marker segments up to the image data.
	for i := 2; i+4 <= len(b); {
		if b[i] != 0xFF {
			return 1
		}
		marker := b[i+1]
		switch {
		case marker == 0xFF:
			i++ // fill byte
			continue
		case marker == 0x01 || (marker >= 0xD0 && marker <= 0xD8):
			i += 2 // markers without a length
			continue
		case marker == 0xDA || marker == 0xD9:
			return 1 // image data begins: no EXIF before it
		}
		segLen := int(binary.BigEndian.Uint16(b[i+2:]))
		end := min(len(b), i+2+segLen)
		if seg := b[i+4 : max(i+4, end)]; marker == 0xE1 && len(seg) >= 6 && string(seg[:6]) == "Exif\x00\x00" {
			return tiffOrientation(seg[6:])
		}
		i += 2 + segLen
	}
	return 1
}

// tiffOrientation finds tag 0x0112 in the first IFD of a TIFF header.
func tiffOrientation(t []byte) int {
	if len(t) < 8 {
		return 1
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 1
	}
	ifd := int(bo.Uint32(t[4:]))
	if ifd < 8 || ifd+2 > len(t) {
		return 1
	}
	// Each IFD entry is 12 bytes: tag, type, count, value.
	for k := range int(bo.Uint16(t[ifd:])) {
		e := ifd + 2 + k*12
		if e+12 > len(t) {
			return 1
		}
		if bo.Uint16(t[e:]) == 0x0112 {
			if v := int(bo.Uint16(t[e+8:])); v >= 1 && v <= 8 {
				return v
			}
			return 1
		}
	}
	return 1
}

// trimDir deletes the oldest files in dir until it uses at most budget bytes.
func trimDir(dir string, budget int64) {
	des, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type item struct {
		path string
		size int64
		mod  time.Time
	}
	var items []item
	var total int64
	// Measure everything; leftovers of interrupted writes go right away.
	for _, de := range des {
		fi, err := de.Info()
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		p := filepath.Join(dir, de.Name())
		if strings.HasPrefix(de.Name(), "tmp-") && time.Since(fi.ModTime()) > time.Hour {
			os.Remove(p)
			continue
		}
		items = append(items, item{p, fi.Size(), fi.ModTime()})
		total += fi.Size()
	}
	slices.SortFunc(items, func(a, b item) int { return a.mod.Compare(b.mod) })
	// Oldest first, until under budget.
	for _, it := range items {
		if total <= budget {
			break
		}
		if os.Remove(it.path) == nil {
			total -= it.size
		}
	}
}
