// Pidgeon web UI — a file browser in VS Code's colours for browsing and downloading.
//
// Big folders stay responsive: only the rows on screen are in the DOM, and
// the subfolders on screen are summarised lazily (item count, whether they
// have subfolders) through one batched /api/peek request. Listings are
// cached and prefetched on hover, so opening a folder is usually instant.
//
// Downloads are ordinary browser downloads (streamed straight to disk), the
// only approach that works for large files on phones. The page can't see a
// native download's progress, so each download URL carries an ID
// (?dl=<client>.<n>) and /api/events reports the bytes the server has sent.
// The same event stream tells the page when the folder it shows changes on
// disk, and delivers files the host sends from the terminal dashboard.
//
// Archives (.zip, .tar, .tar.gz) open like folders; the server reads them in
// place. The grid view shows thumbnails of images. The basket collects files
// and folders from anywhere in the tree into one ZIP.
//
// With the host's --flock on, the other Pidgeon servers in the Nearby list
// open in place: their folders live under /.flock/<id>/ and the server
// fetches them from there, so they browse, preview, download and go into
// ZIPs and the basket like local ones.
//
// Pigeon Post carries short text between this page and the host's terminal.
// Files this device already received in full are ticked in the listing (from
// the server's delivery receipts). Uploads skip files the host already has.

const $ = (sel, el = document) => el.querySelector(sel);
const isMac = /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent);
const MOD = isMac ? "⌘" : "Ctrl+";
const touch = matchMedia("(hover: none)").matches;

const state = {
  path: "/",
  entries: [],             // raw listing of the current folder
  view: [],                // entries as displayed (sorted)
  sel: new Map(),          // name -> entry, within the current folder
  cursor: 0,               // keyboard cursor index into view
  anchor: 0,               // shift-selection anchor
  sort: loadJSON("sort", { key: "name", dir: 1 }),
  layout: loadJSON("layout", "list"), // "list" (table) or "grid" (tiles)
  info: { root: "", auth: "open" },
  downloads: new Map(),    // id -> download record
  seq: 0,
  loadToken: 0,
  sizeToken: 0,
  folderBytes: null,       // { bytes, partial } for selected folders
  tree: new Map(),         // path -> { open, dirs: string[] | null, loading }
  cache: new Map(),        // path -> { entries, at } recent listings, oldest first
  peek: new Map(),         // folder path -> { items, dirs } from /api/peek
  win: null,               // [first, last) row indices currently in the DOM
  fresh: new Set(),        // names that just appeared in this folder (highlighted)
  reveal: null,            // name to put the cursor on once the next folder shows
  zipStore: loadJSON("zipStore", false), // never compress ZIPs: exact size, resumable
  zipSums: loadJSON("zipSums", false),   // add SHA256SUMS to ZIPs
  arcOf: new Map(),        // folder path -> the archive it is inside ("" if none)
  partialArc: new Set(),   // archive folders whose listing is incomplete
  thumbFail: new Set(),    // image paths the server couldn't thumbnail
  basket: new Map(loadJSON("basket", []).map((b) => [b.path, b])), // path -> { path, name, dir, size }
  basketPaths: loadJSON("basketPaths", true), // keep folder paths in the basket ZIP
  basketBytes: null,       // { bytes, partial } for the basket's folders
  posts: new Map(),        // id -> Pigeon Post message
  postUnread: 0,           // messages from the host not seen in the panel yet
  nearby: [],              // other Pidgeon servers on the network, from /api/nearby
  flock: new Map(),        // peer id -> nearby server, for those that open in place
};

const clientId = getClientId();
const coll = new Intl.Collator(undefined, { numeric: true, sensitivity: "base" });

/* ------------------------------------------------------------------ */
/* helpers                                                            */
/* ------------------------------------------------------------------ */

/** Reads a JSON value from localStorage, with a fallback. */
function loadJSON(key, fallback) {
  try { const v = JSON.parse(localStorage.getItem(key)); return v ?? fallback; } catch { return fallback; }
}

/** Writes a JSON value to localStorage, ignoring storage errors. */
function saveJSON(key, v) {
  try { localStorage.setItem(key, JSON.stringify(v)); } catch {}
}

/** Returns a stable random ID for this tab, used to scope progress events. */
function getClientId() {
  try {
    let id = sessionStorage.getItem("cid");
    if (!id) { id = randomId(); sessionStorage.setItem("cid", id); }
    return id;
  } catch { return randomId(); }
}

/** Generates a 16-character URL-safe random ID. */
function randomId() {
  const a = new Uint8Array(12);
  crypto.getRandomValues(a);
  return btoa(String.fromCharCode(...a)).replace(/[+/=]/g, (c) => ({ "+": "-", "/": "_", "=": "" })[c]);
}

const UNITS = ["B", "KB", "MB", "GB", "TB"];
/** Formats a byte count (1024-based, like the terminal UI). */
function fmtBytes(n) {
  if (n == null || n < 0) return "—";
  let i = 0;
  // Divide down until the value fits the unit.
  while (n >= 1024 && i < UNITS.length - 1) { n /= 1024; i++; }
  if (i === 0) return `${n} B`;
  return `${n < 10 ? n.toFixed(2) : n < 100 ? n.toFixed(1) : n.toFixed(0)} ${UNITS[i]}`;
}

