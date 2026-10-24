package server

import (
	"bytes"
	"encoding/binary"
	"io"
	"time"
)

// mediaDate finds when a photo or video was taken, for --inbox-sort date:
// the EXIF DateTimeOriginal of a JPEG, HEIC/HEIF or TIFF-based raw file,
// or the creation time in the movie header of an MP4 / MOV. ok is false
// when the file carries no usable date.
func mediaDate(r io.ReaderAt, size int64) (time.Time, bool) {
	head := make([]byte, min(size, mediaHeadBytes))
	n, _ := r.ReadAt(head, 0)
	head = head[:n]
	// Movies keep the date in the "moov" box, often at the end of the file.
	if len(head) >= 12 && string(head[4:8]) == "ftyp" {
		if t, ok := movieDate(r, size); ok {
			return t, true
		}
	}
	return exifDate(head)
}

// mediaHeadBytes is how much of a file is searched for EXIF data.
const mediaHeadBytes = 512 << 10

// earliestMedia rejects dates no camera could have written (unset clocks
// report 1970, 1904 or 2000-01-01 with a zero time).
var earliestMedia = time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC)

// plausible reports whether t could be a real capture time.
func plausible(t time.Time) bool {
	return t.After(earliestMedia) && t.Before(time.Now().Add(48*time.Hour))
}

// exifDate parses the EXIF block found in b: a JPEG's APP1 segment, a TIFF
// file itself, or (HEIC) an "Exif\0\0" marker followed by a TIFF header.
func exifDate(b []byte) (time.Time, bool) {
	var tiff []byte
	switch {
	case len(b) >= 4 && (string(b[:4]) == "II*\x00" || string(b[:4]) == "MM\x00*"):
		tiff = b
	default:
		// The marker precedes the TIFF header in JPEG and HEIC alike.
		i := bytes.Index(b, []byte("Exif\x00\x00"))
		if i < 0 {
			return time.Time{}, false
		}
		tiff = b[i+6:]
	}
	return tiffDate(tiff)
}

// EXIF tags that hold dates, and the pointer to the EXIF sub-IFD.
const (
	tagDateTime         = 0x0132
	tagExifIFD          = 0x8769
	tagDateTimeOriginal = 0x9003
)

// tiffDate reads DateTimeOriginal (falling back to DateTime) from a TIFF
// header and its IFDs.
func tiffDate(t []byte) (time.Time, bool) {
	if len(t) < 8 {
		return time.Time{}, false
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return time.Time{}, false
	}
	// IFD0 has DateTime and the pointer to the EXIF IFD.
	tags := ifdTags(t, bo, int(bo.Uint32(t[4:])))
	if p, ok := tags[tagExifIFD]; ok {
		if d, ok := exifTime(t, bo, ifdTags(t, bo, int(bo.Uint32(p[8:])))[tagDateTimeOriginal]); ok {
			return d, true
		}
	}
	return exifTime(t, bo, tags[tagDateTime])
}

// ifdTags returns the raw 12-byte entries of one IFD by tag.
func ifdTags(t []byte, bo binary.ByteOrder, off int) map[uint16][]byte {
	out := map[uint16][]byte{}
	if off < 8 || off+2 > len(t) {
		return out
	}
	// Each entry is 12 bytes: tag, type, count, value or offset.
	for k := range int(bo.Uint16(t[off:])) {
		e := off + 2 + k*12
		if e+12 > len(t) {
			break
		}
		out[bo.Uint16(t[e:])] = t[e : e+12]
	}
	return out
}

// exifTime decodes an ASCII "YYYY:MM:DD HH:MM:SS" entry. EXIF times have
// no zone; they are the camera's local time, so they are read as local.
func exifTime(t []byte, bo binary.ByteOrder, e []byte) (time.Time, bool) {
	// Type 2 is ASCII; the 20-byte string lives at the offset in the entry.
	if len(e) != 12 || bo.Uint16(e[2:]) != 2 || bo.Uint32(e[4:]) < 19 {
		return time.Time{}, false
	}
	off := int(bo.Uint32(e[8:]))
	if off+19 > len(t) {
		return time.Time{}, false
	}
	d, err := time.ParseInLocation("2006:01:02 15:04:05", string(t[off:off+19]), time.Local)
	if err != nil || !plausible(d) {
		return time.Time{}, false
	}
	return d, true
}

// movieEpoch is where QuickTime and MP4 times start counting.
var movieEpoch = time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC)

// movieMaxBox bounds how much of a "moov" box is read to find its header.
const movieMaxBox = 32 << 20

// movieDate walks the top-level boxes of an ISO base media file (MP4, MOV,
// HEIC) to "moov", then reads the creation time from its "mvhd" box.
func movieDate(r io.ReaderAt, size int64) (time.Time, bool) {
	var hdr [16]byte
	// Hop from box to box using their headers alone.
	for off := int64(0); off+8 <= size; {
		if _, err := r.ReadAt(hdr[:8], off); err != nil {
			return time.Time{}, false
		}
		n, kind := int64(binary.BigEndian.Uint32(hdr[:4])), string(hdr[4:8])
		head := int64(8)
		switch n {
		case 0:
			n = size - off // the box runs to the end of the file
		case 1:
			// A 64-bit size follows the type.
			if _, err := r.ReadAt(hdr[8:16], off+8); err != nil {
				return time.Time{}, false
			}
			n, head = int64(binary.BigEndian.Uint64(hdr[8:16])), 16
		}
		if n < head {
			return time.Time{}, false
		}
		if kind == "moov" {
			body := make([]byte, min(n-head, movieMaxBox))
			k, _ := r.ReadAt(body, off+head)
			return mvhdTime(body[:k])
		}
		off += n
	}
	return time.Time{}, false
}

// mvhdTime finds the movie header inside a "moov" box body.
func mvhdTime(b []byte) (time.Time, bool) {
	i := bytes.Index(b, []byte("mvhd"))
	if i < 0 || i+16 > len(b) {
		return time.Time{}, false
	}
	v := b[i+4:] // version (1 byte), flags (3 bytes), then the times
	var secs uint64
	switch {
	case v[0] == 0:
		secs = uint64(binary.BigEndian.Uint32(v[4:]))
	case v[0] == 1 && len(v) >= 12:
		secs = binary.BigEndian.Uint64(v[4:])
	default:
		return time.Time{}, false
	}
	// Anything past the year ~2400 is garbage (and would overflow a Duration).
	if secs > 1<<34 {
		return time.Time{}, false
	}
	d := movieEpoch.Add(time.Duration(secs) * time.Second)
	if !plausible(d) {
		return time.Time{}, false
	}
	return d.Local(), true
}
