package server

import (
	"context"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"
)

// mediaTypes lists the formats offered to media players (playlists) and
// TVs (DLNA), with the type a player expects for each.
var mediaTypes = map[string]string{
	// video
	".mp4": "video/mp4", ".m4v": "video/x-m4v", ".mkv": "video/x-matroska", ".mov": "video/quicktime",
	".avi": "video/x-msvideo", ".webm": "video/webm", ".wmv": "video/x-ms-wmv", ".flv": "video/x-flv",
	".mpg": "video/mpeg", ".mpeg": "video/mpeg", ".ts": "video/mp2t", ".m2ts": "video/mp2t",
	".3gp": "video/3gpp", ".ogv": "video/ogg",
	// audio
	".mp3": "audio/mpeg", ".m4a": "audio/mp4", ".aac": "audio/aac", ".flac": "audio/flac", ".wav": "audio/wav",
	".ogg": "audio/ogg", ".oga": "audio/ogg", ".opus": "audio/opus", ".wma": "audio/x-ms-wma",
	".aif": "audio/aiff", ".aiff": "audio/aiff",
	// pictures
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png", ".gif": "image/gif",
	".webp": "image/webp", ".bmp": "image/bmp", ".heic": "image/heic",
}

// mediaKind says whether a file is "video", "audio" or "image" (or ""),
// and its media type.
func mediaKind(name string) (kind, typ string) {
	typ = mediaTypes[strings.ToLower(path.Ext(name))]
	if typ == "" {
		return "", ""
	}
	kind, _, _ = strings.Cut(typ, "/")
	return kind, typ
}

// playlist limits keep a recursive playlist of a huge tree cheap.
const (
	playlistMaxItems = 5000
	playlistMaxTime  = 4 * time.Second
)

// handleM3U returns the videos and songs of a folder as an M3U playlist
// (?path=/Movies, &r=1 to include subfolders) that VLC, mpv, Infuse or a
// TV app can play straight from the server. The links carry the access
// token, since players can't log in; anyone holding the playlist can
// play these files, just like with a copied link.
func (s *Server) handleM3U(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name, err := s.resolve(q.Get("path"))
	if err != nil {
		httpError(w, err)
		return
	}
	var files []string
	if q.Get("r") == "1" {
		files, err = s.mediaUnder(r.Context(), name)
	} else {
		files, err = s.mediaIn(name)
	}
	if err != nil {
		httpError(w, err)
		return
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	suffix := ""
	if s.cfg.Token != "" {
		suffix = "?token=" + url.QueryEscape(s.cfg.Token)
	}
	title := s.name
	if name != "." {
		title = path.Base(name)
	}
	var sb strings.Builder
	sb.WriteString("#EXTM3U\n#PLAYLIST:" + oneLine(title) + "\n")
	// One entry per file, titled without its extension.
	for _, f := range files {
		base := path.Base(f)
		fmt.Fprintf(&sb, "#EXTINF:-1,%s\n%s://%s/f/%s%s\n", oneLine(strings.TrimSuffix(base, path.Ext(base))), scheme, r.Host, escapePath(f), suffix)
	}
	h := w.Header()
	h.Set("Content-Type", "audio/x-mpegurl; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": title + ".m3u8"}))
	w.Write([]byte(sb.String()))
}

// oneLine keeps a title from breaking the playlist's line structure.
func oneLine(s string) string { return strings.NewReplacer("\r", " ", "\n", " ").Replace(s) }

// mediaIn lists the playable files of one folder, in natural order.
func (s *Server) mediaIn(name string) ([]string, error) {
	es, err := s.list(name)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range es {
		if k, _ := mediaKind(e.Name); !e.Dir && (k == "video" || k == "audio") {
			out = append(out, path.Join(name, e.Name))
		}
	}
	slices.SortFunc(out, naturalCompare)
	return out, nil
}

// mediaUnder lists the playable files below a folder, folder by folder.
func (s *Server) mediaUnder(ctx context.Context, name string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, playlistMaxTime)
	defer cancel()
	var out []string
	err := fs.WalkDir(s.fsys, name, func(p string, d fs.DirEntry, err error) error {
		if err != nil || ctx.Err() != nil || len(out) >= playlistMaxItems {
			return fs.SkipAll
		}
		if p != name && s.hidden(path.Dir(p), d.Name()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if k, _ := mediaKind(p); !d.IsDir() && (k == "video" || k == "audio") {
			out = append(out, p)
		}
		return nil
	})
	if err != nil && len(out) == 0 {
		return nil, err
	}
	slices.SortFunc(out, naturalCompare)
	return out, nil
}

// naturalCompare orders names the way people expect: "Episode 2" before
// "Episode 10", case-insensitively.
func naturalCompare(a, b string) int {
	a, b = strings.ToLower(a), strings.ToLower(b)
	// Compare run by run: digit runs by value, everything else by byte.
	for a != "" && b != "" {
		da, db := isDigit(a[0]), isDigit(b[0])
		if da && db {
			na, ra := digitRun(a)
			nb, rb := digitRun(b)
			// Equal numbers compare by length so "01" and "1" stay stable.
			if c := cmpNum(na, nb); c != 0 {
				return c
			}
			a, b = ra, rb
			continue
		}
		if a[0] != b[0] {
			return int(a[0]) - int(b[0])
		}
		a, b = a[1:], b[1:]
	}
	return len(a) - len(b)
}

// isDigit reports whether c is an ASCII digit.
func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// digitRun splits off a leading run of digits.
func digitRun(s string) (string, string) {
	i := 0
	for i < len(s) && isDigit(s[i]) {
		i++
	}
	return s[:i], s[i:]
}

// cmpNum compares two digit strings by value (ignoring leading zeros).
func cmpNum(a, b string) int {
	a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
	if len(a) != len(b) {
		return len(a) - len(b)
	}
	return strings.Compare(a, b)
}