/** Formats seconds compactly ("42s", "3m 05s", "1h 02m"). */
function fmtDur(s) {
  if (!isFinite(s) || s < 0) return "";
  s = Math.round(s);
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m ${String(s % 60).padStart(2, "0")}s`;
  return `${Math.floor(s / 3600)}h ${String(Math.floor((s % 3600) / 60)).padStart(2, "0")}m`;
}

/** Formats a timestamp as YYYY-MM-DD HH:MM (sortable, developer-friendly). */
function fmtDate(ms) {
  const d = new Date(ms), p = (n) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

/** Formats a timestamp relative to now for the compact mobile row. */
const rtf = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" });
function fmtAgo(ms) {
  const s = (Date.now() - ms) / 1000;
  if (s < 0 || s > 30 * 86400) return fmtDate(ms).slice(0, 10);
  if (s < 60) return "just now";
  if (s < 3600) return rtf.format(-Math.floor(s / 60), "minute");
  if (s < 86400) return rtf.format(-Math.floor(s / 3600), "hour");
  return rtf.format(-Math.floor(s / 86400), "day");
}

/** Escapes text for safe insertion into HTML. */
const esc = (s) => String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);
/** Joins a folder path and a child name. */
const join = (dir, name) => (dir === "/" ? "/" : dir + "/") + name;
/** Returns the parent of a folder path. */
const parentOf = (p) => (p === "/" || flockOf(p)?.root === p ? "/" : p.slice(0, p.lastIndexOf("/")) || "/");

/* the flock: folders on other servers, "/.flock/<id>/..." */

const FLOCK_RE = /^\/\.flock\/([0-9a-f]+)(?=\/|$)/;
/** For a path on another server, its peer ID and that server's root
 *  ("/.flock/<id>"); null for a path on this one. */
function flockOf(p) {
  const m = FLOCK_RE.exec(p);
  return m ? { id: m[1], root: m[0] } : null;
}
/** A flock peer's name (its ID until the Nearby list has named it). */
const peerName = (id) => state.flock.get(id)?.name || id;
/** Where a folder is, as people read it: a server's root shows as its name. */
function placeName(p) {
  const f = flockOf(p);
  if (f) return peerName(f.id) + p.slice(f.root.length);
  return p === "/" ? state.info.root || "/" : p.slice(1);
}
/** Percent-encodes each path segment, keeping the slashes. */
const encPath = (p) => p.split("/").map(encodeURIComponent).join("/");
/** An inline reference to an icon in the sprite. */
const icon = (id, cls = "i") => `<svg class="${cls}"><use href="#${id}"/></svg>`;

/* file types: Seti-style colours, and language badges for source files */

const MEDIA = {
  image: "jpg jpeg png gif webp heic heif avif bmp tif tiff ico raw cr2 nef arw dng",
  video: "mp4 m4v mov mkv webm avi wmv flv mpg mpeg 3gp ts",
  audio: "mp3 m4a aac wav flac ogg oga opus wma aiff",
  archive: "zip gz tgz bz2 xz zst 7z rar tar iso dmg apk ipa jar deb rpm whl",
  pdf: "pdf",
};
const LANG = {
  go: ["GO", "blue"], js: ["JS", "yellow"], mjs: ["JS", "yellow"], cjs: ["JS", "yellow"], jsx: ["JSX", "blue"],
  ts: ["TS", "blue"], tsx: ["TSX", "blue"], py: ["PY", "blue"], ipynb: ["NB", "orange"], rb: ["RB", "red"],
  rs: ["RS", "orange"], c: ["C", "blue"], h: ["H", "purple"], cc: ["C++", "blue"], cpp: ["C++", "blue"],
  hpp: ["H++", "purple"], java: ["JV", "red"], kt: ["KT", "orange"], swift: ["SW", "orange"], cs: ["C#", "blue"],
  php: ["PHP", "purple"], sh: ["$_", "green"], bash: ["$_", "green"], zsh: ["$_", "green"], fish: ["$_", "green"],
  ps1: ["PS", "blue"], bat: ["BAT", "yellow"], html: ["<>", "orange"], htm: ["<>", "orange"], css: ["#", "blue"],
  scss: ["#", "pink"], sass: ["#", "pink"], less: ["#", "blue"], vue: ["V", "green"], svelte: ["S", "red"],
  json: ["{}", "yellow"], jsonc: ["{}", "yellow"], yaml: ["YML", "purple"], yml: ["YML", "purple"],
  toml: ["TML", "grey"], ini: ["INI", "grey"], env: ["ENV", "yellow"], xml: ["<>", "orange"], svg: ["SVG", "yellow"],
  sql: ["SQL", "pink"], lua: ["LUA", "blue"], dart: ["DRT", "blue"], ex: ["EX", "purple"], exs: ["EX", "purple"],
  erl: ["ERL", "red"], hs: ["HS", "purple"], zig: ["ZIG", "orange"], nim: ["NIM", "yellow"], r: ["R", "blue"],
  md: ["M↓", "blue"], mdx: ["M↓", "yellow"], tex: ["TEX", "green"], proto: ["PB", "red"], tf: ["TF", "purple"],
  gradle: ["GR", "blue"], lock: ["LCK", "grey"], log: ["LOG", "grey"], csv: ["CSV", "green"], tsv: ["TSV", "green"],
  txt: ["TXT", "grey"], diff: ["±", "green"], patch: ["±", "green"], wasm: ["WA", "purple"],
};
const NAMED = { dockerfile: ["DK", "blue"], makefile: ["MK", "orange"], license: ["LIC", "yellow"], readme: ["i", "blue"], ".gitignore": ["GIT", "orange"] };
const EXT_MEDIA = {};
// Invert the media table: extension -> type.
for (const [t, exts] of Object.entries(MEDIA)) for (const e of exts.split(" ")) EXT_MEDIA[e] = t;

/** Classifies an entry: { kind, html } where html is its 16px icon. */
function fileIcon(e) {
  if (e.dir) return { kind: "folder", html: `<span class="fi t-folder">${icon("i-folder", "")}</span>` };
  const lower = e.name.toLowerCase();
  const dot = lower.lastIndexOf(".");
  const ext = dot > 0 ? lower.slice(dot + 1) : "";
  const named = NAMED[lower] || NAMED[lower.split(".")[0]];
  const lang = named || LANG[ext];
  // Source and config files get a coloured language badge.
  if (lang) return { kind: "code", html: `<span class="fi lang t-${lang[1]}">${esc(lang[0])}</span>` };
  const media = EXT_MEDIA[ext];
  if (media) return { kind: media, html: `<span class="fi t-${media}">${icon("i-" + media, "")}</span>` };
  return { kind: "file", html: `<span class="fi t-file">${icon("i-file", "")}</span>` };
}

/* notifications */

/** Shows a VS Code-style notification in the bottom-right corner. */
function note(msg, kind = "info") {
  const el = document.createElement("div");
  el.className = "note " + kind;
  el.innerHTML = icon(kind === "err" ? "i-error" : kind === "ok" ? "i-check" : "i-info") + `<span>${esc(msg)}</span>`;
  $("#notes").append(el);
  setTimeout(() => el.remove(), kind === "err" ? 6000 : 3200);
  // Keep the stack short.
  while ($("#notes").children.length > 3) $("#notes").firstChild.remove();
}

/** Copies text to the clipboard. Falls back to execCommand because the
 *  async Clipboard API only exists on HTTPS/localhost, and LAN URLs are http. */
async function copyText(text, what) {
  try {
    if (navigator.clipboard && isSecureContext) await navigator.clipboard.writeText(text);
    else {
      const ta = Object.assign(document.createElement("textarea"), { value: text });
      ta.style.cssText = "position:fixed;opacity:0";
      document.body.append(ta);
      ta.select();
      document.execCommand("copy");
      ta.remove();
    }
    note(`Copied ${what} to the clipboard`, "ok");
  } catch {
    note("Couldn't access the clipboard", "err");
  }
}

/** Fetches JSON, sending the user back to the login page on 401. */
async function api(url, opts) {
  const r = await fetch(url, opts);
  // Session expired or token changed: the root shows the token prompt.
  if (r.status === 401) { location.href = "/"; throw new Error("unauthorized"); }
  if (!r.ok) {
    const err = new Error((await r.text()).trim() || r.statusText);
    // A server in the flock that wants credentials says which kind.
    err.login = r.headers.get("X-Pidgeon-Flock-Login") || "";
    throw err;
  }
  return r.json();
}

/* ------------------------------------------------------------------ */
/* splash                                                             */
/* ------------------------------------------------------------------ */

/** Plays the hexagon splash once per tab session; never blocks loading. */
function runSplash() {
  const el = $("#splash");
  const reduce = matchMedia("(prefers-reduced-motion: reduce)").matches;
  let seen = false;
  try { seen = sessionStorage.getItem("splashed") === "1"; sessionStorage.setItem("splashed", "1"); } catch {}
  // Skip for repeat visits in this tab and for reduced-motion users.
  if (seen || reduce || !el.animate) { el.remove(); return; }

  el.hidden = false;
  const POP = 140, LAP = 620, FLASH = 80, FADE = 450;
  // Three dashes share one head position: a short bright head, a blue body
  // and a long faint purple tail, which reads as a comet with a taper.
  // With pathLength=100 a dash of length L whose head is at h needs
  // dashoffset = L - h, so the lap animates h from 0 to 100.
  for (const [cls, len] of [["c1", 4], ["c2", 11], ["c3", 22]]) {
    const p = $(".hex-comet." + cls, el);
    p.style.strokeDasharray = `${len} ${100 - len}`;
    p.animate(
      [{ strokeDashoffset: len, opacity: 0 }, { opacity: 1, offset: 0.08 }, { strokeDashoffset: len - 100, opacity: 1 }],
      { duration: LAP, delay: POP, easing: "cubic-bezier(.65,0,.35,1)", fill: "forwards" },
    );
  }
  const finish = () => {
    // Already fading or gone: nothing to do.
    if (!el.isConnected || el.classList.contains("fade")) return;
    el.classList.add("fade");
    setTimeout(() => el.remove(), FADE + 50);
  };
  setTimeout(() => el.classList.add("flash"), POP + LAP);
  setTimeout(finish, POP + LAP + FLASH);
  el.addEventListener("click", finish, { once: true });
}

/* ------------------------------------------------------------------ */
/* theme                                                              */
/* ------------------------------------------------------------------ */

/** Applies a theme preference: "dark", "light" or "auto". */
function setTheme(pref, announce) {
  const dark = pref === "dark" || (pref === "auto" && !matchMedia("(prefers-color-scheme: light)").matches);
  const root = document.documentElement;
  root.dataset.theme = dark ? "dark" : "light";
  root.dataset.themePref = pref;
  try { localStorage.setItem("theme", pref); } catch {}
  $('meta[name="theme-color"]').content = dark ? "#181818" : "#f8f8f8";
  // The toggle shows what you'd switch to.
  $("use", $("#theme-btn")).setAttribute("href", dark ? "#i-sun" : "#i-moon");
  $("#theme-btn").title = dark ? "Switch to light mode" : "Switch to dark mode";
  if (announce) note(`Color theme: ${pref === "auto" ? "follow system" : dark ? "Dark Modern" : "Light Modern"}`);
}

/** Flips between dark and light. */
const toggleTheme = () => setTheme(document.documentElement.dataset.theme === "dark" ? "light" : "dark", true);

/* ------------------------------------------------------------------ */
/* layout                                                             */
/* ------------------------------------------------------------------ */

/** Applies a file layout: "list" (the table) or "grid" (icon tiles). */
function setLayout(layout) {
  const grid = layout === "grid";
  state.layout = grid ? "grid" : "list";
  saveJSON("layout", state.layout);
  $("#table").classList.toggle("grid", grid);
  // Like the theme toggle, the button shows what you'd switch to.
  const b = $("#layout-btn");
  $("use", b).setAttribute("href", grid ? "#i-list" : "#i-grid");
  b.title = grid ? "Switch to list view" : "Switch to grid view";
  b.setAttribute("aria-label", b.title);

  // Keep the first entry on screen in view across the switch: the scroll
  // offset means something different once rows become lines of tiles.
  const list = $("#list");
  const first = Math.floor(list.scrollTop / rowH) * cols;
  measureRow();
  // Nothing drawn yet (first boot, or an empty folder): nothing to keep.
  if (!state.view.length) return;
  list.scrollTop = Math.floor(first / cols) * rowH;
  renderWindow(true);
}

/** Flips between list and grid. */
const toggleLayout = () => setLayout(state.layout === "grid" ? "list" : "grid");

/** Number of tiles per line in grid view (1 in list view), for arrow keys. */
const gridCols = () => cols;

/* ------------------------------------------------------------------ */
/* browsing                                                           */
/* ------------------------------------------------------------------ */

/** Returns the folder path encoded in the URL hash ("#/a/b"). */
function pathFromHash() {
  let p = "/";
  try { p = decodeURIComponent(location.hash.slice(1)) || "/"; } catch {}
  if (!p.startsWith("/")) p = "/" + p;
  return p.length > 1 ? p.replace(/\/+$/, "") : "/";
}

/** Navigates to a folder (via the hash, so Back works). */
function go(path) {
  const h = "#" + encPath(path);
  if (location.hash === h || (path === "/" && !location.hash)) load(path);
  else location.hash = h;
}

/* listing cache */

const CACHE_MAX = 64;          // folders kept in memory
const FRESH_MS = 10_000;       // a listing this recent is shown without refetching
const PREFETCH_MAX = 5000;     // don't prefetch folders bigger than this on hover
const inflight = new Map();    // path -> Promise of entries, shared by all callers

/** Fetches a folder listing. Concurrent callers share one request, and the
 *  result is cached and used to seed the tree and the folder summaries. */
function fetchListing(path) {
  if (inflight.has(path)) return inflight.get(path);
  const req = api("/api/ls?path=" + encodeURIComponent(path))
    .then((d) => {
      // Remember whether this folder is inside an archive (for the header).
      state.arcOf.set(path, d.archive || "");
      if (d.partial) state.partialArc.add(path); else state.partialArc.delete(path);
      rememberListing(path, d.entries);
      return d.entries;
    })
    .finally(() => inflight.delete(path));
  inflight.set(path, req);
  return req;
}

/** Stores a listing in the cache (evicting the oldest) and in the tree. */
function rememberListing(path, entries) {
  // Re-insert so the Map's order stays oldest-first.
  state.cache.delete(path);
  state.cache.set(path, { entries, at: Date.now() });
  while (state.cache.size > CACHE_MAX) state.cache.delete(state.cache.keys().next().value);
  const dirs = entries.filter((e) => e.dir).map((e) => e.name).sort(coll.compare);
  treeNode(path).dirs = dirs;
  state.peek.set(path, { items: entries.length, dirs });
}

/** Warms the cache for a folder the user is likely to open next. */
function prefetch(path) {
  const c = state.cache.get(path), pk = state.peek.get(path);
  // Already fresh, already loading, or too big to fetch speculatively.
  if ((c && Date.now() - c.at < FRESH_MS) || inflight.has(path) || (pk?.items ?? 0) > PREFETCH_MAX) return;
  fetchListing(path).then(scheduleTree, () => {});
}

/** Loads and renders a folder; stale responses are ignored. A cached
 *  listing is shown at once and then revalidated in the background. */
async function load(path, { force = false } = {}) {
  const token = ++state.loadToken;
  const from = state.path;
  state.path = path;
  state.sel.clear();
  state.cursor = 0;
  renderCrumbs();
  revealInTree(path);
  closeDrawer();
  watchFolder(path);
  // A refresh also forgets what we knew about the subfolders.
  if (force) for (const p of state.peek.keys()) if (parentOf(p) === path && p !== path) state.peek.delete(p);

  const cached = state.cache.get(path);
  if (cached) showEntries(cached.entries, from);
  else { renderSkeleton(); updateSelection(); }
  // Fetched moments ago (usually a hover prefetch): nothing to revalidate.
  if (cached && !force && Date.now() - cached.at < FRESH_MS) return;
  try {
    const entries = await fetchListing(path);
    // A newer navigation started while this one was in flight.
    if (token !== state.loadToken) return;
    if (cached) refreshEntries(entries); else showEntries(entries, from);
    scheduleTree();
  } catch (err) {
    if (token !== state.loadToken) return;
    // Keep showing the cached copy; just say it may be out of date.
    if (cached) { note("Couldn't refresh this folder: " + err.message, "err"); return; }
    state.entries = [];
    state.view = [];
    state.win = null;
    $("#list").innerHTML = "";
    if (err.login) showFlockLogin(err);
    else showEmpty("Couldn't open this folder", err.message, true);
    updateStatus();
  }
}

/** Renders a freshly opened folder's entries. */
function showEntries(entries, from) {
  state.entries = entries;
  renderList();
  // Coming back up: put the cursor on the folder we came from.
  if (parentOf(from) === state.path && from !== state.path) {
    const i = state.view.findIndex((e) => e.name === from.slice(from.lastIndexOf("/") + 1));
    if (i >= 0) setCursor(i);
  }
  // Arriving from a search result: select the file that was picked.
  if (state.reveal) {
    const i = state.view.findIndex((e) => e.name === state.reveal);
    state.reveal = null;
    if (i >= 0) { setCursor(i); selectRange(i, i); state.anchor = i; selectionChanged(); }
  }
  if (!touch && !document.activeElement?.matches("input")) $("#list").focus({ preventScroll: true });
}

/** Swaps in a revalidated listing, keeping scroll, cursor and selection. */
function refreshEntries(entries) {
  const same = entries.length === state.entries.length && entries.every((e, i) => {
    const o = state.entries[i];
    return e.name === o.name && e.dir === o.dir && e.size === o.size && e.mtime === o.mtime;
  });
  // Nothing changed on disk: leave the DOM alone.
  if (same) return;
  const cur = state.view[state.cursor]?.name;
  const byName = new Map(entries.map((e) => [e.name, e]));
  // Keep selected entries that still exist, pointing at their new records.
  for (const [name] of state.sel) byName.has(name) ? state.sel.set(name, byName.get(name)) : state.sel.delete(name);
  state.entries = entries;
  renderList({ keepScroll: true });
  const i = state.view.findIndex((e) => e.name === cur);
  if (i >= 0) state.cursor = i;
  renderWindow(true);
  selectionChanged();
}

/** Renders the breadcrumbs (ancestors only) and the folder title. */
function renderCrumbs() {
  const f = flockOf(state.path);
  // In the flock the trail starts at the other server, not at this one.
  const top = f ? { name: peerName(f.id), path: f.root } : { name: state.info.root || "Files", path: "/" };
  const parts = state.path === top.path ? [] : state.path.slice(top.path.length + (top.path === "/" ? 0 : 1)).split("/");
  let acc = top.path;
  let html = parts.length ? `<a href="#${esc(encPath(top.path))}">${esc(top.name)}</a>` : "";
  // One link per ancestor; the current folder is the title below.
  parts.slice(0, -1).forEach((p) => {
    acc = join(acc, p);
    html += icon("i-chev", "i sep") + `<a href="#${esc(encPath(acc))}">${esc(p)}</a>`;
  });
  const nav = $("#crumbs");
  nav.innerHTML = html;
  nav.hidden = !parts.length;
  nav.scrollLeft = nav.scrollWidth;
  const title = parts.at(-1) || top.name;
  $("#folder-title").textContent = title;
  $("#folder-title").title = title;
  renderArchiveChip();
  $("#cc-label").textContent = `${title} — ${state.info.name || "Pidgeon"}`;
  document.title = `${title} — ${state.info.name || "Pidgeon"}`;
}

/** The archive the current folder is inside ("" if none). Until the
 *  listing says, a path segment with an archive extension is a good guess. */
function currentArchive() {
  const known = state.arcOf.get(state.path);
  if (known !== undefined) return known;
  const segs = state.path.split("/");
  const i = segs.findIndex((seg) => ARCHIVE_RE.test(seg));
  return i > 0 ? segs.slice(0, i + 1).join("/") : "";
}
const ARCHIVE_RE = /\.(zip|tar|tgz|tar\.gz)$/i;

/** Shows the "inside an archive" (or "on another server") chip in the
 *  folder header. */
function renderArchiveChip() {
  const arc = currentArchive(), chip = $("#fh-chip"), f = flockOf(state.path);
  chip.hidden = !arc && !f;
  if (!arc && f) {
    chip.innerHTML = `<span class="fi ic-peer">${icon("i-hex", "")}</span><span>on ${esc(peerName(f.id))} · through ${esc(state.info.root || "this server")}</span>`;
    chip.title = "These files are on another computer; this server fetches them for you";
    return;
  }
  if (!arc) return;
  const name = arc.slice(arc.lastIndexOf("/") + 1);
  const partial = state.partialArc.has(state.path) ? " · too big to list completely" : "";
  chip.innerHTML = `<span class="fi t-archive">${icon("i-archive", "")}</span><span>${arc === state.path ? "archive" : "inside " + esc(name)} · read in place${partial}</span>`;
  chip.title = `Browsing ${arc} without extracting it`;
}

/** Shows placeholder rows while a folder loads. */
function renderSkeleton() {
  $("#state").hidden = true;
  state.win = null;
  const row = (w) => `<li class="row skel"><span></span><span class="name-cell"><span class="bar" style="width:${w}%"></span></span><span class="size"><span class="bar" style="width:50%;margin-left:auto"></span></span><span></span><span></span></li>`;
  $("#list").innerHTML = [46, 30, 58, 24, 40, 52].map(row).join("");
}

/** Shows an empty/error message in place of the list. */
function showEmpty(title, msg, retry) {
  const st = $("#state");
  st.innerHTML = `${icon("i-files", "i big")}<h3>${esc(title)}</h3><div>${esc(msg)}</div>` +
    (retry ? `<button class="btn secondary" id="retry">Retry</button>` : "");
  st.hidden = false;
  if (retry) $("#retry").onclick = () => load(state.path);
}

/** Sorts entries for display: folders first, then by the chosen column. */
function sortEntries(list) {
  const { key, dir } = state.sort;
  return [...list].sort((a, b) => {
    // Folders always group above files, as in the VS Code explorer.
    if (a.dir !== b.dir) return a.dir ? -1 : 1;
    const c = key === "name" ? coll.compare(a.name, b.name) : (a[key] - b[key]) || coll.compare(a.name, b.name);
    return c * dir;
  });
}

/** Renders the file table. Only the rows on screen are built (see
 *  renderWindow), so folders with tens of thousands of entries stay smooth. */
function renderList({ keepScroll = false } = {}) {
  state.view = sortEntries(state.entries);
  state.cursor = Math.min(state.cursor, Math.max(0, state.view.length - 1));
  $("#state").hidden = true;

  // Reflect the sort order in the column headers.
  document.querySelectorAll(".sort").forEach((b) => {
    const on = b.dataset.sort === state.sort.key;
    b.classList.toggle("on", on);
    $(".arrow", b).textContent = on ? (state.sort.dir > 0 ? " ↑" : " ↓") : "";
  });

  const list = $("#list");
  state.win = null;
  if (!state.view.length) {
    list.innerHTML = "";
    showEmpty("This folder is empty", "Nothing to download here yet.");
    updateSelection();
    return;
  }
  if (!keepScroll) list.scrollTop = 0;
  renderWindow(true);
  updateSelection();
}

/* virtual scrolling: rows have a fixed height (--row), so the visible slice
   is plain arithmetic and two spacers stand in for everything else. In grid
   view a "line" is one row of `cols` tiles, each line --tile-h tall; in list
   view a line is a single row. state.win is always in entry indices. */

const OVERSCAN = 20;  // extra entries drawn above and below the viewport
let rowH = 26;        // height of one line
let cols = 1;         // entries per line

/** Reads the line height from CSS (it grows on phones) and, in grid view,
 *  works out how many tiles fit across the list. */
function measureRow() {
  const css = getComputedStyle(document.documentElement);
  const px = (name, fallback) => parseFloat(css.getPropertyValue(name)) || fallback;
  const list = $("#list");
  if (state.layout === "grid") {
    rowH = px("--tile-h", 132);
    const gap = px("--tile-gap", 6), min = px("--tile-min", 120);
    const ls = getComputedStyle(list);
    const w = list.clientWidth - parseFloat(ls.paddingLeft) - parseFloat(ls.paddingRight);
    // As many tiles of at least --tile-min as fit, gaps included.
    cols = Math.max(1, Math.floor((w + gap) / (min + gap)));
  } else {
    rowH = px("--row", 26);
    cols = 1;
  }
  // The CSS grid uses exactly this count, so the math and layout agree.
  list.style.setProperty("--cols", cols);
}

/** Draws the rows in and around the viewport, if they aren't already. */
function renderWindow(force) {
  const list = $("#list"), n = state.view.length;
  if (!n) return;
  const lines = Math.ceil(n / cols);
  const top = Math.floor(list.scrollTop / rowH);
  const bottom = Math.ceil((list.scrollTop + list.clientHeight) / rowH);
  // The drawn slice still covers the viewport: nothing to do.
  if (!force && state.win && top * cols >= state.win[0] && Math.min(n, bottom * cols) <= state.win[1]) return;
  // Whole lines only, so tiles never shift columns as the window moves.
  const over = Math.ceil(OVERSCAN / cols);
  const la = Math.max(0, top - over), lb = Math.min(lines, bottom + over);
  const a = la * cols, b = Math.min(n, lb * cols);
  state.win = [a, b];
  let html = `<li class="pad" style="height:${la * rowH}px" aria-hidden="true"></li>`;
  // Build just the slice; the spacers keep the scrollbar honest.
  for (let i = a; i < b; i++) html += rowHTML(i);
  html += `<li class="pad" style="height:${(lines - lb) * rowH}px" aria-hidden="true"></li>`;
  list.innerHTML = html;
  queuePeeks(a, b);
}

/** Scrolls the list just enough to show row i, drawing it if needed. */
function scrollToRow(i) {
  const list = $("#list"), top = Math.floor(i / cols) * rowH;
  // Above the viewport: align to the top; below: align to the bottom.
  if (top < list.scrollTop) list.scrollTop = top;
  else if (top + rowH > list.scrollTop + list.clientHeight) list.scrollTop = top + rowH - list.clientHeight;
  renderWindow();
}

/** The size column for a folder: its item count, once peeked. */
function folderSize(e) {
  const pk = state.peek.get(join(state.path, e.name));
  if (pk?.items == null) return "";
  return `${pk.items} item${pk.items === 1 ? "" : "s"}`;
}

/** The second line of a row on phones. */
function rowSub(e) {
  if (!e.dir) return `${fmtBytes(e.size)} · ${fmtAgo(e.mtime)}`;
  const count = folderSize(e);
  return count ? `${count} · ${fmtAgo(e.mtime)}` : fmtAgo(e.mtime);
}

/* thumbnails: grid tiles of images show a small server-made thumbnail
   (/api/thumb); a file the server can't thumbnail keeps its icon */

const THUMB_EXT = new Set(["jpg", "jpeg", "png", "gif", "webp", "bmp", "tif", "tiff"]);

/** Whether an entry gets a thumbnail in the grid view. */
const thumbable = (e, p) => state.layout === "grid" && state.info.thumbs && !e.dir && THUMB_EXT.has(extOf(e.name)) && !state.thumbFail.has(p);

/** The thumbnail width to ask for: the tile size at this screen density,
 *  rounded up the way the server rounds (multiples of 64). */
function thumbWidth() {
  const px = Math.min(512, 120 * Math.min(3, devicePixelRatio || 1));
  return Math.ceil(px / 64) * 64;
}

/** The icon cell of a row: a thumbnail for images in the grid, else the icon. */
function iconHTML(e, p, fi) {
  if (!thumbable(e, p)) return fi.html;
  const src = `/api/thumb?path=${encodeURIComponent(p)}&w=${thumbWidth()}&v=${e.mtime}`;
  return `<span class="fi thumb">${fi.html}<img src="${esc(src)}" alt="" loading="lazy" decoding="async" data-p="${esc(p)}"></span>`;
}

/** Builds the HTML for row i of the current view. */
function rowHTML(i) {
  const e = state.view[i];
  const fi = fileIcon(e);
  const p = join(state.path, e.name);
  const sel = state.sel.has(e.name);
  const inBasket = state.basket.has(p);
  const preview = previewKind(e)
    ? `<a class="icon-btn act-preview" href="/f${esc(encPath(p))}?inline=1" target="_blank" rel="noopener" title="Open Preview" aria-label="Preview ${esc(e.name)}">${icon("i-eye")}</a>` : "";
  return `<li class="row${e.dir ? " dir" : ""}${e.archive ? " arc" : ""}${thumbable(e, p) ? " has-thumb" : ""}${sel ? " sel" : ""}${inBasket ? " in-basket" : ""}${i === state.cursor ? " cursor" : ""}${state.fresh.has(e.name) ? " fresh" : ""}" data-i="${i}" role="option" aria-selected="${sel}"${e.archive ? ' title="Archive · double-click to look inside"' : ""}>
    <label class="cb"><input type="checkbox" tabindex="-1" ${sel ? "checked" : ""} aria-label="Select ${esc(e.name)}"><span></span></label>
    <span class="name-cell">${iconHTML(e, p, fi)}<span class="meta"><span class="name">${esc(e.name)}${gotHTML(e)}</span><span class="sub">${esc(rowSub(e))}</span></span></span>
    <span class="size">${e.dir ? esc(folderSize(e)) : fmtBytes(e.size)}</span>
    <span class="date">${fmtDate(e.mtime)}</span>
    <span class="acts">${preview}<button class="icon-btn act-link" title="Copy Link" aria-label="Copy link">${icon("i-link")}</button><button class="icon-btn act-dl" title="${e.dir ? "Download as ZIP" : "Download"}" aria-label="Download ${esc(e.name)}">${icon("i-download")}</button></span>
  </li>`;
}

/** A tick for a file this device already received in full (this version). */
function gotHTML(e) {
  if (!e.got) return "";
  return `<span class="got" title="You downloaded this ${esc(fmtAgo(e.got))}">${icon("i-check")}</span>`;
}

/* lazy folder summaries: the subfolders in the drawn rows are summarised in
   small batches, at most two requests at a time */

const PEEK_BATCH = 40;
const peekQueue = new Set();    // folder paths waiting to be summarised
const peekPending = new Set();  // folder paths in a request right now
let peekTimer = 0, peekBusy = 0;

/** Queues the not-yet-summarised folders among rows a..b. */
function queuePeeks(a, b) {
  // Collect folders we know nothing about and aren't already asking about.
  for (let i = a; i < b; i++) {
    const e = state.view[i];
    if (!e.dir) continue;
    const p = join(state.path, e.name);
    if (!state.peek.has(p) && !peekPending.has(p)) peekQueue.add(p);
  }
  // A short delay lets fast scrolling skip past folders nobody looks at.
  if (peekQueue.size && !peekTimer) peekTimer = setTimeout(flushPeeks, 80);
}

/** Sends one batch of queued folders to /api/peek. */
async function flushPeeks() {
  peekTimer = 0;
  const base = state.path;
  // Forget folders from a folder the user has since left, or scrolled past.
  const [a, b] = state.win || [0, 0];
  const visible = new Set();
  for (let i = a; i < b; i++) if (state.view[i]?.dir) visible.add(join(base, state.view[i].name));
  for (const p of peekQueue) if (!visible.has(p)) peekQueue.delete(p);
  if (!peekQueue.size || peekBusy >= 2) return;

  const batch = [...peekQueue].slice(0, PEEK_BATCH);
  const q = new URLSearchParams({ base });
  // Move the batch from queued to pending, naming folders relative to base.
  for (const p of batch) {
    peekQueue.delete(p);
    peekPending.add(p);
    q.append("p", p.slice(p.lastIndexOf("/") + 1));
  }
  peekBusy++;
  try {
    const res = await api("/api/peek?" + q);
    // Folders missing from the answer couldn't be read; don't ask again.
    for (const p of batch) applyPeek(p, res[p.slice(p.lastIndexOf("/") + 1)] || { items: null, dirs: null });
    updatePeekCells();
    scheduleTree();
  } catch {
    // Network hiccup: they'll be queued again next time they're drawn.
  } finally {
    batch.forEach((p) => peekPending.delete(p));
    peekBusy--;
  }
  if (peekQueue.size && !peekTimer) peekTimer = setTimeout(flushPeeks, 0);
}

/** Records a folder summary and lets the tree use its subfolder names. */
function applyPeek(path, info) {
  state.peek.set(path, info);
  const n = treeNode(path);
  // A full listing is more trustworthy, so only fill in unknown folders.
  if (!n.dirs && info.dirs) n.dirs = [...info.dirs].sort(coll.compare);
}

/** Refreshes the size cell and phone subtitle of every drawn folder row. */
function updatePeekCells() {
  // Folder rows only; file rows don't depend on peeks.
  for (const row of document.querySelectorAll("#list .row.dir")) {
    const e = state.view[+row.dataset.i];
    if (!e) continue;
    $(".size", row).textContent = folderSize(e);
    $(".sub", row).textContent = rowSub(e);
  }
}

/* ------------------------------------------------------------------ */
/* explorer tree                                                      */
/* ------------------------------------------------------------------ */

/** Returns (creating if needed) the tree state for a folder path. */
function treeNode(path) {
  if (!state.tree.has(path)) state.tree.set(path, { open: false, dirs: null, loading: false });
  return state.tree.get(path);
}

/** Loads a folder's subfolders for the tree (sharing the list's request). */
async function loadTreeDirs(path) {
  const n = treeNode(path);
  if (n.dirs || n.loading) return;
  n.loading = true;
  try { await fetchListing(path); } catch { n.dirs = []; }
  n.loading = false;
  scheduleTree();
}

/** Re-renders the tree once per frame, however many updates arrive. */
let treeFrame = 0;
function scheduleTree() {
  if (!treeFrame) treeFrame = requestAnimationFrame(() => { treeFrame = 0; renderTree(); });
}

/** Expands every ancestor of path so the current folder is visible. A
 *  folder in the flock unfolds under its server in the Nearby list. */
function revealInTree(path) {
  treeNode("/").open = true;
  loadTreeDirs("/");
  const f = flockOf(path);
  let acc = f ? f.root : "/";
  if (f) { treeNode(acc).open = true; loadTreeDirs(acc); }
  // Open and load each ancestor in turn.
  for (const seg of path === acc ? [] : path.slice(acc.length + (acc === "/" ? 0 : 1)).split("/")) {
    acc = join(acc, seg);
    const n = treeNode(acc);
    n.open = true;
    loadTreeDirs(acc);
  }
  renderTree();
}

/** Renders the explorer tree from the root's subfolders. */
let treeRevealed = null;
function renderTree() {
  $("#sb-root").textContent = state.info.root || "Files";
  $("#tree-root").classList.toggle("current", state.path === "/");
  $("#tree").innerHTML = treeChildren("/", 0);
  renderNearby();
  // Scroll the current folder into view once per navigation, not on every
  // lazy update, so the tree doesn't jump while you scroll it.
  if (treeRevealed !== state.path) {
    const cur = $("#tree .tnode.current, #nearby-list .tnode.current");
    if (cur || state.path === "/") { cur?.scrollIntoView({ block: "nearest" }); treeRevealed = state.path; }
  }
}

/** Renders the subfolders of path at the given depth. */
function treeChildren(path, depth) {
  const n = treeNode(path);
  if (!n.dirs) return "";
  return n.dirs.map((name) => {
    const p = join(path, name);
    const c = treeNode(p);
    const leaf = c.dirs && c.dirs.length === 0;
    let guides = "";
    // One indent guide per ancestor level.
    for (let k = 0; k < depth; k++) guides += `<span class="guide" style="left:${16 + k * 12}px"></span>`;
    return `<li role="treeitem" aria-expanded="${c.open}">
      <div class="tnode${p === state.path ? " current" : ""}" data-path="${esc(p)}" style="padding-left:${8 + depth * 12}px">${guides}
        ${icon("i-chev", "i chev" + (c.open ? " open" : "") + (leaf ? " leaf" : ""))}
        ${icon(c.open && !leaf ? "i-folder-open" : "i-folder", "i ic-folder")}
        <span class="label">${esc(name)}</span>
      </div>
      ${c.open ? `<ul role="group">${treeChildren(p, depth + 1)}</ul>` : ""}
    </li>`;
  }).join("");
}

/* ------------------------------------------------------------------ */
/* selection & cursor                                                 */
/* ------------------------------------------------------------------ */

/** Moves the keyboard cursor to index i and scrolls it into view. */
function setCursor(i) {
  if (!state.view.length) return;
  state.cursor = Math.max(0, Math.min(i, state.view.length - 1));
  document.querySelectorAll("#list .row.cursor").forEach((r) => r.classList.remove("cursor"));
  scrollToRow(state.cursor);
  $(`#list .row[data-i="${state.cursor}"]`)?.classList.add("cursor");
  // Resting on a folder for a moment warms its listing.
  clearTimeout(cursorPrefetch);
  const e = state.view[state.cursor];
  if (e.dir) cursorPrefetch = setTimeout(() => prefetch(join(state.path, e.name)), 250);
}
let cursorPrefetch;

