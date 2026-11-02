// seadex-scout dashboard: the Library and Queue tabs and the row actions that
// act on Sonarr and Radarr (search, fix indexer flags, import, remove). Uses
// the helpers app.js defines; loaded after it.
"use strict";

const lib = {
  scan: null,       // GET api/library
  queue: null,      // GET api/queue items
  queueError: "",
  busy: new Map(),  // action key -> label shown while it runs
  done: new Map(),  // action key -> result text
  bulk: null,       // { kind, total, done, failed, stop }
};

const POST_HEADERS = { "Content-Type": "application/json", "X-Seadex-Scout": "1" };

async function postJSON(url, body) {
  const res = await fetch(url, { method: "POST", headers: POST_HEADERS, body: body === undefined ? undefined : JSON.stringify(body) });
  if (!res.ok) throw new Error((await res.text()).trim() || `${res.status}`);
  return res.status === 204 ? null : res.json();
}

// ---------- keys and joins ----------

// targetOf is the arr target a finding acts on: its season for a season or
// episodes row, the whole series otherwise.
function targetOf(a) {
  const id = num(a.arr_id);
  if (!id || (a.arr !== "sonarr" && a.arr !== "radarr")) return null;
  const seasonRow = a.scope === "season" || a.scope === "episodes";
  return { arr: a.arr, id, season: a.arr === "sonarr" && seasonRow ? num(a.season) ?? -1 : -1 };
}

function targetKey(t) {
  return t.arr === "sonarr" && t.season >= 0 ? `sonarr:${t.id}:${t.season}` : `${t.arr}:${t.id}`;
}

// scoreFor sums the scan's low files for a target (a whole series adds its seasons).
function scoreFor(t) {
  if (!lib.scan || !t) return null;
  const rows = (lib.scan.seasons || []).filter((s) => s.arr === t.arr && s.id === t.id && (t.arr === "radarr" || t.season < 0 || s.season === t.season));
  if (!rows.length) return null;
  return rows.reduce((acc, s) => ({ low: acc.low + s.low, files: acc.files + s.files, min: Math.min(acc.min, s.min_score) }), { low: 0, files: 0, min: Infinity });
}

// findingIndex maps target keys to the newest finding, season rows first.
function findingIndex(snap) {
  const idx = new Map();
  for (const f of (snap && snap.findings) || []) {
    const t = targetOf(f.attrs);
    if (!t) continue;
    const k = targetKey(t);
    if (!idx.has(k)) idx.set(k, f);
  }
  return idx;
}

// actionFor says what a finding's row can do about it.
function actionFor(a) {
  if (a.status === "at_best" || a.status === "newer_revision") return a.status === "at_best" ? "fix" : "search";
  const kind = kindOf({ attrs: a });
  if (kind === "upgrade" || kind === "missing") return "search";
  return "";
}

// ---------- row actions ----------

function actionKey(kind, t) { return `${kind}:${targetKey(t)}`; }

function actionButton(kind, t, label, title) {
  const key = actionKey(kind, t);
  const btn = el("button", { type: "button", class: "act-btn " + kind, title });
  if (lib.busy.has(key)) { btn.textContent = lib.busy.get(key); btn.disabled = true; }
  else if (lib.done.has(key)) { btn.textContent = lib.done.get(key); btn.classList.add("done"); btn.disabled = true; }
  else btn.textContent = label;
  btn.addEventListener("click", () => runAction(kind, t, btn));
  return btn;
}

async function runAction(kind, t, btn) {
  const key = actionKey(kind, t);
  lib.busy.set(key, kind === "fix" ? "Fixing…" : "Searching…");
  if (btn) { btn.disabled = true; btn.textContent = lib.busy.get(key); }
  try {
    if (kind === "fix") {
      const r = await postJSON("api/arr/fixflags", t);
      const text = r.fixed ? `Fixed ${r.fixed} ✓` : r.skipped ? `0 fixed (${r.skipped} other group)` : "Already OK ✓";
      lib.done.set(key, text);
      if (btn) btn.title = [`${r.fixed} fixed, ${r.already_ok} already scored, ${r.skipped} skipped`,
        r.groups && r.groups.length ? `SeaDex best groups: ${r.groups.join(", ")}` : "",
        ...(r.skips || [])].filter(Boolean).join("\n");
      if (lib.scan && r.fixed) lib.scan.seasons = lib.scan.seasons.filter((s) => !(s.arr === t.arr && s.id === t.id && (t.arr === "radarr" || t.season < 0 || s.season === t.season)));
      return true;
    }
    await postJSON("api/arr/search", t);
    lib.done.set(key, "Searching ✓");
    return true;
  } catch (err) {
    lib.done.delete(key);
    if (btn) { btn.textContent = "Failed – retry"; btn.classList.add("failed"); btn.title = err.message; btn.disabled = false; }
    return false;
  } finally {
    lib.busy.delete(key);
    if (btn && lib.done.has(key)) { btn.textContent = lib.done.get(key); btn.classList.add("done"); }
    if (state.tab === "library") renderLibraryTab();
  }
}

