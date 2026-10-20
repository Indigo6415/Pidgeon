# Pidgeon

Carrier pidgeons delivering bytes to you.

Pidgeon serves the current directory over HTTP so any device on your network can
browse it and download files, with a terminal dashboard to watch and control it.
Run `pidgeon` (or the short alias `pdgn`) in a folder, scan the QR code with your
phone, and grab what you need.

- **Single static binary.** No runtime dependencies. The web UI is embedded. Runs on
  Linux, macOS and Windows.
- **Fast web UI in VS Code's colours.** A folder tree, a file card with breadcrumbs,
  a command palette and a downloads popover, in Dark Modern (default) and Light
  Modern colours. It works on phones and stays smooth in folders with tens of
  thousands of files. You can multi-select, stream a ZIP that keeps the folder
  structure, and copy links or cURL commands.
- **Live TUI dashboard.** Turn each network interface on or off, watch per-connection
  speed and progress, see a 60-second throughput graph, scan a QR code to connect,
  and read the request log.
- **Real file serving.** Correct `Content-Type` and `Content-Length`, HTTP Range
  (resumable and seekable), and streaming throughout. Nothing is ever loaded fully
  into memory.

> Screenshots: _TUI dashboard_ · _web UI (desktop)_ · _web UI (phone)_ go here.

## Usage

```sh
pidgeon                  # serve the current directory on :8080
pdgn ~/Movies            # same program, shorter name, explicit directory
pidgeon -p 9000          # another port (auto-increments if taken)
pidgeon --token auto     # require a random access token (included in the QR link)
pidgeon --bind all       # listen on every interface (0.0.0.0) from the start
pidgeon --headless       # no dashboard, just URLs + a request log
```

| Flag | Default | Meaning |
|---|---|---|
| `-p`, `--port` | `8080` | Port to try first. If it's taken, the next free one is used. |
| `-b`, `--bind` | `default` | Which listeners start enabled: `default` (loopback + LAN), `loopback`, `lan`, `vpn`, `virtual`, `all` (wildcard), or IP addresses, comma-separated. |
| `--token` | — | Require an access token. `auto` generates one. |
| `--auth` | — | Require HTTP basic auth, `user:password`. |
| `--hidden` | off | Also serve dotfiles and dot-directories. These are hidden by default. |
| `--no-splash` | off | Skip the startup animation. |
| `--headless` | auto | Run without the dashboard. This is automatic when stdin or stdout isn't a terminal (pipes, systemd, `docker run` without `-t`) or when started in the background with `&`. |
| `--version` | | Print the version and exit. |

Before the dashboard opens, every URL is printed to the terminal, so they stay in
your scrollback after you quit. The splash is skipped when you pass `--no-splash`,
when `NO_COLOR` is set, or when the terminal is too small. Press any key to skip it.

### Dashboard keys

Everything works with the arrow keys, Enter and Esc alone:

| Key | Action |
|---|---|
| `↑` `↓` | Move through the listeners, then on down into the transfers (one continuous list) |
| `enter` | On a listener, turn it on or off live. On a transfer, open its actions (cancel). |
| `→` | Actions for the selected row: enable or disable, copy URL, open in browser, cancel transfer |
| `esc` | Main menu: show all keys, or quit. `esc` (or `←`) also closes any open menu. |

Letter shortcuts for the same actions: `space` toggle, `tab` switch panel,
`x` cancel transfer, `c` copy URL (OSC 52 clipboard), `o` open in browser,
`?` all keys, `q` quit. If downloads are in progress, `q` asks you to press it
again, and the quit item in the Esc menu shows how many will be cancelled.
`ctrl+c` always quits immediately.

The QR code follows the listener under the cursor. If that listener isn't serving,
it falls back to the primary LAN address.

### Listeners

A *listener* is one address the server binds to. On startup Pidgeon finds:

- loopback (`127.0.0.1`)
- each LAN address. The address used by the default route is marked ★ and is the
  default QR target.
- VPN / overlay addresses (Tailscale `100.64.0.0/10`, `utun`, `wg`, `tun`, …)
- virtual bridges (Docker, VM networks)
- the wildcard (`*`, every interface)