/** Selects or deselects one entry by index, updating its row in place. */
function setSelected(i, on) {
  const e = state.view[i];
  if (!e) return;
  if (on) state.sel.set(e.name, e); else state.sel.delete(e.name);
  const row = $(`#list .row[data-i="${i}"]`);
  if (row) { row.classList.toggle("sel", on); row.setAttribute("aria-selected", on); $("input", row).checked = on; }
}

/** Replaces the selection with the inclusive index range a..b. */
function selectRange(a, b) {
  const [lo, hi] = a < b ? [a, b] : [b, a];
  state.sel.clear();
  // Select everything inside the range; only drawn rows need DOM updates.
  for (let i = Math.max(0, lo); i <= hi && i < state.view.length; i++) state.sel.set(state.view[i].name, state.view[i]);
  syncDrawnSelection();
}

/** Clears the selection. */
function clearSelection() {
  state.sel.clear();
  syncDrawnSelection();
  selectionChanged();
}

/** Makes the drawn rows match state.sel. */
function syncDrawnSelection() {
  // Rows outside the drawn slice pick up their state when they are drawn.
  for (const row of document.querySelectorAll("#list .row[data-i]")) {
    const on = state.sel.has(state.view[+row.dataset.i]?.name);
    row.classList.toggle("sel", on);
    row.setAttribute("aria-selected", on);
    $("input", row).checked = on;
  }
}

/** Call after any selection change: refreshes sizes and the UI. */
function selectionChanged() {
  refreshFolderSize();
  updateSelection();
}

/** Updates the selection toolbar, select-all box and status bar. */
function updateSelection() {
  const sel = [...state.sel.values()];
  const files = sel.filter((e) => !e.dir), folders = sel.filter((e) => e.dir);
  const fileBytes = files.reduce((s, e) => s + e.size, 0);
  let size = fmtBytes(fileBytes);
  // Folder sizes come from the server; show an ellipsis until they arrive.
  if (folders.length) {
    const fb = state.folderBytes;
    size = fb ? (fb.partial ? "≥ " : "") + fmtBytes(fileBytes + fb.bytes) : `${fmtBytes(fileBytes)} + ${folders.length} folder${folders.length > 1 ? "s" : ""}…`;
  }
  const single = sel.length === 1 && !sel[0].dir;
  $("#sel-actions").hidden = sel.length === 0;
  $("#sel-count").textContent = `${sel.length} selected · ${size}`;
  $("#sel-download span").textContent = single ? "Download" : "Download ZIP";
  $("#sb-sel").hidden = sel.length === 0;
  $("#sb-sel").textContent = `${sel.length} selected (${size})`;

  const all = $("#select-all");
  all.checked = state.view.length > 0 && sel.length === state.view.length;
  all.indeterminate = sel.length > 0 && sel.length < state.view.length;
  updateStatus();
}

/** Asks the server for the total size of the selected folders (debounced). */
let sizeTimer;
function refreshFolderSize() {
  state.folderBytes = null;
  clearTimeout(sizeTimer);
  const folders = [...state.sel.values()].filter((e) => e.dir);
  if (!folders.length) return;
  const token = ++state.sizeToken;
  sizeTimer = setTimeout(async () => {
    const q = new URLSearchParams({ base: state.path });
    folders.forEach((f) => q.append("p", f.name));
    try {
      const r = await api("/api/size?" + q);
      // Ignore answers for a selection that has since changed.
      if (token === state.sizeToken) { state.folderBytes = r; updateSelection(); }
    } catch {}
  }, 250);
}

/* ------------------------------------------------------------------ */
/* actions                                                            */
/* ------------------------------------------------------------------ */

/** Opens a folder (or looks inside an archive) or downloads a file. */
function openEntry(e) {
  if (e.dir || e.archive) go(join(state.path, e.name));
  else downloadFile(e);
}

/** Downloads the selection: one file directly, anything else as a ZIP. */
function downloadSelection() {
  const sel = [...state.sel.values()];
  if (!sel.length) return note("Nothing selected");
  if (sel.length === 1 && !sel[0].dir) downloadFile(sel[0]);
  else downloadZip(sel.map((e) => e.name));
}

/** Absolute URL of an entry (file download URL, or folder browse URL). */
function entryURL(e) {
  const p = join(state.path, e.name);
  return e.dir ? location.origin + "/#" + encPath(p) : location.origin + "/f" + encPath(p);
}

/** A curl command that downloads a file with its original name. */
function curlFor(e) {
  let auth = "";
  if (state.info.auth === "token") auth = ` -H 'Authorization: Bearer ${state.info.token}'`;
  else if (state.info.auth === "basic") auth = " -u 'USER:PASSWORD'";
  return `curl -fLOJ${auth} '${entryURL(e).replace(/'/g, "%27")}'`;
}

/** Starts a native browser download and tracks it in the panel. */
function startDownload({ name, url, kind, total, entry }) {
  const id = `${clientId}.${++state.seq}`;
  const sep = url.includes("?") ? "&" : "?";
  state.downloads.set(id, { id, name, url, kind, total, entry, sent: 0, rate: 0, state: "queued", added: Date.now() });

  // A temporary <a download> triggers a normal download without leaving the
  // page; the server's Content-Disposition makes it a save, not a view.
  const a = Object.assign(document.createElement("a"), { href: url + sep + "dl=" + encodeURIComponent(id), download: name, rel: "noopener" });
  document.body.append(a);
  a.click();
  a.remove();

  ensureEvents();
  renderDownloads();
  note(kind === "zip" ? `Streaming ${name}…` : `Downloading ${name}`);
}

/** Downloads one file directly. */
function downloadFile(e) {
  startDownload({ name: e.name, url: "/f" + encPath(join(state.path, e.name)), kind: "file", total: e.size, entry: e });
}

/** Asks the server to prepare a ZIP of the given names, then downloads it. */
async function downloadZip(names) {
  try {
    const r = await api("/api/zip", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ base: state.path, paths: names, store: state.zipStore, sums: state.zipSums }),
    });
    // Stored archives have an exact size: a real progress bar, and resumable.
    startDownload({ name: r.name, url: r.url, kind: "zip", total: r.size >= 0 ? r.size : -1, entry: { name: r.name } });
  } catch (err) {
    note("Couldn't start the ZIP: " + err.message, "err");
  }
}

/* ------------------------------------------------------------------ */
/* downloads panel                                                    */
/* ------------------------------------------------------------------ */

let events = null;
/** Opens the event stream (kept open for the life of the page): download
 *  progress as plain messages, plus named "change" and "send" events. */
function ensureEvents() {
  if (events) return;
  events = new EventSource("/api/events?c=" + encodeURIComponent(clientId));
  // (Re)connected: tell the server which folder to watch for this tab.
  events.onopen = () => watchFolder(state.path);
  events.onmessage = (ev) => {
    let items;
    try { items = JSON.parse(ev.data); } catch { return; }
    // Merge the server's view into our records; the latest request for an
    // ID wins (a download can be retried or resumed).
    let ticked = false;
    for (const it of items) {
      const d = state.downloads.get(it.id);
      if (!d || d.state === "dismissed") continue;
      // A file that just arrived in full gets its tick in the listing.
      if (it.state === "done" && d.state !== "done" && d.kind === "file" && d.entry && !it.changed) { d.entry.got = Date.now(); ticked = true; }
      d.sent = it.sent;
      if (it.total >= 0) d.total = it.total;
      d.rate = it.rate;
      d.state = it.state === "active" ? "active" : it.state === "done" ? "done" : "failed";
      // The host can pause or cap a download; a file can change mid-way.
      d.paused = !!it.paused;
      d.limit = it.limit || 0;
      d.changed = !!it.changed;
    }
    if (ticked) renderWindow(true);
    renderDownloads();
  };
  events.addEventListener("change", (ev) => { try { folderChanged(JSON.parse(ev.data)); } catch {} });
  events.addEventListener("send", (ev) => { try { showSendNote(JSON.parse(ev.data)); } catch {} });
  events.addEventListener("post", (ev) => { try { addPost(JSON.parse(ev.data)); } catch {} });
}

/** Kept for the panel's dismiss button; the stream now stays open. */
function maybeCloseEvents() {}

/* live folder: the server polls the folder this tab shows and pushes a
   "change" event when something is added, removed or modified */

