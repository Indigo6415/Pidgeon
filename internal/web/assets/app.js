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
const parentOf = (p) => (p === "/" ? "/" : p.slice(0, p.lastIndexOf("/")) || "/");
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
const PREVIEWABLE = new Set(["image", "video", "audio", "pdf", "code"]);

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
  if (!r.ok) throw new Error((await r.text()).trim() || r.statusText);
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
    .then((d) => { rememberListing(path, d.entries); return d.entries; })
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
    showEmpty("Couldn't open this folder", err.message, true);
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
  const parts = state.path === "/" ? [] : state.path.slice(1).split("/");
  const rootName = state.info.root || "Files";
  let acc = "";
  let html = parts.length ? `<a href="#/">${esc(rootName)}</a>` : "";
  // One link per ancestor; the current folder is the title below.
  parts.slice(0, -1).forEach((p) => {
    acc += "/" + p;
    html += icon("i-chev", "i sep") + `<a href="#${esc(encPath(acc))}">${esc(p)}</a>`;
  });
  const nav = $("#crumbs");
  nav.innerHTML = html;
  nav.hidden = !parts.length;
  nav.scrollLeft = nav.scrollWidth;
  const title = parts.at(-1) || rootName;
  $("#folder-title").textContent = title;
  $("#folder-title").title = title;
  $("#cc-label").textContent = `${title} — ${state.info.name || "Pidgeon"}`;
  document.title = `${title} — ${state.info.name || "Pidgeon"}`;
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