Turning a listener on binds it immediately. Turning it off closes its socket at
once, but downloads already running through it finish normally. Other listeners
are never affected.

Most operating systems won't bind `0.0.0.0:P` while `192.168.x.y:P` is bound. So
turning on the wildcard parks the specific listeners (shown as `↳ via *`) and
serves them through the wildcard. Turning it off brings them back. Traffic arriving
through the wildcard is still credited to the right row.

### Web UI

It uses VS Code's colours, with a layout of its own:

- **Folder tree** on the left, with folders only. Click to open; the chevron
  expands. The top-left button or ⌘B toggles it.
- **File card** with the folder name, its breadcrumbs, and a table with language
  badges and Seti-style icon colors.
- **Downloads popover** under the download button in the top bar. ⌘J toggles it.
- **Status line** showing item count, selection size, live download speed and the
  auth mode.

Big folders stay quick. Only the rows on screen are drawn, and the subfolders on
screen are summarised lazily: their item counts, and whether the tree needs a
chevron, arrive in small batches from `/api/peek`. Listings are cached and
prefetched when you hover a folder or rest the cursor on it, so opening one is
usually instant. A cached folder appears at once and is refreshed in the
background.

Dark Modern is the default. The moon/sun button switches themes, and the command
palette also offers "Follow System". On phones the folder tree becomes a
slide-out drawer, rows get bigger, and downloads open as a bottom sheet.

| Shortcut (Ctrl on Windows/Linux) | Action |
|---|---|
| `⌘P` | Quick open: fuzzy-find in the current folder |
| `⇧⌘P` / `F1` | Command palette (download selection, themes, sorting, panels, …) |
| `↑` `↓`, `Home` `End` | Move the cursor. `Shift` extends the selection. |
| `Space` | Select or deselect the cursor row |
| `Enter` | Open a folder or download a file (downloads the selection if several are selected) |
| `⌘↑` / `Alt+↑` / `Backspace` | Go to the parent folder |
| `⌘A`, `Esc` | Select all, clear the selection |
| Click, `⌘`-click, `Shift`-click | Select, toggle, select range. Folders open on click; files download on double-click. |
| Right-click | Download, Open Preview, Copy Link, **Copy as cURL** |

### Downloads in the browser

- **Download a file** with a double-click, `Enter`, or the row's download button
  (on a phone, just tap it). It's a streamed, resumable download.
- **Select several items, or download a folder,** and the server builds a ZIP on the
  fly and streams it. The archive keeps the full folder tree, including empty
  folders, so it extracts exactly as it was. Already-compressed formats are stored,
  not recompressed. Unreadable files are listed in `Pidgeon-skipped-files.txt`
  inside the archive.
- **Downloads panel.** It shows queued, active and finished downloads with progress,
  speed and bytes. Browsers save downloads straight to disk and don't tell the page
  how far along they are. So every download URL carries an ID, and the server
  reports the bytes it has sent for that ID over Server-Sent Events. This works on
  every browser, including on phones, without holding files in memory. ZIPs have no
  known size in advance, so they show live bytes rather than a percentage.

A browser always decides where its own downloads go: your Downloads folder or a
save dialog. The server can't write into a folder of your choosing, so the ZIP is
what preserves the folder structure. If you want files written straight into a
directory, that is what `pidgeon pull <url> [dir]` is for. That subcommand is
currently a stub; its planned design is in `internal/pull/pull.go`.

## Install

