"use strict";

// The Library grid (one card per series or film, from the findings) and the
// series page (#series:<arr>:<id>). Both read state.snap only; the actions on
// the series page are library.js's buttons.

// ---------- grouping ----------

// The card status order: what most needs a look comes first.
const CARD_KINDS = {
  upgrade: { label: "Upgrade", cls: "k-upgrade", rank: 0 },
  groups: { label: "Group to check", cls: "k-groups", rank: 1 },
  missing: { label: "Not downloaded", cls: "k-missing", rank: 2 },
  nobest: { label: "No SeaDex best", cls: "k-nobest", rank: 3 },
  atbest: { label: "On a SeaDex pick", cls: "k-atbest", rank: 4 },
  other: { label: "Not compared", cls: "k-nobest", rank: 5 },
};

const GRID_FILTERS = [
  ["", "All"],
  ["need", "Needs you"],
  ["missing", "Not downloaded"],
  ["atbest", "On a SeaDex pick"],
  ["nobest", "No SeaDex best"],
];

// seriesOf groups the findings by series or film, keeping each finding's kind;
// keep, when given, leaves out the findings it refuses (the Library hides
// unmonitored ones, as the overview does).
function seriesOf(snap, keep) {
  const out = new Map();
  for (const f of (snap && snap.findings) || []) {
    if (keep && !keep(f)) continue;
    const a = f.attrs;
    const id = num(a.arr_id);
    if (!id) continue;
    const key = `${a.arr}:${id}`;
    let s = out.get(key);
    if (!s) {
      s = { key, arr: a.arr, id, title: a.title || "?", url: a.arr_url, findings: [], bytes: 0, kind: "other" };
      out.set(key, s);
    }
    const kind = kindOf(f);
    s.findings.push({ f, kind });
    s.bytes += num(a.library_bytes) || 0;
    if (CARD_KINDS[kind].rank < CARD_KINDS[s.kind].rank) s.kind = kind;
  }
  for (const s of out.values()) {
    s.findings.sort((x, y) => (num(x.f.attrs.season) ?? -1) - (num(y.f.attrs.season) ?? -1));
  }
  return out;
}

// cardPair is the "held → SeaDex best" line of a card: the series' most
// actionable finding.
function cardPair(s) {
  const top = s.findings.slice().sort((x, y) => CARD_KINDS[x.kind].rank - CARD_KINDS[y.kind].rank)[0];
  const a = top ? top.f.attrs : {};
  // On a pick (by group, or proven by import): Sonarr may name the group
  // otherwise than SeaDex, so an arrow would read as an upgrade.
  if (top && top.kind === "atbest") return { held: (a.current_group || "–").split(",")[0], best: "", same: true };
  const held = a.status === "not_downloaded" ? "Nothing" : (a.current_group || "–").split(",")[0];
  const best = a.recommended_group || (a.alt_group ? `${a.alt_group} (alt)` : "");
  return { held, best, same: best && held.toLowerCase() === best.toLowerCase() };
}

// ---------- Library grid ----------

const grid = { filter: "", built: null, key: "", series: null };

function gridFilterOf(kind) {
  if (kind === "upgrade" || kind === "groups") return "need";
  return kind;
}

function renderGrid() {
  const snap = state.snap;
  if (!snap) return;
  const q = $("grid-search").value.trim().toLowerCase();
  const arr = $("grid-arr").value;
  const showUnmonitored = $("grid-unmonitored").checked;
  if (grid.built !== snap || grid.key !== String(showUnmonitored)) {
    const unmonitored = unmonitoredTest(snap);
    grid.series = seriesOf(snap, showUnmonitored ? null : (f) => !unmonitored(f));
    grid.built = snap;
    grid.key = String(showUnmonitored);
  }
  const all = [...grid.series.values()].filter((s) => !arr || s.arr === arr);
  const counts = { "": all.length };
  for (const s of all) counts[gridFilterOf(s.kind)] = (counts[gridFilterOf(s.kind)] || 0) + 1;
  $("grid-chips").replaceChildren(...GRID_FILTERS.map(([id, label]) => el("button", {
    type: "button", class: "chip" + (grid.filter === id ? " selected" : ""), "aria-pressed": String(grid.filter === id),
    onclick: () => { grid.filter = id; renderGrid(); },
  }, id ? el("span", { class: "dot " + (id === "need" ? "k-upgrade" : "k-" + id) }) : null, label, el("span", { class: "chip-n", text: String(counts[id] || 0) }))));

  const shown = all.filter((s) => (!grid.filter || gridFilterOf(s.kind) === grid.filter) &&
    (!q || s.title.toLowerCase().includes(q) || s.findings.some(({ f }) =>
      `${f.attrs.current_group || ""} ${f.attrs.recommended_group || ""}`.toLowerCase().includes(q))))
    .sort((x, y) => CARD_KINDS[x.kind].rank - CARD_KINDS[y.kind].rank || x.title.localeCompare(y.title));
  $("grid-count").textContent = shown.length;
  const cards = shown.map((s) => {
    const k = CARD_KINDS[s.kind];
    const pair = cardPair(s);
    const seasons = s.arr === "radarr" ? "Film" : `${s.findings.length} ${s.findings.length === 1 ? "entry" : "entries"}`;
    return el("a", { class: "series-card", href: `#series:${s.arr}:${s.id}` },
      el("div", { class: "series-card-top" },
        el("span", { class: "status-pill " + k.cls, text: k.label }),
        el("span", { class: "muted mono small", text: seasons })),
      el("span", { class: "series-card-title", text: s.title }),
      el("div", { class: "pair mono small" },
        el("span", { class: "grp", text: pair.held }),
        pair.best && !pair.same ? el("span", { class: "arrow", "aria-hidden": "true", text: "→" }) : null,
        pair.best && !pair.same ? el("span", { class: "grp best", text: pair.best }) : null,
        el("span", { class: "muted size", text: s.bytes ? fmtBytes(s.bytes) : "" })));
  });
  const box = $("grid-cards");
  box.replaceChildren(...cards);
  $("grid-empty").hidden = cards.length > 0;
  $("grid-empty").textContent = snap.pass && snap.pass.id ? "No series match these filters." : "Shown after the first check.";
}

