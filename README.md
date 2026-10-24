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
- **Between machines.** `pidgeon pull` copies a folder from another Pidgeon into a
  directory, resumable and only fetching what changed; `pidgeon find` lists the
  servers on the network. With `--flock`, the other servers open in place in the
  web UI, and one ZIP can mix their files with yours. HTTPS with `--tls auto`.
- **Between people.** Share links with an optional PIN, file requests (a link
  others upload with), Pigeon Post (text between the dashboard and a browser),
  and delivery receipts of who received what.
- **On the TV.** `--tv` makes the folder a DLNA media server; any folder also
  plays as an `.m3u` playlist in VLC or mpv.

> Screenshots: _TUI dashboard_ · _web UI (desktop)_ · _web UI (phone)_ go here.

## Usage

```sh
pidgeon                  # serve the current directory on :8080
pdgn ~/Movies            # same program, shorter name, explicit directory
pidgeon -p 9000          # another port (auto-increments if taken)
pidgeon --token auto     # require a random access token (included in the QR link)
pidgeon --bind all       # listen on every interface (0.0.0.0) from the start
pidgeon --headless       # no dashboard, just URLs + a request log
pidgeon ~/Movies ~/Photos report.pdf   # several folders and files side by side
pidgeon --limit 5M       # cap downloads at 5 MB/s in total (--client-limit caps each device)
pidgeon --tls auto       # HTTPS with a self-signed certificate kept between runs
pidgeon --tv ~/Movies    # also show up on TVs and media players as a media server
pidgeon --inbox ~/Phone --inbox-sort date   # phone backups, filed by date, no duplicates
pidgeon --flock          # browse the other Pidgeon servers on the network from this one

pidgeon find                           # list the Pidgeon servers on this network
pidgeon pull                           # pull from the server on this network into ./<its name>
pidgeon pull http://192.168.1.20:8080/#/Photos ~/Photos   # one folder, into ~/Photos
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
| `--limit` | — | Cap the total download speed, e.g. `5M` (per second). Changeable live from the dashboard. |
| `--client-limit` | — | Cap each device's download speed, e.g. `2M`. |
| `--no-thumbs` | off | Don't make image thumbnails for the grid view. Thumbnails are cached in your user cache folder, never in the served folder. |
| `--inbox-sort` | — | `date`: file uploads into `YYYY/MM` folders by when they were taken (EXIF or the video's header, else the file's modification time). Needs `--inbox`. |
| `--tls` | — | Serve HTTPS: `auto` (a self-signed certificate, kept in your config folder so its fingerprint stays the same) or `cert.pem,key.pem`. |
| `--tv` | off | Also be a DLNA / UPnP media server for TVs, consoles, VLC and Kodi. Not with `--tls`. |
| `--tv-name` | `Pidgeon · <folder>` | The name TVs show. |
| `--receipts` | — | Append a delivery receipt (device, file, time, size, SHA-256) to this file, one JSON line each. |
| `--no-mdns` | off | Don't announce the server on the network (`pidgeon find`, `pull` and the web UI's Nearby list use the announcement). |
| `--flock` | off | Open the Nearby servers in place in the web UI, as folders, through this server. Not with `--no-mdns`. |
| `--version` | | Print the version and exit. |

Several paths (`pidgeon ~/Movies ~/Photos report.pdf`) are served side by side:
each becomes a top-level entry, with `Photos (2)` for clashing names. Each one
gets its own `os.Root`, so nothing outside them is reachable.

Before the dashboard opens, every URL is printed to the terminal, so they stay in
your scrollback after you quit. The splash is skipped when you pass `--no-splash`,
when `NO_COLOR` is set, or when the terminal is too small. Press any key to skip it.

### Dashboard keys

Everything works with the arrow keys, Enter and Esc alone:

| Key | Action |
|---|---|
| `↑` `↓` | Move through the listeners, then on down into the transfers (one continuous list) |
| `enter` | On a listener, turn it on or off live. On a transfer, open its actions (cancel). |
| `→` | Actions for the selected row: enable or disable, copy URL, open in browser; for a transfer pause, limit speed or cancel |
| `esc` | Main menu: show all keys, bandwidth limits, or quit. `esc` (or `←`) also closes any open menu. |

Letter shortcuts for the same actions: `space` toggle, `tab` switch panel,
`x` cancel transfer, `p` pause or resume transfer, `c` copy URL (OSC 52 clipboard), `o` open in browser,
`s` new share link, `m` Pigeon Post, `d` switch the bottom panel between requests and deliveries,
`?` all keys, `q` quit. If downloads are in progress, `q` asks you to press it
again, and the quit item in the Esc menu shows how many will be cancelled.
`ctrl+c` always quits immediately.

The QR code follows the listener under the cursor. If that listener isn't serving,
it falls back to the primary LAN address. With `--tls` it also shows the start of the
certificate's fingerprint.

From the Esc menu:

- **Create a share link…** picks a file or folder, then the expiry, the number of
  downloads and an optional PIN (random or your own). The visitor types the PIN once;
  ten wrong guesses lock the link.
- **Request files…** (with `--inbox`) makes a link the other way round: its page is a
  drop zone, and the files land in their own folder of the inbox. Its limit counts files.
- **Pigeon Post…** sends text to a device's browser. Text a device sends pops up in
  the dashboard, where `c` copies it, `o` opens a link in it and `r` replies.
- **Deliveries…** lists every file and ZIP that reached a device in full, with its
  SHA-256 (`--receipts` also writes them to a file).

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

Files this device already downloaded in full are ticked (✓) until they change on
disk. The speech-bubble button in the top bar opens **Pigeon Post**: paste a link or a
note and it shows up in the host's terminal; text the host sends pops up as a
notification. The sidebar lists other Pidgeon servers found on the network under
**Nearby**. The command palette can **Request Files from Someone…** (a file-request
link, with `--inbox`) and **Play Folder in a Media Player (.m3u)**, a playlist VLC,
mpv or Infuse streams straight from the server.

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
| Right-click | Download, Open Preview, Add to Basket, Copy Link, **Copy as cURL**, Show SHA-256; on a folder, Play as Playlist (.m3u) |
| `⇧B` | Add the selection to the basket, or take it out |

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
directory, use `pidgeon pull`.

### Uploads

With `--inbox <dir>`, visitors can drop files anywhere on the page to send them to
you. Before sending, the browser asks which files the inbox already has (by size and
a quick fingerprint) and skips them, and the server drops any exact copy that still
arrives, so backing up a phone twice only sends what's new. The sender's modification
time is kept. `--inbox-sort date` files everything into `YYYY/MM` folders by when it
was taken.

### The flock

With `--flock`, the servers in the sidebar's **Nearby** list open in place instead of
in a new tab: each one is a folder with its own subtree, and its files list, preview,
download, hash and go into the basket like local ones. This server fetches them from
the other one for you, so a ZIP (or the basket) can mix files from several machines;
each server's files go in a folder named after it. Downloads show in the dashboard and
the downloads panel like any other.

A server that needs a token asks for it in place of its listing. The token is kept in
a cookie of that browser only, so typing it gives nobody else access. The other
server sees the requests as coming from this computer (so with `--ask`, it approves
this computer). Share links, playlists and TV mode stay limited to this server's own
files, and the flock is one hop deep: a server never passes on a request that another
flock relayed.

`pidgeon pull` works on a flock folder too (`…/#/.flock/<id>/Photos`), for a server
that needs no token.