/** Tells the server which folder this tab is showing. Folders on other
 *  servers aren't watched: they refresh when you open them. */
function watchFolder(path) {
  if (!events || events.readyState !== EventSource.OPEN || flockOf(path)) return;
  const q = new URLSearchParams({ c: clientId, path });
  fetch("/api/watch?" + q, { method: "POST" }).catch(() => {});
}

/** Applies a change pushed by the server: refetch, then flash what's new. */
async function folderChanged(c) {
  // Any cached copy of that folder is stale now.
  state.cache.delete(c.path);
  if (c.path !== state.path) return;
  if (c.gone) { load(state.path, { force: true }); return; }
  for (const n of c.added) state.fresh.add(n);
  try {
    const entries = await fetchListing(c.path);
    if (c.path === state.path) { refreshEntries(entries); scheduleTree(); }
  } catch {}
  // New rows glow briefly, then settle.
  if (c.added.length) {
    clearTimeout(folderChanged.t);
    folderChanged.t = setTimeout(() => {
      state.fresh.clear();
      document.querySelectorAll("#list .row.fresh").forEach((r) => r.classList.remove("fresh"));
    }, 2600);
    if (document.hidden || c.added.length > 1) note(`${c.added.length} new item${c.added.length > 1 ? "s" : ""} in this folder`);
  }
}

/** Renders the downloads panel, the top-bar badge and the status-bar item. */
function renderDownloads() {
  const list = [...state.downloads.values()].filter((d) => d.state !== "dismissed").reverse();
  const active = list.filter((d) => d.state === "active" || d.state === "queued");
  const rate = active.reduce((s, d) => s + (d.rate || 0), 0);

  $("#dl-badge").hidden = active.length === 0;
  $("#dl-badge").textContent = active.length;
  const sb = $("#sb-dl");
  sb.classList.toggle("busy", active.length > 0);
  const ups = active.filter((d) => d.kind === "upload").length;
  $("#sb-dl-text").textContent = active.length
    ? `${ups === active.length ? `${ups} uploading` : ups ? `${active.length} transfers` : `${active.length} downloading`} · ${fmtBytes(rate)}/s`
    : list.length ? `${list.length} download${list.length > 1 ? "s" : ""}` : "No downloads";
  $("#panel-meta").textContent = active.length ? `${active.length} active · ${fmtBytes(rate)}/s` : "";

  $("#dl-list").innerHTML = list.map((d) => {
    let info, prog;
    const pct = d.total > 0 ? Math.min(100, (d.sent / d.total) * 100) : 0;
    switch (d.state) {
      case "queued":
        info = d.kind === "upload" ? (d.checking ? "checking what the host has…" : "queued") : (Date.now() - d.added) / 1000 > 15 ? "waiting for browser…" : "starting…";
        prog = `<div class="prog queued"><i></i></div>`;
        break;
      case "active":
        // With --ask, an upload sits unread until the host accepts it.
        if (d.kind === "upload" && state.info.ask && !d.rate && d.sent < d.total && Date.now() - d.lastProgress > 1500) {
          info = "waiting for the host to accept…";
          prog = `<div class="prog queued"><i></i></div>`;
        } else if (d.paused) {
          info = `<span class="warn">⏸ paused by the host</span>${d.total > 0 ? ` · ${pct.toFixed(0)}% · ${fmtBytes(d.sent)}/${fmtBytes(d.total)}` : ` · ${fmtBytes(d.sent)}`}`;
          prog = `<div class="prog paused"><i style="width:${d.total > 0 ? pct.toFixed(1) : 100}%"></i></div>`;
        } else if (d.total > 0) {
          const eta = d.rate > 0 ? fmtDur((d.total - d.sent) / d.rate) : "";
          info = `${pct.toFixed(0)}% · ${fmtBytes(d.sent)}/${fmtBytes(d.total)} · ${fmtBytes(d.rate)}/s${eta ? " · " + eta : ""}`;
          prog = `<div class="prog"><i style="width:${pct.toFixed(1)}%"></i></div>`;
        } else {
          info = `${fmtBytes(d.sent)} · ${fmtBytes(d.rate)}/s · zip`;
          prog = `<div class="prog ind"><i></i></div>`;
        }
        // Say when the host's speed limit is what sets the pace.
        if (d.limit && !d.paused) info += ` · <span class="muted">capped at ${fmtBytes(d.limit)}/s</span>`;
        break;
      case "done":
        info = d.kind !== "upload" ? `<span class="ok">✓</span> ${fmtBytes(d.sent)}`
          : d.dup ? `<span class="ok">✓</span> already on the host${d.savedAs ? " as " + esc(d.savedAs) : ""}`
          : `<span class="ok">✓</span> sent · saved as ${esc(d.savedAs || d.name)}`;
        prog = `<div class="prog done"><i></i></div>`;
        // The file was being written while it downloaded: this copy may be torn.
        if (d.changed) {
          info = `<span class="warn">⚠ file changed during download</span>${d.url ? ` · <button class="link-btn dl-retry">download again</button>` : ""}`;
          prog = `<div class="prog changed"><i></i></div>`;
        }
        break;
      default:
        info = d.error ? `<span class="bad">✕ ${esc(d.error)}</span>` : `<span class="bad">✕ interrupted</span> at ${fmtBytes(d.sent)}`;
        prog = `<div class="prog failed"><i></i></div>`;
    }
    const ic = d.kind === "upload" ? `<span class="fi t-up">${icon("i-upload")}</span>` : fileIcon(d.entry || { name: d.name }).html;
    return `<li class="dl ${d.state}" data-id="${esc(d.id)}">
      ${ic}
      <span class="dl-name" title="${esc(d.name)}">${esc(d.name)}</span>
      ${prog}<span class="dl-info">${info}</span>
      <button class="icon-btn dl-x" title="Dismiss" aria-label="Dismiss">${icon("i-x")}</button>
    </li>`;
  }).join("");
}

/** Shows/hides the downloads panel. */
function togglePanel(force) {
  const p = $("#panel");
  const show = force ?? p.hidden;
  if (show && !$("#basket").hidden) $("#basket").hidden = true;
  if (show) togglePosts(false);
  p.hidden = !show;
  $("#dl-btn").classList.toggle("active", show);
}

/* ------------------------------------------------------------------ */
/* sidebar / drawer                                                   */
/* ------------------------------------------------------------------ */

/** Shows/hides the explorer (a drawer on phones, a column on desktop). */
function toggleSidebar() {
  const app = $(".app");
  // Narrow screens: slide the drawer over the content.
  if (matchMedia("(max-width: 760px)").matches) { app.classList.toggle("drawer-open"); return; }
  app.classList.toggle("no-sidebar");
  $("#menu-btn").classList.toggle("active", !app.classList.contains("no-sidebar"));
}
const closeDrawer = () => $(".app").classList.remove("drawer-open");

/** Makes a sash draggable to resize a CSS length variable. */
function makeSash(el, varName, { axis, min, max, invert }) {
  el.addEventListener("pointerdown", (ev) => {
    ev.preventDefault();
    el.setPointerCapture(ev.pointerId);
    el.classList.add("drag");
    const start = axis === "x" ? ev.clientX : ev.clientY;
    const startVal = parseFloat(getComputedStyle(document.documentElement).getPropertyValue(varName));
    const onMove = (e) => {
      const d = (axis === "x" ? e.clientX : e.clientY) - start;
      const v = Math.max(min, Math.min(max, startVal + (invert ? -d : d)));
      document.documentElement.style.setProperty(varName, v + "px");
    };
    const onUp = () => {
      el.classList.remove("drag");
      el.removeEventListener("pointermove", onMove);
      saveJSON(varName, getComputedStyle(document.documentElement).getPropertyValue(varName).trim());
    };
    el.addEventListener("pointermove", onMove);
    el.addEventListener("pointerup", onUp, { once: true });
  });
  const saved = loadJSON(varName, null);
  if (saved) document.documentElement.style.setProperty(varName, saved);
}

/* ------------------------------------------------------------------ */
/* command palette                                                    */
/* ------------------------------------------------------------------ */

const pal = { items: [], active: 0, sepAt: -1, sepText: "" };

/** The commands available from "> ". */
function commands() {
  const sel = state.sel.size;
  // Share links and playlists only reach this server's own files.
  const local = !flockOf(state.path);
  return [
    { label: "Download Selection", desc: sel ? `${sel} item(s)` : "nothing selected", key: "", run: downloadSelection },
    { label: "Select All", key: `${MOD}A`, run: () => { selectRange(0, state.view.length - 1); selectionChanged(); } },
    { label: "Clear Selection", key: "Esc", run: clearSelection },
    { label: "Go to Parent Folder", key: isMac ? "⌘↑" : "Alt+↑", run: () => go(parentOf(state.path)) },
    { label: "Go to Root Folder", run: () => go("/") },
    { label: "Refresh", run: () => load(state.path, { force: true }) },
    { label: "View: Toggle Explorer", key: `${MOD}B`, run: toggleSidebar },
    { label: "View: Toggle Downloads Panel", key: `${MOD}J`, run: () => togglePanel() },
    { label: "View: List", run: () => setLayout("list") },
    { label: "View: Grid", run: () => setLayout("grid") },
    { label: "Downloads: Clear Finished", run: clearFinished },
    { label: `ZIP: ${state.zipStore ? "Compress Again (smaller, no resume)" : "Always Store (exact size, resumable)"}`, run: toggleZipStore },
    { label: `ZIP: ${state.zipSums ? "Leave Out SHA256SUMS" : "Include SHA256SUMS"}`, run: toggleZipSums },
    { label: "Basket: Add or Remove Selection", key: "⇧B", run: () => toggleBasket() },
    { label: "Basket: Show", desc: state.basket.size ? `${state.basket.size} item(s)` : "empty", run: () => toggleBasketPanel(true) },
    { label: "Basket: Download as ZIP", desc: state.basket.size ? basketSize() : "empty", run: downloadBasket },
    { label: "Basket: Empty", run: clearBasket },
    { label: "File: Show SHA-256…", run: () => { const e = state.view[state.cursor]; e && !e.dir ? showHash(e) : note("Put the cursor on a file first"); } },
    { label: "Open Preview", key: "⇧Space", run: () => { const e = state.view[state.cursor]; e && previewKind(e) ? openPreview(e) : note("Nothing to preview here"); } },
    ...(local ? [{ label: "Share This Folder…", run: () => openShareDialog({ name: state.path === "/" ? state.info.root || "Files" : state.path.slice(state.path.lastIndexOf("/") + 1), dir: true }, state.path) }] : []),
    ...(state.info.inbox ? [{ label: "Send Files to This Computer…", run: pickUploads },
      { label: "Request Files from Someone…", desc: "a link they can upload with", run: openRequestDialog }] : []),
    { label: "Pigeon Post: Send Text to the Host…", run: () => togglePosts(true) },
    ...(local ? [{ label: "Play Folder in a Media Player (.m3u)", desc: "VLC, mpv, Infuse…", run: () => playlist(state.path, false) },
      { label: "Play Folder and Subfolders (.m3u)", run: () => playlist(state.path, true) }] : []),
    { label: "Preferences: Color Theme: Dark Modern", run: () => setTheme("dark", true) },
    { label: "Preferences: Color Theme: Light Modern", run: () => setTheme("light", true) },
    { label: "Preferences: Color Theme: Follow System", run: () => setTheme("auto", true) },
    { label: "Sort by Name", run: () => setSort("name") },
    { label: "Sort by Size", run: () => setSort("size") },
    { label: "Sort by Modified", run: () => setSort("mtime") },
    { label: "Copy Link to This Folder", run: () => copyText(location.origin + "/#" + encPath(state.path), "link") },
  ];
}

/** Opens the palette, optionally pre-filled (">" for commands). */
function openPalette(prefix = "") {
  $("#palette").hidden = false;
  const input = $("#pal-input");
  input.value = prefix;
  input.focus();
  renderPalette();
}

/** Closes the palette and returns focus to the list. */
function closePalette() {
  $("#palette").hidden = true;
  stopSearch();
  if (!touch) $("#list").focus({ preventScroll: true });
}

/** Subsequence fuzzy match; returns highlighted HTML or null. */
function fuzzy(text, q) {
  if (!q) return esc(text);
  const lt = text.toLowerCase(), lq = q.toLowerCase();
  // Prefer a contiguous match; fall back to an in-order subsequence.
  const at = lt.indexOf(lq);
  if (at >= 0) return esc(text.slice(0, at)) + "<b>" + esc(text.slice(at, at + q.length)) + "</b>" + esc(text.slice(at + q.length));
  let out = "", j = 0;
  for (let i = 0; i < text.length; i++) {
    if (j < lq.length && lt[i] === lq[j]) { out += "<b>" + esc(text[i]) + "</b>"; j++; } else out += esc(text[i]);
  }
  return j === lq.length ? out : null;
}

/** Rebuilds the palette's result list from the input. keep holds the
 *  highlighted item in place (used while search results stream in). */
function renderPalette(keep = false) {
  const raw = $("#pal-input").value;
  const isCmd = raw.startsWith(">");
  const q = (isCmd ? raw.slice(1) : raw).trim();
  pal.items = [];
  if (isCmd) {
    // Commands, filtered by the query.
    for (const c of commands()) {
      const h = fuzzy(c.label, q);
      if (h !== null) pal.items.push({ ...c, html: h });
    }
  } else {
    if (state.path !== "/" && !q) pal.items.push({ html: "..", desc: "parent folder", iconHtml: `<span class="fi">${icon("i-up")}</span>`, run: () => go(parentOf(state.path)) });
    // Files and folders in the current folder.
    for (const e of state.view) {
      const h = fuzzy(e.name, q);
      if (h === null) continue;
      pal.items.push({ html: h, desc: e.dir ? "folder" : fmtBytes(e.size), iconHtml: fileIcon(e).html, run: () => openEntry(e) });
      if (pal.items.length > 200) break;
    }
    // Then matches from everywhere else, streamed in by the server.
    pal.sepAt = -1;
    if (q) {
      startSearch(q);
      const hits = rankHits(search.hits.filter((h) => parentOf(h.path) !== state.path), q).slice(0, 100);
      if (hits.length || !search.done) {
        pal.sepAt = pal.items.length;
        pal.sepText = !search.done ? "Other folders · searching…" : search.partial ? "Other folders · first matches" : "Other folders";
      }
      for (const h of hits) {
        const name = h.path.slice(h.path.lastIndexOf("/") + 1);
        const e = { name, dir: !!h.dir, size: h.size, mtime: h.mtime };
        pal.items.push({
          html: fuzzy(name, q) ?? esc(name), desc: parentOf(h.path).slice(1) || "/", iconHtml: fileIcon(e).html,
          // A folder opens; a file is revealed (and selected) in its folder.
          run: () => { if (e.dir) go(h.path); else { state.reveal = name; go(parentOf(h.path)); } },
        });
      }
    } else stopSearch();
  }
  pal.active = keep ? Math.min(pal.active, Math.max(0, pal.items.length - 1)) : 0;
  const list = $("#pal-list");
  if (!pal.items.length) {
    const msg = isCmd ? "No matching commands" : q && !search.done ? "Searching every folder…" : q ? "No matching files or folders — type > for commands" : "This folder is empty — type > for commands";
    list.innerHTML = `<li class="pal-empty">${msg}</li>`;
    return;
  }
  list.innerHTML = pal.items.map((it, i) => (i === pal.sepAt ? `<li class="pal-sep">${esc(pal.sepText)}</li>` : "") + `<li class="pal-item${i === pal.active ? " active" : ""}" data-i="${i}" role="option">
    ${it.iconHtml || ""}<span class="lbl">${it.html}</span>${it.desc ? `<span class="desc">${esc(it.desc)}</span>` : ""}${it.key ? `<span class="key">${esc(it.key)}</span>` : ""}
  </li>`).join("") + (pal.sepAt === pal.items.length ? `<li class="pal-sep">${esc(pal.sepText)}</li>` : "");
}

/* tree-wide quick open: /api/search streams matches (newline-delimited
   JSON) while it walks the tree breadth-first */

let search = { q: "", hits: [], done: true, partial: false, ctrl: null, timer: 0 };

/** Starts searching for q (debounced), unless that search is already running. */
function startSearch(q) {
  if (search.q === q) return;
  stopSearch();
  search = { q, hits: [], done: false, partial: false, ctrl: new AbortController(), timer: 0 };
  const s = search;
  s.timer = setTimeout(() => runSearch(s), 140);
}

/** Abandons the current search. */
function stopSearch() {
  clearTimeout(search.timer);
  search.ctrl?.abort();
  search = { q: "", hits: [], done: true, partial: false, ctrl: null, timer: 0 };
}

