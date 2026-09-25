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
let relState = { q: '', tags: [], engine: '', sort: 'newest', failed: false, page: 1 };
let allTags = [];

function loadPersistedFilter() {
  try { const v = JSON.parse(localStorage.getItem(LS_FILTER) || 'null'); if (v) relState = { ...relState, ...v, page: 1 }; } catch {}
}
function persistFilter() {
  try { localStorage.setItem(LS_FILTER, JSON.stringify({ q: relState.q, tags: relState.tags, engine: relState.engine, sort: relState.sort, failed: relState.failed })); } catch {}
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
  sets.push({ name, q: relState.q, tags: relState.tags, engine: relState.engine, sort: relState.sort, failed: relState.failed });
  saveFilterSets(sets); renderFilterSets(); toast('Filter saved');
};
$('#filterSets').onchange = e => {
  if (e.target.value === '') return;
  const s = getFilterSets()[Number(e.target.value)]; if (!s) return;
  relState = { ...relState, q: s.q, tags: s.tags, engine: s.engine, sort: s.sort, failed: s.failed, page: 1 };
  applyFilterToControls(); persistFilter(); queryReleases();
};
$('#clearFilterBtn').onclick = () => {
  relState = { q: '', tags: [], engine: '', sort: 'newest', failed: false, page: 1 };
  applyFilterToControls(); persistFilter(); queryReleases();
};
function applyFilterToControls() {
  $('#relSearch').value = relState.q; $('#relEngine').value = relState.engine; $('#relSort').value = relState.sort; $('#relFailed').checked = relState.failed;
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
  const list = allTags.filter(t => !f || t.tag.toLowerCase().includes(f)).slice(0, 150);
  $('#tagOptions').innerHTML = list.map(t => `<label class="tagoption"><input type="checkbox" data-t="${esc(t.tag)}" ${relState.tags.includes(t.tag) ? 'checked' : ''}> ${esc(t.tag)}<span class="c">${t.count}</span></label>`).join('') || '<div class="muted small" style="padding:6px">No tags yet</div>';
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
  await queryReleases();
}

let relTimer;
function renderPager(el, page, pages) {
  if (pages <= 1) { el.innerHTML = ''; return; }
  const btn = (p, label, disabled, on) => `<button data-p="${p}" ${disabled ? 'disabled' : ''} class="${on ? 'on' : ''}">${label}</button>`;
  let html = btn(page - 1, '\u2039 Prev', page <= 1, false);
  const win = 2;
  for (let p = 1; p <= pages; p++) {
    if (p === 1 || p === pages || Math.abs(p - page) <= win) html += btn(p, p, false, p === page);
    else if (Math.abs(p - page) === win + 1) html += '<span class="muted">&hellip;</span>';
  }
  html += btn(page + 1, 'Next \u203a', page >= pages, false);
  el.innerHTML = html;
  el.onclick = e => { const b = e.target.closest('button[data-p]'); if (!b || b.disabled) return; relState.page = Number(b.dataset.p); queryReleases(); window.scrollTo({ top: 0, behavior: 'smooth' }); };
}

async function queryReleases() {
  const p = new URLSearchParams({
    q: relState.q, tags: relState.tags.join(','), engine: relState.engine, sort: relState.sort,
    failed: relState.failed ? '1' : '0', page: relState.page, page_size: 60,
  });
  const r = await api('/api/releases?' + p);
  $('#relCount').textContent = `${r.total} release${r.total === 1 ? '' : 's'}${r.pages > 1 ? ` · page ${r.page}/${r.pages}` : ''}`;
  $('#relGrid').innerHTML = r.items.length ? r.items.map(x => `
    <div class="tile" data-link="${esc(x.link)}">
      <div class="cv" style="${x.cover ? `background-image:url('${esc(x.cover)}')` : ''}">
        ${x.error ? '<span class="bad">FAILED</span>' : ''}${x.images ? `<span class="n">${x.images} img</span>` : ''}
      </div>
      <div class="tb"><div class="tt">${esc(x.title.replace(/^(\[[^\]]*\]\s*)+/, '') || x.title)}</div>
      <div class="tm">${x.labels.map(labelHTML).join('')}${engineHTML(x.engine)}${versionHTML(x.version)}<div>${ago(x.pub)}</div></div></div>
    </div>`).join('') : '<div class="empty">No releases match. Run a scrape, backfill, or adjust the filters.</div>';
  renderPager($('#pagerTop'), r.page, r.pages);
  renderPager($('#pagerBottom'), r.page, r.pages);
}
$('#relSearch').oninput = () => { clearTimeout(relTimer); relTimer = setTimeout(() => { relState.q = $('#relSearch').value; relState.page = 1; persistFilter(); queryReleases(); }, 250); };
['relEngine', 'relSort', 'relFailed'].forEach(id => $('#' + id).onchange = () => {
  relState.engine = $('#relEngine').value; relState.sort = $('#relSort').value; relState.failed = $('#relFailed').checked; relState.page = 1;
  persistFilter(); queryReleases();
});
$('#relGrid').onclick = e => { const t = e.target.closest('.tile'); if (t) openRelease(t.dataset.link); };

