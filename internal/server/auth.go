package server

import (
	"crypto/subtle"
	"html/template"
	"net/http"
	"strings"

	"github.com/Indigo6415/pidgeon/internal/brand"
)

// auth gates every route when --token and/or --auth is configured. Any one
// valid credential is enough:
//
//   - ?token=... in the URL (sets a cookie, so the QR link logs a phone in)
//   - the token cookie
//   - Authorization: Bearer <token>
//   - HTTP basic auth with the configured user/password
//
// Share links (/s/...) carry their own capability and skip this check; the
// share handler validates the link instead.
func (s *Server) auth(next http.Handler) http.Handler {
	tok := s.cfg.Token
	user, pass := s.cfg.BasicUser, s.cfg.BasicPass
	// No credentials configured: the server is open (see README trust model).
	if tok == "" && user == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A share link is its own credential, scoped to one file or folder;
		// TV mode's endpoints check their own key (TVs can't log in).
		if strings.HasPrefix(r.URL.Path, "/s/") || strings.HasPrefix(r.URL.Path, "/dlna/") {
			next.ServeHTTP(w, r)
			return
		}
		if tok != "" {
			// A token in the query string upgrades to a cookie.
			if q := r.URL.Query().Get("token"); q != "" && equal(q, tok) {
				http.SetCookie(w, &http.Cookie{
					Name:     brand.CookieName,
					Value:    tok,
					Path:     "/",
					HttpOnly: true,
					SameSite: http.SameSiteLaxMode,
					Secure:   r.TLS != nil,
					MaxAge:   30 * 24 * 3600,
				})
				// Strip the token from the address bar for the app page so it
				// doesn't linger in history or get shared by accident.
				if r.URL.Path == "/" {
					http.Redirect(w, r, "/", http.StatusSeeOther)
					return
				}
				next.ServeHTTP(w, r)
				return
			}
			if c, err := r.Cookie(brand.CookieName); err == nil && equal(c.Value, tok) {
				next.ServeHTTP(w, r)
				return
			}
			if b, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && equal(b, tok) {
				next.ServeHTTP(w, r)
				return
			}
		}
		if user != "" {
			// Basic auth: compare both parts in constant time.
			if u, p, ok := r.BasicAuth(); ok && equal(u, user) && equal(p, pass) {
				next.ServeHTTP(w, r)
				return
			}
		}
		s.deny(w, r)
	})
}

// deny answers an unauthenticated request: a token prompt page for the app
// root in token mode, a basic-auth challenge otherwise.
func (s *Server) deny(w http.ResponseWriter, r *http.Request) {
	// Basic auth only: let the browser show its native login prompt.
	if s.cfg.Token == "" {
		w.Header().Set("WWW-Authenticate", `Basic realm="`+brand.Name+`", charset="UTF-8"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// Token mode, browser navigating to the app: show a friendly prompt.
	if r.URL.Path == "/" && r.Method == http.MethodGet {
		if s.cfg.BasicUser != "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="`+brand.Name+`", charset="UTF-8"`)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		loginPage.Execute(w, map[string]any{
			"Name":  brand.Name,
			"Wrong": r.URL.Query().Has("token"),
		})
		return
	}
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// equal compares secrets in constant time.
func equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// loginPage is deliberately self-contained (no external assets) so it works
// before the visitor is authorised to load /assets.
var loginPage = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="dark light">
<title>{{.Name}} · access token</title>
<style>
  :root { --bg:#0e1116; --card:#161b22; --fg:#e6edf3; --muted:#8b949e; --line:#2a313c; --accent:#3dd6c6; }
  @media (prefers-color-scheme: light) { :root { --bg:#f6f7f9; --card:#fff; --fg:#14181f; --muted:#5b6573; --line:#dde1e7; --accent:#0f9488; } }
  * { box-sizing:border-box } body { margin:0; min-height:100dvh; display:grid; place-items:center;
    background:var(--bg); color:var(--fg); font:16px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif; padding:16px }
  form { width:100%; max-width:360px; background:var(--card); border:1px solid var(--line); border-radius:16px; padding:28px }
  h1 { margin:0 0 4px; font-size:20px } p { margin:0 0 20px; color:var(--muted); font-size:14px }
  input { width:100%; font:inherit; padding:12px 14px; border-radius:10px; border:1px solid var(--line); background:var(--bg); color:var(--fg) }
  button { margin-top:12px; width:100%; font:inherit; font-weight:600; padding:12px; border:0; border-radius:10px; background:var(--accent); color:#04201d }
  .err { color:#f47067; font-size:14px; margin:10px 0 0 }
</style></head><body>
<form method="get" action="/">
  <h1>{{.Name}}</h1>
  <p>This server is protected. Enter the access token shown in the host's terminal.</p>
  <input name="token" type="password" autocomplete="current-password" placeholder="Access token" autofocus required>
  {{if .Wrong}}<div class="err">That token didn't match.</div>{{end}}
  <button type="submit">Continue</button>
</form></body></html>`))

// waitingPage is shown to a device the operator hasn't approved yet (--ask).
// It reloads itself until the host decides, so the visitor needs to do
// nothing. Self-contained, like the login page.
var waitingPage = template.Must(template.New("waiting").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="dark light">
<meta http-equiv="refresh" content="3">
<title>{{.Name}} · waiting for approval</title>
<style>
  :root { --bg:#181818; --card:#1f1f1f; --fg:#cccccc; --muted:#9d9d9d; --line:#2b2b2b; --accent:#4daafc; }
  @media (prefers-color-scheme: light) { :root { --bg:#f8f8f8; --card:#fff; --fg:#3b3b3b; --muted:#6f6f6f; --line:#e5e5e5; --accent:#005fb8; } }
  * { box-sizing:border-box } body { margin:0; min-height:100dvh; display:grid; place-items:center;
    background:var(--bg); color:var(--fg); font:16px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif; padding:16px }
  main { width:100%; max-width:380px; background:var(--card); border:1px solid var(--line); border-radius:16px; padding:28px; text-align:center }
  h1 { margin:0 0 6px; font-size:20px } p { margin:0; color:var(--muted); font-size:14px }
  .dot { width:12px; height:12px; margin:0 auto 18px; border-radius:50%; background:var(--accent); animation:p 1.2s ease-in-out infinite }
  code { font-size:13px; color:var(--fg) }
  @keyframes p { 50% { opacity:.25; transform:scale(.7) } }
</style></head><body>
<main>
  <div class="dot"></div>
  <h1>Waiting for the host</h1>
  <p>{{.Name}} asks the person running it to let each new device in.
  They'll see <code>{{if .Device}}{{.Device}} · {{end}}{{.IP}}</code> on their screen.
  This page continues by itself once they allow it.</p>
</main></body></html>`))