/** Reads the result stream, repainting the palette as hits arrive. */
async function runSearch(s) {
  let frame = 0;
  const repaint = () => { if (!frame) frame = requestAnimationFrame(() => { frame = 0; if (search === s && !$("#palette").hidden) renderPalette(true); }); };
  try {
    const r = await fetch("/api/search?q=" + encodeURIComponent(s.q), { signal: s.ctrl.signal });
    if (!r.ok) throw new Error(r.statusText);
    const reader = r.body.getReader(), dec = new TextDecoder();
    let buf = "";
    // Split the stream into lines; each one is a hit or the final summary.
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += dec.decode(value, { stream: true });
      let nl;
      while ((nl = buf.indexOf("\n")) >= 0) {
        const line = buf.slice(0, nl);
        buf = buf.slice(nl + 1);
        if (!line) continue;
        const o = JSON.parse(line);
        if (o.done) s.partial = o.partial; else s.hits.push(o);
      }
      repaint();
    }
  } catch (err) {
    if (err.name === "AbortError") return;
  }
  s.done = true;
  repaint();
}

/** Orders search hits: exact name, prefix, substring, then looser matches;
 *  shallower paths first within each group. */
function rankHits(hits, q) {
  const lq = q.toLowerCase();
  const score = (h) => {
    const n = h.path.slice(h.path.lastIndexOf("/") + 1).toLowerCase();
    const base = n === lq ? 0 : n.startsWith(lq) ? 1 : n.includes(lq) ? 2 : 3;
    return base * 1000 + h.path.split("/").length * 10 + (h.dir ? 0 : 1);
  };
  return hits.map((h) => [score(h), h]).sort((a, b) => a[0] - b[0] || coll.compare(a[1].path, b[1].path)).map((x) => x[1]);
}

/** Moves the palette highlight. */
function palMove(d) {
  if (!pal.items.length) return;
  pal.active = (pal.active + d + pal.items.length) % pal.items.length;
  document.querySelectorAll("#pal-list .pal-item").forEach((li, i) => li.classList.toggle("active", i === pal.active));
  $("#pal-list .pal-item.active")?.scrollIntoView({ block: "nearest" });
}

/** Runs the highlighted palette item. */
function palRun(i = pal.active) {
  const it = pal.items[i];
  closePalette();
  it?.run();
}

/* ------------------------------------------------------------------ */
/* context menu                                                       */
/* ------------------------------------------------------------------ */

/** Shows a right-click menu for the row at index i. */
function showContextMenu(ev, i) {
  ev.preventDefault();
  const e = state.view[i];
  // Right-clicking outside the selection selects just that row, as in VS Code.
  if (!state.sel.has(e.name)) { selectRange(i, i); selectionChanged(); }
  setCursor(i);
  const many = state.sel.size > 1;
  const targets = targetEntries();
  const inBasket = targets.every((x) => state.basket.has(join(state.path, x.name)));
  // Share links and playlists only reach this server's own files.
  const local = !flockOf(state.path);
  const basket = { label: inBasket ? (many ? `Remove ${targets.length} Items from Basket` : "Remove from Basket") : (many ? `Add ${targets.length} Items to Basket` : "Add to Basket"), key: "⇧B", run: () => toggleBasket(targets) };
  const items = many
    ? [{ label: `Download ${state.sel.size} Items as ZIP`, run: downloadSelection }, basket]
    : e.dir
      ? [{ label: "Open", run: () => openEntry(e) }, { label: "Download as ZIP", run: () => downloadZip([e.name]) }, basket, "-",
        ...(local ? [{ label: "Play as Playlist (.m3u)", run: () => playlist(join(state.path, e.name), true) }] : []),
        { label: "Copy Link", run: () => copyText(entryURL(e), "link") },
        ...(local ? [{ label: "Create Share Link…", run: () => openShareDialog(e, join(state.path, e.name)) }] : [])]
      : [
        ...(e.archive ? [{ label: "Look Inside", key: "Enter", run: () => openEntry(e) }] : []),
        { label: "Download", run: () => downloadFile(e) },
        ...(previewKind(e) ? [{ label: "Open Preview", key: "⇧Space", run: () => openPreview(e) }] : []),
        basket,
        "-",
        { label: "Copy Link", run: () => copyText(entryURL(e), "link") },
        { label: "Copy as cURL", run: () => copyText(curlFor(e), "curl command") },
        { label: "Show SHA-256…", run: () => showHash(e) },
        ...(local ? [{ label: "Create Share Link…", run: () => openShareDialog(e, join(state.path, e.name)) }] : []),
      ];
  items.push("-", { label: "Select All", key: `${MOD}A`, run: () => { selectRange(0, state.view.length - 1); selectionChanged(); } });

  closeContextMenu();
  const m = document.createElement("ul");
  m.className = "ctx";
  m.id = "ctx";
  m.innerHTML = items.map((it, k) => it === "-" ? `<li class="ctx-sep"></li>`
    : `<li class="ctx-item" data-k="${k}"><span>${esc(it.label)}</span>${it.key ? `<span class="key">${esc(it.key)}</span>` : ""}</li>`).join("");
  document.body.append(m);
  // Keep the menu on screen.
  const r = m.getBoundingClientRect();
  m.style.left = Math.min(ev.clientX, innerWidth - r.width - 4) + "px";
  m.style.top = Math.min(ev.clientY, innerHeight - r.height - 4) + "px";
  m.addEventListener("click", (e2) => {
    const li = e2.target.closest(".ctx-item");
    if (!li) return;
    closeContextMenu();
    items[+li.dataset.k].run();
  });
}
const closeContextMenu = () => $("#ctx")?.remove();

/* ------------------------------------------------------------------ */
/* preview                                                            */
/* ------------------------------------------------------------------ */

const pv = { list: [], i: 0, kind: "", ctrl: null, md: "rendered", wrap: false };
const TEXT_LIMIT = 256 * 1024; // bytes of a text file loaded at once
const PLAIN = new Set(["txt", "log", "csv", "tsv", "lock"]);

/** What kind of preview an entry gets, or "" if none. */
function previewKind(e) {
  if (!e || e.dir) return "";
  const ext = extOf(e.name);
  // SVG renders as an image; inside <img> its scripts never run.
  if (ext === "svg") return "image";
  const k = fileIcon(e).kind;
  if (k === "code") return ext === "md" || ext === "mdx" ? "markdown" : "text";
  return ["image", "video", "audio", "pdf"].includes(k) ? k : "";
}

/** Lower-case extension of a file name ("" if none). */
const extOf = (name) => { const d = name.lastIndexOf("."); return d > 0 ? name.slice(d + 1).toLowerCase() : ""; };

/** Opens the preview on e; ← and → step through the folder's other
 *  previewable files. e may carry its own path (a file sent from the host). */
function openPreview(e) {
  if (e.path && parentOf(e.path) !== state.path) pv.list = [e];
  else {
    pv.list = state.view.filter((x) => previewKind(x)).map((x) => ({ ...x, path: join(state.path, x.name) }));
  }
  pv.i = Math.max(0, pv.list.findIndex((x) => x.name === e.name));
  if (!pv.list.length) pv.list = [{ ...e, path: e.path || join(state.path, e.name) }];
  $("#preview").hidden = false;
  closeContextMenu();
  renderPreview();
  $("#pv-close").focus({ preventScroll: true });
}

/** Closes the preview, stopping any playback. */
function closePreview() {
  pv.ctrl?.abort();
  $("#pv-body").innerHTML = "";
  $("#preview").hidden = true;
  // Leave the cursor on the file that was being looked at.
  const e = pv.list[pv.i];
  const i = e ? state.view.findIndex((x) => x.name === e.name) : -1;
  if (i >= 0 && e.path === join(state.path, e.name)) setCursor(i);
  if (!touch) $("#list").focus({ preventScroll: true });
}

/** Moves to the previous/next previewable file. */
function stepPreview(d) {
  if (pv.list.length < 2) return;
  pv.i = (pv.i + d + pv.list.length) % pv.list.length;
  renderPreview();
}

/** Draws the current file. */
function renderPreview() {
  pv.ctrl?.abort();
  pv.ctrl = null;
  const e = pv.list[pv.i];
  const kind = (pv.kind = previewKind(e));
  const url = "/f" + encPath(e.path) + "?inline=1";
  $("#pv-icon").innerHTML = fileIcon(e).html;
  $("#pv-name").textContent = e.name;
  $("#pv-meta").textContent = [fmtBytes(e.size), e.mtime ? fmtDate(e.mtime) : "", pv.list.length > 1 ? `${pv.i + 1} of ${pv.list.length}` : ""].filter(Boolean).join(" · ");
  $("#pv-open").href = url;
  $("#pv-prev").hidden = $("#pv-next").hidden = pv.list.length < 2;
  $("#pv-tools").innerHTML = "";
  const body = $("#pv-body");
  body.className = "pv-body pv-" + kind;
  const fail = (msg) => {
    body.className = "pv-body pv-msg";
    body.innerHTML = `${icon("i-eye", "i big")}<h3>${esc(msg)}</h3><button class="btn primary" id="pv-fallback">${icon("i-download")}<span>Download</span></button>`;
    $("#pv-fallback").onclick = () => $("#pv-dl").click();
  };
  switch (kind) {
    case "image":
      body.innerHTML = `<img src="${esc(url)}" alt="${esc(e.name)}">`;
      $("img", body).onerror = () => fail("This browser can't show this image");
      break;
    case "video":
      body.innerHTML = `<video src="${esc(url)}" controls autoplay playsinline></video>`;
      $("video", body).onerror = () => fail("This browser can't play this video");
      break;
    case "audio":
      body.innerHTML = `<div class="pv-audio">${fileIcon(e).html}<audio src="${esc(url)}" controls autoplay></audio></div>`;
      break;
    case "pdf":
      body.innerHTML = `<iframe src="${esc(url)}" title="${esc(e.name)}"></iframe>`;
      break;
    default:
      loadText(e, url, false);
  }
}

/** Loads (part of) a text file and shows it as code or rendered Markdown.
 *  Big files show their first 256 KB, or with tail set their last 256 KB. */
async function loadText(e, url, tail) {
  const body = $("#pv-body");
  body.innerHTML = `<div class="pv-loading">Loading…</div>`;
  const ctrl = (pv.ctrl = new AbortController());
  const big = e.size > TEXT_LIMIT;
  const headers = big ? { Range: tail ? `bytes=-${TEXT_LIMIT}` : `bytes=0-${TEXT_LIMIT - 1}` } : {};
  let text;
  try {
    const r = await fetch(url, { headers, signal: ctrl.signal });
    if (!r.ok && r.status !== 206) throw new Error(r.statusText);
    const buf = new Uint8Array(await r.arrayBuffer());
    // NUL bytes near the start mean it isn't text after all.
    if (buf.subarray(0, 8192).includes(0)) throw new Error("binary");
    text = new TextDecoder().decode(buf);
  } catch (err) {
    if (err.name === "AbortError") return;
    body.className = "pv-body pv-msg";
    body.innerHTML = `${icon("i-eye", "i big")}<h3>${err.message === "binary" ? "This looks like a binary file" : "Couldn't load this file"}</h3>`;
    return;
  }
  if (pv.ctrl !== ctrl) return;
  // A tail starts mid-line; drop the partial first line.
  if (tail && big) text = text.slice(text.indexOf("\n") + 1);
  const ext = extOf(e.name);
  const md = pv.kind === "markdown";

  // Toolbar: Markdown can switch to its source; code can wrap long lines.
  const tools = $("#pv-tools");
  tools.innerHTML = md
    ? `<button class="pv-tool${pv.md === "rendered" ? " on" : ""}" data-md="rendered">Preview</button><button class="pv-tool${pv.md === "source" ? " on" : ""}" data-md="source">Source</button>`
    : `<button class="pv-tool${pv.wrap ? " on" : ""}" data-wrap>Wrap</button>`;
  tools.onclick = (ev) => {
    const b = ev.target.closest(".pv-tool");
    if (!b) return;
    if (b.dataset.md) pv.md = b.dataset.md; else pv.wrap = !pv.wrap;
    loadText(e, url, tail);
  };

  let html = "";
  // A banner explains a partial view and offers the other end.
  if (big) {
    html += `<div class="pv-banner">Showing the ${tail ? "last" : "first"} ${fmtBytes(TEXT_LIMIT)} of ${fmtBytes(e.size)}
      <button class="btn secondary" id="pv-other">${tail ? "Show start" : "Show end"}</button></div>`;
  }
  if (md && pv.md === "rendered") html += `<article class="md">${renderMarkdown(text, parentOf(e.path))}</article>`;
  else {
    const lines = text.split("\n").length;
    const nums = tail && big ? "" : Array.from({ length: lines }, (_, i) => i + 1).join("\n");
    const code = PLAIN.has(ext) ? esc(text) : highlight(text, ext || e.name.toLowerCase());
    html += `<div class="pv-code${pv.wrap ? " wrap" : ""}">${nums ? `<pre class="gutter" aria-hidden="true">${nums}</pre>` : ""}<pre class="src"><code>${code}</code></pre></div>`;
  }
  body.className = "pv-body pv-text";
  body.innerHTML = html;
  if (big) $("#pv-other").onclick = () => loadText(e, url, !tail);
}

/* a small, safe syntax highlighter: comments, strings, numbers, keywords and
   calls, which is most of what makes code readable at a glance */

const KEYWORDS = new Set(`abstract and as async await break case catch class const continue def default defer del do elif else enum except
  export extends false final finally fn for from func function go goto if impl import in instanceof interface is lambda let loop match mod
  module mut namespace new nil none not null of or package pass private protected pub public raise return select self static struct super
  switch then this throw throws true try type typeof undefined unless use using var void when where while with yield fi done esac local
  echo True False None`.split(/\s+/));
const HASH_COMMENTS = new Set(["py", "sh", "bash", "zsh", "fish", "rb", "yaml", "yml", "toml", "ini", "r", "pl", "ps1", "tf", "env",
  "dockerfile", "makefile", ".gitignore", "gitignore", "conf", "properties", "ex", "exs", "nim"]);

/** Returns src as HTML with token classes for the language of ext. */
function highlight(src, ext) {
  const hash = HASH_COMMENTS.has(ext);
  const re = hash
    ? /(#[^\n]*)|("(?:[^"\\\n]|\\.)*"|'(?:[^'\\\n]|\\.)*')|(\b\d[\w.]*)|([A-Za-z_$][\w$]*)/g
    : /(\/\/[^\n]*|\/\*[\s\S]*?\*\/|<!--[\s\S]*?-->)|("(?:[^"\\\n]|\\.)*"|'(?:[^'\\\n]|\\.)*'|`(?:[^`\\]|\\.)*`)|(\b\d[\w.]*)|([A-Za-z_$][\w$]*)/g;
  let out = "", last = 0, m;
  // Walk the tokens; everything between them is copied escaped.
  while ((m = re.exec(src))) {
    out += esc(src.slice(last, m.index));
    const [t, com, str, num, id] = m;
    let cls = "";
    if (com) cls = "c";
    else if (str) cls = "s";
    else if (num) cls = "n";
    else if (KEYWORDS.has(id)) cls = "k";
    else if (src[re.lastIndex] === "(") cls = "f";
    out += cls ? `<span class="tk-${cls}">${esc(t)}</span>` : esc(t);
    last = re.lastIndex;
  }
  return out + esc(src.slice(last));
}

/* Markdown: the common subset (headings, lists, quotes, code, emphasis,
   links, images, rules). Text is escaped before any markup is added, and
   only http(s), mailto and relative URLs are kept. */

