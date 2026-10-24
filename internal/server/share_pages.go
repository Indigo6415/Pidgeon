package server

import "html/template"

// The PIN page and the file-request page are self-contained like the
// other share pages: share visitors can't load the app's assets.

// sharePageStyle is the look both pages share (VS Code Modern colours).
const sharePageStyle = `
  :root { --bg:#181818; --card:#1f1f1f; --fg:#cccccc; --strong:#e7e7e7; --muted:#9d9d9d; --line:#2b2b2b; --input:#313131; --accent:#0078d4; --link:#4daafc; --ok:#89d185; --err:#f85149; }
  @media (prefers-color-scheme: light) { :root { --bg:#f8f8f8; --card:#fff; --fg:#3b3b3b; --strong:#1f1f1f; --muted:#6f6f6f; --line:#e5e5e5; --input:#fff; --accent:#005fb8; --link:#005fb8; --ok:#388a34; --err:#e51400; } }
  * { box-sizing:border-box } body { margin:0; min-height:100dvh; background:var(--bg); color:var(--fg);
    font:14px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",system-ui,sans-serif; padding:24px 16px }
  main { max-width:560px; margin:0 auto; background:var(--card); border:1px solid var(--line); border-radius:12px; padding:24px }
  h1 { margin:0 0 4px; font-size:20px; color:var(--strong); overflow-wrap:anywhere }
  p { margin:0; color:var(--muted) }
  .btn { display:inline-flex; align-items:center; justify-content:center; height:36px; padding:0 16px; border:0; border-radius:7px;
    background:var(--accent); color:#fff; font:inherit; font-weight:600; cursor:pointer }
  footer { text-align:center; color:var(--muted); font-size:12px; margin-top:14px }
`

