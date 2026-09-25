(() => {
'use strict';
const $ = (s, r = document) => r.querySelector(s);
const $$ = (s, r = document) => [...r.querySelectorAll(s)];
const esc = s => String(s ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));

async function api(path, opts = {}) {
  const o = { headers: { 'X-F95-UI': '1' }, ...opts };
  if (o.body && typeof o.body !== 'string') { o.body = JSON.stringify(o.body); o.headers['Content-Type'] = 'application/json'; }
  const r = await fetch(path, o);
  let j = null; try { j = await r.json(); } catch {}
  if (r.status === 401 && !path.startsWith('/api/auth/')) { location.href = '/login'; throw new Error('Session expired'); }
  if (!r.ok) throw new Error((j && j.error) || r.statusText);
  return j;
}
function toast(msg, err) {
  const d = document.createElement('div'); d.className = 'toast' + (err ? ' err' : ''); d.textContent = msg;
  $('#toasts').appendChild(d); setTimeout(() => d.remove(), 4500);
}
const act = async (fn, okMsg) => { try { const r = await fn(); if (okMsg) toast(typeof okMsg === 'function' ? okMsg(r) : okMsg); refresh(); return r; } catch (e) { toast(e.message, true); } };

const fmtBytes = n => n < 1024 ? n + ' B' : n < 1048576 ? (n/1024).toFixed(1) + ' KB' : n < 1073741824 ? (n/1048576).toFixed(1) + ' MB' : (n/1073741824).toFixed(2) + ' GB';
const fmtDur = s => { s = Math.round(s); if (s < 60) return s + 's'; if (s < 3600) return Math.floor(s/60) + 'm ' + s%60 + 's'; return Math.floor(s/3600) + 'h ' + Math.floor(s%3600/60) + 'm'; };
const ago = t => { if (!t) return '-'; const s = (Date.now() - new Date(t)) / 1000; if (s < 0) return 'in ' + fmtDur(-s); if (s < 5) return 'just now'; return fmtDur(s) + ' ago'; };
const inn = t => { if (!t) return '-'; const s = (new Date(t) - Date.now()) / 1000; return s <= 0 ? 'now' : 'in ' + fmtDur(s); };
const clock = t => t ? new Date(t).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit' }) : '-';

// F95-style label colours (status badges from the title brackets)
const LABELC = { UPDATE:'#c0392b', NEW:'#2e9e5b', Completed:'#1f7fb8', Abandoned:'#7a4a2a', Onhold:'#b8901f', SiteRip:'#555', Collection:'#6a6a2a', Poll:'#555', 'Cheat Mod':'#6a4a8a' };
const ENGINEC = { 'Ren\'Py':'#b5651d', Unity:'#4a4a4a', RPGM:'#2a6fc9', HTML:'#2f8f5b', 'Unreal Engine':'#333', QSP:'#555', RAGS:'#555', Java:'#a0522d', Flash:'#a02020', WebGL:'#3a7a7a', Godot:'#3c7fb0', 'Wolf RPG':'#6a4a8a', ADRIFT:'#555', TADS:'#555' };
const labelHTML = t => `<span class="tag" style="background:${LABELC[t] || '#6d4aff'}">${esc(t)}</span>`;
const engineHTML = t => t ? `<span class="tag" style="background:${ENGINEC[t] || '#37474f'}">${esc(t)}</span>` : '';
const tagChipHTML = t => `<span class="tag" style="background:#3a3d46">${esc(t)}</span>`;
const versionHTML = v => v ? `<span class="tag" style="background:#2a2a2a;border:1px solid #444">v${esc(v.replace(/^v/i,''))}</span>` : '';

// ── router ──
const views = ['dashboard', 'releases', 'notifications', 'log', 'runs', 'feed', 'settings'];
let cur = '';
function route() {
  let v = (location.hash.replace(/^#\//, '') || 'dashboard'); if (!views.includes(v)) v = 'dashboard';
  cur = v;
  views.forEach(x => $('#v-' + x).classList.toggle('on', x === v));
  $$('#tabs a').forEach(a => a.classList.toggle('on', a.dataset.v === v));
  if (v === 'releases') loadReleases();
  if (v === 'notifications') loadNotifications();
  if (v === 'runs') loadRuns();
  if (v === 'feed') loadFeedView();
  if (v === 'settings') loadSettings();
  if (v === 'log') { renderLog(true); }
  refresh();
}
window.addEventListener('hashchange', route);

// ── status ──
let S = null;
async function refresh() {
  try { S = await api('/api/status'); } catch (e) { $('#hdrPill').className = 'pill err'; $('#hdrPill b').textContent = 'OFFLINE'; return; }
  renderStatus();
}
function renderStatus() {
  const p = S.progress, run = S.running;
  const pill = $('#hdrPill');
  pill.className = 'pill ' + (run ? 'run' : p.status === 'ERROR' ? 'err' : p.status === 'DONE' ? 'ok' : 'idle');
  $('#hdrPill b').textContent = run ? (p.status === 'FETCHING' ? p.trigger === 'backfill' ? 'BACKFILL' : 'FETCHING' : `${p.trigger === 'backfill' ? 'BACKFILL' : 'ENRICHING'} ${p.current}/${p.total}`) : p.status;

  const nb = $('#notifBadge');
  if (S.unread_notifications > 0) { nb.hidden = false; nb.textContent = S.unread_notifications > 99 ? '99+' : S.unread_notifications; } else nb.hidden = true;

  const c = S.cache, lr = S.last_run;
  const tiles = [
    ['Total releases', c.items, `${c.with_description} enriched` + (c.failed ? ` · ${c.failed} failed` : '') + (c.needs_reparse ? ` · ${c.needs_reparse} awaiting re-scrape` : '')],
    ['Next run', S.scheduler_enabled ? inn(S.next_run) : 'Paused', S.scheduler_enabled ? `every ${S.schedule_hours}h · ${S.next_run ? clock(S.next_run) : ''}` : 'scheduler disabled'],
    ['Last run', lr ? ago(lr.finished) : 'never', lr ? `${lr.status} · ${fmtDur(lr.duration_s)} · ${lr.items} items` : ''],
    ['Images', S.images.count, `${fmtBytes(S.images.bytes)} on disk`],
  ];
  $('#tiles').innerHTML = tiles.map(t => `<div class="stat"><div class="k">${t[0]}</div><div class="v">${esc(t[1])}</div><div class="s">${esc(t[2])}</div></div>`).join('');

  const bar = $('#bar');
  bar.style.width = (run && p.status === 'FETCHING' ? 100 : p.percent) + '%';
  bar.classList.toggle('busy', run && p.status === 'FETCHING');
  $('#barTxt').textContent = run ? (p.status === 'FETCHING' ? (p.item || 'Fetching...') : `${p.current} / ${p.total}  (${p.percent}%)`) : (p.status === 'DONE' ? 'Done' : p.status === 'CANCELLED' ? 'Cancelled' : p.status === 'ERROR' ? 'Error - see log' : 'Idle');
  $('#curItem').textContent = run && p.item ? '\u25B8 ' + p.item : '\u00A0';
  $('#pipeMeta').textContent = run ? `${p.trigger} run · started ${ago(p.started)}${p.failed ? ' · ' + p.failed + ' failed' : ''}` : `mode: ${S.fetch_mode} · uptime ${fmtDur(S.uptime_s)}`;
  ['btnRun', 'btnForce', 'btnFull', 'btnRebuild', 'btnRebuild2', 'btnPurge', 'btnClearCache', 'btnBackfill'].forEach(id => { const b = $('#' + id); if (b) b.disabled = run; });
  $('#btnStop').disabled = !run;
  $('#btnSched').textContent = S.scheduler_enabled ? 'Pause scheduler' : 'Resume scheduler';
  $('#storageKv').innerHTML = [
    ['Images on disk', `${S.images.count} (${fmtBytes(S.images.bytes)})`],
    ['Orphaned images', `${S.images.orphans} (${fmtBytes(S.images.orphan_bytes)})`],
    ['Feed window cache', c.mtime ? `${ago(c.mtime)} · ${c.fresh ? 'fresh' : 'stale'} (TTL ${c.ttl_hours}h)` : 'empty'],
    ['feed.xml', S.feed ? `${fmtBytes(S.feed.size)} · built ${ago(S.feed.mtime)}` : 'not built yet'],
    ['Runs (ok / total)', S.runs_total ? `${S.runs_ok} / ${S.runs_total}` : '-'],
  ].map(([k, v]) => `<span class="k">${k}</span><span class="v">${esc(v)}</span>`).join('');
  if (cur === 'feed') renderFeedKv();
}

$('#btnRun').onclick = () => act(() => api('/api/run', { method: 'POST', body: {} }), 'Run started');
$('#btnForce').onclick = () => act(() => api('/api/run', { method: 'POST', body: { force: true } }), 'Forced refresh started');
$('#btnFull').onclick = () => { if (confirm('Re-scrape every thread from scratch? This takes several minutes.')) act(() => api('/api/run', { method: 'POST', body: { full: true } }), 'Full re-scrape started'); };
$('#btnStop').onclick = () => act(() => api('/api/stop', { method: 'POST' }), 'Stopping...');
$('#btnSched').onclick = () => act(async () => { const c = await api('/api/config'); c.scheduler_enabled = !c.scheduler_enabled; return api('/api/config', { method: 'PUT', body: c }); }, 'Scheduler updated');
const rebuild = () => act(() => api('/api/feed/rebuild', { method: 'POST' }), r => `Feed rebuilt (${r.items} items)`);
$('#btnRebuild').onclick = rebuild; $('#btnRebuild2').onclick = rebuild;
$('#btnPurge').onclick = () => act(() => api('/api/images/purge', { method: 'POST' }), r => `Removed ${r.removed} images (${fmtBytes(r.bytes)})`);
$('#btnClearCache').onclick = () => { if (confirm('Clear the entire release history? The next run will re-scrape everything from the current source feed (older backfilled releases will be gone).')) act(() => api('/api/cache/clear', { method: 'POST' }), 'Release history cleared'); };
$('#btnBackfill').onclick = () => {
  const pages = Math.max(1, Math.min(200, Number($('#bfPages').value) || 5));
  act(() => api('/api/backfill', { method: 'POST', body: { pages } }), r => `Backfill started (${r.pages} page(s)) - watch the live log`);
};

// ── live log ──
const logs = []; let lastId = 0, paused = false, lvl = 'ALL', q = '';
const MAXLOG = 3000;
const lineHTML = (e, hl) => {
  const t = new Date(e.ts).toLocaleTimeString([], { hour12: false });
  let m = esc(e.msg);
  if (hl) m = m.replace(new RegExp(hl.replace(/[.*+?^${}()|[\]\\]/g, '\\$&').replace(/&/g,'&amp;'), 'gi'), x => `<mark>${x}</mark>`);
  const hi = /^(\[\d+\/\d+\]|Starting|Pipeline complete|Backfill complete|Run cancelled)/.test(e.msg) ? ' hi' : '';
  return `<div class="ll ${e.level}${hi}"><span class="t">${t}</span> <span class="lv">${e.level}</span><span class="m">${m}</span></div>`;
};
const passes = e => (lvl === 'ALL' || e.level === lvl || (lvl === 'WARN' && e.level === 'ERROR')) && (!q || e.msg.toLowerCase().includes(q));
function renderLog() {
  const box = $('#logBox'); if (!box) return;
  const list = logs.filter(passes);
  box.innerHTML = list.slice(-1500).map(e => lineHTML(e, q)).join('');
  $('#logCount').textContent = `${list.length} / ${logs.length} lines`;
  if ($('#logFollow').checked) box.scrollTop = box.scrollHeight;
}
function renderMini() {
  const box = $('#miniLog'); box.innerHTML = logs.slice(-60).map(e => lineHTML(e)).join(''); box.scrollTop = box.scrollHeight;
}
let pend = [], raf = 0;
function pushLog(e) {
  if (e.id <= lastId) return; lastId = e.id;
  logs.push(e); if (logs.length > MAXLOG) logs.shift();
  if (paused) return;
  pend.push(e);
  if (!raf) raf = setTimeout(flushLog, 80);
}
function flushLog() {
  raf = 0; const batch = pend; pend = [];
  if (cur === 'log') {
    const box = $('#logBox'), follow = $('#logFollow').checked;
    const add = batch.filter(passes); if (add.length) box.insertAdjacentHTML('beforeend', add.map(e => lineHTML(e, q)).join(''));
    while (box.childElementCount > 1500) box.firstElementChild.remove();
    $('#logCount').textContent = `${logs.filter(passes).length} / ${logs.length} lines`;
    if (follow) box.scrollTop = box.scrollHeight;
  }
  if (cur === 'dashboard') { const m = $('#miniLog'); m.insertAdjacentHTML('beforeend', batch.map(e => lineHTML(e)).join('')); while (m.childElementCount > 60) m.firstElementChild.remove(); m.scrollTop = m.scrollHeight; }
}
function connectLog() {
  const es = new EventSource('/api/logs/stream?since=' + lastId);
  es.onopen = () => $('#liveDot').classList.add('on');
  es.onmessage = ev => pushLog(JSON.parse(ev.data));
  es.onerror = () => { $('#liveDot').classList.remove('on'); };
}
$('#lvlSeg').onclick = e => { const b = e.target.closest('button'); if (!b) return; $$('#lvlSeg button').forEach(x => x.classList.toggle('on', x === b)); lvl = b.dataset.l; renderLog(); };
$('#logSearch').oninput = e => { q = e.target.value.toLowerCase(); renderLog(); };
$('#logWrap').onchange = e => $('#logBox').classList.toggle('nowrap', !e.target.checked);
$('#logFollow').onchange = () => { if ($('#logFollow').checked) $('#logBox').scrollTop = 1e9; };
$('#logBox').addEventListener('wheel', () => { const b = $('#logBox'); if (b.scrollHeight - b.scrollTop - b.clientHeight > 40) $('#logFollow').checked = false; });
$('#logPause').onclick = () => { paused = !paused; $('#logPause').textContent = paused ? 'Resume' : 'Pause'; if (!paused) { pend = []; renderLog(); renderMini(); } };
$('#logClear').onclick = () => { if (confirm('Clear the log file and buffer?')) act(async () => { await api('/api/logs/clear', { method: 'POST' }); logs.length = 0; renderLog(); renderMini(); }, 'Log cleared'); };

// ── releases: filters, saved filter sets (persisted in localStorage) ──
const LS_FILTER = 'f95_current_filter', LS_SETS = 'f95_filter_sets';
let relState = { q: '', tags: [], engine: '', sort: 'newest', failed: false, watched: false, page: 1 };
let allTags = [];

function loadPersistedFilter() {
  try { const v = JSON.parse(localStorage.getItem(LS_FILTER) || 'null'); if (v) relState = { ...relState, ...v, page: 1 }; } catch {}
}
function persistFilter() {
  try { localStorage.setItem(LS_FILTER, JSON.stringify({ q: relState.q, tags: relState.tags, engine: relState.engine, sort: relState.sort, failed: relState.failed, watched: relState.watched })); } catch {}
}
function getFilterSets() { try { return JSON.parse(localStorage.getItem(LS_SETS) || '[]'); } catch { return []; } }
function saveFilterSets(v) { try { localStorage.setItem(LS_SETS, JSON.stringify(v)); } catch {} }

function renderFilterSets() {
  const sel = $('#filterSets'); const sets = getFilterSets();
  sel.innerHTML = '<option value="">Saved filters...</option>' + sets.map((s, i) => `<option value="${i}">${esc(s.name)}</option>`).join('');
}
$('#saveFilterBtn').onclick = () => {
  const name = prompt('Name this filter set:'); if (!name) return;
  const sets = getFilterSets().filter(s => s.name !== name);
  sets.push({ name, q: relState.q, tags: relState.tags, engine: relState.engine, sort: relState.sort, failed: relState.failed, watched: relState.watched });
  saveFilterSets(sets); renderFilterSets(); toast('Filter saved');
};
$('#filterSets').onchange = e => {
  if (e.target.value === '') return;
  const s = getFilterSets()[Number(e.target.value)]; if (!s) return;
  relState = { ...relState, q: s.q, tags: s.tags, engine: s.engine, sort: s.sort, failed: s.failed, watched: !!s.watched, page: 1 };
  applyFilterToControls(); persistFilter(); queryReleases();
};
$('#clearFilterBtn').onclick = () => {
  relState = { q: '', tags: [], engine: '', sort: 'newest', failed: false, watched: false, page: 1 };
  applyFilterToControls(); persistFilter(); queryReleases();
};
function applyFilterToControls() {
  $('#relSearch').value = relState.q; $('#relEngine').value = relState.engine; $('#relSort').value = relState.sort; $('#relFailed').checked = relState.failed; $('#relWatched').checked = relState.watched;
  renderChips();
}

function renderChips() {
  $('#tagChips').innerHTML = relState.tags.map(t => `<span class="chip">${esc(t)}<button data-t="${esc(t)}" type="button">&times;</button></span>`).join('');
}
$('#tagChips').onclick = e => {
  const b = e.target.closest('button'); if (!b) return;
  relState.tags = relState.tags.filter(t => t !== b.dataset.t); relState.page = 1;
  renderChips(); persistFilter(); queryReleases();
};

function renderTagOptions(filter) {
  const f = (filter || '').toLowerCase();
  const list = allTags.filter(t => !f || t.value.toLowerCase().includes(f)).slice(0, 150);
  $('#tagOptions').innerHTML = list.map(t => `<label class="tagoption" title="${esc(t.value)}"><input type="checkbox" data-t="${esc(t.value)}" ${relState.tags.includes(t.value) ? 'checked' : ''}><span class="t">${esc(t.value)}</span><span class="c">${t.count}</span></label>`).join('') || '<div class="muted small" style="padding:6px">No tags yet</div>';
}
$('#tagPickerBtn').onclick = () => { const p = $('#tagPanel'); p.hidden = !p.hidden; if (!p.hidden) { $('#tagPanelSearch').value = ''; renderTagOptions(''); $('#tagPanelSearch').focus(); } };
$('#tagPanelSearch').oninput = e => renderTagOptions(e.target.value);
$('#tagOptions').onchange = e => {
  const cb = e.target.closest('input[type=checkbox]'); if (!cb) return;
  const t = cb.dataset.t;
  if (cb.checked) { if (!relState.tags.includes(t)) relState.tags.push(t); }
  else relState.tags = relState.tags.filter(x => x !== t);
  relState.page = 1; renderChips(); persistFilter(); queryReleases();
};
document.addEventListener('click', e => {
  const panel = $('#tagPanel'), btn = $('#tagPickerBtn');
  if (!panel.hidden && !panel.contains(e.target) && e.target !== btn) panel.hidden = true;
});

// ── cover size slider: persisted in localStorage so it survives both
// browser reloads and app restarts (localStorage is unaffected by either). ──
const LS_TILE_SIZE = 'f95_tile_size';
function applyTileSize(px) {
  document.documentElement.style.setProperty('--tile-min', px + 'px');
}
(function initTileSize() {
  let v = parseInt(localStorage.getItem(LS_TILE_SIZE), 10);
  if (!v || v < 160 || v > 520) v = 230;
  $('#relTileSize').value = v;
  applyTileSize(v);
})();
$('#relTileSize').oninput = e => {
  const v = Number(e.target.value);
  applyTileSize(v);
  try { localStorage.setItem(LS_TILE_SIZE, String(v)); } catch {}
};

async function loadReleases() {
  loadPersistedFilter();
  applyFilterToControls();
  renderFilterSets();
  try { allTags = await api('/api/tags?limit=500'); } catch { allTags = []; }
  try {
    const engines = await api('/api/engines');
    const sel = $('#relEngine');
    sel.innerHTML = '<option value="">All engines</option>' + engines.map(e => `<option value="${esc(e.value)}">${esc(e.value)} (${e.count})</option>`).join('');
    sel.value = relState.engine;
  } catch {}
  setupScrollObserver();
  await queryReleases();
}

let relTimer;

// ── infinite scroll: relState.page/pages track what's already loaded and
// appended to #relGrid; a filter change starts over via queryReleases(true),
// scrolling near the bottom (or clicking "Load more") appends the next page. ──
let relLoading = false;
let relScrollObserver;

function tileHTML(x) {
  return `
    <div class="tile" data-link="${esc(x.link)}" data-cover="${esc(x.cover || '')}">
      <div class="cv" style="${x.cover ? `background-image:url('${esc(x.cover)}')` : ''}">
        <button type="button" class="watch${x.watched ? ' on' : ''}" data-watch="${esc(x.link)}" title="${x.watched ? 'Stop monitoring' : 'Monitor for updates'}">${x.watched ? '★' : '☆'}</button>
        ${x.error ? '<span class="bad">FAILED</span>' : ''}${x.images ? `<span class="n">${x.images} img</span>` : ''}
      </div>
      <div class="tb"><div class="tt">${esc(x.title.replace(/^(\[[^\]]*\]\s*)+/, '') || x.title)}</div>
      <div class="tm">${x.labels.map(labelHTML).join('')}${engineHTML(x.engine)}${versionHTML(x.version)}<div>${ago(x.pub)}</div></div></div>
    </div>`;
}

// ── hover slideshow: cycles a release tile's screenshots while the mouse is
// over it, reverting to the static cover on mouse-leave. Images for a tile
// are only fetched the first time it's actually hovered (not eagerly for the
// whole grid), and cached per-link for the rest of the session. ──
let hoverCfg = { enabled: true, delay: 900, startDelay: 400 };
const hoverImgCache = new Map(); // link -> [urls] | Promise
let hoverState = null; // { link, el, cover, timer } - slideshow actually running
let hoverPending = null; // { el, timer } - waiting out startDelay before it begins

function stopHoverSlideshow() {
  if (hoverPending) { clearTimeout(hoverPending.timer); hoverPending = null; }
  if (!hoverState) return;
  clearInterval(hoverState.timer);
  hoverState.el.style.backgroundImage = hoverState.cover ? `url('${hoverState.cover}')` : '';
  hoverState = null;
}

// Waits hoverCfg.startDelay before actually starting the slideshow, so
// dragging the mouse across the grid on the way to something else doesn't
// fire off an image fetch and swap the cover for nothing.
function startHoverSlideshow(link, el, cover) {
  if (!hoverCfg.enabled) return;
  if ((hoverState && hoverState.el === el) || (hoverPending && hoverPending.el === el)) return;
  stopHoverSlideshow();
  hoverPending = { el, timer: setTimeout(() => { hoverPending = null; beginHoverSlideshow(link, el, cover); }, hoverCfg.startDelay) };
}

async function beginHoverSlideshow(link, el, cover) {
  hoverState = { link, el, cover, timer: 0 };
  let imgs = hoverImgCache.get(link);
  if (!imgs) {
    imgs = api('/api/release?link=' + encodeURIComponent(link)).then(d => d.images && d.images.length ? d.images : [cover]).catch(() => [cover]);
    hoverImgCache.set(link, imgs);
  }
  const urls = await imgs;
  hoverImgCache.set(link, urls); // resolve to the plain array once loaded
  // the tile may have been un-hovered while the fetch was in flight
  if (!hoverState || hoverState.el !== el || !urls || urls.length < 2) return;
  let i = 0;
  hoverState.timer = setInterval(() => {
    i = (i + 1) % urls.length;
    el.style.backgroundImage = `url('${urls[i]}')`;
  }, hoverCfg.delay);
}

async function toggleWatch(link, on, btn) {
  try {
    await api('/api/releases/watch', { method: 'POST', body: { link, watched: on } });
    if (btn) { btn.classList.toggle('on', on); btn.textContent = on ? '★' : '☆'; btn.title = on ? 'Stop monitoring' : 'Monitor for updates'; }
    toast(on ? 'Monitoring release' : 'No longer monitoring');
  } catch (e) { toast(e.message, true); }
}

function updateScrollEnd(page, pages, loading) {
  const btn = $('#relLoadMore'), loadingEl = $('#relLoading'), endEl = $('#relEnd');
  const more = pages > page;
  btn.hidden = loading || !more;
  loadingEl.hidden = !loading;
  endEl.hidden = loading || more;
}

async function fetchReleasesPage(page) {
  const p = new URLSearchParams({
    q: relState.q, tags: relState.tags.join(','), engine: relState.engine, sort: relState.sort,
    failed: relState.failed ? '1' : '0', watched: relState.watched ? '1' : '0', page, page_size: 60,
  });
  return api('/api/releases?' + p);
}

// queryReleases(true) (the default) resets to page 1 and replaces the grid -
// call this after any filter/sort change. queryReleases(false) is used
// internally by loadMoreReleases and appends instead.
async function queryReleases(reset = true) {
  if (reset) {
    relState.page = 1;
    relState.pages = 1;
    $('#relGrid').innerHTML = '';
  }
  if (relLoading) return;
  relLoading = true;
  updateScrollEnd(relState.page, relState.pages || 1, true);
  try {
    const r = await fetchReleasesPage(relState.page);
    relState.pages = r.pages;
    $('#relCount').textContent = `${r.total} release${r.total === 1 ? '' : 's'}${r.pages > 1 ? ` \u00b7 showing ${Math.min(relState.page * 60, r.total)} of ${r.total}` : ''}`;
    if (r.items.length) {
      $('#relGrid').insertAdjacentHTML('beforeend', r.items.map(tileHTML).join(''));
    } else if (reset) {
      $('#relGrid').innerHTML = '<div class="empty">No releases match. Run a scrape, backfill, or adjust the filters.</div>';
    }
  } finally {
    relLoading = false;
    updateScrollEnd(relState.page, relState.pages || 1, false);
  }
}

async function loadMoreReleases() {
  if (relLoading || relState.page >= (relState.pages || 1)) return;
  relState.page += 1;
  await queryReleases(false);
}

function setupScrollObserver() {
  if (relScrollObserver || !('IntersectionObserver' in window)) return;
  relScrollObserver = new IntersectionObserver(entries => {
    if (entries.some(e => e.isIntersecting)) loadMoreReleases();
  }, { rootMargin: '600px 0px' });
  relScrollObserver.observe($('#relScrollEnd'));
}
$('#relLoadMore').onclick = () => loadMoreReleases();

$('#relSearch').oninput = () => { clearTimeout(relTimer); relTimer = setTimeout(() => { relState.q = $('#relSearch').value; persistFilter(); queryReleases(); }, 250); };
['relEngine', 'relSort', 'relFailed', 'relWatched'].forEach(id => $('#' + id).onchange = () => {
  relState.engine = $('#relEngine').value; relState.sort = $('#relSort').value; relState.failed = $('#relFailed').checked; relState.watched = $('#relWatched').checked;
  persistFilter(); queryReleases();
});
$('#relGrid').onclick = e => {
  const w = e.target.closest('[data-watch]');
  if (w) { e.stopPropagation(); toggleWatch(w.dataset.watch, !w.classList.contains('on'), w); return; }
  const t = e.target.closest('.tile'); if (t) openRelease(t.dataset.link);
};
$('#relGrid').addEventListener('mouseover', e => {
  const cv = e.target.closest('.tile .cv'); if (!cv) return;
  const tile = cv.closest('.tile');
  startHoverSlideshow(tile.dataset.link, cv, tile.dataset.cover);
});
$('#relGrid').addEventListener('mouseout', e => {
  const cv = e.target.closest('.tile .cv'); if (!cv) return;
  if (cv.contains(e.relatedTarget)) return;
  stopHoverSlideshow();
});

// overviewHTML renders just the release's blurb text - no banner image, no
// duplicate screenshot grid (those live in the Screenshots section below and
// in the hero strip above). Prefers the dedicated Overview field; falls back
// to the general description for rows enriched before that field existed.
function overviewHTML(r) {
  if (r.overview) return r.overview.split(/\n{2,}/).map(p => `<p>${esc(p)}</p>`).join('');
  if (r.extra_description) return r.extra_description;
  return '<span class="muted">No description available.</span>';
}

function detailHTML(r, d) {
  const hero = d.cover;
  const info = [
    ['Developer', r.developer], ['Engine', r.engine], ['Version', r.version],
    ['Published', clock(r.pub_date_iso)], ['Thread updated', r.thread_updated],
    ['Enriched', r.enriched_at ? clock(r.enriched_at) : ''], ['OS', r.os], ['Language', r.language],
    ['Censored', r.censored], ['Store', r.store],
  ].filter(([, v]) => v);
  return `
    <div class="rel-hero" style="${hero ? `background-image:url('${esc(hero)}')` : ''}">
      <div class="rh-in">
        <div class="rh-title">${esc(r.title.replace(/^(\[[^\]]*\]\s*)+/, '') || r.title)}</div>
        <div class="rh-sub">${r.labels.map(labelHTML).join('')}${engineHTML(r.engine)}${versionHTML(r.version)}</div>
      </div>
    </div>
    <div class="btnrow" style="margin:0 0 14px">
      <a class="btn primary" href="${esc(r.link)}" target="_blank" rel="noopener noreferrer">Open thread</a>
      <button class="btn" id="mReenrich">Re-scrape this thread</button>
      <button type="button" class="watch-btn${r.watched ? ' on' : ''}" id="mWatch" data-link="${esc(r.link)}"><span class="watch-star">${r.watched ? '★' : '☆'}</span><span id="mWatchLabel">${r.watched ? 'Monitoring' : 'Monitor for updates'}</span></button>
    </div>
    ${r.enrich_error ? `<div class="banner">Last enrichment error: ${esc(r.enrich_error)}</div>` : ''}
    <div class="rel-layout">
      <div>
        <h4>Overview</h4>
        <div class="rel-desc">${overviewHTML(r)}</div>
      </div>
      <div>
        <h4>Info</h4>
        <dl class="rel-info">${info.map(([k, v]) => `<dt>${esc(k)}</dt><dd>${esc(v)}</dd>`).join('')}</dl>
        ${r.tags && r.tags.length ? `<h4>Tags</h4><div class="rel-tags">${r.tags.map(tagChipHTML).join('')}</div>` : ''}
      </div>
    </div>
    ${d.images.length ? `<h4>Screenshots</h4><div class="shots">${d.images.map(u => `<img loading="lazy" src="${esc(u)}" data-full="${esc(u)}">`).join('')}</div>` : ''}`;
}

let currentRelLink = null;

function relTileLinks() {
  return Array.from($('#relGrid').querySelectorAll('.tile')).map(t => t.dataset.link);
}

// j/k navigation between releases while the detail modal is open. Stepping
// past the last currently-loaded tile triggers the same infinite-scroll
// page load used when scrolling the grid, so navigation never dead-ends
// just because the next release hasn't been fetched yet.
async function navigateRelease(delta) {
  if (!currentRelLink) return;
  let links = relTileLinks();
  let idx = links.indexOf(currentRelLink);
  if (idx === -1) return;
  let nextIdx = idx + delta;
  if (nextIdx < 0) return;
  if (nextIdx >= links.length) {
    if (relLoading || relState.page >= (relState.pages || 1)) return;
    await loadMoreReleases();
    links = relTileLinks();
    if (nextIdx >= links.length) return;
  }
  openRelease(links[nextIdx]);
}

async function openRelease(link) {
  const d = await api('/api/release?link=' + encodeURIComponent(link)).catch(e => toast(e.message, true)); if (!d) return;
  const r = d.release;
  currentRelLink = link;
  $('#mTitle').textContent = r.title;
  $('#modalBox').classList.add('wide');
  $('#mBody').innerHTML = detailHTML(r, d);
  $('#mReenrich').onclick = async () => { const ok = await act(() => api('/api/reenrich', { method: 'POST', body: { link } }), 'Re-scrape started'); if (ok) closeModal(); };
  $('#mWatch').onclick = () => toggleWatchModal(link, !r.watched);
  $('#modal').hidden = false;
}
function toggleWatchModal(link, on) {
  toggleWatch(link, on).then(() => {
    const btn = $('#mWatch'); if (!btn) return;
    btn.classList.toggle('on', on);
    $('.watch-star', btn).textContent = on ? '★' : '☆';
    $('#mWatchLabel').textContent = on ? 'Monitoring' : 'Monitor for updates';
    const grid = $(`[data-watch="${CSS.escape(link)}"]`); if (grid) { grid.classList.toggle('on', on); grid.textContent = on ? '★' : '☆'; }
  });
}
const closeModal = () => { $('#modal').hidden = true; $('#mBody').innerHTML = ''; $('#modalBox').classList.remove('wide'); currentRelLink = null; };
$('#mClose').onclick = closeModal;
$('#modal').onclick = e => { if (e.target.id === 'modal') closeModal(); };

// screenshot lightbox (nested above the release modal)
$('#mBody').addEventListener('click', e => {
  const img = e.target.closest('.shots img'); if (!img) return;
  $('#lightboxImg').src = img.dataset.full; $('#lightbox').hidden = false;
});
$('#lbClose').onclick = () => { $('#lightbox').hidden = true; $('#lightboxImg').src = ''; };
$('#lightbox').onclick = e => { if (e.target.id === 'lightbox') { $('#lightbox').hidden = true; $('#lightboxImg').src = ''; } };
document.addEventListener('keydown', e => {
  if (e.key === 'Escape') {
    if (!$('#lightbox').hidden) { $('#lightbox').hidden = true; $('#lightboxImg').src = ''; return; }
    if (!$('#modal').hidden) closeModal();
    return;
  }
  if ($('#modal').hidden || !$('#lightbox').hidden) return;
  const tag = (e.target.tagName || '').toLowerCase();
  if (tag === 'input' || tag === 'textarea' || tag === 'select' || e.target.isContentEditable) return;
  if (e.key === 'j' || e.key === 'J') { e.preventDefault(); navigateRelease(1); }
  else if (e.key === 'k' || e.key === 'K') { e.preventDefault(); navigateRelease(-1); }
});

// ── notifications ──
async function loadNotifications() {
  const unread = $('#notifUnread').checked;
  const r = await api('/api/notifications?unread=' + (unread ? '1' : '0') + '&limit=300');
  $('#notifCount').textContent = `${r.items.length} shown · ${r.unread} unread`;
  $('#notifGrid').innerHTML = r.items.length ? r.items.map(n => `
    <div class="notif-tile ${n.read ? 'read' : ''}" data-link="${esc(n.link)}" data-id="${n.id}">
      <div class="cv" style="${n.cover ? `background-image:url('${esc(n.cover)}')` : ''}"></div>
      <div class="body">
        <div class="tt"><span class="kind ${n.kind}">${n.kind === 'new' ? 'New' : 'Update'}</span>${esc(n.title.replace(/^(\[[^\]]*\]\s*)+/, '') || n.title)}</div>
        <div class="meta">${n.kind === 'update' ? `v${esc(n.old_version || '?')} &rarr; v${esc(n.new_version || '?')}` : (n.new_version ? 'v' + esc(n.new_version) : '')} &middot; ${ago(n.created_at)}${n.pushed ? ' &middot; pushed' : ''}${n.push_error ? ' &middot; push failed' : ''}</div>
        <div style="margin-top:6px">${(n.labels || []).map(labelHTML).join('')}${(n.tags || []).slice(0, 6).map(tagChipHTML).join('')}</div>
      </div>
      <button class="x" data-clear="${n.id}" title="Clear">&times;</button>
    </div>`).join('') : '<div class="empty">No notifications yet. They appear here when a monitored release is new or its version changes.</div>';
}
$('#notifUnread').onchange = loadNotifications;
$('#notifMarkAll').onclick = () => act(() => api('/api/notifications/read', { method: 'POST', body: { all: true } }), 'Marked all as read').then(loadNotifications);
$('#notifClearAll').onclick = () => { if (confirm('Clear all notifications?')) act(() => api('/api/notifications/clear', { method: 'POST', body: { all: true } }), 'Notifications cleared').then(loadNotifications); };
$('#notifGrid').onclick = async e => {
  const x = e.target.closest('[data-clear]');
  if (x) { await api('/api/notifications/clear', { method: 'POST', body: { id: Number(x.dataset.clear) } }); loadNotifications(); refresh(); return; }
  const t = e.target.closest('.notif-tile'); if (!t) return;
  await api('/api/notifications/read', { method: 'POST', body: { id: Number(t.dataset.id), read: true } });
  openRelease(t.dataset.link); loadNotifications(); refresh();
};

// ── runs ──
async function loadRuns() {
  const h = await api('/api/history');
  $('#runsTbl').innerHTML = `<tr><th>#</th><th>Started</th><th>Trigger</th><th>Status</th><th>Duration</th><th>Items</th><th>Scraped</th><th>Reused</th><th>Failed</th><th>New</th><th>Updated</th><th>New images</th></tr>` +
    (h.length ? h.map(r => `<tr><td>${r.id}</td><td>${clock(r.started)}</td><td>${esc(r.trigger)}</td>
      <td class="st-${r.status}" title="${esc(r.error || '')}">${esc(r.status)}${r.error ? ' ⚠' : ''}</td><td>${fmtDur(r.duration_s)}</td><td>${r.items}</td><td>${r.enriched}</td><td>${r.reused}</td><td>${r.failed || '-'}</td><td>${r.new_releases || '-'}</td><td>${r.updated_releases || '-'}</td><td>${r.images_new}</td></tr>`).join('')
      : '<tr><td colspan="12" class="muted" style="text-align:center;padding:30px">No runs recorded yet.</td></tr>');
}

// ── feed ──
function loadFeedView() {
  api('/api/config').then(c => {
    $('#feedUrl').value = (c.public_base_url || '') + '/feed.xml';
    $('#feedLocal').value = location.origin + '/feed.xml';
  }); renderFeedKv();
}
function renderFeedKv() {
  if (!S) return;
  $('#feedKv').innerHTML = [['Items in feed window', S.cache.window_items ?? '-'], ['Total in database', S.cache.items], ['Size', S.feed ? fmtBytes(S.feed.size) : '-'], ['Last built', S.feed ? clock(S.feed.mtime) + ' (' + ago(S.feed.mtime) + ')' : 'never']]
    .map(([k, v]) => `<span class="k">${k}</span><span class="v">${esc(v)}</span>`).join('');
}
const copier = (btn, inp) => $(btn).onclick = async () => { try { await navigator.clipboard.writeText($(inp).value); } catch { $(inp).select(); document.execCommand('copy'); } toast('Copied'); };
copier('#copyFeed', '#feedUrl'); copier('#copyLocal', '#feedLocal');

// ── settings ──
async function loadSettings() {
  const c = await api('/api/config'); const f = $('#cfgForm');
  for (const el of f.elements) { if (!el.name) continue; if (el.type === 'checkbox') el.checked = !!c[el.name]; else el.value = c[el.name] ?? ''; }
}
$('#cfgReset').onclick = loadSettings;
$('#cfgForm').onsubmit = async e => {
  e.preventDefault(); const f = e.target, body = {};
  for (const el of f.elements) { if (!el.name) continue; body[el.name] = el.type === 'checkbox' ? el.checked : el.type === 'number' ? Number(el.value) : el.value; }
  const m = $('#cfgMsg');
  try { await api('/api/config', { method: 'PUT', body }); m.textContent = 'Saved.'; m.style.color = 'var(--ok)'; toast('Settings saved'); loadHoverCfg(); refresh(); }
  catch (err) { m.textContent = err.message; m.style.color = 'var(--err)'; }
};
$('#btnPushoverTest').onclick = async () => {
  const m = $('#pushMsg');
  try { await api('/api/pushover/test', { method: 'POST' }); m.textContent = 'Sent - check your device.'; m.style.color = 'var(--ok)'; }
  catch (err) { m.textContent = err.message; m.style.color = 'var(--err)'; }
};
$('#btnCheckLogin').onclick = async () => {
  const box = $('#loginCheckResult'); const btn = $('#btnCheckLogin');
  btn.disabled = true; box.innerHTML = '<div class="banner">Checking&hellip;</div>';
  try {
    const r = await api('/api/f95/check-login', { method: 'POST' });
    const cls = r.logged_in ? 'ok' : (r.cookie_set ? 'warn' : '');
    box.innerHTML = `<div class="banner ${cls}"><b>${r.logged_in ? 'Logged in' : r.cookie_set ? 'Cookie set but not confirmed logged in' : 'No cookie configured'}</b>` +
      (r.checked_title ? ` &middot; probed <i>${esc(r.checked_title)}</i>` : '') +
      (r.detail ? `<div class="small" style="margin-top:4px">${esc(r.detail)}</div>` : '') + `</div>`;
  } catch (err) {
    box.innerHTML = `<div class="banner">${esc(err.message)}</div>`;
  } finally { btn.disabled = false; }
};

// ── account ──
$('#btnLogout').onclick = async () => { try { await api('/api/auth/logout', { method: 'POST' }); } catch {} location.href = '/login'; };
$('#pwForm').onsubmit = async e => {
  e.preventDefault(); const f = e.target, m = $('#pwMsg');
  if (f.new.value !== f.confirm.value) { m.textContent = 'Passwords do not match'; m.style.color = 'var(--err)'; return; }
  try { await api('/api/account/password', { method: 'POST', body: { current: f.current.value, new: f.new.value } }); f.reset(); m.textContent = 'Password changed.'; m.style.color = 'var(--ok)'; toast('Password changed'); }
  catch (err) { m.textContent = err.message; m.style.color = 'var(--err)'; }
};

async function loadHoverCfg() {
  try { const c = await api('/api/config'); hoverCfg = { enabled: !!c.hover_slideshow_enabled, delay: c.hover_slideshow_delay_ms || 900, startDelay: c.hover_slideshow_start_delay_ms ?? 400 }; } catch {}
}

// ── boot ──
setInterval(() => { $('#hdrClock').textContent = new Date().toLocaleTimeString([], { hour12: false }); }, 1000);
setInterval(refresh, 2000);
(async () => {
  try { (await api('/api/logs?limit=500')).forEach(e => { if (e.id > lastId) { lastId = e.id; logs.push(e); } }); } catch {}
  try { const me = await (await fetch('/api/auth/status')).json(); $('#hdrUser').textContent = me.username || ''; $('#acctUser').textContent = me.username ? 'signed in as ' + me.username : ''; } catch {}
  loadHoverCfg();
  renderMini(); connectLog(); route();
})();
})();
