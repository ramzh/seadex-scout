// seadex-scout built-in dashboard. Plain JavaScript, no dependencies: the page
// polls /api/state and renders it. Every value from the log is untrusted, so it
// only ever reaches the page through textContent and http(s)-only links.
"use strict";

const REFRESH_MS = 30000;
const SEADEX = "https://seadex.moe/";
const HOUR = 3600e3;
const DAY = 24 * HOUR;

const state = {
  snap: null,
  tab: "overview",
  sorts: { upgrades: null, atbest: null },
  qbitSent: new Map(), // Nyaa URL -> category, so a redraw keeps "Added"
  report: null,
  reportVerdict: "",
};

// ---------- helpers ----------

const $ = (id) => document.getElementById(id);

function el(tag, props, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k === "class") node.className = v;
    else if (k === "text") node.textContent = v;
    else if (k.startsWith("on")) node.addEventListener(k.slice(2), v);
    else node.setAttribute(k, v === true ? "" : String(v));
  }
  for (const c of children.flat()) {
    if (c === undefined || c === null || c === false) continue;
    node.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return node;
}

function safeURL(u) {
  if (typeof u !== "string" || u === "") return null;
  try {
    const parsed = new URL(u);
    return parsed.protocol === "https:" || parsed.protocol === "http:" ? parsed.href : null;
  } catch {
    return null;
  }
}

function link(href, text, cls) {
  const url = safeURL(href);
  if (!url) return document.createTextNode(text);
  return el("a", { href: url, target: "_blank", rel: "noopener noreferrer", class: cls }, text);
}

function num(v) {
  if (v === null || v === undefined || v === "") return null;
  const n = typeof v === "number" ? v : Number(v);
  return Number.isFinite(n) ? n : null;
}

function fmtBytes(b, signed) {
  const n = num(b);
  if (n === null) return "–";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let v = Math.abs(n);
  let i = 0;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  const s = (v >= 100 || i === 0 ? v.toFixed(0) : v.toFixed(1)) + " " + units[i];
  if (!signed) return s;
  return (n > 0 ? "+" : n < 0 ? "−" : "") + s;
}

function ago(t, now) {
  if (!t) return "never";
  const ms = (now || Date.now()) - new Date(t).getTime();
  if (!Number.isFinite(ms)) return "–";
  const abs = Math.abs(ms);
  const units = [[DAY, "d"], [HOUR, "h"], [60e3, "m"], [1e3, "s"]];
  for (const [size, u] of units) {
    if (abs >= size) {
      const n = Math.floor(abs / size);
      return ms >= 0 ? `${n}${u} ago` : `in ${n}${u}`;
    }
  }
  return "just now";
}

function fmtTime(t) {
  if (!t) return "–";
  const d = new Date(t);
  if (Number.isNaN(d.getTime())) return "–";
  return d.toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" });
}

function fmtDuration(s) {
  // Go duration strings, such as "24m13.2s" or "1h2m0s".
  if (typeof s !== "string") return "–";
  const m = s.match(/^(?:(\d+)h)?(?:(\d+)m)?(?:([\d.]+)s)?$/);
  if (!m) return s;
  const h = Number(m[1] || 0), mi = Number(m[2] || 0), se = Number(m[3] || 0);
  if (h) return `${h}h ${mi}m`;
  if (mi) return `${mi}m ${Math.round(se)}s`;
  return `${se < 10 ? se.toFixed(1) : Math.round(se)}s`;
}

function durationSeconds(s) {
  if (typeof s !== "string") return null;
  let total = 0, matched = false;
  for (const [, v, u] of s.matchAll(/([\d.]+)(h|ms|m|s|µs|us|ns)/g)) {
    matched = true;
    const n = Number(v);
    total += u === "h" ? n * 3600 : u === "m" ? n * 60 : u === "s" ? n : u === "ms" ? n / 1e3 : 0;
  }
  return matched ? total : null;
}

function arrName(arr) {
  return arr === "radarr" ? "Radarr" : arr === "sonarr" ? "Sonarr" : arr || "";
}

function seasonLabel(a) {
  if (a.episodes) return a.episodes;
  const s = num(a.season);
  if (a.arr === "radarr") return "Film";
  if (s === null) return "";
  return s === 0 ? "Specials" : `Season ${s}`;
}

function setRows(tbody, rows, cols, emptyText) {
  tbody.replaceChildren(...(rows.length ? rows : [el("tr", {}, el("td", { colspan: cols, class: "empty", text: emptyText }))]));
}

function groupWithRevision(group, rev) {
  if (!group) return "–";
  return rev ? `${group} ${rev}` : group;
}

// groupList shows at most three groups, the rest as "+N more" with the full
// list on hover. Files with no group make Sonarr read words of the episode
// titles as groups, which would otherwise stretch the column.
function groupList(groups, rev) {
  const list = (Array.isArray(groups) ? groups : String(groups || "").split(",")).map((g) => g.trim()).filter(Boolean);
  if (!list.length) return el("td", { class: "groups", text: "–" });
  const shown = list.slice(0, 3).join(", ") + (rev ? ` ${rev}` : "");
  return el("td", { class: "groups", title: list.length > 3 ? list.join(", ") : null },
    shown, list.length > 3 ? el("span", { class: "sub", text: `+${list.length - 3} more` }) : null);
}

// ---------- theme and tabs ----------

