const BIG = 500e6; // whole file lives in phone memory as a Blob before sharing

const $ = (s) => document.querySelector(s);
const status = $('#status'), main = $('#main');
// Everything user-visible goes through textContent: titles come from the web.
function el(tag, props = {}, ...kids) {
  const e = document.createElement(tag);
  Object.assign(e, props);
  e.append(...kids);
  return e;
}
const mb = (n) => n >= 1e9 ? (n / 1e9).toFixed(1) + ' GB' : (n / 1e6).toFixed(1) + ' MB';
const mmss = (s) => { s = Math.floor(s); return Math.floor(s / 60) + ':' + String(s % 60).padStart(2, '0'); };
const isURL = (v) => { try { new URL(v); return true; } catch { return false; } };

// Firefox for Android can't share files at all; ask once, up front, so it
// gets a Download link instead of first pulling the whole video into memory.
const canShareFiles = (() => {
  try { return !!(navigator.canShare && navigator.canShare({ files: [new File([], 'x.mp4', { type: 'video/mp4' })] })); }
  catch { return false; }
})();

let gen = 0;      // bumps on every navigation; stale pollers check it and stop
let pollTimer = null;
let me = null;    // GET /api/me, loaded once per page load

const SITES = ['youtube', 'reddit', 'instagram', 'tiktok', 'facebook', 'twitter']; // sites with a cookie slot

async function api(path, opts) {
  const r = await fetch('/api' + path, opts);
  if (r.status === 401) {
    // Not logged in (never happens with AUTH=none). Come back to this very
    // page, shared link included, once the login is done.
    location.href = '/auth/login?return=' + encodeURIComponent(location.pathname + location.search);
    return new Promise(() => {});
  }
  let body = null;
  try { body = await r.json(); } catch {}
  if (!r.ok && !(body && body.state)) {
    const e = new Error((body && body.error) || 'HTTP ' + r.status);
    e.status = r.status;
    throw e;
  }
  return body;
}
const jsonOpts = (method, data) => ({ method, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(data) });

function go(path) { history.pushState(null, '', path); route(); }
function navLink(href, text) {
  const a = el('a', { href, textContent: text });
  a.onclick = (e) => { e.preventDefault(); go(href); };
  return a;
}

function renderNav() {
  const nav = $('#nav');
  nav.replaceChildren(navLink('/?settings', 'Settings'));
  if (me && me.auth === 'oidc') nav.append(el('a', { href: '/auth/logout', textContent: 'Log out' }));
}