/** Builds the HTML for row i of the current view. */
function rowHTML(i) {
  const e = state.view[i];
  const fi = fileIcon(e);
  const p = join(state.path, e.name);
  const sel = state.sel.has(e.name);
  const preview = !e.dir && PREVIEWABLE.has(fi.kind)
    ? `<a class="icon-btn act-preview" href="/f${esc(encPath(p))}?inline=1" target="_blank" rel="noopener" title="Open Preview" aria-label="Preview ${esc(e.name)}">${icon("i-eye")}</a>` : "";
  return `<li class="row${e.dir ? " dir" : ""}${sel ? " sel" : ""}${i === state.cursor ? " cursor" : ""}" data-i="${i}" role="option" aria-selected="${sel}">
    <label class="cb"><input type="checkbox" tabindex="-1" ${sel ? "checked" : ""} aria-label="Select ${esc(e.name)}"><span></span></label>
    <span class="name-cell">${fi.html}<span class="meta"><span class="name">${esc(e.name)}</span><span class="sub">${esc(rowSub(e))}</span></span></span>
    <span class="size">${e.dir ? esc(folderSize(e)) : fmtBytes(e.size)}</span>
    <span class="date">${fmtDate(e.mtime)}</span>
    <span class="acts">${preview}<button class="icon-btn act-link" title="Copy Link" aria-label="Copy link">${icon("i-link")}</button><button class="icon-btn act-dl" title="${e.dir ? "Download as ZIP" : "Download"}" aria-label="Download ${esc(e.name)}">${icon("i-download")}</button></span>
  </li>`;
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

/** Expands every ancestor of path so the current folder is visible. */
function revealInTree(path) {
  let acc = "/";
  treeNode("/").open = true;
  loadTreeDirs("/");
  // Open and load each ancestor in turn.
  for (const seg of path === "/" ? [] : path.slice(1).split("/")) {
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
  // Scroll the current folder into view once per navigation, not on every
  // lazy update, so the tree doesn't jump while you scroll it.
  if (treeRevealed !== state.path) {
    const cur = $("#tree .tnode.current");
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

/** Opens a folder or downloads a file. */
function openEntry(e) {
  if (e.dir) go(join(state.path, e.name));
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
  state.downloads.set(id, { id, name, kind, total, entry, sent: 0, rate: 0, state: "queued", added: Date.now() });

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
      body: JSON.stringify({ base: state.path, paths: names }),
    });
    startDownload({ name: r.name, url: r.url, kind: "zip", total: -1, entry: { name: r.name } });
  } catch (err) {
    note("Couldn't start the ZIP: " + err.message, "err");
  }
}

/* ------------------------------------------------------------------ */
/* downloads panel                                                    */
/* ------------------------------------------------------------------ */

let events = null, eventsIdle = null;
/** Opens the progress stream while any download is unfinished. */
function ensureEvents() {
  clearTimeout(eventsIdle);
  if (events) return;
  events = new EventSource("/api/events?c=" + encodeURIComponent(clientId));
  events.onmessage = (ev) => {
    let items;
    try { items = JSON.parse(ev.data); } catch { return; }
    // Merge the server's view into our records; the latest request for an
    // ID wins (a download can be retried or resumed).
    for (const it of items) {
      const d = state.downloads.get(it.id);
      if (!d || d.state === "dismissed") continue;
      d.sent = it.sent;
      if (it.total >= 0) d.total = it.total;
      d.rate = it.rate;
      d.state = it.state === "active" ? "active" : it.state === "done" ? "done" : "failed";
    }
    renderDownloads();
    maybeCloseEvents();
  };
}

/** Closes the stream a few seconds after everything has finished. */
function maybeCloseEvents() {
  const pending = [...state.downloads.values()].some((d) => d.state === "queued" || d.state === "active");
  clearTimeout(eventsIdle);
  if (!pending && events) eventsIdle = setTimeout(() => { events?.close(); events = null; }, 5000);
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
  $("#sb-dl-text").textContent = active.length
    ? `${active.length} downloading · ${fmtBytes(rate)}/s`
    : list.length ? `${list.length} download${list.length > 1 ? "s" : ""}` : "No downloads";
  $("#panel-meta").textContent = active.length ? `${active.length} active · ${fmtBytes(rate)}/s` : "";

  $("#dl-list").innerHTML = list.map((d) => {
    let info, prog;
    const pct = d.total > 0 ? Math.min(100, (d.sent / d.total) * 100) : 0;
    switch (d.state) {
      case "queued":
        info = (Date.now() - d.added) / 1000 > 15 ? "waiting for browser…" : "starting…";
        prog = `<div class="prog queued"><i></i></div>`;
        break;
      case "active":
        if (d.total > 0) {
          const eta = d.rate > 0 ? fmtDur((d.total - d.sent) / d.rate) : "";
          info = `${pct.toFixed(0)}% · ${fmtBytes(d.sent)}/${fmtBytes(d.total)} · ${fmtBytes(d.rate)}/s${eta ? " · " + eta : ""}`;
          prog = `<div class="prog"><i style="width:${pct.toFixed(1)}%"></i></div>`;
        } else {
          info = `${fmtBytes(d.sent)} · ${fmtBytes(d.rate)}/s · zip`;
          prog = `<div class="prog ind"><i></i></div>`;
        }
        break;
      case "done":
        info = `<span class="ok">✓</span> ${fmtBytes(d.sent)}`;
        prog = `<div class="prog done"><i></i></div>`;
        break;
      default:
        info = `<span class="bad">✕ interrupted</span> at ${fmtBytes(d.sent)}`;
        prog = `<div class="prog failed"><i></i></div>`;
    }
    return `<li class="dl ${d.state}" data-id="${esc(d.id)}">
      ${fileIcon(d.entry || { name: d.name }).html}
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

const pal = { items: [], active: 0 };

/** The commands available from "> ". */
function commands() {
  const sel = state.sel.size;
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

/** Rebuilds the palette's result list from the input. */
function renderPalette() {
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
  }
  pal.active = 0;
  const list = $("#pal-list");
  if (!pal.items.length) {
    list.innerHTML = `<li class="pal-empty">${isCmd ? "No matching commands" : "No matching files in this folder — type > for commands"}</li>`;
    return;
  }
  list.innerHTML = pal.items.map((it, i) => `<li class="pal-item${i === 0 ? " active" : ""}" data-i="${i}" role="option">
    ${it.iconHtml || ""}<span class="lbl">${it.html}</span>${it.desc ? `<span class="desc">${esc(it.desc)}</span>` : ""}${it.key ? `<span class="key">${esc(it.key)}</span>` : ""}
  </li>`).join("");
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
  const items = many
    ? [{ label: `Download ${state.sel.size} Items as ZIP`, run: downloadSelection }]
    : e.dir
      ? [{ label: "Open", run: () => openEntry(e) }, { label: "Download as ZIP", run: () => downloadZip([e.name]) }, "-", { label: "Copy Link", run: () => copyText(entryURL(e), "link") }]
      : [
        { label: "Download", run: () => downloadFile(e) },
        ...(PREVIEWABLE.has(fileIcon(e).kind) ? [{ label: "Open Preview", run: () => open("/f" + encPath(join(state.path, e.name)) + "?inline=1", "_blank", "noopener") }] : []),
        "-",
        { label: "Copy Link", run: () => copyText(entryURL(e), "link") },
        { label: "Copy as cURL", run: () => copyText(curlFor(e), "curl command") },
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
/* misc                                                               */
/* ------------------------------------------------------------------ */

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
    if (ev.target.closest(".act-preview")) return; // native link
    setCursor(i);
    if (ev.metaKey || ev.ctrlKey) { setSelected(i, !state.sel.has(e.name)); state.anchor = i; selectionChanged(); return; }
    if (ev.shiftKey) { selectRange(state.anchor, i); selectionChanged(); return; }
    state.anchor = i;
    // While something is selected on a phone, taps extend the selection.
    if (touch && state.sel.size) { setSelected(i, !state.sel.has(e.name)); selectionChanged(); return; }
    if (e.dir) { go(join(state.path, e.name)); return; }
    if (touch) { downloadFile(e); return; }
    selectRange(i, i);
    selectionChanged();
  });
  list.addEventListener("dblclick", (ev) => {
    const row = ev.target.closest(".row");
    if (!row || ev.target.closest(".cb, .acts")) return;
    const e = state.view[+row.dataset.i];
    if (e && !e.dir) downloadFile(e);
  });
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
  $("#tree").addEventListener("click", (ev) => {
    const node = ev.target.closest(".tnode");
    if (!node) return;
    const p = node.dataset.path, n = treeNode(p);
    if (ev.target.closest(".chev") || p === state.path) {
      n.open = !n.open;
      if (n.open) loadTreeDirs(p);
      renderTree();
      return;
    }
    n.open = true;
    go(p);
  });
  $("#tree-root").addEventListener("click", () => go("/"));
  $("#collapse-all").addEventListener("click", () => {
    for (const [p, n] of state.tree) if (p !== "/") n.open = false;
    renderTree();
  });

  $("#sel-clear").addEventListener("click", clearSelection);
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
    if (li && ev.target.closest(".dl-x")) {
      const d = state.downloads.get(li.dataset.id);
      if (d) d.state = "dismissed";
      renderDownloads();
      maybeCloseEvents();
    }
  });

  $("#theme-btn").addEventListener("click", toggleTheme);
  matchMedia("(prefers-color-scheme: light)").addEventListener("change", () => {
    // Only "auto" follows the OS live.
    if (document.documentElement.dataset.themePref === "auto") setTheme("auto");
  });

  $("#cc").addEventListener("click", () => openPalette());
  $("#palette").addEventListener("mousedown", (ev) => { if (ev.target.id === "palette") closePalette(); });
  $("#pal-input").addEventListener("input", renderPalette);
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
    if (mod && k === "p") { ev.preventDefault(); openPalette(ev.shiftKey ? ">" : ""); }
    else if (ev.key === "F1") { ev.preventDefault(); openPalette(">"); }
    else if (mod && k === "b" && !ev.shiftKey) { ev.preventDefault(); toggleSidebar(); }
    else if (mod && k === "j" && !ev.shiftKey) { ev.preventDefault(); togglePanel(); }
    else if (ev.key === "Escape") { closeContextMenu(); closeDrawer(); }
  });

  makeSash($("#sash"), "--sidebar-w", { axis: "x", min: 170, max: 520 });

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
  load(pathFromHash());
  try {
    state.info = await api("/api/info");
    $("#sb-auth").textContent = { token: "🔒 token", basic: "🔒 basic auth", open: "no auth" }[state.info.auth] || "";
    renderCrumbs();
    renderTree();
  } catch {}
}

main();