// ---------- series page ----------

// openSeries is library.js's entry point; it now opens the page.
function openSeriesPage(arr, id) {
  location.hash = `#series:${arr}:${id}`;
}

function renderSeries(arr, id) {
  const snap = state.snap;
  if (!snap) return;
  const all = seriesOf(snap);
  const s = all.get(`${arr}:${id}`);
  const head = $("series-head");
  const body = $("series-body");
  if (!s) {
    head.replaceChildren(el("h2", { text: "Not found" }));
    body.replaceChildren(el("p", { class: "empty", text: "The scout has no finding for this series. It may have been removed, or it has no SeaDex entry." }));
    return;
  }
  const k = CARD_KINDS[s.kind];
  const alIDs = [...new Set(s.findings.map(({ f }) => f.attrs.al_id).filter(Boolean))];
  const scanRows = ((lib.scan && lib.scan.seasons) || []).filter((r) => r.arr === arr && r.id === id);
  const low = scanRows.reduce((n, r) => n + r.low, 0);
  head.replaceChildren(
    el("div", { class: "series-badges" },
      el("span", { class: "status-pill " + k.cls, text: k.label }),
      el("span", { class: "tag", text: arrName(arr) }),
      s.bytes ? el("span", { class: "tag", text: fmtBytes(s.bytes) + " on disk" }) : null,
      lib.scan ? el("span", { class: "tag" + (low ? " warn" : ""), text: low ? `${low} files below ${lib.scan.min_score}` : "every file scores SeaDex" }) : null),
    el("h2", { class: "series-title" }, s.title),
    el("div", { class: "series-links" },
      s.url ? link(s.url, `Open in ${arrName(arr)}`, "btn-link") : null,
      ...alIDs.map((al) => link(SEADEX + al + "/", alIDs.length > 1 ? `SeaDex ${al}` : "SeaDex entry", "btn-link"))));

  const cards = s.findings.map(({ f, kind }) => seasonCard(f, kind));
  // Seasons the score scan found low that no finding covers.
  for (const r of scanRows) {
    const season = r.arr === "sonarr" ? r.season : -1;
    if (s.findings.some(({ f }) => (num(f.attrs.season) ?? -1) === season)) continue;
    cards.push(el("section", { class: "card season-card" },
      el("div", { class: "season-head" }, el("h3", { text: season === 0 ? "Specials" : season > 0 ? `Season ${season}` : "Film" }),
        el("span", { class: "status-pill k-nobest", text: "Not compared" })),
      el("p", { class: "muted small", text: `${r.low} of ${r.files} files below ${r.min_score}. No SeaDex entry the scout could match covers it.` })));
  }

  const notes = [...new Set(s.findings.map(({ f }) => String(f.attrs.seadex_notes || "").trim()).filter(Boolean))];
  const events = (snap.events || []).filter((e) => e.attrs && e.attrs.arr === arr && e.attrs.arr_url === s.url).slice(-12).reverse();
  const now = new Date(snap.now).getTime();
  const side = el("div", { class: "series-side" },
    el("section", { class: "card" }, el("h3", { text: "What happened" }),
      events.length ? el("ol", { class: "timeline" }, ...events.map((e) => {
        const found = e.msg === "upgrade found";
        const a = e.attrs;
        return el("li", { class: found ? "found" : "resolved" },
          el("span", { class: "when mono small", title: fmtTime(e.time), text: ago(e.time, now) }),
          el("span", {}, `${found ? "Upgrade found" : "Resolved"} · ${seasonLabel(a)}`,
            el("span", { class: "sub", text: `${a.current_group || "–"} → ${a.recommended_group || "–"}` })));
      })) : el("p", { class: "muted small", text: "No upgrade has come or gone for this series yet." })),
    notes.length ? el("section", { class: "card" }, el("h3", { text: "SeaDex notes" }),
      ...notes.map((n) => el("blockquote", { class: "notes", text: n }))) : null);
  body.replaceChildren(el("div", { class: "series-main" }, ...cards), side);
}