// findingActions is the Actions cell of an Overview finding row.
function findingActions(f) {
  const a = f.attrs;
  const t = targetOf(a);
  const td = el("td", { class: "actions" });
  if (!t) return td;
  const score = scoreFor(t);
  const action = actionFor(a);
  if (score) td.append(el("span", { class: "score-badge", title: `Lowest custom format score ${score.min}`, text: `${score.low}/${score.files} below ${lib.scan.min_score}` }));
  if (action === "fix" && score) td.append(actionButton("fix", t, "Fix flags", "Give this season's files from the SeaDex best group the Freeleech25 + Scene flags, so the Seadex custom format scores them"));
  if (action === "search") td.append(actionButton("search", t, "Search", `Ask ${arrName(t.arr)} to search ${t.arr === "radarr" ? "this movie" : t.season >= 0 ? "this season" : "this series"} now`));
  return td;
}

// ---------- Library tab ----------

async function loadLibrary(force) {
  try {
    if (force) await postJSON("api/library/scan").catch((err) => { if (!/already running/.test(err.message)) throw err; });
    lib.scan = await getJSON("api/library");
  } catch (err) {
    $("lib-status").textContent = "Could not read the library scan: " + err.message;
    return;
  }
  if (lib.scan.running && !lib.scanPoll) lib.scanPoll = setInterval(() => loadLibrary(false), 5000);
  if (!lib.scan.running && lib.scanPoll) { clearInterval(lib.scanPoll); lib.scanPoll = null; }
  renderLibraryTab();
  if (state.snap) renderOverview(state.snap);
}

// libraryRows joins the scan's seasons with the findings and the action each allows.
function libraryRows() {
  const idx = findingIndex(state.snap);
  return ((lib.scan && lib.scan.seasons) || []).map((s) => {
    const t = { arr: s.arr, id: s.id, season: s.arr === "sonarr" ? s.season : -1 };
    const f = idx.get(targetKey(t)) || (s.arr === "sonarr" ? idx.get(`sonarr:${s.id}`) : null);
    const a = f ? f.attrs : null;
    let action = a ? actionFor(a) : "";
    // A whole-series finding acts on the season the scan found.
    return { s, t, a, action };
  });
}