// Android puts a shared link in `text` at least as often as in `url`, often
// with words around it -- reading only `url` is why share targets appear to do nothing.
function sharedLink(p) {
  for (const k of ['url', 'text', 'title']) {
    const v = (p.get(k) || '').trim();
    if (isURL(v)) return v;
    const m = v.match(/https?:\/\/[^\s<>"]+/);
    if (m) return m[0];
  }
  return '';
}

async function send(url, target) {
  const g = ++gen;
  status.textContent = '';
  // Say what is happening straight away, before the server answers.
  renderProgress({ url, target, source: '' }, 'Sending…', null);
  try {
    const r = await api('/downloads', jsonOpts('POST', { url, target }));
    if (g !== gen) return;
    if (r.existing) status.textContent = 'Already downloaded';
    // replaceState: a refresh must not re-submit the shared link.
    history.replaceState(null, '', '/?item=' + r.id);
    watch(r.id, Date.now(), g, !r.existing);
  } catch (e) {
    if (g === gen) renderNote('✗ ' + e.message, true);
  }
}

// The screen shown after a share until the file is ready: what was shared,
// which stage it is in, and whether it is fine to close the app.
function renderProgress(it, stage, pct) {
  const short = (it.url || '').replace(/^https?:\/\//, '');
  const where = [it.source, short.length > 40 ? short.slice(0, 40) + '…' : short].filter(Boolean).join(' · ');
  const kids = [el('h2', { textContent: '⬇ Got it — downloading' }),
    el('p', { className: 'meta', textContent: where })];
  const bar = el('progress', { className: 'stage', max: 100 });
  if (pct != null) bar.value = pct; // no value = indeterminate
  kids.push(bar, el('p', { className: 'stage-text', textContent: stage }));
  const label = targetLabel(it.target);
  if (label && !stage.startsWith('Fitting')) kids.push(el('p', { className: 'meta', textContent: 'Then: fit for ' + label }));
  kids.push(el('p', { className: 'meta hint', textContent: me && me.notify && me.notify.set
    ? 'You can close this — you’ll get a notification when it’s ready.'
    : 'You can close this — it’ll be in your Recent list.' }));
  main.replaceChildren(el('div', { className: 'progress-card' }, ...kids));
}

// Label of a conversion target, or '' for "Download only".
function targetLabel(name) {
  if (!name || name === 'original') return '';
  const t = ((me && me.targets) || []).find(x => x.name === name);
  return t ? t.label : name;
}

function renderTargets() {
  const box = $('#targets');
  box.replaceChildren();
  for (const t of me.targets || []) {
    const b = el('button', { type: 'button', textContent: t.label });
    if (t.name === me.defaultTarget) b.className = 'default';
    b.onclick = () => {
      const u = $('#u').value.trim();
      if (u && $('#f').reportValidity()) send(u, t.name);
    };
    box.append(b);
  }
}

// fromShare: opened by a share or the paste box, so show the progress screen
// (including the target conversion) rather than an item opened from Recent.
async function watch(id, started, g, fromShare) {
  clearTimeout(pollTimer);
  if (g !== gen) return;
  const again = () => { pollTimer = setTimeout(() => watch(id, started, g, fromShare), 2000); };
  let it;
  try {
    it = await api('/items/' + id);
  } catch (e) {
    renderNote(e.status === 404 ? 'Not found.' : '✗ ' + e.message, true);
    return;
  }
  if (g !== gen) return;
  if (it.status === 'waiting' || it.status === 'queued') {
    renderProgress(it, 'Waiting for the downloader…', null);
    again();
    return;
  }
  if (it.status === 'downloading') {
    const pct = Math.round(it.percent || 0);
    renderProgress(it, 'Downloading ' + pct + '%', pct);
    again();
    return;
  }
  // The server can report "finished" before the file is visible over NFS. For a
  // download queued in the last 30 min, wait for it rather than showing a
  // dead "File missing" card. (timestamp is nanoseconds since the epoch.)
  const recent = it.timestamp && Date.now() - it.timestamp / 1e6 < 30 * 60e3;
  if (it.status === 'finished' && !it.exists && recent && Date.now() - started < 60000) {
    if (fromShare) renderProgress(it, 'Finishing…', 100);
    else renderNote((it.title || 'Download') + ' — finishing…');
    again();
    return;
  }
  // After a share with a target, stay on the progress screen while the server
  // fits the file for that app ("convert" = not picked up yet, briefly).
  const tp = fromShare && it.status === 'finished' && (it.presets || []).find(p => p.name === it.target);
  if (tp && (tp.state === 'queued' || tp.state === 'converting' || (tp.state === 'convert' && Date.now() - started < 120000))) {
    const pct = tp.state === 'converting' ? Math.round(tp.percent || 0) : null;
    renderProgress(it, 'Fitting for ' + tp.label + (pct != null ? ' ' + pct + '%' : '…'), pct);
    again();
    return;
  }
  renderCard(it, g);
}

function backLink() {
  const a = el('a', { href: '/', textContent: '← Recent' });
  a.onclick = (e) => { e.preventDefault(); go('/'); };
  return a;
}

function renderNote(text, bad) {
  main.replaceChildren(backLink(), el('p', { className: bad ? 'banner' : 'meta', textContent: text }));
}

async function renderRecent(g) {
  const list = el('ul', { className: 'recent' });
  main.replaceChildren(el('h2', { textContent: 'Recent' }), list);
  try {
    const items = await api('/items?limit=50');
    if (g !== gen) return;
    if (!items.length) list.append(el('li', { textContent: 'Nothing downloaded yet.' }));
    for (const it of items) {
      const a = el('a', { href: '/?item=' + it.id, textContent: it.title || it.url });
      a.onclick = (e) => { e.preventDefault(); go('/?item=' + it.id); };
      const state = it.status === 'finished' ? (it.exists ? mb(it.size) : 'file missing') : it.status;
      const meta = [it.source, state, it.owner].filter(Boolean).join(' · ');
      const kids = [a, el('span', { className: 'meta', textContent: meta })];
      if (it.public) kids.push(el('span', { className: 'badge', textContent: 'public' }));
      list.append(el('li', {}, ...kids));
    }
  } catch (e) {
    list.append(el('li', { className: 'banner', textContent: '✗ ' + e.message }));
  }
}

function renderCard(it, g) {
  const link = el('input', { type: 'checkbox', id: 'link' }); // off by default
  const meta = [it.source, it.owner, it.duration ? mmss(it.duration) : '', it.size ? mb(it.size) : ''].filter(Boolean).join(' · ');
  const kids = [backLink(), el('h2', { textContent: it.title || it.url }), el('p', { className: 'meta', textContent: meta })];
  if (it.status === 'error') {
    kids.push(el('p', { className: 'banner', textContent: 'Download failed: ' + (it.error || 'unknown error') }));
    if (it.needsCookies && SITES.includes(it.source)) {
      const site = it.source;
      kids.push(el('p', {}, navLink('/?settings#cookies-' + site, 'Add ' + site + ' cookies')));
    }
  } else {
    kids.push(el('label', { className: 'toggle' }, link, 'Include link'));
    for (const p of it.presets || []) kids.push(presetRow(it, p, link, g));
  }
  if (it.mine || (me && me.admin)) kids.push(ownerControls(it));
  main.replaceChildren(...kids);
}


// Public switch and Delete: owner or admin only (the server enforces it too).
function ownerControls(it) {
  const pub = el('input', { type: 'checkbox', checked: !!it.public, disabled: it.status !== 'finished' });
  const msg = el('p', { className: 'banner', hidden: true });
  pub.onchange = async () => {
    msg.hidden = true;
    try { await api(`/items/${it.id}/public`, jsonOpts('POST', { public: pub.checked })); }
    catch (e) { pub.checked = !pub.checked; msg.textContent = '✗ ' + e.message; msg.hidden = false; }
  };
  const del = el('button', { className: 'danger', textContent: 'Delete' });
  del.onclick = async () => {
    if (!confirm('Delete this download and its file?')) return;
    try { await api('/items/' + it.id, { method: 'DELETE' }); go('/'); }
    catch (e) { msg.textContent = '✗ ' + e.message; msg.hidden = false; }
  };
  return el('div', { className: 'owner' }, el('label', { className: 'toggle' }, pub, 'Public'), del, msg);
}

// Chrome on Android refuses to share a file whose name looks like a path
// trick: "He Impressed Frieza....mp4" got NotAllowedError ("Permission
// denied") while "video.mp4" with the same bytes shared fine (/diag.html).
// Mirrors the server's contentDisposition() cleanup.
function fileName(it) {
  const base = (it.title || '')
    .replace(/[\\/:*?"<>|\u0000-\u001f]+/g, ' ')
    .replace(/\.{2,}/g, '.')
    .replace(/\s+/g, ' ')
    .slice(0, 80)
    .replace(/[.\s]+$/, '')
    .trim();
  return (base || 'video') + '.mp4';
}

function presetRow(it, p, link, g) {
  const info = el('span', { className: 'info' });
  const btn = el('button');
  const row = el('div', { className: 'preset' }, el('span', { textContent: p.label }), info, btn);
  const idle = (label, text, onclick) => { btn.textContent = label; info.textContent = text; btn.disabled = !onclick; btn.onclick = onclick; };
  // The fallback: a plain link the browser's download manager saves (server
  // sends the filename), then share it from Downloads with Android's own sheet.
  let dl = null;
  function offerDownload(text) {
    info.textContent = text;
    if (!dl) {
      dl = el('a', { className: 'download', textContent: 'Download',
        href: `/api/items/${it.id}/file?preset=${p.name}&download=1` });
      btn.replaceWith(dl);
    }
  }

  function show(v) {
    p = v;
    switch (v.state) {
      case 'ready':
        if (canShareFiles) idle('Get', mb(v.estBytes), () => fetchFile());
        else offerDownload(mb(v.estBytes) + ' · this browser can’t share files');
        break;
      case 'convert': idle('Convert', '≈ ' + mb(v.estBytes), () => prepare()); break;
      case 'queued': idle('…', 'Queued', null); poll(); break;
      case 'converting': idle('…', 'Converting ' + Math.round(v.percent || 0) + '%', null); poll(); break;
      case 'too_long': idle('—', 'Too long for ' + v.label + ' (max ≈ ' + mmss(v.maxDuration) + ')', null); break;
      case 'failed': idle('Retry', 'Failed: ' + (v.error || '').slice(-200), () => prepare()); break;
      default: idle('—', 'File missing', null);
    }
  }
  async function prepare() {
    btn.disabled = true;
    try { show(await api(`/items/${it.id}/prepare?preset=${p.name}`, { method: 'POST' })); }
    catch (e) { info.textContent = '✗ ' + e.message; btn.disabled = false; }
  }
  // The conversion runs on the server regardless; a failed poll (MeTube
  // blip, re-queued URL) must back off and retry, not freeze the row.
  function poll(delay = 1500) {
    setTimeout(async () => {
      if (g !== gen) return;
      try {
        const d = await api('/items/' + it.id);
        const v = (d.presets || []).find(x => x.name === p.name);
        if (!v) throw new Error('item changed');
        show(v);
        if (v.state === 'ready' && canShareFiles) fetchFile();
      } catch (e) {
        info.textContent = 'Retrying… (' + e.message + ')';
        poll(Math.min(delay * 2, 15000));
      }
    }, delay);
  }
  // Two taps, unavoidably: navigator.share() needs a FRESH tap, and fetching
  // a large file takes longer than a tap stays valid. Tap 1 loads, tap 2 shares.
  async function fetchFile() {
    if (p.estBytes > BIG && !confirm(`This file is ${mb(p.estBytes)}. Loading it uses that much phone memory. Continue?`)) return;
    btn.disabled = true;
    try {
      const r = await fetch(`/api/items/${it.id}/file?preset=${p.name}`);
      if (!r.ok) throw new Error('HTTP ' + r.status);
      const total = +r.headers.get('Content-Length') || p.estBytes || 1;
      const reader = r.body.getReader();
      const chunks = [];
      let got = 0;
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        chunks.push(value);
        got += value.length;
        info.textContent = 'Loading ' + Math.min(100, Math.round(got * 100 / total)) + '%';
      }
      const file = new File(chunks, fileName(it), { type: 'video/mp4' });
      if (!navigator.canShare || !navigator.canShare({ files: [file] })) {
        offerDownload(mb(file.size) + ' · this browser can’t share this file');
        return;
      }
      idle('Share', mb(file.size), async () => {
        const data = { files: [file] };
        if (link.checked) data.text = it.url;
        try { await navigator.share(data); }
        catch (e) {
          // Chrome rejects shares over its file-size limit with NotAllowedError
          // ("Permission denied") -- the file is fine, the share sheet isn't.
          if (e.name === 'NotAllowedError') offerDownload(mb(file.size) + ' · too big to share directly — download it, then share from Downloads');
          else if (e.name !== 'AbortError') info.textContent = '✗ ' + e.message;
        }
      });
    } catch (e) {
      info.textContent = '✗ ' + e.message;
      btn.disabled = false;
    }
  }
  show(p);
  return row;
}

// ---- Settings page (/?settings) ----

function section(title, ...kids) { return el('section', {}, el('h2', { textContent: title }), ...kids); }

// Runs fn(); shows its failure in `out` instead of throwing.
async function attempt(out, fn) {
  out.textContent = '';
  try { await fn(); } catch (e) { out.textContent = '✗ ' + e.message; }
}

// The folder name is what the owner passes to `grab migrate --user`.
function accountSection() {
  return section('Account', el('p', { className: 'meta' },
    'Signed in as ', el('strong', { textContent: me.name }),
    '. Your private files are in the folder users/', el('code', { id: 'user-dir', textContent: me.dir }), '/ on the server.'));
}

function targetSection() {
  const sel = el('select', { id: 'default-target' });
  for (const t of me.targets) sel.append(el('option', { value: t.name, textContent: t.label, selected: t.name === me.defaultTarget }));
  const out = el('span', { className: 'meta' });
  sel.onchange = () => attempt(out, async () => {
    await api('/me', jsonOpts('PUT', { defaultTarget: sel.value }));
    me.defaultTarget = sel.value;
    out.textContent = 'Saved ✓';
  });
  return section('Default target', sel, out);
}

function notifySection() {
  if (me.auth !== 'oidc') return section('Notifications', el('p', { className: 'meta', textContent: 'Set APPRISE_URLS on the server' }));
  // Write-only: the saved URLs are secrets and never come back.
  const ta = el('textarea', { rows: 3, placeholder: 'One service://… URL per line' });
  const out = el('span', { className: 'meta' });
  const show = () => { out.textContent = me.notify.set ? 'Saved ✓ (' + me.notify.count + ')' : 'Not set'; };
  show();
  const save = el('button', { textContent: 'Save' });
  save.onclick = () => attempt(out, async () => {
    const urls = ta.value.split('\n').map(s => s.trim()).filter(Boolean);
    await api('/me/notify', jsonOpts('PUT', { urls }));
    me.notify = { set: urls.length > 0, count: urls.length };
    ta.value = '';
    show();
  });
  const clear = el('button', { textContent: 'Clear' });
  clear.onclick = () => attempt(out, async () => {
    await api('/me/notify', { method: 'DELETE' });
    me.notify = { set: false, count: 0 };
    show();
  });
  const test = el('button', { textContent: 'Send test' });
  test.onclick = () => attempt(out, async () => { await api('/me/notify/test', { method: 'POST' }); out.textContent = 'Test sent ✓'; });
  return section('Notifications', ta, el('div', { className: 'row' }, save, clear, test), out);
}

async function tokenSection() {
  const list = el('ul', { className: 'plain' });
  const out = el('p', { className: 'meta' });
  const shown = el('div', { className: 'once' });
  async function load() {
    const ts = await api('/tokens');
    list.replaceChildren();
    if (!ts.length) list.append(el('li', { className: 'meta', textContent: 'No tokens yet.' }));
    for (const t of ts) {
      const rev = el('button', { textContent: 'Revoke' });
      rev.onclick = () => attempt(out, async () => { await api('/tokens/' + t.id, { method: 'DELETE' }); await load(); });
      list.append(el('li', {}, el('span', { textContent: t.label }), rev));
    }
  }
  const label = el('input', { type: 'text', id: 'token-label', placeholder: 'Label, e.g. phone shortcut', maxLength: 64 });
  const create = el('button', { textContent: 'Create token' });
  create.onclick = () => attempt(out, async () => {
    if (!label.value.trim()) throw new Error('Give the token a label');
    const t = await api('/tokens', jsonOpts('POST', { label: label.value.trim() }));
    label.value = '';
    // The plaintext exists only in this response; it is never shown again.
    const copy = el('button', { textContent: 'Copy' });
    copy.onclick = async () => {
      try { await navigator.clipboard.writeText(t.token); copy.textContent = 'Copied ✓'; }
      catch { copy.textContent = 'Copy failed — select it by hand'; }
    };
    shown.replaceChildren(el('p', { className: 'meta', textContent: 'Copy this token now — it will not be shown again.' }),
      el('code', { id: 'new-token', textContent: t.token }), copy);
    await load();
  });
  await load();
  return section('API tokens', list, label, create, out, shown);
}

async function cookieSection() {
  const have = await api('/cookies');
  const out = el('p', { className: 'meta' });
  const strong = (t) => el('strong', { textContent: t });
  const ext = (href, t) => el('a', { href, textContent: t, target: '_blank', rel: 'noopener' });
  const help = el('p', { className: 'help' },
    'Some posts need you to be logged in. Export cookies with a "cookies.txt" browser extension, from a ',
    strong('private window'),
    ' (log in there, export, then close the window — YouTube changes the cookies of windows you keep open). ' +
    'Cookies expire; if downloads start failing again, export fresh ones. ' +
    'They give grab your login, so treat them like a password — a spare account is safer. Guides: ',
    ext('https://github.com/yt-dlp/yt-dlp/wiki/FAQ#how-do-i-pass-cookies-to-yt-dlp', 'yt-dlp: exporting cookies'),
    ' · ',
    ext('https://github.com/alexta69/metube/wiki/Using-browser-cookies', 'MeTube: using browser cookies'));
  const rows = SITES.map((site) => {
    const info = el('span', { className: 'meta' });
    const file = el('input', { type: 'file', accept: '.txt,text/plain' });
    const del = el('button', { textContent: 'Delete' });
    const set = (u) => {
      info.textContent = u ? 'Uploaded ' + new Date(u).toLocaleDateString() : 'Not set';
      del.hidden = !u;
    };
    set(have[site] && have[site].updated);
    file.onchange = () => attempt(out, async () => {
      if (!file.files.length) return;
      await api('/cookies/' + site, { method: 'PUT', headers: { 'Content-Type': 'text/plain' }, body: file.files[0] });
      file.value = '';
      set(new Date().toISOString());
    });
    del.onclick = () => attempt(out, async () => { await api('/cookies/' + site, { method: 'DELETE' }); set(null); });
    return el('div', { className: 'cookie', id: 'cookies-' + site },
      el('strong', { textContent: site }), info, file, del);
  });
  return section('Cookies', help, ...rows, out);
}

async function renderSettings(g) {
  main.replaceChildren(backLink(), el('h2', { textContent: 'Settings' }));
  // Each section appears as it is ready and fails alone: one bad request must
  // not blank the rest of the page.
  const parts = [accountSection, targetSection, notifySection, tokenSection, cookieSection];
  const slots = parts.map(() => el('div'));
  main.append(...slots);
  await Promise.all(parts.map(async (build, i) => {
    try {
      const sec = await build();
      if (g === gen) slots[i].replaceChildren(sec);
    } catch (e) {
      if (g === gen) slots[i].replaceChildren(el('p', { className: 'banner', textContent: '✗ ' + e.message }));
    }
  }));
  if (g !== gen) return;
  const t = location.hash.length > 1 && document.getElementById(location.hash.slice(1));
  if (t) { t.classList.add('target'); t.scrollIntoView(); }
}

async function route() {
  clearTimeout(pollTimer);
  const g = ++gen;
  const p = new URLSearchParams(location.search);
  if (!me) {
    try { me = await api('/me'); }
    catch (e) { status.textContent = '✗ ' + e.message; return; }
    if (g !== gen) return;
    renderNav();
  }
  renderTargets();
  const shared = sharedLink(p);
  if (shared) { $('#u').value = shared; send(shared, me.defaultTarget); return; }
  if (p.has('settings')) { status.textContent = ''; renderSettings(g); return; }
  const item = p.get('item');
  if (item && /^[0-9a-f]{16}$/.test(item)) { watch(item, Date.now(), g); return; }
  status.textContent = '';
  renderRecent(g);
}

$('#f').addEventListener('submit', (e) => {
  e.preventDefault();
  if (me) send($('#u').value.trim(), me.defaultTarget);
});
addEventListener('popstate', route);
route();

if ('serviceWorker' in navigator) { navigator.serviceWorker.register('/sw.js'); }