// seasonCard is one finding: what is on disk beside SeaDex's best (or alt),
// its sizes and links, coverage, and the actions library.js offers for it.
function seasonCard(f, kind) {
  const a = f.attrs;
  const k = CARD_KINDS[kind];
  const why = whyOf(a);
  const actions = findingActions(f);
  const uncovered = typeof a.uncovered_files === "string" && a.uncovered_files ? a.uncovered_files.split(",").length : 0;
  const best = a.recommended_group
    ? sideBlock("SeaDex best", a.recommended_group, [a.kind, a.resolution, a.codec, a.tracker].filter(Boolean).join(" · "),
      a.recommended_bytes ? fmtBytes(a.recommended_bytes) + (num(a.size_change_bytes) ? ` (${fmtBytes(a.size_change_bytes, true)})` : "") : "",
      releaseLinks(a, ""), true)
    : a.alt_group
      ? sideBlock("SeaDex alt", a.alt_group, a.alt_seadex_tags || "", a.alt_recommended_bytes ? fmtBytes(a.alt_recommended_bytes) : "", releaseLinks(a, "alt_"), true)
      : sideBlock("SeaDex", noBestText(a), "", "", [], false);
  const held = a.status === "not_downloaded"
    ? sideBlock("On disk", "Nothing yet", "", "", [], false)
    : sideBlock("On disk", a.current_group || "–", a.current_revision ? `revision ${a.current_revision}` : "", a.library_bytes ? fmtBytes(a.library_bytes) : "", [], false);
  return el("section", { class: "card season-card" },
    el("div", { class: "season-head" },
      el("h3", { text: seasonLabel(a) || "Whole series" }),
      el("span", { class: "status-pill " + k.cls, text: k.label }),
      why.text !== k.label && !(kind === "atbest" && a.status === "at_best") ? el("span", { class: "tag " + why.cls, text: why.text }) : null),
    el("div", { class: "compare" }, held, el("span", { class: "arrow big", "aria-hidden": "true", text: "→" }), best),
    uncovered ? el("p", { class: "muted small", text: `${uncovered} file${uncovered === 1 ? "" : "s"} hold an episode SeaDex's best does not have; its alt counts as the best for ${uncovered === 1 ? "it" : "them"}.` }) : null,
    lastSearch(a),
    actions.childNodes.length ? el("div", { class: "season-actions" }, ...actions.childNodes) : null);
}

// lastSearch is the newest search of the finding's season or film, with what
// came of it and why (library.js keeps them).
function lastSearch(a) {
  const t = targetOf(a);
  const j = t && searchFor(t);
  if (!j) return null;
  const st = searchState(j);
  const when = j.finished || j.started || j.added;
  return el("div", { class: "last-search" },
    el("span", { class: "tag " + st.cls, text: st.text }),
    el("span", { class: "muted small", title: fmtTime(when), text: `last search ${ago(when, new Date(state.snap.now).getTime())}` }),
    st.detail ? el("span", { class: "small last-why", text: st.detail }) : null);
}

function sideBlock(label, group, detail, size, links, best) {
  return el("div", { class: "side" + (best ? " best" : "") },
    el("span", { class: "side-label", text: label }),
    el("span", { class: "side-group", text: group }),
    detail ? el("span", { class: "muted small", text: detail }) : null,
    size ? el("span", { class: "mono small", text: size }) : null,
    links.length ? el("span", { class: "links" }, ...links) : null);
}

// ---------- wiring ----------

// app.js runs first (the scripts are deferred, in order), so this file wires
// itself and shows the page the address names again, now it can draw it.
(function initSeries() {
  for (const id of ["grid-search", "grid-arr", "grid-unmonitored"]) {
    if (id !== "grid-search") remember(id);
    $(id).addEventListener("input", () => state.tab === "grid" && renderGrid());
  }
  if (state.tab === "grid" || state.tab === "series") showTab(location.hash.slice(1));
})();