function renderLibraryTab() {
  const scan = lib.scan;
  const status = $("lib-status");
  const now = state.snap ? new Date(state.snap.now).getTime() : Date.now();
  $("lib-scan").disabled = Boolean(scan && scan.running);
  if (!scan) { status.textContent = "Loading…"; return; }
  status.textContent = scan.running ? `Scanning Sonarr and Radarr… (started ${ago(scan.started_at, now)})`
    : scan.finished_at ? `Scanned ${ago(scan.finished_at, now)}: ${scan.scanned} titles, ${scan.files.toLocaleString()} files` + (scan.error ? ` · errors: ${scan.error}` : "")
    : "Not scanned yet. The first scan runs a minute after the start.";
  $("lib-min").textContent = scan.min_score;

  const all = libraryRows();
  const q = $("lib-search").value.trim().toLowerCase();
  const show = $("lib-filter").value;
  const rows = all.filter((r) => (!show || (show === "none" ? !r.action : r.action === show)) &&
    (!q || [r.s.title, ...(r.s.groups || []), r.a && r.a.recommended_group].some((v) => typeof v === "string" && v.toLowerCase().includes(q))));

  const count = (k) => all.filter((r) => r.action === k).reduce((n, r) => n + r.s.low, 0);
  $("lib-kpi-low").textContent = scan.finished_at ? (scan.low_files || 0).toLocaleString() : "–";
  $("lib-kpi-low-sub").textContent = `in ${all.length} seasons and movies`;
  $("lib-kpi-fix").textContent = scan.finished_at ? count("fix").toLocaleString() : "–";
  $("lib-kpi-search").textContent = scan.finished_at ? count("search").toLocaleString() : "–";
  $("lib-kpi-none").textContent = scan.finished_at ? count("").toLocaleString() : "–";
  for (const tile of document.querySelectorAll(".kpi[data-lib]")) tile.classList.toggle("selected", tile.dataset.lib === show);

  $("lib-count").textContent = rows.length;
  renderBulk(rows);
  setRows($("lib-rows").tBodies[0], rows.map((r) => {
    const why = r.a ? whyOf(r.a) : { text: "Not on SeaDex or not compared", cls: "" };
    const actions = el("td", { class: "actions" });
    if (r.action === "fix") actions.append(actionButton("fix", r.t, "Fix flags", "Flag this season's files from the SeaDex best group as a best grab would"));
    if (r.action === "search") actions.append(actionButton("search", r.t, "Search", `Ask ${arrName(r.t.arr)} to search now`));
    return el("tr", {},
      el("td", {}, r.a ? link(r.a.arr_url, r.s.title) : r.s.title, " ", seriesButton(r.s.arr, r.s.id, r.s.title), el("span", { class: "sub", text: `${arrName(r.s.arr)} · ${r.s.arr === "radarr" ? "Film" : r.s.season === 0 ? "Specials" : "Season " + r.s.season}` })),
      el("td", { class: "num", text: `${r.s.low} / ${r.s.files}` }),
      el("td", { class: "num", text: String(r.s.min_score) }),
      el("td", { class: "opt", text: (r.s.groups || []).join(", ") || "–" }),
      el("td", { class: "opt", text: r.a ? (r.a.recommended_group || "–") : "–" }),
      el("td", {}, el("span", { class: "tag " + why.cls, text: why.text })),
      actions);
  }), 7, scan.finished_at ? "Every monitored file reaches the minimum score." : "Waiting for the first scan.");
}

// ---------- bulk actions ----------

function renderBulk(rows) {
  const box = $("lib-bulk");
  const b = lib.bulk;
  if (b) {
    box.replaceChildren(
      el("span", { class: "muted small", text: `${b.kind === "fix" ? "Fixing flags" : "Searching"}: ${b.done} of ${b.total}${b.failed ? `, ${b.failed} failed` : ""}${b.wait ? ` · next in ${b.wait}s` : ""}` }),
      el("button", { type: "button", onclick: () => { b.stop = true; } }, "Stop"));
    return;
  }
  const fix = rows.filter((r) => r.action === "fix" && !lib.done.has(actionKey("fix", r.t)));
  const search = rows.filter((r) => r.action === "search" && !lib.done.has(actionKey("search", r.t)));
  box.replaceChildren(
    el("button", { type: "button", disabled: !fix.length, title: "Fix the flags of every season shown that holds the SeaDex best", onclick: () => runBulk("fix", fix) }, `Fix flags on ${fix.length} shown`),
    el("button", { type: "button", disabled: !search.length, title: "Search every season shown, one a minute so the trackers see a trickle", onclick: () => runBulk("search", search) }, `Search ${search.length} shown`));
}

async function runBulk(kind, rows) {
  if (!rows.length) return;
  if (kind === "search" && !confirm(`Search ${rows.length} seasons, one a minute (about ${rows.length} minutes)? Keep this page open.`)) return;
  const b = lib.bulk = { kind, total: rows.length, done: 0, failed: 0, stop: false, wait: 0 };
  for (const [i, r] of rows.entries()) {
    if (b.stop) break;
    if (!(await runAction(kind, r.t))) b.failed++;
    b.done++;
    renderLibraryTab();
    if (kind === "search" && i < rows.length - 1) {
      for (b.wait = 60; b.wait > 0 && !b.stop; b.wait--) { renderLibraryTab(); await new Promise((ok) => setTimeout(ok, 1000)); }
    }
  }
  lib.bulk = null;
  renderLibraryTab();
}

// ---------- Queue tab ----------

async function loadQueue() {
  $("queue-status").textContent = "Reading the queues…";
  try {
    lib.queue = (await getJSON("api/queue")).items || [];
    lib.queueError = "";
  } catch (err) {
    lib.queueError = err.message;
  }
  renderQueue();
}