// pinPageTmpl asks for a link's PIN.
var pinPageTmpl = template.Must(template.New("pin").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="dark light">
<title>{{.Brand}} · PIN</title>
<style>` + sharePageStyle + `
  main { max-width:360px; text-align:center }
  input { width:100%; margin:18px 0 12px; font:600 24px/1 ui-monospace,Menlo,Consolas,monospace; letter-spacing:.3em; text-align:center;
    padding:12px; border-radius:8px; border:1px solid var(--line); background:var(--input); color:var(--strong) }
  .btn { width:100% } .err { color:var(--err); font-size:13px; margin:-4px 0 10px }
</style></head><body>
<main>
  <form method="post" action="/s/{{.ID}}">
    <h1>Enter the PIN</h1>
    <p>This link opens {{.What}}. Whoever sent it has the PIN.</p>
    <input name="pin" inputmode="numeric" pattern="[0-9]*" autocomplete="one-time-code" maxlength="12" autofocus required aria-label="PIN">
    <input type="hidden" name="next" value="{{.Next}}">
    {{if .Wrong}}<div class="err">That PIN didn't match.</div>{{end}}
    <button class="btn" type="submit">Continue</button>
  </form>
</main>
<footer>Shared with {{.Brand}}</footer>
</body></html>`))

// requestPageTmpl is a file request's drop zone. Files go up one at a time
// with a progress bar each; a quick pre-check skips files the host already
// has (same fingerprint as the web app and the server, see dedupe.go).
var requestPageTmpl = template.Must(template.New("request").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="dark light">
<title>{{.Title}} · {{.Brand}}</title>
<style>` + sharePageStyle + `
  .notes { font-size:12px; margin-top:2px }
  .zone { display:flex; flex-direction:column; align-items:center; gap:10px; margin-top:18px; padding:30px 16px; border:2px dashed var(--line);
    border-radius:12px; text-align:center; cursor:pointer; transition:border-color .15s, background .15s }
  .zone.over { border-color:var(--link); background:color-mix(in srgb, var(--link) 10%, transparent) }
  .zone svg { width:34px; height:34px; fill:none; stroke:var(--link); stroke-width:1.5; stroke-linecap:round; stroke-linejoin:round }
  ul { list-style:none; margin:16px 0 0; padding:0 }
  li { padding:8px 0; border-top:1px solid var(--line) }
  .row { display:flex; gap:10px; align-items:baseline } .n { flex:1; min-width:0; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; color:var(--strong) }
  .s { color:var(--muted); font:12px ui-monospace,Menlo,Consolas,monospace; white-space:nowrap }
  .s.ok { color:var(--ok) } .s.err { color:var(--err) }
  .bar { height:3px; margin-top:6px; background:var(--line); border-radius:2px; overflow:hidden } .bar i { display:block; height:100%; width:0; background:var(--accent) }
  .done { margin-top:14px; color:var(--ok); font-weight:600 }
</style></head><body>
<main>
  <h1>{{.Title}}</h1>
  <p>Send files to the person who shared this link. They can't see anything else on your device.</p>
  {{if .Notes}}<p class="notes">{{.Notes}}</p>{{end}}
  <label class="zone" id="zone">
    <svg viewBox="0 0 24 24"><path d="M12 15.5v-12m0 0L7.5 8M12 3.5 16.5 8M4.5 20h15"/></svg>
    <b>Choose files</b><span>or drop them here</span>
    <input type="file" id="pick" multiple hidden>
  </label>
  <ul id="files"></ul>
  <div class="done" id="done" hidden></div>
</main>
<footer>Shared with {{.Brand}}</footer>
<script>
const id = {{.ID}}, max = {{.Max}};
let left = {{.Left}};
const $ = (s) => document.querySelector(s);
const UNITS = ["B", "KB", "MB", "GB", "TB"];
// Formats a byte count like the rest of the app.
const fmt = (n) => { let i = 0; while (n >= 1024 && i < 4) { n /= 1024; i++; } return i ? (n < 10 ? n.toFixed(2) : n < 100 ? n.toFixed(1) : n.toFixed(0)) + " " + UNITS[i] : n + " B"; };
const esc = (s) => String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);

// CRC-32 (IEEE) table for the duplicate fingerprint.
const CRC = new Uint32Array(256).map((_, n) => { let c = n; for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1; return c >>> 0; });
// CRC-32 of a byte array.
const crc32 = (b) => { let c = ~0; for (let i = 0; i < b.length; i++) c = CRC[(c ^ b[i]) & 255] ^ (c >>> 8); return (~c >>> 0).toString(16).padStart(8, "0"); };
// Fingerprint: CRC-32s of 64 KB at the start, middle and end of the file.
async function fingerprint(f) {
  const K = 65536, starts = [0, Math.max(0, Math.floor(f.size / 2) - K / 2), Math.max(0, f.size - K)];
  let out = "";
  for (const s of starts) out += crc32(new Uint8Array(await f.slice(s, Math.min(f.size, s + K)).arrayBuffer()));
  return out;
}

let queue = Promise.resolve(), sent = 0, skipped = 0;
// Adds files to the list and sends them one after another.
async function add(files) {
  if (!files.length) return;
  const rows = files.map((f) => {
    const li = document.createElement("li");
    li.innerHTML = '<div class="row"><span class="n">' + esc(f.name) + '</span><span class="s">' + fmt(f.size) + '</span></div><div class="bar"><i></i></div>';
    $("#files").append(li);
    return { f, li, st: li.querySelector(".s"), bar: li.querySelector(".bar i") };
  });
  // Ask which of these the host already has.
  let have = [];
  try {
    const fps = await Promise.all(files.map(fingerprint));
    const r = await fetch("/s/" + id + "/check", { method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ files: files.map((f, i) => ({ name: f.name, size: f.size, fp: fps[i] })) }) });
    if (r.ok) have = (await r.json()).have || [];
  } catch {}
  for (const [i, row] of rows.entries()) {
    if (have[i]) { row.st.textContent = "already there"; row.st.className = "s ok"; row.bar.style.width = "100%"; skipped++; continue; }
    queue = queue.then(() => send(row));
  }
  queue.then(summary);
}

// Sends one file with progress (XHR, since fetch can't report uploads).
function send(row) {
  return new Promise((resolve) => {
    const { f, st, bar } = row;
    const fail = (msg) => { st.textContent = msg; st.className = "s err"; resolve(); };
    if (left === 0) return fail("no more files accepted");
    if (max && f.size > max) return fail("too large");
    const xhr = new XMLHttpRequest();
    xhr.open("PUT", "/s/" + id + "/upload?name=" + encodeURIComponent(f.name) + "&mtime=" + (f.lastModified || 0));
    xhr.upload.onprogress = (ev) => { if (ev.lengthComputable) { bar.style.width = (ev.loaded / ev.total * 100) + "%"; st.textContent = Math.floor(ev.loaded / ev.total * 100) + "%"; } };
    xhr.onload = () => {
      if (xhr.status !== 200) return fail(xhr.responseText.trim() || "failed");
      let dup = false;
      try { dup = JSON.parse(xhr.responseText).duplicate; } catch {}
      bar.style.width = "100%";
      st.textContent = dup ? "already there" : "sent ✓";
      st.className = "s ok";
      if (dup) skipped++; else { sent++; if (left > 0) left--; }
      resolve();
    };
    xhr.onerror = () => fail("connection lost");
    xhr.send(f);
  });
}

// Says how it went once the queue is empty.
function summary() {
  const parts = [];
  if (sent) parts.push(sent + " file" + (sent > 1 ? "s" : "") + " sent");
  if (skipped) parts.push(skipped + " already there");
  $("#done").hidden = !parts.length;
  $("#done").textContent = parts.join(" · ");
}

$("#pick").addEventListener("change", (ev) => { add([...ev.target.files]); ev.target.value = ""; });
const zone = $("#zone");
addEventListener("dragover", (ev) => { ev.preventDefault(); zone.classList.add("over"); });
addEventListener("dragleave", () => zone.classList.remove("over"));
addEventListener("drop", (ev) => { ev.preventDefault(); zone.classList.remove("over"); add([...ev.dataTransfer.files]); });
</script>
</body></html>`))