### `pidgeon pull` and `pidgeon find`

```sh
pidgeon find                                   # servers on this network (mDNS)
pidgeon pull [url] [dir]                       # a folder (or file) into dir
pidgeon pull --mirror --verify <url> dir       # also replace changed files; check every SHA-256
```

`pull` takes the server's address, a folder link copied from the web UI
(`…/#/Photos`), a file link (`…/f/…`), or a share link (`…/s/…`, with `--pin` if it
has one). Without a URL it finds the server on the network, asking which one if there
are several. The target defaults to the remote folder's name, like `git clone`.

Folders are recreated as they are; a file arrives as `<name>.part` and is renamed when
complete, with the server's modification time. An interrupted run resumes the
`.part` files, and dropped connections are retried the same way. Files already there
with the same size and time are skipped, so running it again only fetches what's new.
A local file that differs is kept unless you pass `--mirror`. `--verify` compares
every file with the server's SHA-256. `-j` sets how many files download at once
(default 4). `--token` is needed for a protected server (or `?token=` in the URL).

Over HTTPS, a self-signed certificate is accepted when its fingerprint matches
`--fingerprint`, the one the server announces on the network, or, on a terminal,
after you compare it with the dashboard's and say yes.

### TV mode

`--tv` announces the served folder on the network (SSDP) as a DLNA / UPnP media
server, so smart TVs, consoles, VLC and Kodi list it under media servers. They browse
folders, videos, music and photos (with thumbnails) and play with seeking. Only media
files are offered.

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