function queueState(it) {
  if (it.state === "importPending" || it.state === "importBlocked") return { text: "Waiting to import", cls: "warn" };
  if (it.state === "failedPending" || it.status === "failed") return { text: "Failed", cls: "bad" };
  if (it.state === "importing") return { text: "Importing", cls: "info" };
  if (it.size_left > 0) return { text: it.status === "paused" ? "Paused" : "Downloading", cls: "" };
  return { text: it.state || it.status || "–", cls: "" };
}

function renderQueue() {
  const items = (lib.queue || []).filter((it) => !$("queue-stuck").checked || it.stuck);
  const stuck = (lib.queue || []).filter((it) => it.stuck).length;
  $("queue-status").textContent = lib.queueError ? "Could not read the queues: " + lib.queueError
    : lib.queue ? `${lib.queue.length} downloads, ${stuck} waiting on you` : "";
  $("queue-count").textContent = items.length;
  setRows($("queue-rows").tBodies[0], items.map((it) => {
    const st = queueState(it);
    const pct = it.size > 0 ? Math.round((100 * (it.size - it.size_left)) / it.size) : 0;
    const actions = el("td", { class: "actions" });
    if (it.can_import && it.stuck) {
      actions.append(el("button", { type: "button", class: "act-btn fix", title: "Import the files Sonarr matched to episodes that have no file or score below the minimum, as hardlinks, with the grab's indexer flags", onclick: (e) => importQueued(e.currentTarget, it) }, "Import"));
    }
    if (it.stuck || it.size_left === 0) {
      actions.append(el("button", { type: "button", class: "act-btn", title: "Remove from the queue only: the torrent keeps seeding and nothing is blocklisted", onclick: (e) => removeQueued(e.currentTarget, it) }, "Remove"));
    }
    return el("tr", { class: it.stuck ? "stuck" : null },
      el("td", {}, it.title, el("span", { class: "sub", text: `${arrName(it.arr)}${it.series ? " · " + it.series : ""}${it.indexer ? " · " + it.indexer : ""}` })),
      el("td", {}, el("span", { class: "tag " + st.cls, text: st.text })),
      el("td", { class: "num", text: it.size_left > 0 ? `${pct}% of ${fmtBytes(it.size)}` : fmtBytes(it.size) }),
      el("td", { class: "small opt", text: (it.messages || []).slice(0, 3).join(" · ") || "–" }),
      actions);
  }), 5, lib.queue ? "Nothing in the queues." : "Loading…");
}

async function importQueued(btn, it) {
  btn.disabled = true;
  btn.textContent = "Checking…";
  try {
    const p = await postJSON("api/queue/import", { id: it.id, dry: true });
    const skipped = (p.reasons || []).map((x) => "  skip " + x).join("\n");
    if (!p.files) {
      btn.textContent = "Nothing to import";
      btn.title = skipped || "Every file is matched and already scores high enough";
      btn.disabled = false;
      return;
    }
    if (!confirm(`Import ${p.files} files from "${it.title}" as hardlinks, with indexer flags ${p.indexer_flags}?` + (skipped ? `\n\n${skipped}` : ""))) {
      btn.textContent = "Import";
      btn.disabled = false;
      return;
    }
    btn.textContent = "Importing…";
    const r = await postJSON("api/queue/import", { id: it.id });
    btn.textContent = r.files ? `Importing ${r.files} ✓` : "Nothing to import";
    btn.classList.add("done");
    btn.title = [`${r.files} files sent to Sonarr with indexer flags ${r.indexer_flags}`, ...(r.reasons || []).map((x) => "skipped " + x)].join("\n");
    setTimeout(loadQueue, 15000);
  } catch (err) {
    btn.textContent = "Failed – retry";
    btn.classList.add("failed");
    btn.title = err.message;
    btn.disabled = false;
  }
}

async function removeQueued(btn, it) {
  if (!confirm(`Remove "${it.title}" from ${arrName(it.arr)}'s queue? The torrent stays in qBittorrent and keeps seeding.`)) return;
  btn.disabled = true;
  try {
    await postJSON("api/queue/remove", { arr: it.arr, id: it.id });
    lib.queue = lib.queue.filter((x) => !(x.arr === it.arr && x.id === it.id));
    renderQueue();
  } catch (err) {
    btn.textContent = "Failed";
    btn.title = err.message;
    btn.disabled = false;
  }
}