You need [Go 1.26+](https://go.dev/dl/). The result is one static binary plus the
`pdgn` alias.

### macOS / Linux

```sh
git clone https://github.com/Indigo6415/pidgeon && cd pidgeon
./install.sh            # or: make install
```

`install.sh` puts the binaries in `$INSTALL_DIR`, else `$GOBIN`, else
`~/go/bin`. If that directory isn't writable it falls back to `/usr/local/bin`,
which may need sudo: `sudo INSTALL_DIR=/usr/local/bin ./install.sh`. If the
install directory isn't on your `PATH`, the script prints the exact line to add
for zsh, bash or fish. A typical one:

```sh
echo 'export PATH="$HOME/go/bin:$PATH"' >> ~/.zshrc && source ~/.zshrc
```

Without cloning:

```sh
go install github.com/Indigo6415/pidgeon/cmd/...@latest   # installs pidgeon and pdgn into ~/go/bin
```

### Windows

```powershell
git clone https://github.com/Indigo6415/pidgeon; cd pidgeon
.\install.ps1 -AddToPath     # builds pidgeon.exe + pdgn.exe into %USERPROFILE%\go\bin
```

Or use `go install github.com/Indigo6415/pidgeon/cmd/...@latest`, then add
`%USERPROFILE%\go\bin` to your PATH. Open a new terminal afterwards.

### Make targets

| Target | What it does |
|---|---|
| `make build` | Builds `bin/pidgeon` and `bin/pdgn` |
| `make install` / `make uninstall` | See above |
| `make test` | Runs the tests with `-race` |
| `make cross` | Release binaries for linux/macOS/windows × amd64/arm64 in `bin/dist` |

## Security and trust model

**By default Pidgeon has no authentication.** Anyone who can reach one of the
enabled addresses can list and download everything under the served directory.
That's fine on your home Wi-Fi and wrong on a café network.

- `--token auto` (or `--token <value>`) gates every request. The QR code and printed
  links include `?token=…`. The first visit swaps it for an HttpOnly cookie and
  strips it from the address bar. Scripts can send `Authorization: Bearer <token>`.
- `--auth user:pass` adds HTTP basic auth instead of, or as well as, a token.
- "Copy as cURL" includes the token as a Bearer header. `/api/info` returns the token
  only to visitors who are already authenticated.
- Both are sent in plain HTTP. They keep out casual visitors on a network you
  mostly trust. They are not protection against someone sniffing that network.
  Use a VPN listener (e.g. Tailscale) to cross untrusted networks.

What's always enforced:

- The server is read-only. There are no upload, delete or rename endpoints.
- All file access goes through Go's `os.Root`, which refuses `..` traversal and
  symlinks that point outside the served directory.
- Dotfiles and dot-directories (`.git`, `.env`, `.ssh`, …) are hidden and refused
  unless you pass `--hidden`.
- Only the addresses you enable are bound. The wildcard is off unless you turn it
  on.
- HTML, SVG and XML files opened inline are served with `Content-Security-Policy:
  sandbox`, so a served page can't run as the app. Every response sends
  `Referrer-Policy: no-referrer` so tokens don't leak.

## Architecture

```
cmd/pidgeon, cmd/pdgn      thin mains → internal/app.Main
internal/
  brand/      the name, alias, tagline and pigeon art (rename here)
  app/        flags, startup banner, signals, graceful shutdown, headless mode
  listeners/  interface discovery + Manager: one http.Server per bound address
  server/     http.Handler: /api/ls, /api/peek, /f/<path>, /api/zip + /zip/<token>, /api/events, auth
  stats/      central store: atomic counters, transfer registry, 60s history, request log
  tui/        Bubble Tea app: splash → dashboard (Lipgloss, Bubbles key/help)
  web/        embed.FS with index.html, app.css, app.js (vanilla JS, no build step)
  pull/       `pidgeon pull` stub
```

How a download is measured: every request goes through a `ResponseRecorder`
(status, bytes, global throughput). Downloads also wrap the writer in a
`CountingWriter` registered as a *transfer*. That writer deliberately hides
`io.ReaderFrom`, so every byte passes through `Write` and is counted, at the cost
of `sendfile`. A sampler runs at 4 Hz and turns byte counts into smoothed
per-transfer rates and the per-second history. The TUI redraws from a snapshot
at 8 Hz, and the web UI gets its share through SSE.

Shutdown on `q`, SIGINT, SIGTERM or SIGHUP (SIGHUP is left alone under `nohup`):

1. Progress streams end.
2. Every listener stops accepting connections.
3. In-flight transfers get a 3-second grace period, then their connections are
   closed.
4. A summary is printed.

## Development

```sh
make run                                         # serve . from source
make test
PIDGEON_PREVIEW=1 go test ./internal/tui -run Preview -v   # print splash frames + a dashboard
```

The pigeon lives in `internal/brand/art.go` and is used by both the terminal and
web splash screens. Redraw it there.