If PowerShell refuses to run scripts, or you'd rather double-click, use
`install.bat` instead. It runs `install.ps1` with the same arguments
(`install.bat -AddToPath`) and bypasses the execution policy.

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
- Without `--tls`, both are sent in plain HTTP. They keep out casual visitors on a
  network you mostly trust, not someone sniffing it. `--tls auto` encrypts
  everything with a self-signed certificate. Browsers warn about it once, because
  they can't know it is yours: check that the SHA-256 fingerprint they show matches
  the one in the banner and the dashboard. A VPN listener (e.g. Tailscale) is the
  other way across untrusted networks.
- Share links can require a PIN. The link alone then isn't enough, and ten wrong
  PINs lock it.
- `--tv` makes the media files (videos, music, pictures) playable by anything on the
  network, even with `--token`, because TVs can't log in. They are reached through a
  random path that only the network announcement reveals. `--ask` still makes a TV
  wait for your approval like any new device.
- `.m3u` playlists carry the access token in their links, because media players
  can't log in. Treat a playlist like a link.
- The server announces itself on the local network (mDNS) with its folder's name,
  port, and whether it needs a token. `--no-mdns` turns that off.
- `--flock` lets anyone who can reach this server browse the other servers it finds,
  even ones they couldn't reach directly (from a VPN listener, say). It only connects
  to the address a server's own announcement came from, never to loopback. Files it
  relays are sandboxed like served HTML, whatever type the other server claims. A
  server that needs a token stays closed until each visitor types it.

What's always enforced:

- The served folder is read-only. There are no delete or rename endpoints, and the
  only writable place is the inbox (`--inbox`), which uploads and file requests
  write into through their own `os.Root` and never overwrite. Archives (`.zip`,
  `.tar`, `.tar.gz`) are browsed by reading them in place; nothing is ever extracted
  to disk.
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
  server/     http.Handler: /api/ls, /api/peek, /f/<path>, /api/zip + /zip/<token>, /api/events, auth,
              share links + file requests, the inbox, Pigeon Post, playlists, DLNA (/dlna/<key>/)
  stats/      central store: atomic counters, transfer registry, 60s history, request log, receipts
  tui/        Bubble Tea app: splash → dashboard (Lipgloss, Bubbles key/help)
  web/        embed.FS with index.html, app.css, app.js (vanilla JS, no build step)
  pull/       `pidgeon pull` and `pidgeon find`
  certs/      the --tls certificate (self-signed, kept between runs) and fingerprint pinning
  mcast/      IPv4 multicast on every interface, for discovery
  mdns/       announces the server as _pidgeon._tcp and finds the others (and --flock relays to them)
  ssdp/       announces --tv's media server to TVs
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