function initTheme() {
  let saved = null;
  try { saved = localStorage.getItem("theme"); } catch { /* storage blocked */ }
  if (saved === "light" || saved === "dark") document.documentElement.dataset.theme = saved;
  $("theme-toggle").addEventListener("click", () => {
    const current = document.documentElement.dataset.theme ||
      (matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light");
    const next = current === "dark" ? "light" : "dark";
    document.documentElement.dataset.theme = next;
    try { localStorage.setItem("theme", next); } catch { /* storage blocked */ }
  });
}

function showTab(name) {
  if (!document.getElementById("tab-" + name)) name = "overview";
  state.tab = name;
  for (const b of document.querySelectorAll(".tabs button")) {
    b.setAttribute("aria-selected", String(b.dataset.tab === name));
  }
  for (const p of document.querySelectorAll(".tab-panel")) p.hidden = p.id !== "tab-" + name;
  if (location.hash !== "#" + name) history.replaceState(null, "", "#" + name);
  if (name === "log") loadLogs();
  if (name === "reports") loadReports();
  if (typeof onTabShown === "function") onTabShown(name); // library.js
}

function initTabs() {
  for (const b of document.querySelectorAll(".tabs button")) {
    b.addEventListener("click", () => showTab(b.dataset.tab));
  }
  showTab(location.hash.slice(1) || "overview");
  window.addEventListener("hashchange", () => showTab(location.hash.slice(1)));
}

// ---------- data ----------

async function getJSON(url) {
  const res = await fetch(url, { headers: { Accept: "application/json" }, cache: "no-store" });
  if (!res.ok) throw new Error(`${url}: ${res.status}`);
  return res.json();
}

async function refresh() {
  try {
    state.snap = await getJSON("api/state");
    render();
  } catch (err) {
    const pill = $("status-pill");
    pill.className = "pill bad";
    pill.textContent = "Dashboard offline";
    $("subtitle").textContent = "Could not reach seadex-scout. Retrying…";
    console.error(err);
  }
}

// ---------- attention ----------

function attentionItems(snap) {
  const now = new Date(snap.now).getTime();
  const started = new Date(snap.started_at).getTime();
  const items = [];

  const lastCheck = Math.max(
    snap.last_check ? new Date(snap.last_check.time).getTime() : 0,
    snap.reconcile_started ? new Date(snap.reconcile_started).getTime() : 0,
  );
  // The same allowance as the container's own health check: three check
  // intervals, and never less than 3 hours.
  const interval = pollIntervalMs(snap);
  const allowance = Math.max(3 * HOUR, 3 * (interval || 0));
  if (interval !== -1 && now - Math.max(lastCheck, started) > allowance) {
    items.push({ cls: "bad", title: "Last check", text: `No check has finished or started in ${fmtSpan(allowance)} (last ${ago(lastCheck || null, now)}).` });
  }
  const lastFull = snap.reconcile_complete ? new Date(snap.reconcile_complete).getTime() : 0;
  if (now - Math.max(lastFull, started) > 72 * HOUR) {
    items.push({ cls: "bad", title: "Last full check", text: `No daily full check has finished in 72 hours (last ${ago(lastFull || null, now)}).` });
  }
  const errors = recentErrors(snap);
  if (errors.length) {
    items.push({ cls: "warn", title: `Errors in 7 days: ${errors.length}`, text: errors[errors.length - 1].msg, tab: "health" });
  }
  for (const [cond, line] of Object.entries(snap.conditions || {})) {
    if (!conditionActive(snap, line, now)) continue;
    const lib = cond.startsWith("library-");
    const arr = line.attrs && line.attrs.arr ? ` (${arrName(line.attrs.arr)})` : "";
    items.push({
      cls: lib ? "bad" : "info",
      title: lib ? `Library problem${arr}` : "Upstream outage",
      text: `${cond}: ${line.msg}${lib ? "" : ". Nothing to do; existing findings are kept."}`,
      tab: "health",
    });
  }
  const feed = snap.indexer && snap.indexer.last_snapshot;
  if (feed && num(feed.attrs && feed.attrs.ab_releases_skipped) > 0) {
    items.push({ cls: "warn", title: "AnimeBytes feed", text: `${feed.attrs.ab_releases_skipped} AnimeBytes releases held back: indexer.ab_passkey is not set.` });
  }
  return items;
}

// pollIntervalMs returns the configured check interval in milliseconds, -1
// when checks are triggered from outside (poll_interval: off), or null when
// the configuration has not been logged since this start.
function pollIntervalMs(snap) {
  const raw = snap.config && snap.config.attrs && snap.config.attrs.poll_interval;
  if (raw === "external") return -1;
  const secs = durationSeconds(raw);
  return secs ? secs * 1000 : null;
}

// conditionActive reports whether a lasting problem still holds. The scout logs
// it again on every check while it lasts, so it has cleared once a check has
// gone by without it: an hour, or one and a half check intervals when longer.
function conditionActive(snap, line, now) {
  const interval = pollIntervalMs(snap);
  const window = Math.max(HOUR, 1.5 * (interval > 0 ? interval : 0));
  return now - new Date(line.time).getTime() <= window;
}

function fmtSpan(ms) {
  if (ms >= DAY && ms % DAY === 0) return `${ms / DAY} day${ms === DAY ? "" : "s"}`;
  if (ms >= HOUR) return `${+(ms / HOUR).toFixed(1)} hour${ms === HOUR ? "" : "s"}`;
  return `${Math.round(ms / 60e3)} minutes`;
}

function recentErrors(snap) {
  const now = new Date(snap.now).getTime();
  return (snap.errors || []).filter((e) => now - new Date(e.time).getTime() <= 7 * DAY);
}

// ---------- render: header ----------

function render() {
  const snap = state.snap;
  if (!snap) return;
  const items = attentionItems(snap);

  const pill = $("status-pill");
  if (items.some((i) => i.cls === "bad")) { pill.className = "pill bad"; pill.textContent = "Needs attention"; }
  else if (items.length) { pill.className = "pill warn"; pill.textContent = "Check health"; }
  else if (!snap.pass || !snap.pass.id) { pill.className = "pill info"; pill.textContent = "Waiting for first check"; }
  else { pill.className = "pill good"; pill.textContent = "Healthy"; }

  const sub = [];
  if (snap.pass && snap.pass.id) sub.push(`Findings from ${ago(snap.pass.at, new Date(snap.now).getTime())}`);
  else sub.push("No findings yet. The first full check can take 25 minutes or more");
  $("subtitle").textContent = sub.join(" · ");
  // The page's refresh reads this container only; SeaDex is contacted solely by
  // the scout's own check schedule, so say which is which.
  const interval = pollIntervalMs(snap);
  const lastSeaDex = snap.last_check ? `, last ${ago(snap.last_check.time, new Date(snap.now).getTime())}` : "";
  const schedule = interval === -1 ? "SeaDex checks are started from outside the container"
    : interval ? `SeaDex checked every ${fmtSpan(interval)}${lastSeaDex}, full check daily`
    : "SeaDex checked on the scout's schedule";
  $("footer-text").textContent = `Page refreshed ${fmtTime(snap.now)} from this container every ${REFRESH_MS / 1000}s (never contacts SeaDex) · ${schedule}` +
    (snap.version ? ` · build ${snap.version}` : "");

  const box = $("attention");
  box.hidden = items.length === 0;
  box.replaceChildren(...items.map((i) => {
    const node = el("div", { class: "alert " + i.cls }, el("strong", { text: i.title }), el("span", { text: i.text }));
    if (i.tab) { node.dataset.tab = i.tab; node.addEventListener("click", () => showTab(i.tab)); }
    return node;
  }));

  renderOverview(snap);
  renderActivity(snap);
  renderHealth(snap);
  if (state.tab === "reports") renderReportJob(snap.report_job);
}

// ---------- render: overview ----------

// unmonitoredTest returns whether a finding's series, season or movie is
// unmonitored in Sonarr or Radarr. Rows read before the scout logged arr ids,
// or before the flags were read, count as monitored.
function unmonitoredTest(snap) {
  const reason = unmonitoredReason(snap);
  return (f) => reason(f) !== "";
}

// unmonitoredReason says which flag makes a row unmonitored, or "" when none
// does: the Scout health tab lists them, so a wrong one can be seen.
function unmonitoredReason(snap) {
  const keys = new Set((snap.monitored && snap.monitored.unmonitored) || []);
  return (f) => {
    const a = f.attrs;
    if (!a.arr_id || !keys.size) return "";
    const base = `${a.arr}:${a.arr_id}`;
    if (keys.has(base)) return a.arr === "radarr" ? "movie unmonitored in Radarr" : "series unmonitored in Sonarr";
    // A season's own flag counts only for a row that is that season, or
    // episodes of it. A whole-series row carries season 0, which would
    // otherwise read the Specials flag, unmonitored on most shows.
    const seasonRow = a.scope === "season" || a.scope === "episodes";
    if (a.arr === "sonarr" && seasonRow && a.season !== undefined && keys.has(`${base}:${a.season}`)) {
      return Number(a.season) === 0 ? "Specials unmonitored in Sonarr" : `season ${a.season} unmonitored in Sonarr`;
    }
    return "";
  };
}

// setTone colours a stat tile by the value inside it, keeping its other classes.
function setTone(valueNode, tone) {
  const tile = valueNode.parentElement;
  for (const t of ["good", "warn", "bad"]) tile.classList.toggle(t, t === tone);
}

// The row kinds of the two finding tables, by status.
const UPGRADE = new Set(["better_release", "newer_revision"]);
const GROUPS = new Set(["mixed_group_manual", "unverifiable"]);
const NO_BEST = new Set(["theoretical_best", "incomplete", "no_best"]);

// kindOf sorts a finding into the Why filter's kinds; "atbest" rows go to the
// Already on a SeaDex pick table instead of Upgrades available.
function kindOf(f) {
  const a = f.attrs;
  if (a.status === "at_best") return "atbest";
  if (NO_BEST.has(a.status)) return a.current_tier === "alt" ? "atbest" : "nobest";
  if (UPGRADE.has(a.status)) return "upgrade";
  // An undecided row whose group is a named SeaDex alt is held at that alt:
  // an optional upgrade, like a better_release held at an alt.
  if (a.status === "unverifiable" && a.current_tier === "alt") return "upgrade";
  if (GROUPS.has(a.status)) return "groups";
  if (a.status === "not_downloaded") return "missing";
  return "other";
}

const KIND_ORDER = { upgrade: 0, missing: 1, groups: 2, nobest: 3, atbest: 4, other: 5 };

function whyOf(a) {
  switch (a.status) {
    case "newer_revision": return { text: "Newer version", cls: "info" };
    case "better_release": return a.current_tier === "alt"
      ? { text: "SeaDex alt", cls: "good" } : { text: "Neither best nor alt", cls: "warn" };
    case "mixed_group_manual": return { text: "Several release groups", cls: "" };
    case "unverifiable": {
      if (a.current_tier === "alt") return { text: "SeaDex alt", cls: "good" };
      const yours = (a.current_group || "").trim().toLowerCase();
      if (!yours || yours === "nogrp") return { text: "Your group can't be read", cls: "" };
      if ((a.recommended_group || "").trim().toLowerCase() === "nogrp") return { text: "SeaDex best has no group", cls: "" };
      return { text: "Group could not be read", cls: "" };
    }
    case "not_downloaded": return { text: "Not downloaded", cls: "bad" };
    case "at_best": return { text: "At SeaDex best", cls: "good" };
    default: return a.current_tier === "alt"
      ? { text: "At a SeaDex alt", cls: "good" } : { text: "No SeaDex best", cls: "" };
  }
}

function renderOverview(snap) {
  const now = new Date(snap.now).getTime();
  const ready = Boolean(snap.pass && snap.pass.id);
  const showOptional = $("upgrades-optional").checked;
  const unmonitored = unmonitoredTest(snap);
  const showUnmonitored = $("upgrades-unmonitored").checked;
  const all = (snap.findings || []).filter((f) => kindOf(f) !== "other");
  const rows = showUnmonitored ? all : all.filter((f) => !unmonitored(f));
  const hidden = all.length - rows.length;
  const m = snap.monitored || {};
  $("unmonitored-note").textContent = m.error && !m.at ? "Could not read monitored flags from Sonarr or Radarr"
    : !showUnmonitored && hidden ? `${hidden} unmonitored hidden` : "";
  const isOptional = (f) => (f.attrs.status === "better_release" || f.attrs.status === "unverifiable") && f.attrs.current_tier === "alt";
  const todo = rows.filter((f) => kindOf(f) !== "atbest" && (showOptional || !isOptional(f)));
  const atBest = rows.filter((f) => kindOf(f) === "atbest");

  const upgradesShown = todo.filter((f) => kindOf(f) === "upgrade").length;
  const optional = rows.filter(isOptional).length;
  $("kpi-upgrades").textContent = ready ? upgradesShown : "–";
  $("kpi-upgrades-sub").textContent = optional && !showOptional ? `${optional} more optional (you have an alt)` : "better releases and newer versions";
  for (const [id, kind] of [["kpi-missing", "missing"], ["kpi-groups", "groups"], ["kpi-nobest", "nobest"]]) {
    $(id).textContent = ready ? rows.filter((f) => kindOf(f) === kind).length : "–";
  }
  for (const tile of document.querySelectorAll(".kpi[data-why]")) {
    tile.classList.toggle("selected", tile.dataset.why === $("upgrades-why").value);
  }

  const lib = snap.library && snap.library.attrs;
  if (lib && num(lib.anime_items)) {
    const pct = Math.round((100 * num(lib.items_all_best_or_alt)) / num(lib.anime_items));
    $("kpi-bestalt").textContent = pct + "%";
    $("kpi-bestalt-sub").textContent = `${lib.items_all_best_or_alt} of ${lib.anime_items} anime · ${lib.items_all_best} at best`;
  } else {
    $("kpi-bestalt").textContent = "–";
    $("kpi-bestalt-sub").textContent = "after the first full check";
  }

  const lc = snap.last_check;
  $("kpi-lastcheck").textContent = lc ? ago(lc.time, now) : "–";
  $("kpi-lastcheck-sub").textContent = lc
    ? (lc.msg.includes("degraded") ? `${lc.msg} (${lc.attrs.reason || "?"})` : lc.msg.startsWith("cycle") ? "full check" : "quick check")
    : "running…";
  setTone($("kpi-lastcheck"), lc && lc.msg.includes("degraded") ? "warn" : "");

  renderLibrary(snap, now);
  renderRefresh(snap.refresh || {}, now);
  const why = $("upgrades-why").value;
  renderFindings("upgrades", todo.filter((f) => !why || kindOf(f) === why), now,
    ready ? "Nothing to do. Your library is on SeaDex's picks." : "Waiting for the first check.");
  renderFindings("atbest", atBest, now, ready ? "None yet." : "Waiting for the first check.");
}

const LIBRARY_BARS = [
  ["have_best", "At SeaDex best", "--bar-best"],
  ["have_alt", "At a SeaDex alt", "--bar-alt"],
  ["have_older_revision", "Older version of best", "--bar-older"],
  ["have_unlisted", "Neither best nor alt", "--bar-unlisted"],
  ["no_file", "Season not found", "--bar-other"],
  ["unverified", "Untagged release", "--bar-other"],
  ["unattributed", "Episode not known", "--bar-other"],
  ["withheld", "Withheld", "--bar-other"],
  ["not_on_seadex", "Not on SeaDex", "--bar-other"],
];

function renderLibrary(snap, now) {
  const lib = snap.library;
  const box = $("library");
  if (!lib) {
    $("library-asof").textContent = "";
    box.replaceChildren(el("p", { class: "empty", text: "Shown after the first daily full check." }));
    return;
  }
  $("library-asof").textContent = `From the full check ${ago(lib.time, now)} · ${lib.attrs.rows} SeaDex entries`;
  const max = Math.max(1, ...LIBRARY_BARS.map(([k]) => num(lib.attrs[k]) || 0));
  box.replaceChildren(...LIBRARY_BARS
    .filter(([k]) => lib.attrs[k] !== undefined)
    .map(([k, label, color]) => {
      const v = num(lib.attrs[k]) || 0;
      const fill = el("div", { class: "bar-fill" });
      fill.style.width = `${(100 * v) / max}%`;
      fill.style.background = `var(${color})`;
      return el("div", { class: "bar-row" },
        el("span", { class: "label", text: label, title: label }),
        el("div", { class: "bar-track" }, fill),
        el("span", { class: "num", text: v.toLocaleString() }));
    }));
}

// FINDING_COLUMNS is the column layout both finding tables share. key names
// the sort, when the column sorts.
const FINDING_COLUMNS = [
  { label: "Title", key: "title" },
  { label: "On disk", key: "disk", num: true, title: "The size of the files Sonarr or Radarr holds for this season or film" },
  { label: "Why", key: "why" },
  { label: "You have", key: "current" },
  { label: "SeaDex best", key: "best" },
  { label: "Size / change", key: "change", num: true, title: "Download size, and below it how much your library grows (+) or shrinks (−)" },
  { label: "Link" },
  { label: "SeaDex alt", key: "alt" },
  { label: "Size / change", key: "altchange", num: true, title: "Download size, and below it how much your library grows (+) or shrinks (−)" },
  { label: "Link" },
  { label: "First seen", key: "first", num: true },
  { label: "Actions" },
];

const TEXT_SORTS = new Set(["title", "why", "current", "best", "alt"]);

function sortValue(f, key, showOptional) {
  const a = f.attrs;
  switch (key) {
    case "title": return (a.title || "").toLowerCase();
    case "why": return whyOf(a).text;
    case "current": return (a.current_group || "~").toLowerCase();
    case "best": return (a.recommended_group || "~").toLowerCase();
    case "disk": return num(a.library_bytes) ?? -Infinity;
    case "change": return changeOf(a, "") ?? -Infinity;
    case "alt": return (a.alt_group || a.alt_groups || "~").toLowerCase();
    case "altchange": return changeOf(a, "alt_") ?? -Infinity;
    case "first": return num(a.first_seen) ?? 0;
    default: return null;
  }
}

// defaultOrder is the tables' own order: by kind, upgrades by their rank in
// the view shown, then by title.
function defaultOrder(x, y, showOptional) {
  const kx = KIND_ORDER[kindOf(x)], ky = KIND_ORDER[kindOf(y)];
  if (kx !== ky) return kx - ky;
  const rank = (f) => {
    const r = num(showOptional ? f.attrs.rank_none : f.attrs.rank_alt);
    return r && r > 0 ? r : Infinity;
  };
  const rx = rank(x), ry = rank(y);
  if (rx !== ry) return rx < ry ? -1 : 1;
  return (x.attrs.title || "").localeCompare(y.attrs.title || "");
}

// changeOf is a release's size change. A season with nothing on disk grows
// by the whole download.
function changeOf(a, prefix) {
  const change = num(a[prefix + "size_change_bytes"]);
  if (change !== null) return change;
  if (a.status === "not_downloaded") return num(a[prefix + "recommended_bytes"]);
  return approxChange(a, prefix);
}

// approxChange is the fallback when the scout could not tie a release's files
// to the files on disk one by one: the download against everything the
// season or film holds (the On disk column). It is right when the release
// covers what you have, so it is shown marked as approximate.
function approxChange(a, prefix) {
  const size = num(a[prefix + "recommended_bytes"]), disk = num(a.library_bytes);
  return size !== null && disk !== null && disk > 0 ? size - disk : null;
}

function isApprox(a, prefix) {
  return num(a[prefix + "size_change_bytes"]) === null && a.status !== "not_downloaded" && approxChange(a, prefix) !== null;
}

function renderFindings(table, list, now, emptyText) {
  const q = $("upgrades-search").value.trim().toLowerCase();
  const arr = $("upgrades-arr").value;
  const showOptional = $("upgrades-optional").checked;
  const sort = state.sorts[table];
  const rows = list
    .filter((f) => !arr || f.attrs.arr === arr)
    .filter((f) => !q || [f.attrs.title, f.attrs.current_group, f.attrs.recommended_group, f.attrs.alt_group, f.attrs.alt_groups]
      .some((s) => typeof s === "string" && s.toLowerCase().includes(q)))
    .sort((x, y) => {
      if (!sort) return defaultOrder(x, y, showOptional);
      const a = sortValue(x, sort.key, showOptional), b = sortValue(y, sort.key, showOptional);
      const c = a < b ? -1 : a > b ? 1 : 0;
      return (sort.asc ? c : -c) || defaultOrder(x, y, showOptional);
    });

  $(table + "-count").textContent = rows.length;
  const tbl = $(table);
  tbl.tHead.replaceChildren(el("tr", {}, FINDING_COLUMNS.map((col) => {
    const sorted = sort && col.key && sort.key === col.key;
    const th = el("th", { class: [col.num ? "num" : "", sorted ? "sorted" : "", sorted && sort.asc ? "asc" : ""].join(" ").trim() || null, title: col.title, "data-sort": col.key }, col.label);
    if (col.key) th.addEventListener("click", () => {
      const s = state.sorts[table];
      state.sorts[table] = s && s.key === col.key ? { key: col.key, asc: !s.asc } : { key: col.key, asc: TEXT_SORTS.has(col.key) };
      renderOverview(state.snap);
    });
    return th;
  })));
  setRows(tbl.tBodies[0], rows.flatMap((f) => findingRows(f, now)), FINDING_COLUMNS.length, emptyText);
}

// findingRows is a finding's row, then one sub-row per further SeaDex best
// or alt release, filling only the best and alt columns.
function findingRows(f, now) {
  const a = f.attrs;
  const bests = parseOptions(a.best_options);
  const alts = parseOptions(a.alt_options);
  const row = findingRow(f, now);
  const n = Math.max(bests.length, alts.length);
  if (!n) return [row];
  row.classList.add("has-subs");
  const entry = a.al_id ? SEADEX + a.al_id + "/" : "";
  const subs = [];
  for (let i = 0; i < n; i++) {
    const tr = el("tr", { class: "sub-row" + (i === n - 1 ? " last-sub" : "") },
      el("td", {}), el("td", {}), el("td", {}), el("td", {}),
      ...optionCells(a, bests[i], entry, "best"),
      ...optionCells(a, alts[i], entry, "alt"),
      el("td", {}), el("td", {}));
    subs.push(tr);
  }
  return [row, ...subs];
}

// parseOptions reads a best_options or alt_options list, leaving out the
// headline release the row itself already shows.
function parseOptions(raw) {
  if (typeof raw !== "string" || !raw) return [];
  try {
    const list = JSON.parse(raw);
    return Array.isArray(list) ? list.filter((o) => o && !o.headline) : [];
  } catch {
    return [];
  }
}

// optionCells is a release's three cells: name and info line, size and
// change, and its links with the → qBit and → Prowlarr buttons.
function optionCells(a, o, entry, rung) {
  if (!o) return [el("td", {}), el("td", {}), el("td", {})];
  const links = [];
  if (o.nyaa_url) links.push(link(o.nyaa_url, "Nyaa"));
  if (o.public_url) links.push(link(o.public_url, o.public_tracker || "Tracker"));
  if (o.ab_url) links.push(link(o.ab_url, o.ab_tracker || "AnimeBytes"));
  const bytes = num(o.bytes);
  let change = num(o.change);
  let approx = false;
  if (change === null && bytes !== null) {
    if (a.status === "not_downloaded") change = bytes;
    else if (num(a.library_bytes) > 0) { change = bytes - num(a.library_bytes); approx = true; }
  }
  return [
    el("td", {}, withNotes(link(entry, o.group || "?"), a), o.tags ? el("span", { class: "sub", text: o.tags }) : null),
    bytes !== null ? sizeCell(bytes, change, approx) : el("td", { class: "num", text: "–" }),
    linkCell(a, links, o.nyaa_url),
  ];
}

function findingRow(f, now) {
  const a = f.attrs;
  const why = whyOf(a);
  const newer = a.status === "newer_revision";
  const entry = a.al_id ? SEADEX + a.al_id + "/" : "";
  const bestLinks = releaseLinks(a, "");
  if (!bestLinks.length && a.release_url) bestLinks.push(link(a.release_url, a.tracker || "Release"));
  const notes = [a.seadex_tags, a.entry_incomplete ? "SeaDex entry incomplete" : ""].filter(Boolean).join(" · ");
  const best = a.recommended_group
    ? el("td", {}, withNotes(link(entry, groupWithRevision(a.recommended_group, newer ? a.recommended_revision : "")), a), notes ? el("span", { class: "sub", text: notes }) : null)
    : el("td", {}, withNotes(link(entry, noBestText(a)), a), el("span", { class: "sub", text: noBestDetail(a) }));
  return el("tr", {},
    el("td", {}, link(a.arr_url, a.title || "?"), " ", typeof seriesButton === "function" ? seriesButton(a.arr, num(a.arr_id), a.title || "?") : null,
      el("span", { class: "sub", text: `${arrName(a.arr)} · ${seasonLabel(a)}` })),
    el("td", { class: "num", text: a.status === "not_downloaded" ? "–" : fmtBytes(a.library_bytes) }),
    el("td", {}, el("span", { class: "tag " + why.cls, text: why.text })),
    a.status === "not_downloaded" ? el("td", { text: "–" }) : groupList(a.current_group, newer ? a.current_revision : ""),
    best,
    a.recommended_group ? sizeCell(a.recommended_bytes, changeOf(a, ""), isApprox(a, "")) : el("td", { class: "num", text: "–" }),
    linkCell(a, bestLinks, a.nyaa_url),
    altCell(a, entry),
    a.alt_group ? sizeCell(a.alt_recommended_bytes, changeOf(a, "alt_"), isApprox(a, "alt_")) : el("td", { class: "num", text: "–" }),
    linkCell(a, releaseLinks(a, "alt_"), a.alt_nyaa_url),
    el("td", { class: "num", text: a.first_seen ? ago(num(a.first_seen), now) : "–" }),
    typeof findingActions === "function" ? findingActions(f) : el("td", {}));
}

function noBestText(a) {
  switch (a.no_best_reason || a.status) {
    case "theoretical": case "theoretical_best": return "No best yet";
    case "incomplete": return "No best yet";
    default: return "No best you can get";
  }
}

function noBestDetail(a) {
  switch (a.no_best_reason || a.status) {
    case "theoretical": case "theoretical_best": return "SeaDex names a theoretical best only";
    case "incomplete": return "SeaDex entry incomplete";
    default: return "its best is filtered out or on a tracker you have off";
  }
}

// linkCell is a release's links with, under a Nyaa link, the → qBit button
// and then the → Prowlarr button, each when it is set up.
function linkCell(a, links, nyaaURL) {
  const qbit = state.snap && state.snap.qbit;
  const td = el("td", {}, el("div", { class: "links" }, links));
  if (qbit && qbit.enabled && nyaaURL && (a.arr === "sonarr" || a.arr === "radarr")) {
    const category = (qbit.categories || {})[a.arr];
    const btn = el("button", { type: "button", class: "qbit-btn", title: `Add this Nyaa release to qBittorrent${category ? ` under the category ${category}` : ""}` }, "→ qBit");
    if (state.qbitSent.has(nyaaURL)) {
      btn.textContent = "Added ✓";
      btn.classList.add("done");
      btn.disabled = true;
    }
    btn.addEventListener("click", () => sendToQbit(btn, nyaaURL, a.arr));
    td.append(btn);
  }
  const usenet = state.snap && state.snap.usenet;
  if (usenet && usenet.enabled && nyaaURL && (a.arr === "sonarr" || a.arr === "radarr")) {
    const btn = el("button", { type: "button", class: "qbit-btn", title: "Search Prowlarr for this Nyaa release's title and send a usenet copy to SABnzbd" }, "→ Prowlarr");
    btn.addEventListener("click", () => openUsenet(nyaaURL, a));
    td.append(btn);
  }
  return td;
}

// ---------- hidden as unmonitored ----------

function renderHidden(snap, now) {
  const m = snap.monitored || {};
  const reason = unmonitoredReason(snap);
  const hidden = (snap.findings || []).map((f) => [f, reason(f)]).filter(([, r]) => r);
  const counts = { series: 0, seasons: 0, movies: 0 };
  for (const k of m.unmonitored || []) {
    if (k.startsWith("radarr:")) counts.movies++;
    else if (k.split(":").length === 3) counts.seasons++;
    else counts.series++;
  }
  $("hidden-status").textContent = !m.at
    ? (m.error ? `Not read yet: ${m.error}` : "Not read yet.")
    : `Read from Sonarr and Radarr ${ago(m.at, now)}: ${counts.series} series, ${counts.seasons} seasons and ${counts.movies} movies are unmonitored.` +
      (m.error ? ` The last read failed, so these are from before: ${m.error}` : "");
  $("hidden-count").textContent = hidden.length;
  setRows($("hidden-rows").tBodies[0], hidden.map(([f, r]) => el("tr", {},
    el("td", {}, link(f.attrs.arr_url, f.attrs.title || "?")),
    el("td", { text: `${arrName(f.attrs.arr)} · ${seasonLabel(f.attrs)}` }),
    el("td", { text: f.attrs.scope || "–" }),
    el("td", { text: r }))), 4, "No rows are hidden as unmonitored.");
}

// ---------- local library refresh ----------

let refreshPoll = null;

function renderRefresh(job, now) {
  const btn = $("refresh-library");
  const out = $("refresh-status");
  btn.disabled = Boolean(job.running);
  if (job.running) {
    out.textContent = `Refreshing Sonarr and Radarr… (${ago(job.started_at, now).replace(" ago", "")})`;
    // Poll faster while it runs, so the tables update as soon as it ends.
    if (!refreshPoll) refreshPoll = setInterval(refresh, 5000);
    return;
  }
  if (refreshPoll) {
    clearInterval(refreshPoll);
    refreshPoll = null;
  }
  out.textContent = !job.finished_at ? ""
    : job.error ? `Refresh failed: ${job.error}`
    : `Library refreshed ${ago(job.finished_at, now)}`;
}

async function refreshLibrary() {
  const btn = $("refresh-library");
  btn.disabled = true;
  try {
    const res = await fetch("api/library/refresh", { method: "POST", headers: { "X-Seadex-Scout": "1" } });
    if (!res.ok && res.status !== 409) throw new Error((await res.text()).trim());
  } catch (err) {
    $("refresh-status").textContent = "Could not start: " + err.message;
    btn.disabled = false;
    return;
  }
  refresh();
}

// ---------- Prowlarr search, SABnzbd send ----------

async function openUsenet(nyaaURL, a) {
  const dlg = $("usenet-dialog");
  const body = $("usenet-results").tBodies[0];
  $("usenet-query").textContent = "…";
  $("usenet-status").textContent = "Reading the Nyaa title and searching Prowlarr. This can take a while.";
  body.replaceChildren();
  if (!dlg.open) dlg.showModal();
  const category = ((state.snap.usenet || {}).categories || {})[a.arr];
  try {
    const res = await fetch("api/prowlarr/search", {
      method: "POST",
      headers: { "Content-Type": "application/json", "X-Seadex-Scout": "1" },
      body: JSON.stringify({ url: nyaaURL }),
    });
    if (!res.ok) throw new Error((await res.text()).trim());
    const found = await res.json();
    $("usenet-query").textContent = found.query;
    const exact = found.results.filter((r) => r.exact).length;
    $("usenet-status").textContent = found.results.length
      ? `${found.results.length} usenet results, ${exact} with exactly this title. Send adds it to SABnzbd${category ? ` under ${category}` : ""} for ${arrName(a.arr)}.`
      : `No usenet results${found.other ? ` (${found.other} torrent results left out)` : ""}.`;
    const now = Date.now();
    body.replaceChildren(...found.results.map((r) => {
      const send = el("button", { type: "button", class: "qbit-btn" }, "Send");
      send.addEventListener("click", () => sendToSab(send, found.token, r.id, a.arr));
      return el("tr", {},
        el("td", {}, r.exact ? el("span", { class: "exact", text: "✓ exact " }) : null, r.title),
        el("td", { text: r.indexer || "–" }),
        el("td", { class: "num", text: fmtBytes(r.size) }),
        el("td", { class: "num", text: r.posted ? ago(r.posted, now) : "–" }),
        el("td", { class: "num", text: String(r.grabs ?? "–") }),
        el("td", {}, send));
    }));
  } catch (err) {
    $("usenet-status").textContent = "Failed: " + err.message;
  }
}

async function sendToSab(btn, token, id, arr) {
  btn.disabled = true;
  btn.textContent = "Sending…";
  try {
    const res = await fetch("api/sabnzbd/add", {
      method: "POST",
      headers: { "Content-Type": "application/json", "X-Seadex-Scout": "1" },
      body: JSON.stringify({ token, id, arr }),
    });
    if (!res.ok) throw new Error((await res.text()).trim());
    const out = await res.json();
    btn.textContent = "Sent ✓";
    btn.classList.add("done");
    btn.title = out.category ? `Added to SABnzbd under ${out.category}` : "Added to SABnzbd";
  } catch (err) {
    btn.textContent = "Failed – retry";
    btn.classList.add("failed");
    btn.title = err.message;
    btn.disabled = false;
  }
}

function renderUsenet(u) {
  $("usenet-test").disabled = !u.enabled;
  const cats = u.categories || {};
  const rows = u.enabled ? [
    ["Status", "On: Nyaa links have a → Prowlarr button"],
    ["Sonarr category", cats.sonarr || "SABnzbd's default"],
    ["Radarr category", cats.radarr || "SABnzbd's default"],
  ] : [["Status", "Off. Set the Prowlarr URL and API key and the SABnzbd URL and API key to search Prowlarr for a Nyaa release's title and send a usenet copy to SABnzbd."]];
  $("usenet").replaceChildren(...rows.flatMap(([k, v]) => [el("dt", { text: k }), el("dd", { text: v })]));
}

async function testUsenet() {
  const btn = $("usenet-test");
  const out = $("usenet-test-result");
  btn.disabled = true;
  out.textContent = "Connecting…";
  try {
    const res = await fetch("api/usenet/test", { method: "POST", headers: { "X-Seadex-Scout": "1" } });
    if (!res.ok) throw new Error((await res.text()).trim());
    const v = await res.json();
    out.textContent = `Connected to Prowlarr ${v.prowlarr} and SABnzbd ${v.sabnzbd}`;
  } catch (err) {
    out.textContent = "Failed: " + err.message;
  }
  btn.disabled = false;
}

async function sendToQbit(btn, url, arr) {
  btn.disabled = true;
  btn.textContent = "Sending…";
  try {
    const res = await fetch("api/qbit/add", {
      method: "POST",
      headers: { "Content-Type": "application/json", "X-Seadex-Scout": "1" },
      body: JSON.stringify({ url, arr }),
    });
    if (!res.ok) throw new Error((await res.text()).trim());
    const out = await res.json();
    state.qbitSent.set(url, out.category || "");
    btn.textContent = "Added ✓";
    btn.classList.add("done");
    btn.title = out.category ? `Added under ${out.category}` : "Added";
  } catch (err) {
    btn.textContent = "Failed – retry";
    btn.classList.add("failed");
    btn.title = err.message;
    btn.disabled = false;
  }
}

// sizeCell shows a download size with the size change it brings underneath.
// An approximate change is marked "≈" and says why on hover.
function sizeCell(bytes, change, approx) {
  const c = num(change);
  const sub = el("span", { class: "sub " + (c > 0 ? "pos" : c < 0 ? "neg" : ""), text: (approx ? "≈ " : "") + fmtBytes(change, true) });
  if (approx) sub.title = "Approximate: the download against everything on disk for this season or film, because its files could not be matched to yours one by one";
  return el("td", { class: "num" }, fmtBytes(bytes), sub);
}

// releaseLinks lists a release's sources, Nyaa first. prefix is "" for SeaDex
// best and "alt_" for the alt release.
function releaseLinks(a, prefix) {
  const links = [];
  if (a[prefix + "nyaa_url"]) links.push(link(a[prefix + "nyaa_url"], "Nyaa"));
  if (a[prefix + "public_url"]) links.push(link(a[prefix + "public_url"], a[prefix + "public_tracker"] || "Tracker"));
  if (a[prefix + "ab_url"]) links.push(link(a[prefix + "ab_url"], a[prefix + "ab_tracker"] || "AnimeBytes"));
  return links;
}

// altCell names the alt release shown beside it, linked to the SeaDex entry
// like the best, with any other alt groups SeaDex lists underneath.
// withNotes shows the SeaDex entry's notes when hovering a group name, and
// marks names that have them with a dotted underline.
function withNotes(node, a) {
  const notes = typeof a.seadex_notes === "string" ? a.seadex_notes.trim() : "";
  if (notes && node instanceof Element) {
    node.title = "SeaDex notes:\n" + notes;
    node.classList.add("has-notes");
  }
  return node;
}

function altCell(a, entry) {
  const all = typeof a.alt_groups === "string" && a.alt_groups ? a.alt_groups.split(",") : [];
  if (!a.alt_group) return el("td", { text: all.join(", ") || "–" });
  // Other alts SeaDex lists show as their own sub-rows when they can be got;
  // only the names of ones that cannot are left here.
  const shown = new Set(parseOptions(a.alt_options).map((o) => (o.group || "").toLowerCase()));
  const others = all.filter((g) => g.toLowerCase() !== a.alt_group.toLowerCase() && !shown.has(g.toLowerCase()));
  const sameGroup = (a.recommended_group || "").toLowerCase() === a.alt_group.toLowerCase();
  return el("td", {}, withNotes(link(entry, a.alt_group), a),
    a.alt_seadex_tags ? el("span", { class: "sub", text: a.alt_seadex_tags }) : null,
    sameGroup ? el("span", { class: "sub", text: "a different release by the same group" }) : null,
    others.length ? el("span", { class: "sub", text: "also " + others.join(", ") }) : null);
}

// ---------- render: activity ----------

function renderActivity(snap) {
  const now = new Date(snap.now).getTime();
  const events = (snap.events || []).slice().reverse();
  setRows($("events").tBodies[0], events.map((e) => {
    const a = e.attrs || {};
    const found = e.msg === "upgrade found";
    const label = found ? (a.after_start ? "Found at start" : "New upgrade") : "Resolved";
    return el("tr", {},
      el("td", { title: fmtTime(e.time), text: ago(e.time, now) }),
      el("td", {}, el("span", { class: "tag " + (found ? (a.after_start ? "" : "warn") : "good"), text: label })),
      el("td", {}, link(a.arr_url, a.title || "?"), el("span", { class: "sub", text: `${arrName(a.arr)} · ${seasonLabel(a)}` })),
      groupList(a.current_group, ""),
      el("td", { text: a.recommended_group || "–" }));
  }), 5, "No upgrade has come or gone yet.");
}

// ---------- render: health ----------

function renderHealth(snap) {
  const now = new Date(snap.now).getTime();
  $("h-started").textContent = ago(snap.started_at, now).replace(" ago", "");
  $("h-started-sub").textContent = fmtTime(snap.started_at);

  const full = snap.last_full_check;
  $("h-full").textContent = snap.reconcile_complete ? ago(snap.reconcile_complete, now) : "–";
  $("h-full-sub").textContent = full
    ? `${full.msg}${full.attrs.duration ? " in " + fmtDuration(full.attrs.duration) : ""}${full.attrs.library_items ? " · " + full.attrs.library_items + " library items" : ""}`
    : "runs once a day";

  const errs = recentErrors(snap);
  $("h-errors").textContent = errs.length;
  $("h-errors").parentElement.className = "kpi" + (errs.length ? " warn" : " good");

  const ix = snap.indexer || {};
  $("h-indexer").textContent = (ix.requests || 0).toLocaleString();
  $("h-indexer-sub").textContent = ix.last_request ? `last ${ago(ix.last_request, now)}` : "indexer not used yet";

  const conds = Object.entries(snap.conditions || {}).sort((a, b) => b[1].time.localeCompare(a[1].time));
  $("conditions").replaceChildren(...(conds.length ? conds.map(([cond, line]) => {
    const active = conditionActive(snap, line, now);
    return el("div", { class: "alert " + (active ? (cond.startsWith("library-") ? "bad" : "info") : "cleared") },
      el("strong", { text: `${cond}${active ? "" : " (cleared)"}` }),
      el("span", { text: `${line.msg} · last logged ${ago(line.time, now)}` }));
  }) : [el("p", { class: "empty", text: "None. SeaDex, AniList, the ID map and your library reads are all fine." })]));

  const cfg = snap.config && snap.config.attrs;
  const kv = $("config");
  if (cfg) {
    const yes = (v) => (v === true ? "yes" : v === false ? "no" : String(v ?? "–"));
    kv.replaceChildren(...[
      ["Sonarr", yes(cfg.sonarr_enabled)], ["Radarr", yes(cfg.radarr_enabled)],
      ["Check interval", cfg.poll_interval], ["AnimeBytes", yes(cfg.animebytes)],
      ["Leave out remuxes", yes(cfg.exclude_remux)], ["Require dual audio", yes(cfg.require_dual_audio)],
      ["Leave out specials", yes(cfg.exclude_specials)], ["Ignored shows", cfg.ignored_findings],
      ["Include tags", cfg.include_tags], ["Exclude tags", cfg.exclude_tags],
    ].flatMap(([k, v]) => [el("dt", { text: k }), el("dd", { text: String(v ?? "–") })]));
  } else {
    kv.replaceChildren(el("dd", { text: "Not logged since this start." }));
  }

  renderCycles(snap.cycles || []);
  renderNotify(snap.notify || {}, now);
  renderQbit(snap.qbit || {});
  renderUsenet(snap.usenet || {});
  renderHidden(snap, now);

  setRows($("errors").tBodies[0], (snap.errors || []).slice().reverse().map((e) => {
    const detail = Object.entries(e.attrs || {}).map(([k, v]) => `${k}=${typeof v === "string" ? v : JSON.stringify(v)}`).join("  ");
    return el("tr", {},
      el("td", { title: fmtTime(e.time), text: ago(e.time, now) }),
      el("td", { text: e.msg }),
      el("td", { class: "small", text: detail }));
  }), 3, "No errors logged.");

  setRows($("degraded").tBodies[0], (snap.degraded || []).slice().reverse().map((e) => el("tr", {},
    el("td", { title: fmtTime(e.time), text: ago(e.time, now) }),
    el("td", { text: e.msg.startsWith("cycle") ? "Full check" : "Quick check" }),
    el("td", { text: (e.attrs && e.attrs.reason) || "–" }))), 3, "No degraded checks.");

  const snapLine = ix.last_snapshot && ix.last_snapshot.attrs;
  const scopes = Object.entries(ix.by_scope || {}).map(([k, v]) => `${k}: ${v}`).join(", ");
  $("indexer").replaceChildren(...[
    ["Requests", (ix.requests || 0).toLocaleString()],
    ["RSS polls and tests", (ix.feed_requests || 0).toLocaleString()],
    ["Results returned", (ix.returned || 0).toLocaleString()],
    ["By tracker", scopes || "–"],
    ["Last request", ix.last_request ? fmtTime(ix.last_request) : "–"],
    ["Feed last rebuilt", ix.last_snapshot ? fmtTime(ix.last_snapshot.time) : "–"],
    ["Nyaa releases in feed", snapLine ? snapLine.nyaa_feed : "–"],
    ["AnimeBytes releases in feed", snapLine ? snapLine.ab_feed : "–"],
  ].flatMap(([k, v]) => [el("dt", { text: k }), el("dd", { text: String(v ?? "–") })]));
}

function renderCycles(cycles) {
  const box = $("cycles-chart");
  const pts = cycles.map((c) => ({ t: c.time, s: durationSeconds(c.attrs && c.attrs.duration), bad: c.msg === "cycle degraded" }))
    .filter((p) => p.s !== null);
  if (!pts.length) {
    box.replaceChildren(el("p", { class: "empty", text: "Shown after the first full check." }));
    return;
  }
  const ns = "http://www.w3.org/2000/svg";
  const W = 800, H = 160, padL = 44, padB = 20, padT = 8;
  const max = Math.max(60, ...pts.map((p) => p.s));
  const svg = document.createElementNS(ns, "svg");
  svg.setAttribute("viewBox", `0 0 ${W} ${H}`);
  svg.setAttribute("preserveAspectRatio", "none");
  const bw = (W - padL) / Math.max(pts.length, 12);
  pts.forEach((p, i) => {
    const h = ((H - padB - padT) * p.s) / max;
    const r = document.createElementNS(ns, "rect");
    r.setAttribute("x", padL + i * bw + 1);
    r.setAttribute("y", H - padB - h);
    r.setAttribute("width", Math.max(1, bw - 2));
    r.setAttribute("height", h);
    r.setAttribute("class", "bar" + (p.bad ? " degraded" : ""));
    const title = document.createElementNS(ns, "title");
    title.textContent = `${fmtTime(p.t)}: ${Math.round(p.s / 60)} min${p.bad ? " (degraded)" : ""}`;
    r.append(title);
    svg.append(r);
  });
  for (const frac of [0, 0.5, 1]) {
    const t = document.createElementNS(ns, "text");
    t.setAttribute("x", 0);
    t.setAttribute("y", H - padB - (H - padB - padT) * frac + 4);
    t.setAttribute("class", "axis");
    t.textContent = `${Math.round((max * frac) / 60)}m`;
    svg.append(t);
  }
  box.replaceChildren(svg);
}

// ---------- notifications ----------

const EVENT_NAMES = { upgrades: "new upgrades", problems: "problems and their end", errors: "errors", reports: "finished reports" };

function renderNotify(n, now) {
  $("notify-test").disabled = !n.enabled;
  const rows = n.enabled ? [
    ["Sends to", n.target || "–"],
    ["Sends", (n.events || []).map((e) => EVENT_NAMES[e] || e).join(", ") || "nothing"],
    ["Sent since start", String(n.sent || 0) + (n.last_sent ? `, last ${ago(n.last_sent, now)}` : "")],
    ["Last error", n.last_error ? `${n.last_error} (${ago(n.error_at, now)})` : "none"],
  ] : [
    ["Status", "Off. Set SEADEX_SCOUT_NTFY_URL to an ntfy topic, such as https://ntfy.sh/your-topic, to get a message for each new upgrade."],
  ];
  $("notify").replaceChildren(...rows.flatMap(([k, v]) => [el("dt", { text: k }), el("dd", { text: v })]));
}

function renderQbit(q) {
  $("qbit-test").disabled = !q.enabled;
  const cats = q.categories || {};
  const rows = q.enabled ? [
    ["Status", "On: Nyaa links have a → qBit button"],
    ["Sonarr category", cats.sonarr || "none"],
    ["Radarr category", cats.radarr || "none"],
  ] : [["Status", "Off. Set SEADEX_SCOUT_QBIT_URL to qBittorrent's Web UI address to add a → qBit button under each Nyaa link."]];
  $("qbit").replaceChildren(...rows.flatMap(([k, v]) => [el("dt", { text: k }), el("dd", { text: v })]));
}

async function testQbit() {
  const btn = $("qbit-test");
  const out = $("qbit-test-result");
  btn.disabled = true;
  out.textContent = "Connecting…";
  try {
    const res = await fetch("api/qbit/test", { method: "POST", headers: { "X-Seadex-Scout": "1" } });
    if (!res.ok) throw new Error((await res.text()).trim());
    const v = await res.json();
    out.textContent = `Connected to qBittorrent ${v.version}`;
  } catch (err) {
    out.textContent = "Failed: " + err.message;
  }
  btn.disabled = false;
}

async function sendTestNotification() {
  const btn = $("notify-test");
  const out = $("notify-test-result");
  btn.disabled = true;
  out.textContent = "Sending…";
  try {
    const res = await fetch("api/notify/test", { method: "POST", headers: { "X-Seadex-Scout": "1" } });
    out.textContent = res.ok ? "Sent. Check your ntfy app." : "Failed: " + (await res.text()).trim();
  } catch (err) {
    out.textContent = "Failed: " + err.message;
  }
  btn.disabled = false;
  refresh();
}

// ---------- reports ----------

function renderReportJob(job) {
  const btn = $("report-run");
  const label = $("report-job");
  if (!job) return;
  btn.disabled = job.running;
  if (job.running) {
    label.textContent = `Report running since ${fmtTime(job.started_at)}…`;
  } else if (job.finished_at) {
    label.textContent = `Last run finished ${ago(job.finished_at)}${job.error ? " with an error: " + job.error : ""}${job.result ? " · " + job.result : ""}`;
  } else {
    label.textContent = "";
  }
}

async function loadReports() {
  try {
    const data = await getJSON("api/reports");
    renderReportJob(data.job);
    setRows($("reports").tBodies[0], data.reports.map((r) => el("tr", {},
      el("td", { text: r.stem }),
      el("td", { title: fmtTime(r.modified), text: `${fmtTime(r.modified)} (${ago(r.modified)})` }),
      el("td", { class: "num", text: fmtBytes(r.size) }),
      el("td", {}, el("div", { class: "links" },
        r.json ? el("a", { href: "#reports", onclick: (ev) => { ev.preventDefault(); openReport(r.stem); }, text: "View" }) : null,
        r.markdown ? el("a", { href: `api/reports/${encodeURIComponent(r.stem)}.md?download`, text: "Markdown" }) : null,
        r.json ? el("a", { href: `api/reports/${encodeURIComponent(r.stem)}.json?download`, text: "JSON" }) : null)),
    )), 4, "No reports yet. Run one to compare every season with SeaDex.");
    if (data.job && data.job.running) setTimeout(() => { if (state.tab === "reports") loadReports(); }, 15000);
  } catch (err) {
    setRows($("reports").tBodies[0], [], 4, "Could not read the report folder.");
    console.error(err);
  }
}

async function runReport() {
  const btn = $("report-run");
  btn.disabled = true;
  try {
    const res = await fetch("api/reports/run", { method: "POST", headers: { "X-Seadex-Scout": "1" } });
    if (!res.ok && res.status !== 409) throw new Error(await res.text());
  } catch (err) {
    $("report-job").textContent = "Could not start the report: " + err.message;
  }
  loadReports();
}

const VERDICTS = {
  have_best: ["At SeaDex best", "good"],
  have_alt: ["At a SeaDex alt", "good"],
  have_older_revision: ["Older version", "warn"],
  have_unlisted: ["Neither best nor alt", "warn"],
  no_file: ["Season not found", ""],
  unverified: ["Untagged release", ""],
  unattributed: ["Episode not known", ""],
  withheld: ["Withheld", ""],
  not_on_seadex: ["Not on SeaDex", ""],
};

async function openReport(stem) {
  const view = $("report-view");
  view.hidden = false;
  $("report-title").textContent = stem;
  $("report-rows").tBodies[0].replaceChildren(el("tr", {}, el("td", { colspan: 6, class: "empty", text: "Loading…" })));
  try {
    state.report = await getJSON(`api/reports/${encodeURIComponent(stem)}.json`);
    state.reportVerdict = "";
    const sel = $("report-verdict");
    sel.replaceChildren(el("option", { value: "", text: "Every verdict" }),
      ...Object.keys(state.report.totals || {}).map((v) => el("option", { value: v, text: (VERDICTS[v] || [v])[0] })));
    renderReport();
    view.scrollIntoView({ behavior: "smooth", block: "start" });
  } catch (err) {
    $("report-rows").tBodies[0].replaceChildren(el("tr", {}, el("td", { colspan: 6, class: "empty", text: "Could not load the report." })));
    console.error(err);
  }
}

function renderReport() {
  const rep = state.report;
  if (!rep) return;
  const verdict = $("report-verdict").value;
  const q = $("report-search").value.trim().toLowerCase();
  $("report-totals").replaceChildren(...Object.entries(rep.totals || {}).map(([v, n]) =>
    el("button", { type: "button", class: verdict === v ? "active" : "", onclick: () => { $("report-verdict").value = verdict === v ? "" : v; renderReport(); } },
      `${(VERDICTS[v] || [v])[0]}: ${n}`)));

  const rows = (rep.rows || []).filter((r) => (!verdict || r.verdict === verdict) &&
    (!q || [r.title, ...(r.current_groups || []), ...(r.releases || []).map((x) => x.group)]
      .some((s) => typeof s === "string" && s.toLowerCase().includes(q))));
  const LIMIT = 1000;
  setRows($("report-rows").tBodies[0], rows.slice(0, LIMIT).map((r) => {
    const [label, cls] = VERDICTS[r.verdict] || [r.verdict, ""];
    const rels = (r.releases || []).map((x) => {
      const text = `${x.group || "?"}${x.best ? " (best)" : ""} · ${x.tracker}`;
      return x.url ? link(x.url, text) : el("span", { class: "tag", text });
    });
    return el("tr", {},
      el("td", {}, link(r.arr_url, r.title || "?"), r.seadex_url ? el("span", { class: "sub" }, link(r.seadex_url, "SeaDex")) : null),
      el("td", { text: arrName(r.arr) }),
      el("td", { text: r.special ? "Special" : r.arr === "radarr" ? "Film" : r.season !== undefined ? `S${r.season}` : "–" }),
      el("td", {}, el("span", { class: "tag " + cls, text: label })),
      groupList(r.current_groups, ""),
      el("td", {}, el("div", { class: "links" }, rels)));
  }), 6, "No rows match.");
  $("report-more").textContent = `Generated ${fmtTime(rep.generated_at)} · ${rows.length} of ${(rep.rows || []).length} rows${rows.length > LIMIT ? `, first ${LIMIT} shown; narrow the filter to see the rest` : ""}`;
}

// ---------- log ----------

async function loadLogs() {
  try {
    const lines = await getJSON("api/logs");
    const order = { INFO: 0, WARN: 1, ERROR: 2 };
    const min = order[$("log-level").value] ?? 0;
    const q = $("log-search").value.trim().toLowerCase();
    const shown = lines.filter((l) => (order[l.level] ?? 0) >= min).reverse()
      .map((l) => ({ l, attrs: Object.entries(l.attrs || {}).map(([k, v]) => `${k}=${typeof v === "string" ? JSON.stringify(v) : JSON.stringify(v)}`).join(" ") }))
      .filter(({ l, attrs }) => !q || l.msg.toLowerCase().includes(q) || attrs.toLowerCase().includes(q));
    $("log").replaceChildren(...(shown.length ? shown.map(({ l, attrs }) => el("div", { class: "log-line" },
      el("span", { class: "muted", text: new Date(l.time).toLocaleTimeString() + " " }),
      el("span", { class: "lvl " + l.level, text: l.level }),
      el("span", { text: l.msg + " " }),
      el("span", { class: "attrs", text: attrs }))) : [el("p", { class: "empty", text: "No lines." })]));
  } catch (err) {
    $("log").replaceChildren(el("p", { class: "empty", text: "Could not load the log." }));
    console.error(err);
  }
}

// ---------- wiring ----------

function init() {
  initTheme();
  initTabs();
  for (const id of ["upgrades-search", "upgrades-arr", "upgrades-why", "upgrades-optional", "upgrades-unmonitored"]) {
    $(id).addEventListener("input", () => state.snap && renderOverview(state.snap));
  }
  $("report-run").addEventListener("click", runReport);
  $("notify-test").addEventListener("click", sendTestNotification);
  $("qbit-test").addEventListener("click", testQbit);
  $("usenet-test").addEventListener("click", testUsenet);
  $("refresh-library").addEventListener("click", refreshLibrary);
  // A count tile filters the table to its kind; clicking it again shows everything.
  for (const tile of document.querySelectorAll(".kpi[data-why]")) {
    tile.addEventListener("click", () => {
      const sel = $("upgrades-why");
      sel.value = sel.value === tile.dataset.why ? "" : tile.dataset.why;
      if (state.snap) renderOverview(state.snap);
      if (sel.value) $("upgrades").scrollIntoView({ behavior: "smooth", block: "start" });
    });
  }
  $("usenet-close").addEventListener("click", () => $("usenet-dialog").close());
  $("report-search").addEventListener("input", renderReport);
  $("report-verdict").addEventListener("input", renderReport);
  $("log-level").addEventListener("input", loadLogs);
  $("log-search").addEventListener("input", loadLogs);

  refresh();
  setInterval(() => {
    refresh();
    if (state.tab === "log") loadLogs();
  }, REFRESH_MS);
  document.addEventListener("visibilitychange", () => { if (!document.hidden) refresh(); });
}

init();
