package pull

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/Indigo6415/pidgeon/internal/brand"
)

// entry is one item of a remote folder, as /api/ls returns it.
type entry struct {
	Name    string `json:"name"`
	Dir     bool   `json:"dir"`
	Size    int64  `json:"size"`
	MTime   int64  `json:"mtime"` // Unix ms
	Archive bool   `json:"archive"`
}

// listing is a remote folder.
type listing struct {
	Name    string  `json:"name"`
	Entries []entry `json:"entries"`
}

// client talks to one Pidgeon server: through its API, or through a share
// link (which needs no token and sees only what was shared).
type client struct {
	http  *http.Client
	base  string // "http(s)://host:port"
	token string
	user  string // basic auth
	pass  string
	pin   string // share link PIN
	share string // share link ID; "" = the API
}

// errAuth means the server wants credentials we don't have (or that are wrong).
var errAuth = errors.New("this server needs an access token: pass --token (it is shown in the host's terminal)")

// newClient builds a client; tlsCfg is nil for plain HTTP or the system's
// verification.
func newClient(base string, tlsCfg *tls.Config) *client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsCfg
	tr.ResponseHeaderTimeout = 30 * time.Second
	return &client{http: &http.Client{Transport: tr}, base: strings.TrimSuffix(base, "/")}
}

// get sends a GET with the client's credentials.
func (c *client) get(ctx context.Context, p string, hdr http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+p, nil)
	if err != nil {
		return nil, err
	}
	maps.Copy(req.Header, hdr)
	req.Header.Set("User-Agent", brand.Name+"-pull/"+brand.ResolvedVersion())
	// One Authorization header: the token wins over basic auth.
	switch {
	case c.token != "":
		req.Header.Set("Authorization", "Bearer "+c.token)
	case c.user != "":
		req.SetBasicAuth(c.user, c.pass)
	}
	if c.pin != "" {
		req.Header.Set("X-Pidgeon-PIN", c.pin)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		if c.share != "" {
			return nil, errors.New("this link needs a PIN: pass --pin")
		}
		return nil, errAuth
	}
	return resp, nil
}

// getJSON fetches p and decodes the JSON answer into v.
func (c *client) getJSON(ctx context.Context, p string, v any) error {
	resp, err := c.get(ctx, p, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// escape percent-encodes each segment of a remote path ("/a b/c").
func escape(p string) string {
	segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// list reads a remote folder ("/" or "/a/b").
func (c *client) list(ctx context.Context, p string) (listing, error) {
	var l listing
	if c.share != "" {
		u := "/s/" + c.share + "/"
		if p != "/" {
			u += escape(p) + "/"
		}
		return l, c.getJSON(ctx, u+"?json=1", &l)
	}
	return l, c.getJSON(ctx, "/api/ls?path="+url.QueryEscape(p), &l)
}

// fileURL is where a remote file is downloaded from.
func (c *client) fileURL(p string) string {
	if c.share != "" {
		if p == "/" {
			return "/s/" + c.share // a link to a single file
		}
		return "/s/" + c.share + "/" + escape(p)
	}
	return "/f/" + escape(p)
}

// hash asks the server for a file's SHA-256 ("" when it can't say, e.g.
// through a share link).
func (c *client) hash(ctx context.Context, p string) (string, error) {
	if c.share != "" {
		return "", nil
	}
	var v struct {
		SHA256 string `json:"sha256"`
	}
	err := c.getJSON(ctx, "/api/hash?path="+url.QueryEscape(p), &v)
	return v.SHA256, err
}

// rootName is the name of the server's served folder.
func (c *client) rootName(ctx context.Context) string {
	if c.share != "" {
		var l listing
		if c.getJSON(ctx, "/s/"+c.share+"/?json=1", &l) == nil {
			return l.Name
		}
		return ""
	}
	var info struct {
		Root string `json:"root"`
	}
	c.getJSON(ctx, "/api/info", &info)
	return info.Root
}

// stat describes the remote path p: a folder or a file (found by listing
// its parent). A share link to a single file is its own root.
func (c *client) stat(ctx context.Context, p string) (entry, error) {
	if p == "/" {
		if c.share != "" {
			// The root of a share link may be one file.
			var f struct {
				File  bool   `json:"file"`
				Name  string `json:"name"`
				Size  int64  `json:"size"`
				MTime int64  `json:"mtime"`
			}
			if err := c.getJSON(ctx, "/s/"+c.share+"?json=1", &f); err != nil {
				return entry{}, err
			}
			if f.File {
				return entry{Name: f.Name, Size: f.Size, MTime: f.MTime}, nil
			}
		}
		return entry{Name: "", Dir: true}, nil
	}
	l, err := c.list(ctx, path.Dir(p))
	if err != nil {
		return entry{}, err
	}
	for _, e := range l.Entries {
		if e.Name == path.Base(p) {
			return e, nil
		}
	}
	return entry{}, fmt.Errorf("%s: not found on the server", p)
}