function detailHTML(r, d) {
  return `
    <div class="btnrow" style="margin:0 0 10px"><a class="btn primary" href="${esc(r.link)}" target="_blank" rel="noopener noreferrer">Open thread</a>
      <button class="btn" id="mReenrich">Re-scrape this thread</button></div>
    <div class="kv">
      <span class="k">Published</span><span class="v">${clock(r.pub_date_iso)}</span>
      <span class="k">Enriched</span><span class="v">${r.enriched_at ? clock(r.enriched_at) : 'unknown'}</span>
      <span class="k">Engine</span><span class="v">${esc(r.engine) || '-'}</span>
      <span class="k">Version</span><span class="v">${esc(r.version) || '-'}</span>
      ${r.developer ? `<span class="k">Developer</span><span class="v">${esc(r.developer)}</span>` : ''}
      ${r.thread_updated ? `<span class="k">Thread updated</span><span class="v">${esc(r.thread_updated)}</span>` : ''}
      <span class="k">Description</span><span class="v">${r.extra_description.length} chars</span>
      <span class="k">Images</span><span class="v">${d.images.length}</span>
    </div>
    ${r.enrich_error ? `<div class="banner" style="margin-top:12px">Last enrichment error: ${esc(r.enrich_error)}</div>` : ''}
    <div style="margin-top:10px">${r.labels.map(labelHTML).join('')}${engineHTML(r.engine)}</div>
    ${r.tags && r.tags.length ? `<div style="margin-top:8px">${r.tags.map(tagChipHTML).join('')}</div>` : ''}
    <h4>Feed rendering</h4><iframe class="pv" sandbox="" id="mFrame"></iframe>
    ${d.images.length ? `<h4>Images</h4><div class="shots">${d.images.map(u => `<img loading="lazy" src="${esc(u)}" data-full="${esc(u)}">`).join('')}</div>` : ''}`;
}

async function openRelease(link) {
  const d = await api('/api/release?link=' + encodeURIComponent(link)).catch(e => toast(e.message, true)); if (!d) return;
  const r = d.release;
  $('#mTitle').textContent = r.title;
  const doc = `<!doctype html><meta charset=utf-8><base target=_blank><body style="margin:12px;background:#111;color:#ddd;font:14px/1.5 sans-serif">${d.feed_html}`;
  $('#mBody').innerHTML = detailHTML(r, d);
  $('#mFrame').srcdoc = doc;
  $('#mReenrich').onclick = async () => { const ok = await act(() => api('/api/reenrich', { method: 'POST', body: { link } }), 'Re-scrape started'); if (ok) closeModal(); };
  $('#modal').hidden = false;
}
const closeModal = () => { $('#modal').hidden = true; $('#mBody').innerHTML = ''; };
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
  if (e.key !== 'Escape') return;
  if (!$('#lightbox').hidden) { $('#lightbox').hidden = true; $('#lightboxImg').src = ''; return; }
  if (!$('#modal').hidden) closeModal();
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
  try { await api('/api/config', { method: 'PUT', body }); m.textContent = 'Saved.'; m.style.color = 'var(--ok)'; toast('Settings saved'); refresh(); }
  catch (err) { m.textContent = err.message; m.style.color = 'var(--err)'; }
};
$('#btnPushoverTest').onclick = async () => {
  const m = $('#pushMsg');
  try { await api('/api/pushover/test', { method: 'POST' }); m.textContent = 'Sent - check your device.'; m.style.color = 'var(--ok)'; }
  catch (err) { m.textContent = err.message; m.style.color = 'var(--err)'; }
};

// ── account ──
$('#btnLogout').onclick = async () => { try { await api('/api/auth/logout', { method: 'POST' }); } catch {} location.href = '/login'; };
$('#pwForm').onsubmit = async e => {
  e.preventDefault(); const f = e.target, m = $('#pwMsg');
  if (f.new.value !== f.confirm.value) { m.textContent = 'Passwords do not match'; m.style.color = 'var(--err)'; return; }
  try { await api('/api/account/password', { method: 'POST', body: { current: f.current.value, new: f.new.value } }); f.reset(); m.textContent = 'Password changed.'; m.style.color = 'var(--ok)'; toast('Password changed'); }
  catch (err) { m.textContent = err.message; m.style.color = 'var(--err)'; }
};

// ── boot ──
setInterval(() => { $('#hdrClock').textContent = new Date().toLocaleTimeString([], { hour12: false }); }, 1000);
setInterval(refresh, 2000);
(async () => {
  try { (await api('/api/logs?limit=500')).forEach(e => { if (e.id > lastId) { lastId = e.id; logs.push(e); } }); } catch {}
  try { const me = await (await fetch('/api/auth/status')).json(); $('#hdrUser').textContent = me.username || ''; $('#acctUser').textContent = me.username ? 'signed in as ' + me.username : ''; } catch {}
  renderMini(); connectLog(); route();
})();
})();