/** Renders Markdown to safe HTML; relative links resolve against dir. */
function renderMarkdown(src, dir) {
  const out = [];
  let para = [], list = null, quote = [], code = null;
  const flushPara = () => { if (para.length) out.push(`<p>${mdInline(para.join(" "), dir)}</p>`); para = []; };
  const flushList = () => { if (list) out.push(`<${list.tag}>${list.items.map((t) => `<li>${mdInline(t, dir)}</li>`).join("")}</${list.tag}>`); list = null; };
  const flushQuote = () => { if (quote.length) out.push(`<blockquote>${renderMarkdown(quote.join("\n"), dir)}</blockquote>`); quote = []; };
  const flush = () => { flushPara(); flushList(); flushQuote(); };
  // One line at a time; fenced code swallows everything until its fence.
  for (const line of src.replace(/\r\n?/g, "\n").split("\n")) {
    if (code) {
      if (/^\s*(```|~~~)/.test(line)) { out.push(`<pre><code>${esc(code.join("\n"))}</code></pre>`); code = null; }
      else code.push(line);
      continue;
    }
    let m;
    if (/^\s*(```|~~~)/.test(line)) { flush(); code = []; }
    else if ((m = line.match(/^(#{1,6})\s+(.*?)\s*#*\s*$/))) { flush(); out.push(`<h${m[1].length}>${mdInline(m[2], dir)}</h${m[1].length}>`); }
    else if (/^\s*([-*_])(\s*\1){2,}\s*$/.test(line)) { flush(); out.push("<hr>"); }
    else if ((m = line.match(/^>\s?(.*)$/))) { flushPara(); flushList(); quote.push(m[1]); }
    else if ((m = line.match(/^\s*([-*+]|\d+[.)])\s+(.*)$/))) {
      flushPara(); flushQuote();
      const tag = /\d/.test(m[1]) ? "ol" : "ul";
      if (!list || list.tag !== tag) { flushList(); list = { tag, items: [] }; }
      list.items.push(m[2]);
    } else if (!line.trim()) flush();
    else if (list && /^\s{2,}/.test(line)) list.items[list.items.length - 1] += " " + line.trim();
    else { flushList(); flushQuote(); para.push(line.trim()); }
  }
  if (code) out.push(`<pre><code>${esc(code.join("\n"))}</code></pre>`);
  flush();
  return out.join("\n");
}

/** Inline Markdown: code spans first (their contents stay literal), then
 *  images, links, bold and italics on the escaped remainder. */
function mdInline(text, dir) {
  return text.split(/(`[^`]+`)/).map((part, i) => {
    if (i % 2) return `<code>${esc(part.slice(1, -1))}</code>`;
    return esc(part)
      .replace(/!\[([^\]]*)\]\(([^)\s]+)[^)]*\)/g, (_, alt, u) => {
        const href = mdURL(u, dir, true);
        return href ? `<img src="${href}" alt="${alt}">` : alt;
      })
      .replace(/\[([^\]]+)\]\(([^)\s]+)[^)]*\)/g, (_, t, u) => {
        const href = mdURL(u, dir, false);
        return href ? `<a href="${href}" target="_blank" rel="noopener">${t}</a>` : t;
      })
      .replace(/\*\*([^*]+)\*\*|__([^_]+)__/g, (_, a, b) => `<strong>${a ?? b}</strong>`)
      .replace(/(^|[^\w*])\*([^*\s][^*]*)\*|(^|\W)_([^_\s][^_]*)_(?=\W|$)/g, (_, p1, a, p2, b) => `${p1 ?? p2}<em>${a ?? b}</em>`);
  }).join("");
}

/** Makes a Markdown URL safe: http(s)/mailto stay, anchors stay, relative
 *  paths point at the file in this server. Anything else is dropped. The
 *  input is already HTML-escaped, which is also what an attribute needs. */
function mdURL(u, dir, inline) {
  const raw = u.replace(/&amp;/g, "&").replace(/&quot;/g, '"').replace(/&#39;/g, "'").replace(/&lt;/g, "<").replace(/&gt;/g, ">");
  if (/^(https?:|mailto:)/i.test(raw) || raw.startsWith("#")) return u;
  if (/^[a-z][a-z0-9+.-]*:/i.test(raw) || raw.startsWith("//")) return "";
  // Resolve ./ and ../ against the Markdown file's folder.
  const parts = (raw.startsWith("/") ? [] : dir.split("/")).filter(Boolean);
  for (const seg of raw.split(/[?#]/)[0].split("/")) {
    if (seg === "..") parts.pop(); else if (seg && seg !== ".") parts.push(seg);
  }
  return esc("/f" + encPath("/" + parts.map((p) => { try { return decodeURIComponent(p); } catch { return p; } }).join("/")) + (inline ? "?inline=1" : ""));
}

/* ------------------------------------------------------------------ */
/* share links                                                        */
/* ------------------------------------------------------------------ */

/** The dialog rows shared by share links and file requests. */
function shareRowsHTML(request) {
  return `
    <label class="dlg-row">Link expires
      <select id="sh-ttl"><option value="900">in 15 minutes</option><option value="3600" selected>in 1 hour</option>
      <option value="86400">in 1 day</option><option value="604800">in 7 days</option><option value="0">never (while the server runs)</option></select></label>
    <label class="dlg-row">${request ? "Files accepted" : "Downloads allowed"}
      <select id="sh-uses">${request
        ? `<option value="1">1</option><option value="10">10</option><option value="100">100</option><option value="0" selected>unlimited</option>`
        : `<option value="1">1</option><option value="3" selected>3</option><option value="10">10</option><option value="0">unlimited</option>`}</select></label>
    <label class="dlg-row">PIN <span class="dlg-hint">optional · 4–12 digits</span>
      <input id="sh-pin" inputmode="numeric" pattern="[0-9]*" maxlength="12" autocomplete="off" placeholder="none"></label>`;
}

/** Creates a link from the dialog's fields and shows it, ready to copy. */
async function createLink(body, name, request) {
  const box = $("#dialog-box");
  const pin = $("#sh-pin").value.trim();
  if (pin && !/^[0-9]{4,12}$/.test(pin)) { note("A PIN is 4 to 12 digits", "err"); $("#sh-pin").focus(); return; }
  try {
    const r = await api("/api/shares", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ ...body, ttl: +$("#sh-ttl").value, uses: +$("#sh-uses").value, pin }),
    });
    // Prefer the address this page uses, unless it is this machine's own
    // loopback, which nobody else can open.
    const url = /^(localhost|127\.|\[::1\])/.test(location.hostname) && r.url.startsWith("http") ? r.url : location.origin + "/s/" + r.id;
    const limits = [r.expires ? "expires " + fmtDate(r.expires) : "no expiry",
      request ? (r.maxUses ? r.maxUses + " file" + (r.maxUses > 1 ? "s" : "") : "any number of files")
        : (r.maxUses ? r.maxUses + " download" + (r.maxUses > 1 ? "s" : "") : "unlimited downloads")];
    if (pin) limits.push("PIN " + pin);
    box.innerHTML = `
      <h2>${icon("i-check")}${request ? "Request ready" : "Link ready"}</h2>
      <p class="dlg-sub">${esc(name)} · ${esc(limits.join(" · "))}</p>
      <div class="dlg-link"><input id="sh-url" readonly value="${esc(url)}"><button class="btn primary" id="sh-copy">Copy</button></div>
      <p class="dlg-note">${request ? `Files land in <b>${esc(r.path)}</b> on the host. ` : ""}${pin ? "Tell the PIN separately. " : ""}The host can revoke it at any time from the ${esc(state.info.name || "Pidgeon")} dashboard.</p>
      <div class="dlg-actions"><button class="btn secondary" id="sh-done">Done</button></div>`;
    $("#sh-url").select();
    $("#sh-copy").onclick = () => copyText(url, request ? "request link" : "share link");
    $("#sh-done").onclick = closeDialog;
  } catch (err) {
    note("Couldn't create the link: " + err.message, "err");
  }
}

/** Shows the share dialog for an entry at path. */
function openShareDialog(e, path) {
  const box = $("#dialog-box");
  box.innerHTML = `
    <h2>${icon("i-share")}Share ${e.dir ? "folder" : "file"}</h2>
    <p class="dlg-sub">Anyone with the link can download <b>${esc(e.name)}</b>${e.dir ? " and what's inside" : ""}. No token needed; nothing else is visible.</p>
    ${shareRowsHTML(false)}
    <div class="dlg-actions"><button class="btn secondary" id="sh-cancel">Cancel</button><button class="btn primary" id="sh-create">${icon("i-link")}<span>Create link</span></button></div>`;
  $("#dialog").hidden = false;
  $("#sh-ttl").focus();
  $("#sh-cancel").onclick = closeDialog;
  $("#sh-create").onclick = () => createLink({ path }, e.name, false);
}

/** Shows the dialog for a file request: a link others upload files with. */
function openRequestDialog() {
  if (!state.info.inbox) return note("Uploads are off on this server", "err");
  const box = $("#dialog-box");
  box.innerHTML = `
    <h2>${icon("i-upload")}Request files</h2>
    <p class="dlg-sub">Anyone with the link can send files into a folder of the host's inbox. They see nothing else.</p>
    <label class="dlg-row">Folder name <input id="rq-folder" maxlength="120" autocomplete="off" placeholder="Request ${esc(fmtDate(Date.now()).replace(":", ""))}"></label>
    ${shareRowsHTML(true)}
    <div class="dlg-actions"><button class="btn secondary" id="sh-cancel">Cancel</button><button class="btn primary" id="sh-create">${icon("i-link")}<span>Create link</span></button></div>`;
  $("#dialog").hidden = false;
  $("#rq-folder").focus();
  $("#sh-cancel").onclick = closeDialog;
  $("#sh-create").onclick = () => {
    const folder = $("#rq-folder").value.trim();
    createLink({ request: true, folder }, folder || "New folder", true);
  };
}

/** Closes the dialog. */
function closeDialog() {
  $("#dialog").hidden = true;
  $("#dialog-box").innerHTML = "";
  if (!touch) $("#list").focus({ preventScroll: true });
}

/* ------------------------------------------------------------------ */
/* files from the host and to the host                                */
/* ------------------------------------------------------------------ */

/** Shows a file the host sent from the dashboard, with a download button.
 *  It stays until dismissed (or for two minutes). */
function showSendNote(m) {
  const e = { name: m.name, size: m.size, mtime: m.mtime, path: m.path };
  const el = document.createElement("div");
  el.className = "note send";
  el.innerHTML = `${icon("i-mail")}<span><b>${esc(m.name)}</b> arrived from the host · ${fmtBytes(m.size)}</span>
    ${previewKind(e) ? `<button class="btn secondary" data-a="view">Preview</button>` : ""}
    <button class="btn primary" data-a="get">Download</button><button class="icon-btn" data-a="x" aria-label="Dismiss">${icon("i-x")}</button>`;
  el.addEventListener("click", (ev) => {
    const a = ev.target.closest("[data-a]")?.dataset.a;
    if (a === "get") startDownload({ name: e.name, url: "/f" + encPath(e.path), kind: "file", total: e.size, entry: e });
    if (a === "view") openPreview(e);
    if (a) el.remove();
  });
  $("#notes").append(el);
  setTimeout(() => el.remove(), 120_000);
  // Flash the tab title when the page isn't in front.
  if (document.hidden) {
    const t = document.title;
    document.title = "📨 " + m.name;
    addEventListener("focus", () => { document.title = t; }, { once: true });
  }
}

/** Opens the file picker for uploads. */
function pickUploads() {
  if (!state.info.inbox) return note("Uploads are off on this server", "err");
  $("#up-input").click();
}

const uploadQueue = [];
let uploading = 0;
const UPLOAD_PARALLEL = 2;

/** Queues files to send into the host's inbox. Files the host already has
 *  (same size and fingerprint) are marked as there and not sent. */
async function uploadFiles(files) {
  if (!files.length) return;
  const max = state.info.inboxMax || 0;
  const todo = [];
  // Each file gets a row in the transfers panel right away.
  for (const f of files) {
    const id = `up.${++state.seq}`;
    const d = { id, name: f.name, kind: "upload", total: f.size, sent: 0, rate: 0, state: "queued", checking: true, added: Date.now(), lastProgress: Date.now(), entry: { name: f.name } };
    state.downloads.set(id, d);
    if (max && f.size > max) { d.state = "failed"; d.error = `larger than the ${fmtBytes(max)} limit`; continue; }
    todo.push([d, f]);
  }
  togglePanel(true);
  renderDownloads();
  const have = await alreadyOnHost(todo.map(([, f]) => f));
  let skipped = 0;
  for (const [i, [d, f]] of todo.entries()) {
    d.checking = false;
    if (have[i]) { d.state = "done"; d.dup = true; d.sent = d.total; skipped++; continue; }
    uploadQueue.push([d, f]);
  }
  if (skipped) note(`${skipped} file${skipped > 1 ? "s" : ""} already on the host, not sent again`, "ok");
  renderDownloads();
  pumpUploads();
}

/* duplicate check: CRC-32s of 64 KB at the start, middle and end of each
   file, compared by the server with same-sized files in its inbox */

const CRC_TABLE = new Uint32Array(256).map((_, n) => { let c = n; for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1; return c >>> 0; });

/** CRC-32 (IEEE) of a byte array, as 8 hex digits. */
function crc32(b) {
  let c = ~0;
  for (let i = 0; i < b.length; i++) c = CRC_TABLE[(c ^ b[i]) & 255] ^ (c >>> 8);
  return (~c >>> 0).toString(16).padStart(8, "0");
}

/** The quick fingerprint of a file (the same as the server's). */
async function fingerprint(f) {
  const K = 65536, starts = [0, Math.max(0, Math.floor(f.size / 2) - K / 2), Math.max(0, f.size - K)];
  let out = "";
  for (const s of starts) out += crc32(new Uint8Array(await f.slice(s, Math.min(f.size, s + K)).arrayBuffer()));
  return out;
}

/** Asks the host which of these files its inbox already holds. Any failure
 *  just means everything is sent (the server drops exact copies anyway). */
async function alreadyOnHost(files) {
  if (!files.length) return [];
  try {
    const fps = [];
    for (const f of files) fps.push(await fingerprint(f));
    const r = await api("/api/upload/check", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ files: files.map((f, i) => ({ name: f.name, size: f.size, fp: fps[i] })) }),
    });
    return r.have || [];
  } catch { return []; }
}

/** Starts queued uploads, a couple at a time. */
function pumpUploads() {
  while (uploading < UPLOAD_PARALLEL && uploadQueue.length) {
    const [d, f] = uploadQueue.shift();
    uploading++;
    sendUpload(d, f).finally(() => { uploading--; pumpUploads(); });
  }
}

/** Sends one file with XMLHttpRequest (fetch can't report upload progress). */
function sendUpload(d, f) {
  return new Promise((resolve) => {
    const xhr = new XMLHttpRequest();
    let lastAt = Date.now(), lastSent = 0;
    xhr.open("PUT", "/api/upload?name=" + encodeURIComponent(f.name) + "&mtime=" + (f.lastModified || 0));
    xhr.upload.onprogress = (ev) => {
      const now = Date.now();
      // Smooth the rate over the last moments, like the server does.
      if (now - lastAt >= 400) {
        const inst = ((ev.loaded - lastSent) * 1000) / (now - lastAt);
        d.rate = d.rate ? d.rate * 0.6 + inst * 0.4 : inst;
        lastAt = now; lastSent = ev.loaded;
      }
      if (ev.loaded > d.sent) d.lastProgress = now;
      d.sent = ev.loaded;
      d.state = "active";
      renderDownloads();
    };
    xhr.onload = () => {
      let msg = xhr.responseText.trim();
      if (xhr.status === 200) {
        let dup = false;
        try { const r = JSON.parse(msg); d.savedAs = r.name; dup = !!r.duplicate; } catch {}
        d.state = "done";
        d.dup = dup;
        d.sent = d.total;
        note(dup ? `${d.name} was already on the host` : `Sent ${d.name}`, "ok");
      } else {
        d.state = "failed";
        d.error = xhr.status === 401 ? "not signed in" : msg || "upload failed";
      }
      d.rate = 0;
      renderDownloads();
      resolve();
    };
    xhr.onerror = xhr.onabort = () => { d.state = "failed"; d.error = "connection lost"; d.rate = 0; renderDownloads(); resolve(); };
    d.state = "active";
    renderDownloads();
    xhr.send(f);
  });
}

/* ------------------------------------------------------------------ */
/* Pigeon Post: text between this page and the host's terminal         */
/* ------------------------------------------------------------------ */

/** Fetches this device's conversation with the host. */
async function loadPosts() {
  try {
    for (const m of await api("/api/post")) state.posts.set(m.id, m);
    renderPosts();
  } catch {}
}

/** Adds a message (sent here, or pushed by the server) and announces new
 *  ones from the host. */
function addPost(m) {
  const fresh = !state.posts.has(m.id);
  state.posts.set(m.id, m);
  if (fresh && m.host && $("#post").hidden) {
    state.postUnread++;
    showPostNote(m);
  }
  renderPosts();
}

/** Escapes text and turns web links into links. */
function linkify(text) {
  return esc(text).replace(/https?:\/\/[^\s<]+/g, (u) => `<a href="${u}" target="_blank" rel="noopener noreferrer">${u}</a>`);
}

/** The first web link in a text, if any. */
const firstLink = (text) => (text.match(/https?:\/\/[^\s<>"']+/) || [""])[0];

/** Renders the conversation and the unread badge. */
function renderPosts() {
  const list = [...state.posts.values()].sort((a, b) => a.id - b.id);
  $("#post-badge").hidden = !state.postUnread;
  $("#post-badge").textContent = state.postUnread;
  $("#post-list").innerHTML = list.length ? list.map((m) => `
    <li class="msg ${m.mine ? "mine" : "host"}" data-id="${m.id}">
      <div class="bubble">${linkify(m.text)}</div>
      <div class="msg-meta">${m.mine ? "you" : "host"} · ${esc(fmtAgo(m.time))}<button class="link-btn msg-copy">copy</button></div>
    </li>`).join("")
    : `<li class="post-empty">Paste a link or a note here and it shows up in the host's terminal. Text the host sends you appears here too.</li>`;
  const l = $("#post-list");
  l.scrollTop = l.scrollHeight;
}

/** Shows/hides the Pigeon Post panel. */
function togglePosts(force) {
  const p = $("#post");
  const show = force ?? p.hidden;
  if (show) { togglePanel(false); toggleBasketPanel(false); }
  p.hidden = !show;
  $("#post-btn").classList.toggle("active", show);
  if (show) {
    state.postUnread = 0;
    renderPosts();
    $("#post-text").focus();
  }
}

/** Sends the text box's contents to the host. */
async function sendPost(ev) {
  ev?.preventDefault();
  const box = $("#post-text"), text = box.value.trim();
  if (!text) return;
  try {
    const m = await api("/api/post", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ text }) });
    box.value = "";
    addPost(m);
  } catch (err) {
    note("Couldn't send: " + err.message, "err");
  }
}

/** Shows a message from the host with Copy / Open / Reply. It stays until
 *  dismissed (or for two minutes). */
function showPostNote(m) {
  const link = firstLink(m.text);
  const el = document.createElement("div");
  el.className = "note send";
  el.innerHTML = `${icon("i-chat")}<span><b>From the host:</b> ${esc(m.text.length > 140 ? m.text.slice(0, 140) + "…" : m.text)}</span>
    <button class="btn secondary" data-a="copy">Copy</button>
    ${link ? `<button class="btn secondary" data-a="open">Open</button>` : ""}
    <button class="btn primary" data-a="reply">Reply</button><button class="icon-btn" data-a="x" aria-label="Dismiss">${icon("i-x")}</button>`;
  el.addEventListener("click", (ev) => {
    const a = ev.target.closest("[data-a]")?.dataset.a;
    if (a === "copy") copyText(m.text, "message");
    if (a === "open") open(link, "_blank", "noopener");
    if (a === "reply") togglePosts(true);
    if (a) el.remove();
  });
  $("#notes").append(el);
  setTimeout(() => el.remove(), 120_000);
  // Flash the tab title when the page isn't in front.
  if (document.hidden) {
    const t = document.title;
    document.title = "💬 " + m.text.slice(0, 40);
    addEventListener("focus", () => { document.title = t; }, { once: true });
  }
}

/* ------------------------------------------------------------------ */
/* nearby servers and playlists                                       */
/* ------------------------------------------------------------------ */

/** Lists the other Pidgeon servers on the network in the sidebar. */
async function loadNearby() {
  if (!state.info.nearby) return;
  try {
    state.nearby = (await api("/api/nearby")).filter((n) => /^https?:\/\//.test(n.url));
    // Servers the flock opens in place are known by their ID.
    for (const n of state.nearby) if (n.path && n.id) state.flock.set(n.id, n);
    renderNearby();
    // A folder opened by its link before the list arrived gets its server's name.
    if (flockOf(state.path)) renderCrumbs();
  } catch {}
}

/** Draws the Nearby list. With the flock on, a server is a folder with its
 *  own subtree, like the tree above; otherwise it is a link to its page. */
function renderNearby() {
  const list = state.nearby;
  $("#nearby").hidden = !list.length;
  $("#nearby-list").innerHTML = list.map((n) => {
    const locked = n.auth && n.auth !== "open";
    const lock = locked ? `<span class="nb-lock" aria-label="needs a token">🔒</span>` : "";
    if (!n.path) {
      return `<li><a class="tnode" href="${esc(n.url)}" target="_blank" rel="noopener" title="${esc(n.url)}${locked ? " · needs a token" : ""}">
        ${icon("i-hex", "i ic-peer")}<span>${esc(n.name)}</span>${lock}</a></li>`;
    }
    const c = treeNode(n.path);
    const leaf = c.dirs && c.dirs.length === 0;
    return `<li role="treeitem" aria-expanded="${c.open}">
      <div class="tnode${n.path === state.path ? " current" : ""}" data-path="${esc(n.path)}" title="${esc(n.name)}${locked ? " · needs a token" : ""}">
        ${icon("i-chev", "i chev" + (c.open ? " open" : "") + (leaf ? " leaf" : ""))}
        ${icon("i-hex", "i ic-peer")}
        <span class="label">${esc(n.name)}</span>${lock}
      </div>
      ${c.open ? `<ul role="group">${treeChildren(n.path, 1)}</ul>` : ""}
    </li>`;
  }).join("");
}

/** Asks for the token (or user name and password) a server in the flock
 *  wants, in place of its listing. They are kept for this browser only. */
function showFlockLogin(err) {
  const f = flockOf(state.path);
  if (!f) return showEmpty("Couldn't open this folder", err.message, true);
  const basic = err.login === "basic";
  const st = $("#state");
  // The server's message names the peer, even before the Nearby list has.
  st.innerHTML = `${icon("i-hex", "i big")}<h3>${esc(err.message)}</h3>
    <div>${basic ? "Ask whoever runs it." : "It is shown in the terminal of the computer it runs on, and is part of its QR link."}</div>
    <form class="flock-login" id="flock-login">
      ${basic ? `<input name="user" placeholder="user name" autocomplete="username" required><input name="pass" type="password" placeholder="password" autocomplete="current-password" required>`
        : `<input name="token" placeholder="access token" autocomplete="off" spellcheck="false" required>`}
      <button class="btn primary">Connect</button>
    </form>
    <p class="flock-err" id="flock-err"></p>`;
  st.hidden = false;
  const form = $("#flock-login");
  $("input", form).focus();
  form.onsubmit = async (ev) => {
    ev.preventDefault();
    try {
      await api("/api/flock/login", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ id: f.id, ...Object.fromEntries(new FormData(form)) }),
      });
    } catch (e2) {
      $("#flock-err").textContent = e2.message;
      return;
    }
    note(`Connected to ${peerName(f.id)}`, "ok");
    // What failed without the token is worth asking again.
    for (const m of [state.tree, state.peek, state.cache]) for (const p of [...m.keys()]) if (p.startsWith(f.root)) m.delete(p);
    load(state.path, { force: true });
  };
}

