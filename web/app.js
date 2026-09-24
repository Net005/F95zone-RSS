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

// F95-style prefix label colours
const TAGC = { UPDATE:'#c0392b', NEW:'#2e9e5b', 'Ren\'Py':'#b5651d', Unity:'#4a4a4a', RPGM:'#2a6fc9', HTML:'#2f8f5b', Unreal:'#333', 'Unreal Engine':'#333', VN:'#7d4fbf',
  Completed:'#1f7fb8', Abandoned:'#7a4a2a', Onhold:'#b8901f', QSP:'#555', RAGS:'#555', Java:'#a0522d', Flash:'#a02020', WebGL:'#3a7a7a', Godot:'#3c7fb0', 'Wolf RPG':'#6a4a8a', Collection:'#6a6a2a', SiteRip:'#555' };
const tagHTML = t => {
  if (/^(engine|genre|status|source|type)-/.test(t)) return '';           // derived duplicates
  const c = /^v?\d/.test(t) ? '#3a3a3a' : (TAGC[t] || '#444');
  return `<span class="tag" style="background:${c}">${esc(t)}</span>`;
};

// ── router ──
const views = ['dashboard', 'log', 'releases', 'runs', 'feed', 'settings'];
let cur = '';
function route() {
  let v = (location.hash.replace(/^#\//, '') || 'dashboard'); if (!views.includes(v)) v = 'dashboard';
  cur = v;
  views.forEach(x => $('#v-' + x).classList.toggle('on', x === v));
  $$('#tabs a').forEach(a => a.classList.toggle('on', a.dataset.v === v));
  if (v === 'releases') loadReleases();
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
  $('#hdrPill b').textContent = run ? (p.status === 'FETCHING' ? 'FETCHING' : `ENRICHING ${p.current}/${p.total}`) : p.status;

  const c = S.cache, lr = S.last_run;
  const tiles = [
    ['Cached releases', c.items, `${c.with_description} with description` + (c.failed ? ` · ${c.failed} failed` : '')],
    ['Next run', S.scheduler_enabled ? inn(S.next_run) : 'Paused', S.scheduler_enabled ? `every ${S.schedule_hours}h · ${S.next_run ? clock(S.next_run) : ''}` : 'scheduler disabled'],
    ['Last run', lr ? ago(lr.finished) : 'never', lr ? `${lr.status} · ${fmtDur(lr.duration_s)} · ${lr.items} items` : ''],
    ['Images', S.images.count, `${fmtBytes(S.images.bytes)} on disk`],
  ];
  $('#tiles').innerHTML = tiles.map(t => `<div class="stat"><div class="k">${t[0]}</div><div class="v">${esc(t[1])}</div><div class="s">${esc(t[2])}</div></div>`).join('');

  const bar = $('#bar');
  bar.style.width = (run && p.status === 'FETCHING' ? 100 : p.percent) + '%';
  bar.classList.toggle('busy', run && p.status === 'FETCHING');
  $('#barTxt').textContent = run ? (p.status === 'FETCHING' ? 'Fetching source feed...' : `${p.current} / ${p.total}  (${p.percent}%)`) : (p.status === 'DONE' ? 'Done' : p.status === 'CANCELLED' ? 'Cancelled' : p.status === 'ERROR' ? 'Error - see log' : 'Idle');
  $('#curItem').textContent = run && p.item ? '\u25B8 ' + p.item : '\u00A0';
  $('#pipeMeta').textContent = run ? `${p.trigger} run · started ${ago(p.started)}${p.failed ? ' · ' + p.failed + ' failed' : ''}` : `mode: ${S.fetch_mode} · uptime ${fmtDur(S.uptime_s)}`;
  ['btnRun', 'btnForce', 'btnFull', 'btnRebuild', 'btnRebuild2', 'btnPurge', 'btnClearCache'].forEach(id => { const b = $('#' + id); if (b) b.disabled = run; });
  $('#btnStop').disabled = !run;
  $('#btnSched').textContent = S.scheduler_enabled ? 'Pause scheduler' : 'Resume scheduler';
  $('#storageKv').innerHTML = [
    ['Images on disk', `${S.images.count} (${fmtBytes(S.images.bytes)})`],
    ['Orphaned images', `${S.images.orphans} (${fmtBytes(S.images.orphan_bytes)})`],
    ['Cache', c.mtime ? `${ago(c.mtime)} · ${c.fresh ? 'fresh' : 'stale'} (TTL ${c.ttl_hours}h)` : 'empty'],
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
$('#btnClearCache').onclick = () => { if (confirm('Clear the release cache? The next run will re-scrape everything.')) act(() => api('/api/cache/clear', { method: 'POST' }), 'Cache cleared'); };

// ── live log ──
const logs = []; let lastId = 0, paused = false, lvl = 'ALL', q = '';
const MAXLOG = 3000;
const lineHTML = (e, hl) => {
  const t = new Date(e.ts).toLocaleTimeString([], { hour12: false });
  let m = esc(e.msg);
  if (hl) m = m.replace(new RegExp(hl.replace(/[.*+?^${}()|[\]\\]/g, '\\$&').replace(/&/g,'&amp;'), 'gi'), x => `<mark>${x}</mark>`);
  const hi = /^(\[\d+\/\d+\]|Starting|Pipeline complete|Run cancelled)/.test(e.msg) ? ' hi' : '';
  return `<div class="ll ${e.level}${hi}"><span class="t">${t}</span> <span class="lv">${e.level}</span><span class="m">${m}</span></div>`;
};
const passes = e => (lvl === 'ALL' || e.level === lvl || (lvl === 'WARN' && e.level === 'ERROR')) && (!q || e.msg.toLowerCase().includes(q));
function renderLog(full) {
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

// ── releases ──
let relTimer;
async function loadReleases() {
  const tags = await api('/api/tags').catch(() => []);
  const sel = $('#relTag'), prev = sel.value;
  sel.innerHTML = '<option value="">All tags</option>' + tags.filter(t => !/^(engine|genre|status|source|type)-/.test(t.tag)).slice(0, 80).map(t => `<option value="${esc(t.tag)}">${esc(t.tag)} (${t.count})</option>`).join('');
  sel.value = prev;
  await queryReleases();
}
async function queryReleases() {
  const p = new URLSearchParams({ q: $('#relSearch').value, tag: $('#relTag').value, sort: $('#relSort').value, failed: $('#relFailed').checked ? '1' : '0' });
  const r = await api('/api/releases?' + p);
  $('#relCount').textContent = `${r.total} release${r.total === 1 ? '' : 's'}`;
  $('#relGrid').innerHTML = r.items.length ? r.items.map(x => `
    <div class="tile" data-link="${esc(x.link)}">
      <div class="cv" style="${x.cover ? `background-image:url('${esc(x.cover)}')` : ''}">
        ${x.error ? '<span class="bad">FAILED</span>' : ''}${x.images ? `<span class="n">${x.images} img</span>` : ''}
      </div>
      <div class="tb"><div class="tt">${esc(x.title.replace(/^(\[[^\]]*\]\s*)+/, '') || x.title)}</div>
      <div class="tm">${x.categories.map(tagHTML).join('')}<div>${ago(x.pub)}</div></div></div>
    </div>`).join('') : '<div class="empty">No releases match. Run a scrape or adjust the filters.</div>';
}
['relSearch'].forEach(id => $('#' + id).oninput = () => { clearTimeout(relTimer); relTimer = setTimeout(queryReleases, 200); });
['relTag', 'relSort', 'relFailed'].forEach(id => $('#' + id).onchange = queryReleases);
$('#relGrid').onclick = e => { const t = e.target.closest('.tile'); if (t) openRelease(t.dataset.link); };

async function openRelease(link) {
  const d = await api('/api/release?link=' + encodeURIComponent(link)).catch(e => toast(e.message, true)); if (!d) return;
  const r = d.release;
  $('#mTitle').textContent = r.title;
  const doc = `<!doctype html><meta charset=utf-8><base target=_blank><body style="margin:12px;background:#111;color:#ddd;font:14px/1.5 sans-serif">${d.feed_html}`;
  $('#mBody').innerHTML = `
    <div class="btnrow" style="margin:0 0 10px"><a class="btn primary" href="${esc(r.link)}" target="_blank" rel="noopener noreferrer">Open thread</a>
      <button class="btn" id="mReenrich">Re-scrape this thread</button></div>
    <div class="kv"><span class="k">Published</span><span class="v">${clock(r.pub_date_iso)}</span>
      <span class="k">Enriched</span><span class="v">${r.enriched_at ? clock(r.enriched_at) : 'unknown'}</span>
      <span class="k">Description</span><span class="v">${r.extra_description.length} chars</span>
      <span class="k">Images</span><span class="v">${d.images.length}</span></div>
    ${r.enrich_error ? `<div class="banner" style="margin-top:12px">Last enrichment error: ${esc(r.enrich_error)}</div>` : ''}
    <div style="margin-top:10px">${r.categories.map(tagHTML).join('')}</div>
    <h4>Feed rendering</h4><iframe class="pv" sandbox="" id="mFrame"></iframe>
    ${d.images.length ? `<h4>Images</h4><div class="shots">${d.images.map(u => `<a href="${esc(u)}" target="_blank" rel="noopener noreferrer"><img loading="lazy" src="${esc(u)}"></a>`).join('')}</div>` : ''}`;
  $('#mFrame').srcdoc = doc;
  $('#mReenrich').onclick = async () => { const ok = await act(() => api('/api/reenrich', { method: 'POST', body: { link } }), 'Re-scrape started'); if (ok) closeModal(); };
  $('#modal').hidden = false;
}
const closeModal = () => { $('#modal').hidden = true; $('#mBody').innerHTML = ''; };
$('#mClose').onclick = closeModal;
$('#modal').onclick = e => { if (e.target.id === 'modal') closeModal(); };
document.addEventListener('keydown', e => { if (e.key === 'Escape') closeModal(); });

// ── runs ──
async function loadRuns() {
  const h = await api('/api/history');
  $('#runsTbl').innerHTML = `<tr><th>#</th><th>Started</th><th>Trigger</th><th>Status</th><th>Duration</th><th>Items</th><th>Scraped</th><th>Reused</th><th>Failed</th><th>New images</th></tr>` +
    (h.length ? h.map(r => `<tr><td>${r.id}</td><td>${clock(r.started)}</td><td>${esc(r.trigger)}</td>
      <td class="st-${r.status}" title="${esc(r.error || '')}">${esc(r.status)}${r.error ? ' ⚠' : ''}</td><td>${fmtDur(r.duration_s)}</td><td>${r.items}</td><td>${r.enriched}</td><td>${r.reused}</td><td>${r.failed || '-'}</td><td>${r.images_new}</td></tr>`).join('')
      : '<tr><td colspan="10" class="muted" style="text-align:center;padding:30px">No runs recorded yet.</td></tr>');
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
  $('#feedKv').innerHTML = [['Items', S.cache.items], ['Size', S.feed ? fmtBytes(S.feed.size) : '-'], ['Last built', S.feed ? clock(S.feed.mtime) + ' (' + ago(S.feed.mtime) + ')' : 'never']]
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