// ---------- per-series view ----------

// seriesButton opens the per-series view for a finding's or scan row's title.
function seriesButton(arr, id, title) {
  if (!id) return null;
  return el("button", { type: "button", class: "info-btn", title: `Everything about ${title}`, "aria-label": `Details for ${title}`, onclick: () => openSeries(arr, id, title) }, "ⓘ");
}

function openSeries(arr, id, title) {
  const findings = ((state.snap && state.snap.findings) || []).filter((f) => f.attrs.arr === arr && num(f.attrs.arr_id) === id);
  const scanRows = ((lib.scan && lib.scan.seasons) || []).filter((s) => s.arr === arr && s.id === id);
  const first = findings[0] && findings[0].attrs;
  $("series-title").replaceChildren(first ? link(first.arr_url, title) : document.createTextNode(title));
  const low = scanRows.reduce((n, s) => n + s.low, 0);
  $("series-sub").textContent = `${arrName(arr)} · ${findings.length} SeaDex rows · ${low} files below ${lib.scan ? lib.scan.min_score : "the minimum"}` +
    (first && first.al_id ? "" : "");
  const seasons = new Map();
  for (const f of findings) {
    const t = targetOf(f.attrs);
    seasons.set(t ? targetKey(t) : Math.random(), { f, t });
  }
  for (const s of scanRows) {
    const t = { arr: s.arr, id: s.id, season: s.arr === "sonarr" ? s.season : -1 };
    if (!seasons.has(targetKey(t))) seasons.set(targetKey(t), { f: null, t });
  }
  const rows = [...seasons.values()].sort((x, y) => (x.t ? x.t.season : 0) - (y.t ? y.t.season : 0));
  setRows($("series-rows").tBodies[0], rows.map(({ f, t }) => {
    const a = f ? f.attrs : null;
    const why = a ? whyOf(a) : { text: "Not compared", cls: "" };
    const score = scoreFor(t);
    const action = a ? actionFor(a) : "";
    const actions = el("td", { class: "actions" });
    if (t && action === "fix" && score) actions.append(actionButton("fix", t, "Fix flags", "Flag this season's files from the SeaDex best group"));
    if (t && action === "search") actions.append(actionButton("search", t, "Search", "Search this season now"));
    const entry = a && a.al_id ? SEADEX + a.al_id + "/" : "";
    return el("tr", {},
      el("td", { text: a ? seasonLabel(a) || "Whole series" : t.season === 0 ? "Specials" : t.season > 0 ? `Season ${t.season}` : "Film" }),
      el("td", {}, el("span", { class: "tag " + why.cls, text: why.text })),
      el("td", { text: a && a.status !== "not_downloaded" ? (a.current_group || "–") : "–" }),
      el("td", {}, a && a.recommended_group ? link(entry, a.recommended_group) : el("span", { class: "muted", text: a ? noBestText(a) : "–" })),
      el("td", { class: "num", text: score ? `${score.low} / ${score.files}` : "–" }),
      actions);
  }), 6, "Nothing known about this title.");
  const dlg = $("series-dialog");
  if (!dlg.open) dlg.showModal();
}

// ---------- wiring ----------

function onTabShown(name) {
  if (name === "library") (lib.scan ? renderLibraryTab() : loadLibrary(false));
  if (name === "queue") loadQueue();
}

(function initLibrary() {
  $("lib-scan").addEventListener("click", () => loadLibrary(true));
  for (const id of ["lib-search", "lib-filter"]) $(id).addEventListener("input", renderLibraryTab);
  for (const tile of document.querySelectorAll(".kpi[data-lib]")) {
    tile.addEventListener("click", () => {
      const sel = $("lib-filter");
      sel.value = sel.value === tile.dataset.lib ? "" : tile.dataset.lib;
      renderLibraryTab();
    });
  }
  $("queue-reload").addEventListener("click", loadQueue);
  $("series-close").addEventListener("click", () => $("series-dialog").close());
  $("queue-stuck").addEventListener("input", renderQueue);
  loadLibrary(false); // the Overview's score badges need it too
  onTabShown(state.tab);
  setInterval(() => { if (state.tab === "queue" && !document.hidden) loadQueue(); }, 60000);
})();