/** Downloads a folder's videos and music as an .m3u playlist that media
 *  players stream straight from this server. */
function playlist(path, recursive) {
  const a = Object.assign(document.createElement("a"), { href: "/api/m3u?path=" + encodeURIComponent(path) + (recursive ? "&r=1" : ""), download: "" });
  document.body.append(a);
  a.click();
  a.remove();
  note("Open the playlist with VLC, mpv or another player to stream the folder");
}

/* ------------------------------------------------------------------ */
/* basket                                                             */
/* ------------------------------------------------------------------ */

/* The basket collects files and folders from anywhere in the tree (even
   from inside archives) and downloads them as one ZIP. It is kept in
   localStorage, so it survives reloads and moving between folders. */

/** The entries a basket action applies to: the selection, else the cursor row. */
function targetEntries() {
  if (state.sel.size) return [...state.sel.values()];
  const e = state.view[state.cursor];
  return e ? [e] : [];
}

/** Adds entries to the basket, or removes them if they are all in it already. */
function toggleBasket(entries = targetEntries()) {
  if (!entries.length) return note("Nothing selected");
  const items = entries.map((e) => ({ path: join(state.path, e.name), name: e.name, dir: !!e.dir, size: e.dir ? 0 : e.size }));
  const allIn = items.every((it) => state.basket.has(it.path));
  // Either take them all out, or put the missing ones in.
  for (const it of items) allIn ? state.basket.delete(it.path) : state.basket.set(it.path, it);
  basketChanged();
  const what = items.length === 1 ? items[0].name : `${items.length} items`;
  note(allIn ? `Removed ${what} from the basket` : `Added ${what} to the basket · ${state.basket.size} in total`, "ok");
}

/** Saves the basket and refreshes everything that shows it. */
function basketChanged() {
  saveJSON("basket", [...state.basket.values()]);
  state.basketBytes = null;
  refreshBasketSize();
  renderBasket();
  // Drawn rows show a marker for what's in the basket.
  for (const row of document.querySelectorAll("#list .row[data-i]")) {
    const e = state.view[+row.dataset.i];
    if (e) row.classList.toggle("in-basket", state.basket.has(join(state.path, e.name)));
  }
}

/** Asks the server for the total size of the basket's folders (debounced). */
let basketTimer = 0, basketToken = 0;
function refreshBasketSize() {
  clearTimeout(basketTimer);
  const folders = [...state.basket.values()].filter((b) => b.dir);
  if (!folders.length) return;
  const token = ++basketToken;
  basketTimer = setTimeout(async () => {
    const q = new URLSearchParams({ base: "/" });
    folders.forEach((f) => q.append("p", f.path.slice(1)));
    try {
      const r = await api("/api/size?" + q);
      if (token === basketToken) { state.basketBytes = r; renderBasket(); }
    } catch {}
  }, 300);
}

/** The basket's total size as text ("≥ 2.3 GB" while folders are partial). */
function basketSize() {
  const items = [...state.basket.values()];
  const files = items.reduce((n, b) => n + (b.dir ? 0 : b.size || 0), 0);
  if (!items.some((b) => b.dir)) return fmtBytes(files);
  const fb = state.basketBytes;
  return fb ? (fb.partial ? "≥ " : "") + fmtBytes(files + fb.bytes) : fmtBytes(files) + " + folders…";
}

/** Renders the status-bar chip and, when open, the basket popover. */
function renderBasket() {
  const n = state.basket.size;
  const chip = $("#sb-basket");
  chip.hidden = n === 0;
  $("#sb-basket-text").textContent = `${n} · ${basketSize()}`;
  if (!n && !$("#basket").hidden) toggleBasketPanel(false);
  $("#bk-meta").textContent = n ? `${n} item${n === 1 ? "" : "s"} · ${basketSize()}` : "";
  $("#bk-paths").checked = state.basketPaths;
  $("#bk-list").innerHTML = [...state.basket.values()].map((b) => `<li class="bk" data-p="${esc(b.path)}">
      ${fileIcon(b).html}
      <span class="bk-name" title="${esc(b.path)}">${esc(b.name)}</span>
      <span class="bk-where">${esc(placeName(parentOf(b.path)))}${b.dir ? "" : " · " + fmtBytes(b.size)}</span>
      <button class="icon-btn bk-x" title="Remove from basket" aria-label="Remove ${esc(b.name)} from basket">${icon("i-x")}</button>
    </li>`).join("");
}

/** Shows/hides the basket popover (it shares the downloads popover's spot). */
function toggleBasketPanel(force) {
  const p = $("#basket");
  const show = force ?? p.hidden;
  if (show) { togglePanel(false); togglePosts(false); }
  p.hidden = !show;
  if (show) renderBasket();
}

/** Downloads the basket as one ZIP. A missing item is dropped and reported. */
async function downloadBasket() {
  const items = [...state.basket.values()];
  if (!items.length) return note("The basket is empty");
  const name = `${state.info.root || "Pidgeon"} basket`;
  try {
    const r = await api("/api/zip", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ base: "/", paths: items.map((b) => b.path.slice(1)), store: state.zipStore, sums: state.zipSums, flat: !state.basketPaths, name }),
    });
    startDownload({ name: r.name, url: r.url, kind: "zip", total: r.size >= 0 ? r.size : -1, entry: { name: r.name } });
    toggleBasketPanel(false);
  } catch (err) {
    const gone = /^not found: (.*)$/.exec(err.message);
    // Files can disappear after they were added: drop them and say so.
    if (gone && state.basket.has(gone[1])) {
      state.basket.delete(gone[1]);
      basketChanged();
      return note(`${gone[1]} is no longer there, so it was taken out of the basket. Try again.`, "err");
    }
    note("Couldn't start the ZIP: " + err.message, "err");
  }
}

/** Empties the basket. */
function clearBasket() {
  state.basket.clear();
  basketChanged();
  note("Basket emptied");
}

/* ------------------------------------------------------------------ */
/* checksums                                                          */
/* ------------------------------------------------------------------ */

/** Shows a file's SHA-256 (computed by the server) in a dialog, with a box
 *  to paste the expected hash into and compare. */
async function showHash(e, path = join(state.path, e.name)) {
  const box = $("#dialog-box");
  box.innerHTML = `<h2>${icon("i-hash")}SHA-256</h2><p class="dlg-sub"><b>${esc(e.name)}</b> · ${fmtBytes(e.size)}</p>
    <div class="hash-out" id="hash-out"><span class="pv-loading">Reading the file on the server…</span></div>
    <div class="dlg-actions"><button class="btn secondary" id="hash-close">Close</button></div>`;
  $("#dialog").hidden = false;
  $("#hash-close").onclick = closeDialog;
  $("#hash-close").focus();
  let r;
  try {
    r = await api("/api/hash?path=" + encodeURIComponent(path));
  } catch (err) {
    if (!$("#dialog").hidden) $("#hash-out").innerHTML = `<span class="bad">Couldn't hash this file: ${esc(err.message)}</span>`;
    return;
  }
  // The dialog may have been closed (or reused) while the server was reading.
  if ($("#dialog").hidden || !$("#hash-out")) return;
  $("#hash-out").innerHTML = `<div class="dlg-link"><input id="hash-val" readonly value="${r.sha256}"><button class="btn primary" id="hash-copy">Copy</button></div>
    <label class="dlg-row hash-cmp">Compare with<input id="hash-cmp" type="text" spellcheck="false" autocomplete="off" placeholder="paste the expected hash"></label>
    <p class="dlg-note" id="hash-verdict"></p>`;
  $("#hash-val").select();
  $("#hash-copy").onclick = () => copyText(r.sha256, "SHA-256");
  $("#hash-cmp").oninput = (ev) => {
    const v = ev.target.value.trim().toLowerCase();
    const out = $("#hash-verdict");
    out.className = "dlg-note" + (v ? (v === r.sha256 ? " ok" : " bad") : "");
    out.textContent = !v ? "" : v === r.sha256 ? "✓ Match: the file is exactly what you expected." : "✕ No match: this isn't the file you expected (or it was damaged).";
  };
}

/* ------------------------------------------------------------------ */
/* misc                                                               */
/* ------------------------------------------------------------------ */

/** Flips adding SHA256SUMS to ZIPs. */
function toggleZipSums() {
  state.zipSums = !state.zipSums;
  saveJSON("zipSums", state.zipSums);
  note(state.zipSums ? "ZIPs now include SHA256SUMS (check with: sha256sum -c SHA256SUMS)" : "ZIPs no longer include SHA256SUMS");
}

/** Flips "always store" for ZIPs (bigger, but exact size and resumable). */
function toggleZipStore() {
  state.zipStore = !state.zipStore;
  saveJSON("zipStore", state.zipStore);
  note(state.zipStore ? "ZIPs are now stored uncompressed: exact size and resumable" : "ZIPs are compressed again (only all-compressed selections are resumable)");
}

/** Shows when the server will stop by itself (--expire, --once, --max-bytes). */
function renderLimits() {
  const i = state.info, el = $("#sb-limit");
  const parts = [];
  if (i.expires) {
    const left = (i.expires - Date.now()) / 1000;
    parts.push(left > 0 ? `closes in ${fmtDur(left < 120 ? left : Math.ceil(left / 60) * 60)}` : "closing");
  }
  if (i.once) parts.push("closes after one download");
  if (i.maxBytes) parts.push(`${fmtBytes(i.maxBytes)} data limit`);
  el.hidden = !parts.length;
  el.textContent = parts.length ? "⏱ " + parts.join(" · ") : "";
  el.classList.toggle("warn", !!i.expires && i.expires - Date.now() < 60_000);
}

/** Changes the sort column (toggling direction if it is already active). */
function setSort(key) {
  state.sort = state.sort.key === key ? { key, dir: -state.sort.dir } : { key, dir: key === "name" ? 1 : -1 };
  saveJSON("sort", state.sort);
  const cur = state.view[state.cursor]?.name;
  renderList();
  // Keep the cursor on the same entry after re-sorting.
  const i = state.view.findIndex((e) => e.name === cur);
  if (i >= 0) setCursor(i);
}

/** Removes finished downloads from the panel. */
function clearFinished() {
  for (const [id, d] of state.downloads) if (d.state === "done" || d.state === "failed") state.downloads.delete(id);
  renderDownloads();
}

/** Updates the left side of the status bar. */
function updateStatus() {
  const n = state.view.length;
  const dirs = state.view.filter((e) => e.dir).length;
  $("#sb-items").textContent = `${n - dirs} file${n - dirs === 1 ? "" : "s"}, ${dirs} folder${dirs === 1 ? "" : "s"}`;
}

/* ------------------------------------------------------------------ */
/* wiring                                                             */
/* ------------------------------------------------------------------ */

/** Attaches all event handlers. */
function wire() {
  addEventListener("hashchange", () => load(pathFromHash()));

  const list = $("#list");

  // Virtual scrolling: draw the newly visible slice at most once per frame.
  let scrollFrame = 0;
  list.addEventListener("scroll", () => {
    if (!scrollFrame) scrollFrame = requestAnimationFrame(() => { scrollFrame = 0; renderWindow(); });
  }, { passive: true });
  // A taller list (or the phone breakpoint changing --row) needs a redraw.
  new ResizeObserver(() => { measureRow(); renderWindow(true); }).observe(list);

  // Hovering a folder for a moment prefetches it, so opening it is instant.
  let hoverTimer;
  const hoverPrefetch = (sel, pathOf) => (ev) => {
    clearTimeout(hoverTimer);
    const el = ev.target.closest(sel);
    if (!el || touch) return;
    hoverTimer = setTimeout(() => { const p = pathOf(el); if (p) prefetch(p); }, 120);
  };
  list.addEventListener("pointerover", hoverPrefetch(".row.dir", (row) => {
    const e = state.view[+row.dataset.i];
    return e && join(state.path, e.name);
  }));
  $("#tree").addEventListener("pointerover", hoverPrefetch(".tnode", (node) => node.dataset.path));
  $("#nearby-list").addEventListener("pointerover", hoverPrefetch(".tnode", (node) => node.dataset.path));

  // Checkbox clicks toggle selection without moving to other rows.
  list.addEventListener("change", (ev) => {
    const row = ev.target.closest(".row");
    if (!row) return;
    const i = +row.dataset.i;
    setSelected(i, ev.target.checked);
    state.anchor = i;
    setCursor(i);
    selectionChanged();
  });

  // Row clicks follow VS Code explorer rules: click selects, ⌘/Ctrl-click
  // toggles, Shift-click selects a range, folders open on click and files
  // download on double-click. On touch screens a tap downloads a file.
  list.addEventListener("click", (ev) => {
    const row = ev.target.closest(".row");
    if (!row || row.classList.contains("skel") || ev.target.closest(".cb")) return;
    const i = +row.dataset.i, e = state.view[i];
    if (ev.target.closest(".act-dl")) { e.dir ? downloadZip([e.name]) : downloadFile(e); return; }
    if (ev.target.closest(".act-link")) { copyText(entryURL(e), "link"); return; }
    // The eye opens the in-app preview; with a modifier it stays a normal link.
    if (ev.target.closest(".act-preview")) {
      if (!(ev.metaKey || ev.ctrlKey || ev.shiftKey)) { ev.preventDefault(); openPreview(e); }
      return;
    }
    setCursor(i);
    if (ev.metaKey || ev.ctrlKey) { setSelected(i, !state.sel.has(e.name)); state.anchor = i; selectionChanged(); return; }
    if (ev.shiftKey) { selectRange(state.anchor, i); selectionChanged(); return; }
    state.anchor = i;
    // While something is selected on a phone, taps extend the selection.
    if (touch && state.sel.size) { setSelected(i, !state.sel.has(e.name)); selectionChanged(); return; }
    if (e.dir) { go(join(state.path, e.name)); return; }
    // On a phone a tap on an archive looks inside; its download button downloads it.
    if (touch) { openEntry(e); return; }
    selectRange(i, i);
    selectionChanged();
  });
  list.addEventListener("dblclick", (ev) => {
    const row = ev.target.closest(".row");
    if (!row || ev.target.closest(".cb, .acts")) return;
    const e = state.view[+row.dataset.i];
    if (e && !e.dir) openEntry(e);
  });
  // A thumbnail the server can't make falls back to the file's icon, and
  // isn't asked for again.
  list.addEventListener("error", (ev) => {
    const img = ev.target;
    if (img.tagName !== "IMG" || !img.closest(".thumb")) return;
    state.thumbFail.add(img.dataset.p);
    img.closest(".row")?.classList.remove("has-thumb");
    img.remove();
  }, true);
  list.addEventListener("load", (ev) => {
    if (ev.target.tagName === "IMG") ev.target.closest(".thumb")?.classList.add("loaded");
  }, true);
  list.addEventListener("contextmenu", (ev) => {
    const row = ev.target.closest(".row");
    if (row && !row.classList.contains("skel")) showContextMenu(ev, +row.dataset.i);
  });

  // Keyboard navigation in the list.
  list.addEventListener("keydown", (ev) => {
    const n = state.view.length;
    if (!n) return;
    const mod = ev.metaKey || ev.ctrlKey;
    // In grid view, up/down jump a whole row of tiles.
    const cols = gridCols();
    const moveTo = (i) => {
      setCursor(i);
      // Shift extends from the anchor; plain arrows just move the cursor.
      if (ev.shiftKey) { selectRange(state.anchor, state.cursor); selectionChanged(); } else state.anchor = state.cursor;
    };
    switch (ev.key) {
      case "ArrowDown": ev.preventDefault(); moveTo(state.cursor + cols); break;
      case "ArrowUp":
        ev.preventDefault();
        // ⌘↑ / Alt+↑ goes up a folder, like Finder and VS Code.
        if (mod || ev.altKey) go(parentOf(state.path)); else moveTo(state.cursor - cols);
        break;
      case "ArrowLeft":
      case "ArrowRight":
        // Left/right only move between tiles; the list has no columns.
        if (state.layout !== "grid") break;
        ev.preventDefault();
        moveTo(state.cursor + (ev.key === "ArrowRight" ? 1 : -1));
        break;
      case "Home": ev.preventDefault(); moveTo(0); break;
      case "End": ev.preventDefault(); moveTo(n - 1); break;
      case "PageDown": ev.preventDefault(); moveTo(state.cursor + 15); break;
      case "PageUp": ev.preventDefault(); moveTo(state.cursor - 15); break;
      case " ": {
        ev.preventDefault();
        const e = state.view[state.cursor];
        // Shift+Space previews the cursor row, like Quick Look.
        if (ev.shiftKey) { if (previewKind(e)) openPreview(e); break; }
        setSelected(state.cursor, !state.sel.has(e.name));
        state.anchor = state.cursor;
        selectionChanged();
        break;
      }
      case "Enter":
        ev.preventDefault();
        // Enter downloads the selection if there is one, else opens the cursor row.
        if (state.sel.size > 1) downloadSelection(); else openEntry(state.view[state.cursor]);
        break;
      case "Backspace": ev.preventDefault(); go(parentOf(state.path)); break;
      case "B": if (ev.shiftKey && !mod) { ev.preventDefault(); toggleBasket(); } break;
      case "a": if (mod) { ev.preventDefault(); selectRange(0, n - 1); selectionChanged(); } break;
      case "Escape": if (state.sel.size) { ev.preventDefault(); clearSelection(); } break;
      case "ContextMenu": {
        const row = $(`#list .row[data-i="${state.cursor}"]`);
        if (row) { const r = row.getBoundingClientRect(); showContextMenu({ preventDefault() {}, clientX: r.left + 60, clientY: r.bottom }, state.cursor); }
        break;
      }
    }
  });

  $("#select-all").addEventListener("change", (ev) => {
    if (ev.target.checked) selectRange(0, state.view.length - 1); else clearSelection();
    selectionChanged();
  });
  document.querySelectorAll(".sort").forEach((b) => b.addEventListener("click", () => setSort(b.dataset.sort)));

  // Explorer tree: chevron toggles, label navigates. Clicking the folder
  // that is already open in the file list collapses or expands it instead.
  // Servers in the Nearby list that open in place work the same way; the
  // others are plain links.
  const treeClick = (ev) => {
    const node = ev.target.closest(".tnode");
    if (!node?.dataset.path) return;
    const p = node.dataset.path, n = treeNode(p);
    if (ev.target.closest(".chev") || p === state.path) {
      n.open = !n.open;
      if (n.open) loadTreeDirs(p);
      renderTree();
      return;
    }
    n.open = true;
    go(p);
  };
  $("#tree").addEventListener("click", treeClick);
  $("#nearby-list").addEventListener("click", treeClick);
  $("#tree-root").addEventListener("click", () => go("/"));
  $("#collapse-all").addEventListener("click", () => {
    for (const [p, n] of state.tree) if (p !== "/") n.open = false;
    renderTree();
  });

  $("#sel-clear").addEventListener("click", clearSelection);
  $("#sel-basket").addEventListener("click", () => toggleBasket());
  $("#sel-download").addEventListener("click", downloadSelection);
  $("#refresh").addEventListener("click", () => load(state.path, { force: true }));
  $("#layout-btn").addEventListener("click", toggleLayout);

  $("#menu-btn").addEventListener("click", toggleSidebar);
  $("#scrim").addEventListener("click", closeDrawer);
  $("#dl-btn").addEventListener("click", () => togglePanel());
  $("#sb-dl").addEventListener("click", () => togglePanel());
  $("#panel-close").addEventListener("click", () => togglePanel(false));
  $("#dl-clear").addEventListener("click", clearFinished);
  $("#dl-list").addEventListener("click", (ev) => {
    const li = ev.target.closest(".dl");
    // "download again" after the file changed mid-download.
    if (li && ev.target.closest(".dl-retry")) {
      const d = state.downloads.get(li.dataset.id);
      if (d) { d.state = "dismissed"; startDownload({ name: d.name, url: d.url, kind: d.kind, total: d.kind === "zip" ? -1 : d.total, entry: d.entry }); }
      return;
    }
    if (li && ev.target.closest(".dl-x")) {
      const d = state.downloads.get(li.dataset.id);
      if (d) d.state = "dismissed";
      renderDownloads();
      maybeCloseEvents();
    }
  });

  // Basket popover.
  $("#sb-basket").addEventListener("click", () => toggleBasketPanel());
  $("#bk-close").addEventListener("click", () => toggleBasketPanel(false));
  $("#bk-clear").addEventListener("click", clearBasket);
  $("#bk-download").addEventListener("click", downloadBasket);
  $("#bk-paths").addEventListener("change", (ev) => { state.basketPaths = ev.target.checked; saveJSON("basketPaths", state.basketPaths); });
  $("#bk-list").addEventListener("click", (ev) => {
    const li = ev.target.closest(".bk");
    if (!li) return;
    // The × removes; the name jumps to where the item lives.
    if (ev.target.closest(".bk-x")) { state.basket.delete(li.dataset.p); basketChanged(); return; }
    const b = state.basket.get(li.dataset.p);
    if (!b) return;
    toggleBasketPanel(false);
    if (b.dir) go(b.path); else { state.reveal = b.name; go(parentOf(b.path)); }
  });

  $("#theme-btn").addEventListener("click", toggleTheme);
  matchMedia("(prefers-color-scheme: light)").addEventListener("change", () => {
    // Only "auto" follows the OS live.
    if (document.documentElement.dataset.themePref === "auto") setTheme("auto");
  });

  $("#cc").addEventListener("click", () => openPalette());
  $("#palette").addEventListener("mousedown", (ev) => { if (ev.target.id === "palette") closePalette(); });
  $("#pal-input").addEventListener("input", () => renderPalette());
  $("#pal-input").addEventListener("keydown", (ev) => {
    if (ev.key === "ArrowDown") { ev.preventDefault(); palMove(1); }
    else if (ev.key === "ArrowUp") { ev.preventDefault(); palMove(-1); }
    else if (ev.key === "Enter") { ev.preventDefault(); palRun(); }
    else if (ev.key === "Escape") { ev.preventDefault(); closePalette(); }
  });
  $("#pal-list").addEventListener("click", (ev) => {
    const li = ev.target.closest(".pal-item");
    if (li) palRun(+li.dataset.i);
  });

  document.addEventListener("mousedown", (ev) => { if (!ev.target.closest("#ctx")) closeContextMenu(); });
  addEventListener("blur", closeContextMenu);

  // Workbench shortcuts.
  addEventListener("keydown", (ev) => {
    const mod = ev.metaKey || ev.ctrlKey;
    const k = ev.key.toLowerCase();
    // An open preview or dialog owns the keyboard.
    if (!$("#dialog").hidden) { if (ev.key === "Escape") { ev.preventDefault(); closeDialog(); } return; }
    if (!$("#preview").hidden) {
      if (ev.key === "Escape") { ev.preventDefault(); closePreview(); }
      else if (ev.key === "ArrowLeft" && !ev.target.closest("video, audio")) { ev.preventDefault(); stepPreview(-1); }
      else if (ev.key === "ArrowRight" && !ev.target.closest("video, audio")) { ev.preventDefault(); stepPreview(1); }
      return;
    }
    if (mod && k === "p") { ev.preventDefault(); openPalette(ev.shiftKey ? ">" : ""); }
    else if (ev.key === "F1") { ev.preventDefault(); openPalette(">"); }
    else if (mod && k === "b" && !ev.shiftKey) { ev.preventDefault(); toggleSidebar(); }
    else if (mod && k === "j" && !ev.shiftKey) { ev.preventDefault(); togglePanel(); }
    else if (ev.key === "Escape") { closeContextMenu(); closeDrawer(); if (!$("#basket").hidden) toggleBasketPanel(false); }
  });

  makeSash($("#sash"), "--sidebar-w", { axis: "x", min: 170, max: 520 });

  // Preview controls.
  $("#pv-close").addEventListener("click", closePreview);
  $("#pv-prev").addEventListener("click", () => stepPreview(-1));
  $("#pv-next").addEventListener("click", () => stepPreview(1));
  $("#pv-dl").addEventListener("click", () => { const e = pv.list[pv.i]; if (e) startDownload({ name: e.name, url: "/f" + encPath(e.path), kind: "file", total: e.size, entry: e }); });
  $("#preview").addEventListener("mousedown", (ev) => { if (ev.target.id === "preview") closePreview(); });
  // Swipe between files on touch screens.
  let swipeX = null;
  $("#pv-body").addEventListener("touchstart", (ev) => { swipeX = ev.touches.length === 1 ? ev.touches[0].clientX : null; }, { passive: true });
  $("#pv-body").addEventListener("touchend", (ev) => {
    if (swipeX == null) return;
    const dx = ev.changedTouches[0].clientX - swipeX;
    swipeX = null;
    if (Math.abs(dx) > 60 && pv.kind === "image") stepPreview(dx < 0 ? 1 : -1);
  });
  $("#dialog").addEventListener("mousedown", (ev) => { if (ev.target.id === "dialog") closeDialog(); });

  // Uploads: the button, the file picker, and drag and drop anywhere.
  $("#up-btn").addEventListener("click", pickUploads);
  $("#up-input").addEventListener("change", (ev) => { uploadFiles([...ev.target.files]); ev.target.value = ""; });
  let dragDepth = 0;
  const hasFiles = (ev) => state.info.inbox && [...(ev.dataTransfer?.types || [])].includes("Files");
  addEventListener("dragenter", (ev) => { if (!hasFiles(ev)) return; ev.preventDefault(); dragDepth++; $("#drop").hidden = false; });
  addEventListener("dragover", (ev) => { if (hasFiles(ev)) ev.preventDefault(); });
  addEventListener("dragleave", () => { if (dragDepth && --dragDepth === 0) $("#drop").hidden = true; });
  addEventListener("drop", (ev) => {
    if (!hasFiles(ev)) return;
    ev.preventDefault();
    dragDepth = 0;
    $("#drop").hidden = true;
    uploadFiles([...ev.dataTransfer.files]);
  });

  // Pigeon Post: the panel, its form (Enter sends, Shift+Enter breaks the
  // line) and the copy buttons on messages.
  $("#post-btn").addEventListener("click", () => togglePosts());
  $("#post-close").addEventListener("click", () => togglePosts(false));
  $("#post-form").addEventListener("submit", sendPost);
  $("#post-text").addEventListener("keydown", (ev) => {
    if (ev.key === "Enter" && !ev.shiftKey && !ev.isComposing) sendPost(ev);
    if (ev.key === "Escape") togglePosts(false);
  });
  $("#post-list").addEventListener("click", (ev) => {
    const li = ev.target.closest(".msg");
    if (li && ev.target.closest(".msg-copy")) copyText(state.posts.get(+li.dataset.id)?.text || "", "message");
  });

  // The session-limit countdown in the status bar.
  setInterval(renderLimits, 1000);

  // Keep "starting…" labels fresh even without server events.
  setInterval(() => { if ([...state.downloads.values()].some((d) => d.state === "queued")) renderDownloads(); }, 1000);
}

/** Boots the app. */
async function main() {
  runSplash();
  setTheme(document.documentElement.dataset.themePref || loadJSON("theme", null) || "dark");
  $("#cc-kbd").textContent = `${MOD}P`;
  measureRow();
  setLayout(state.layout);
  wire();
  renderDownloads();
  renderBasket();
  refreshBasketSize();
  ensureEvents();
  load(pathFromHash());
  try {
    state.info = await api("/api/info");
    $("#sb-auth").textContent = { token: "🔒 token", basic: "🔒 basic auth", open: "no auth" }[state.info.auth] || "";
    renderCrumbs();
    renderTree();
    // Uploads are offered only when the host turned the inbox on.
    $("#up-btn").hidden = !state.info.inbox;
    $("#dl-empty").textContent = state.info.inbox
      ? "Nothing yet. Click a file to download it, or drop files anywhere to send them to this computer."
      : $("#dl-empty").textContent;
    renderLimits();
    renderBasket();
    loadPosts();
    loadNearby();
    setInterval(loadNearby, 60_000);
    if (state.info.tv) $("#sb-auth").textContent += " · 📺 TV";
    // The grid was drawn before the server said whether it makes thumbnails.
    if (state.info.thumbs && state.layout === "grid") renderWindow(true);
  } catch {}
}

main();
