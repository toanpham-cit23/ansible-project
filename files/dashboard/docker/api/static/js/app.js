const state = { range: 'live', devices: [], me: null, timer: null, term: null, zoom: 1,
                logPage: 1, logLevel: 'all', logQuery: '', logFilterOpen: false };
const $ = (s) => document.querySelector(s);
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => (
  { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

function toast(msg, bad) {
  let t = $('#toast');
  if (!t) { t = document.createElement('div'); t.id = 'toast'; document.body.appendChild(t); }
  t.textContent = msg;
  t.className = 'toast show' + (bad ? ' bad' : '');
  clearTimeout(t._h);
  t._h = setTimeout(() => { t.className = 'toast'; }, 2400);
}

function applyTheme() {
  document.documentElement.dataset.theme = state.me.theme;
  document.documentElement.dataset.layout = state.me.layout;
}

function section(id, title, body, open) {
  const closed = localStorage['sec_' + id] === '1';
  return `<div class="section${closed ? ' closed' : ''}" data-sec="${id}">
    <div class="sec"><span class="chev">▾</span> ${esc(title)}</div>
    <div class="sec-body">${body}</div></div>`;
}

function confirmDialog(title, body, okText) {
  return new Promise((resolve) => {
    const o = document.createElement('div');
    o.className = 'overlay';
    o.innerHTML = `<div class="modal" role="dialog" aria-modal="true">
      <h2>${esc(title)}</h2><p>${body}</p>
      <div class="m-actions">
        <button class="act" data-no>Cancel</button>
        <button class="act primary" data-yes>${esc(okText)}</button>
      </div></div>`;
    const done = (v) => { o.remove(); resolve(v); };
    o.addEventListener('click', (e) => {
      if (e.target.closest('[data-yes]')) done(true);
      else if (e.target.closest('[data-no]') || e.target === o) done(false);
    });
    document.body.appendChild(o);
    o.querySelector('[data-yes]').focus();
  });
}

async function boot() {
  try { state.me = await api.get('/api/me'); } catch (e) { return; }
  applyTheme();
  if (localStorage.side === '1') $('#side').classList.add('collapsed');

  $('#toggle').addEventListener('click', () => {
    const c = $('#side').classList.toggle('collapsed');
    localStorage.side = c ? '1' : '0';
  });
  $('#logout').addEventListener('click', async () => {
    try { await api.post('/api/logout'); } catch (e) { /* session may already be gone */ }
    document.cookie = 'sess=; Max-Age=0; path=/';
    location.href = '/';
  });
  $('#ranges').addEventListener('click', (e) => {
    const b = e.target.closest('button');
    if (!b) return;
    state.range = b.dataset.r;
    document.querySelectorAll('#ranges button').forEach((x) => x.classList.toggle('on', x === b));
    route();
  });
  $('#view').addEventListener('click', onClick);
  $('#view').addEventListener('change', onChange);
  $('#view').addEventListener('change', onChange);
  $('#view').addEventListener('input', onInput);
  $('#view').addEventListener('keydown', onKey);
  $('#view').addEventListener('wheel', onWheel, { passive: false });
  window.addEventListener('hashchange', route);
  route();
}

async function onChange(e) {
  const sel = e.target.closest('select.log-act');
  if (!sel) return;
  try {
    await api.post('/api/logs/action', { sig: sel.dataset.sig, action: sel.value });
    toast(sel.value === 'none' ? 'Decision cleared' : 'Marked as ' + sel.value);
    route();
  } catch (x) {
    toast(x.message, true);
  }
}

async function onClick(e) {
  const sec = e.target.closest('.sec');
  if (sec) {
    const wrap = sec.closest('.section');
    const closed = wrap.classList.toggle('closed');
    localStorage['sec_' + wrap.dataset.sec] = closed ? '1' : '0';
    return;
  }

  const b = e.target.closest('button');
  if (!b) return;

  if (b.dataset.page) {
    state.logPage = Number(b.dataset.page);
    route();
    return;
  }

  if (b.dataset.level) {
    state.logLevel = b.dataset.level;
    state.logPage = 1;
    state.logFilterOpen = false;
    route();
    return;
  }

  if (b.dataset.act === 'log-filter') {
    state.logFilterOpen = !state.logFilterOpen;
    route();
    return;
  }

  if (b.dataset.act === 'log-search') {
    state.logQuery = ($('#log-q') || {}).value || '';
    state.logPage = 1;
    route();
    return;
  }

  if (b.dataset.scope) {
    state.scope = b.dataset.scope;
    route();
    return;
  }

  if (b.dataset.act === 'pw') {
    const oldPw = $('#pw-old').value, newPw = $('#pw-new').value;
    if (newPw.length < 8) { toast('New password needs 8+ characters', true); return; }
    try {
      await api.post('/api/account/password', { old: oldPw, new: newPw });
      $('#pw-old').value = ''; $('#pw-new').value = '';
      toast('Password changed');
    } catch (x) { toast(x.message, true); }
    return;
  }

  if (b.dataset.act === 'adduser') {
    const username = $('#nu-name').value.trim();
    const password = $('#nu-pass').value;
    const role = $('#nu-role').value;
    if (!username || password.length < 8) { toast('Username and 8+ char password required', true); return; }
    try {
      await api.post('/api/users', { username, password, role });
      toast('User created');
      route();
    } catch (x) { toast(x.message, true); }
    return;
  }

  if (b.dataset.act === 'deluser') {
    if (!confirm('Delete this user?')) return;
    try { await api.call('DELETE', '/api/users/' + b.dataset.uid); toast('User deleted'); route(); }
    catch (x) { toast(x.message, true); }
    return;
  }

  if (b.dataset.zoom) {
    state.zoom = b.dataset.zoom === 'reset' ? 1
      : Math.min(2.4, Math.max(0.5, state.zoom + (b.dataset.zoom === 'in' ? 0.2 : -0.2)));
    applyZoom();
    return;
  }

  if (b.dataset.set) {
    try {
      await api.put('/api/settings', { [b.dataset.set]: b.dataset.v });
      state.me = await api.get('/api/me');
      applyTheme();
      // evaluate every button against its own setting, otherwise the other
      // group (theme vs layout) gets its highlight cleared
      document.querySelectorAll('[data-set]').forEach((x) => {
        x.classList.toggle('on', x.dataset.v === state.me[x.dataset.set]);
      });
      if (b.dataset.set === 'theme') route();
      toast('Saved');
    } catch (x) { toast(x.message, true); }
    return;
  }

  if (b.dataset.act === 'term') {
    const d = state.devices.find((x) => String(x.id) === b.dataset.id);
    const ok = await confirmDialog(
      'Open remote terminal?',
      `You are about to open a remote command session to <b>${esc(d.hostname)}</b> (${esc(d.mgmt_ip)}) as <b>root</b>.<br><br>
       Only allowlisted read commands are permitted, and every command is written to the event log.`,
      'Allow session');
    if (!ok) { toast('Cancelled'); return; }
    state.term = { dev: d, history: [] };
    if (location.hash !== '#/terminal') location.hash = '#/terminal';
    route();
    return;
  }

  if (b.dataset.act === 'run') { await runCmd(); return; }
  if (b.dataset.act === 'close') { state.term = null; location.hash = '#/monitoring'; route(); return; }

  const card = b.closest('[data-id]');
  if (!card) return;
  const id = card.dataset.id;

  if (b.dataset.act === 'save') {
    const body = {};
    card.querySelectorAll('input[data-f]').forEach((i) => {
      body[i.dataset.f] = i.dataset.f.endsWith('port') ? Number(i.value) : i.value;
    });
    b.disabled = true;
    try {
      await api.patch(`/api/devices/${id}`, body);
      card.querySelectorAll('input.dirty').forEach((i) => i.classList.remove('dirty'));
      toast('Saved');
    } catch (x) { toast(x.message, true); }
    finally { b.disabled = false; }
  }

  if (b.dataset.act === 'cred') {
    const u = prompt('Username for ' + (card.dataset.host || 'device'));
    if (!u) return;
    const p = prompt('New password (write-only, cannot be read back)');
    if (!p) return;
    try { await api.put(`/api/devices/${id}/credentials`, { username: u, password: p });
          toast('Credentials stored'); }
    catch (x) { toast(x.message, true); }
  }
}

function onChange(e) {
  const el = e.target.closest('input[data-f]');
  if (el) el.classList.add('dirty');
}

function onInput(e) {
  if (e.target.id === 'tcmd') e.target.classList.remove('dirty');
}

function onKey(e) {
  if (e.target.id === 'tcmd' && e.key === 'Enter') {
    e.preventDefault();
    runCmd();
  }
}

function onWheel(e) {
  if (!e.target.closest('.topo-box')) return;
  e.preventDefault();
  state.zoom = Math.min(2.4, Math.max(0.5, state.zoom + (e.deltaY < 0 ? 0.1 : -0.1)));
  applyZoom();
}

function applyZoom() {
  const svg = document.querySelector('.topo-box svg');
  const lbl = document.querySelector('[data-zlabel]');
  if (svg) svg.style.width = (state.zoom * 100) + '%';
  if (lbl) lbl.textContent = Math.round(state.zoom * 100) + '%';
}

async function runCmd() {
  const inp = $('#tcmd');
  const cmd = (inp.value || '').trim();
  if (!cmd || !state.term) return;
  inp.value = '';
  const t = state.term;
  const entry = { cmd, out: 'running…', err: '' };
  t.history.push(entry);
  renderTerm();
  try {
    const r = await api.post(`/api/terminal/${t.dev.id}/exec`, { cmd });
    entry.out = r.output || '(no output)';
  } catch (x) {
    entry.err = x.message;
    entry.out = '';
  }
  renderTerm();
}

function renderTerm() {
  const t = state.term;
  const out = $('#tout');
  if (!out) return;
  const body = t.history.map((h) => (
    `<div class="cmd">${esc(t.dev.hostname)}# ${esc(h.cmd)}</div>` +
    (h.err ? `<div class="bad">error: ${esc(h.err)}</div>`
           : `<pre style="margin:0 0 10px;white-space:pre-wrap;font:inherit">${esc(h.out)}</pre>`)
  )).join('');
  out.innerHTML = body + `<div class="cmd">${esc(t.dev.hostname)}# <span class="cursor"></span></div>`;
  out.scrollTop = out.scrollHeight;
}

const toSec = (v) => {
  const m = String(v).match(/(\d{4})-(\d\d)-(\d\d) (\d\d):(\d\d)(?::(\d\d))?/);
  if (!m) return 0;
  return Date.UTC(+m[1], +m[2] - 1, +m[3], +m[4], +m[5], +(m[6] || 0)) / 1000;
};

const avg = (xs) => {
  const v = xs.filter((x) => typeof x === 'number' && isFinite(x));
  return v.length ? v.reduce((a, b) => a + b, 0) / v.length : null;
};

const views = {
  async dashboard() {
    const devs = await api.get('/api/devices');
    state.devices = devs;
    for (const d of devs) d._h = await api.get(`/api/devices/${d.id}/health?range=${state.range}`);

    if (state.scope !== 'all' && !devs.some((d) => String(d.id) === String(state.scope))) {
      state.scope = 'all';
    }
    const scoped = state.scope === 'all'
      ? devs : devs.filter((d) => String(d.id) === String(state.scope));

    const last = (d, k) => { const h = d._h[d._h.length - 1]; return h ? h[k] : null; };
    const cpu = avg(scoped.map((d) => last(d, 'cpu_pct')));
    const mem = avg(scoped.map((d) => last(d, 'mem_pct')));
    const dsk = avg(scoped.map((d) => last(d, 'disk_pct')));
    const up = devs.filter((d) => d.last_state === 'up').length;
    const down = devs.length - up;

    const tsSet = new Set();
    scoped.forEach((d) => d._h.forEach((r) => tsSet.add(String(r.ts).slice(0, 16))));
    const labels = [...tsSet].sort();

    const series = (key) => scoped.map((d, i) => {
      const pts = d._h.map((r) => [toSec(r.ts), r[key]]).filter((q) => q[0]);
      const values = labels.map((t) => {
        const T = toSec(t);
        let best = null, bd = Infinity;
        for (const [ts, v] of pts) {
          const dd = Math.abs(ts - T);
          if (dd < bd) { bd = dd; best = v; }
        }
        return bd <= 90 ? best : null;
      });
      return { name: d.hostname, color: PALETTE[i % PALETTE.length], values };
    });

    const scopeRow = `<div class="row scope">
      <span class="muted" style="font-size:12.5px;margin-right:4px">Scope</span>
      <button class="act ${state.scope === 'all' ? 'on' : ''}" data-scope="all">All</button>
      ${devs.map((d) => `<button class="act ${String(state.scope) === String(d.id) ? 'on' : ''}" data-scope="${d.id}">${esc(d.hostname)}</button>`).join('')}
    </div>`;

    const cluster = scopeRow + `
      <div class="grid3">
        <div class="panel"><div class="p-title">CPU</div><div id="g-cpu"></div></div>
        <div class="panel"><div class="p-title">Memory</div><div id="g-mem"></div></div>
        <div class="panel"><div class="p-title">Disk</div><div id="g-dsk"></div></div>
      </div>
      <div class="grid3 capacity" style="margin-top:9px">
        <div class="panel"><div class="p-title">Network received</div>
          <div class="chart-wrap"><canvas class="chart" id="c-in"></canvas></div><div class="legend" id="l-in"></div></div>
        <div class="panel"><div class="p-title">Network sent</div>
          <div class="chart-wrap"><canvas class="chart" id="c-out"></canvas></div><div class="legend" id="l-out"></div></div>
        <div class="panel"><div class="p-title">CPU per device</div>
          <div class="chart-wrap"><canvas class="chart" id="c-cpu"></canvas></div><div class="legend" id="l-cpu"></div></div>
      </div>`;

    const cards = devs.map((d) => {
      const c = last(d, 'cpu_pct'), m = last(d, 'mem_pct'), k = last(d, 'disk_pct');
      return `<div class="dev-card" data-id="${d.id}">
        <div class="dev-head"><b>${esc(d.hostname)}</b>
          <span class="pill ${d.last_state === 'up' ? 'up' : 'down'}">${esc(d.last_state || 'unknown')}</span></div>
        <canvas class="mini" data-dev="${d.id}"></canvas>
        <div class="dev-nums">
          <span>CPU <b>${c == null ? '–' : c + '%'}</b></span>
          <span>MEM <b>${m == null ? '–' : m + '%'}</b></span>
          <span>DSK <b>${k == null ? '–' : k + '%'}</b></span>
        </div>
      </div>`;
    }).join('');

    const devices = `
      <div class="stat-grid">
        <div class="panel"><div class="p-title">Number of devices</div><div class="p-big">${devs.length}</div></div>
        <div class="panel ${down ? 'err' : 'ok'}"><div class="p-title">Unavailable</div><div class="p-big">${down}</div></div>
      </div>
      <div class="dev-grid">${cards}</div>`;

    $('#view').innerHTML =
      section('cluster', 'Cluster', cluster, true) +
      section('devices', 'Devices', devices, true);

    drawGauge($('#g-cpu'), cpu, 'avg cpu');
    drawGauge($('#g-mem'), mem, 'avg memory');
    drawGauge($('#g-dsk'), dsk, 'avg disk');

    const mk = (cid, lid, key, unit) => {
      const s = series(key);
      drawChart($(cid), s, labels, { unit, min: 0 });
      $(lid).innerHTML = s.map((x) => `<span><i style="background:${x.color}"></i>${esc(x.name)}</span>`).join('');
    };
    mk('#c-in', '#l-in', 'net_in_bps', ' bps');
    mk('#c-out', '#l-out', 'net_out_bps', ' bps');
    mk('#c-cpu', '#l-cpu', 'cpu_pct', '%');

    for (const d of devs) {
      const c = document.querySelector(`canvas.mini[data-dev="${d.id}"]`);
      if (c) drawChart(c, [{ name: 'cpu', color: PALETTE[0], values: d._h.map((r) => r.cpu_pct) }],
        d._h.map((r) => String(r.ts).slice(0, 16)), { unit: '%', min: 0 });
    }
  },

  async monitoring() {
    const t = await api.get('/api/topology');
    const ctrl = t.nodes.find((n) => n.effective_kind === 'server');
    const rest = t.nodes.filter((n) => n !== ctrl);

    const W = 640, H = 300;
    const cx = W / 2, cy = 74, R = 138;
    const pos = {};
    if (ctrl) pos[ctrl.id] = [cx, cy];
    rest.forEach((n, i) => {
      const a = (35 + (110 / Math.max(1, rest.length - 1)) * i) * Math.PI / 180;
      pos[n.id] = [cx + R * Math.cos(a), cy + R * Math.sin(a)];
    });

    let svg = `<svg viewBox="0 0 ${W} ${H}" style="width:${state.zoom * 100}%" role="img" aria-label="Topology">`;
    for (const l of t.links) {
      const a = pos[l.a_device_id], b = pos[l.b_device_id];
      if (!a || !b) continue;
      const mx = (a[0] + b[0]) / 2 + (b[0] - a[0]) * 0.10;
      const my = (a[1] + b[1]) / 2 + 14;
      const d = `M ${a[0]} ${a[1]} Q ${mx} ${my} ${b[0]} ${b[1]}`;
      const toCtrl = ctrl && (l.a_device_id === ctrl.id || l.b_device_id === ctrl.id);
      svg += `<path class="${toCtrl ? 'topo-flow' : 'topo-link'}" d="${d}"/>`;
    }
    for (const n of t.nodes) {
      const [x, y] = pos[n.id];
      const isCtrl = n.effective_kind === 'server';
      const cls = isCtrl ? 'ctrl' : (n.last_state === 'up' ? 'up' : 'down');
      const r = isCtrl ? 17 : 12;
      svg += `<circle cx="${x}" cy="${y}" r="${r}" class="topo-node ${cls}"/>`;
      svg += `<text x="${x}" y="${y + r + 17}" text-anchor="middle" class="topo-lbl">${esc(n.hostname)}</text>`;
      svg += `<text x="${x}" y="${y + r + 32}" text-anchor="middle" class="topo-ip">${esc(n.mgmt_ip)}</text>`;
    }
    svg += '</svg>';

    const topo = `
      <div class="row" style="justify-content:space-between;margin-bottom:9px">
        <div class="zoom">
          <button data-zoom="out" title="Zoom out">−</button>
          <span data-zlabel>${Math.round(state.zoom * 100)}%</span>
          <button data-zoom="in" title="Zoom in">+</button>
          <button data-zoom="reset" title="Reset">⟲</button>
        </div>
        <div class="legend" style="margin:0">
          <span><i style="background:var(--accent)"></i>controller link (live)</span>
          <span><i style="background:var(--line)"></i>OSPF adjacency</span>
        </div>
      </div>
      <div class="topo-box">${svg}</div>`;

    const nodes = `<div class="panel"><table>
      <thead><tr><th>Host</th><th>Address</th><th>Class</th><th>State</th><th></th></tr></thead>
      <tbody>${t.nodes.map((n) => `<tr>
        <td><b>${esc(n.hostname)}</b></td><td>${esc(n.mgmt_ip)}</td><td>${esc(n.effective_kind)}</td>
        <td><span class="pill ${n.last_state === 'up' ? 'up' : 'down'}">${esc(n.last_state || 'unknown')}</span></td>
        <td>${n.effective_kind === 'server' ? '' : `<button class="act" data-act="term" data-id="${n.id}">terminal</button>`}</td>
      </tr>`).join('')}</tbody></table></div>`;

    $('#view').innerHTML = section('topo', 'Topology', topo, true) + section('nodes', 'Nodes', nodes, true);
  },


  async logs() {
    const page = state.logPage || 1;
    const level = state.logLevel || 'all';
    const q = state.logQuery || '';
    const data = await api.get('/api/logs?page=' + page + '&per_page=10&level=' + level +
                               '&q=' + encodeURIComponent(q));
    state.logPage = data.page;

    const CHIP = { info: 'chip-info', warning: 'chip-warn', critical: 'chip-crit' };
    const rows = data.items.length
      ? data.items.map((it) => `
        <tr>
          <td class="muted mono">${esc(it.ts)}</td>
          <td>${esc(it.device || '\u2014')}</td>
          <td><span class="chip ${CHIP[it.level] || 'chip-info'}">${esc(it.level)}</span></td>
          <td class="log-msg">${esc(it.message)}</td>
          <td>
            <select class="log-act" data-sig="${esc(it.sig)}">
              <option value="none"${it.action === '' ? ' selected' : ''}>\u2014</option>
              <option value="allow"${it.action === 'allow' ? ' selected' : ''}>Allow</option>
              <option value="ignore"${it.action === 'ignore' ? ' selected' : ''}>Ignore</option>
              <option value="block"${it.action === 'block' ? ' selected' : ''}>Block</option>
            </select>
          </td>
        </tr>`).join('')
      : '<tr><td colspan="5" class="muted" style="text-align:center;padding:22px">No entries match this filter.</td></tr>';

    const total = data.pages;
    const pages = [];
    if (total <= 7) {
      for (let i = 1; i <= total; i++) pages.push(i);
    } else {
      pages.push(1);
      const from = Math.max(2, data.page - 1);
      const to = Math.min(total - 1, data.page + 1);
      if (from > 2) pages.push('\u2026');
      for (let i = from; i <= to; i++) pages.push(i);
      if (to < total - 1) pages.push('\u2026');
      pages.push(total);
    }
    const pager = pages.map((n) => (n === '\u2026'
      ? '<span class="pg-dots">\u2026</span>'
      : '<button class="pg' + (n === data.page ? ' on' : '') + '" data-page="' + n + '">' + n + '</button>'
    )).join('');

    const label = level === 'all' ? 'All categories' : level[0].toUpperCase() + level.slice(1);
    const body = `
      <div class="row log-toolbar">
        <div class="filter-wrap">
          <button class="act" data-act="log-filter" title="Filter by category" style="display:inline-flex;align-items:center;gap:7px;height:34px;white-space:nowrap">
            <svg class="ic" viewBox="0 0 24 24" style="width:15px;height:15px;flex:0 0 auto"><g fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M3.6 5.4h16.8l-6.6 7.6v6.2l-3.6 1.8v-8z"/></g></svg>
            ${label}
          </button>
          <div class="filter-menu${state.logFilterOpen ? ' open' : ''}">
            <button data-level="all">All</button>
            <button data-level="info">Info</button>
            <button data-level="warning">Warning</button>
            <button data-level="critical">Critical</button>
          </div>
        </div>
        <input id="log-q" placeholder="search message or device" value="${esc(q)}" style="width:260px">
        <button class="act" data-act="log-search">Search</button>
        <span class="muted" style="margin-left:auto">${data.total} entr${data.total === 1 ? 'y' : 'ies'}</span>
      </div>
      <div class="panel" style="padding:0;overflow:hidden">
        <table class="log-table">
          <thead><tr><th style="width:150px">Time</th><th style="width:110px">Device</th>
            <th style="width:100px">Level</th><th>Message</th><th style="width:120px">Action</th></tr></thead>
          <tbody>${rows}</tbody>
        </table>
      </div>
      <div class="pager">${pager}</div>
      <p class="muted" style="font-size:12px;margin-top:10px">
        <b>Block</b> suppresses this event class from now on &nbsp;\u00b7&nbsp;
        <b>Ignore</b> hides it from this view &nbsp;\u00b7&nbsp;
        <b>Allow</b> marks it as expected.
      </p>`;

    $('#view').innerHTML = section('logs', 'Logs', body, true);
  },
  async database() {
    const devs = await api.get('/api/devices');
    state.devices = devs;
    const admin = state.me.role === 'admin';
    const dis = admin ? '' : 'disabled';

    const body = `<div class="grid">${devs.map((d) => `
      <div class="card" data-id="${d.id}" data-host="${esc(d.hostname)}">
        <div class="row" style="justify-content:space-between;margin-bottom:11px">
          <div><b style="font-size:15px">${esc(d.hostname)}</b>
            <div class="muted" style="font-size:12.5px">${esc(d.site || '—')} · ${esc(d.effective_kind)}</div></div>
          <span class="pill ${d.last_state === 'up' ? 'up' : 'down'}">${esc(d.last_state || 'unknown')}</span>
        </div>
        <div style="display:grid;grid-template-columns:1fr 1fr;gap:10px">
          <label class="muted" style="font-size:12px">Address<br>
            <input value="${esc(d.mgmt_ip)}" data-f="mgmt_ip" style="width:100%;margin-top:4px" ${dis}></label>
          <label class="muted" style="font-size:12px">SSH port<br>
            <input value="${d.ssh_port ?? 22}" data-f="ssh_port" style="width:100%;margin-top:4px" ${dis}></label>
          <label class="muted" style="font-size:12px">Asset tag<br>
            <input value="${esc(d.asset_tag || '')}" data-f="asset_tag" style="width:100%;margin-top:4px" ${dis}></label>
          <label class="muted" style="font-size:12px">Vendor<br>
            <input value="${esc(d.vendor || '')}" disabled style="width:100%;margin-top:4px;opacity:.6"></label>
        </div>
        <div class="row" style="margin-top:13px;justify-content:flex-end">
          ${admin ? `<button class="act" data-act="cred">Set credential</button>
                     <button class="act primary" data-act="save">Save</button>` : '<span class="muted">read-only</span>'}
        </div>
      </div>`).join('')}</div>`;
    $('#view').innerHTML = section('db', 'Inventory', body, true);
  },

  async terminal() {
    if (!state.term) { location.hash = '#/monitoring'; return; }

    const d = state.term.dev;
    const body = `
      <div class="panel">
        <div class="row" style="justify-content:space-between;margin-bottom:10px">
          <span class="muted">${esc(d.mgmt_ip)} · root</span>
          <button class="act" data-act="close">Close session</button>
        </div>
        <div class="term-out" id="tout"></div>
        <div class="term-line">
          <span class="prompt">${esc(d.hostname)}#</span>
          <input id="tcmd" autocomplete="off" spellcheck="false"
                 placeholder="type a command and press Enter">
          <button class="act primary" data-act="run">Run</button>
        </div>
        <p class="muted" style="font-size:12px;margin-top:8px">
          Allowed: show ip ospf neighbor · show ip route summary · show version · show interfaces ·
          show run · uptime · free -m · df -h · ip -br a
        </p>
      </div>`;
    $('#view').innerHTML = section('term', 'Remote terminal · ' + d.hostname, body, true);
    renderTerm();
    const inp = $('#tcmd');
    if (inp) inp.focus();
  },

  async settings() {
    const m = state.me;
    const on = (k, v) => (m[k] === v ? 'on' : '');
    const admin = m.role === 'admin';

    const appearance = `<div class="grid">
        <div class="card"><div class="k">Theme</div>
          <div class="row" style="margin-top:10px">
            <button class="act ${on('theme', 'dark')}" data-set="theme" data-v="dark">Black</button>
            <button class="act ${on('theme', 'light')}" data-set="theme" data-v="light">White</button>
          </div></div>
        <div class="card"><div class="k">Layout</div>
          <div class="row" style="margin-top:10px">
            <button class="act ${on('layout', 'detailed')}" data-set="layout" data-v="detailed">Detailed</button>
            <button class="act ${on('layout', 'simple')}" data-set="layout" data-v="simple">Simple</button>
          </div>
          <p class="muted" style="font-size:12px;margin-top:9px">Simple hides the capacity charts and the per-device sparklines.</p>
        </div>
      </div>`;

    const account = `<div class="grid">
        <div class="card"><div class="k">Signed in as</div>
          <div style="margin-top:9px"><b>${esc(m.user)}</b> <span class="muted">· ${esc(m.role)}</span></div>
        </div>
        <div class="card"><div class="k">Change password</div>
          <div style="display:grid;gap:8px;margin-top:10px">
            <input id="pw-old" type="password" placeholder="current password" autocomplete="current-password">
            <input id="pw-new" type="password" placeholder="new password (8+ characters)" autocomplete="new-password">
            <button class="act primary" data-act="pw">Update password</button>
          </div>
        </div>
      </div>`;

    let users = '';
    if (admin) {
      const list = await api.get('/api/users');
      users = `<div class="panel">
        <table><thead><tr><th>User</th><th>Role</th><th>Created</th><th></th></tr></thead><tbody>
        ${list.map((u) => `<tr>
          <td><b>${esc(u.username)}</b></td>
          <td>${esc(u.role)}</td>
          <td class="muted">${esc(u.created_at || '')}</td>
          <td>${u.username === m.user ? '<span class="muted">you</span>'
            : `<button class="act" data-act="deluser" data-uid="${u.id}">delete</button>`}</td>
        </tr>`).join('')}
        </tbody></table>
        <div class="row" style="margin-top:13px">
          <input id="nu-name" placeholder="username" style="width:150px">
          <input id="nu-pass" type="password" placeholder="password (8+)" style="width:175px">
          <select id="nu-role">
            <option value="viewer">viewer</option>
            <option value="admin">admin</option>
          </select>
          <button class="act primary" data-act="adduser">Create user</button>
        </div>
        <p class="muted" style="font-size:12px;margin-top:9px">viewer can read; admin can edit devices, change settings and open terminals.</p>
      </div>`;
    }

    $('#view').innerHTML =
      section('appearance', 'Appearance', appearance, true) +
      section('account', 'Account', account, true) +
      (admin ? section('users', 'Users', users, true) : '');
  }
};

function route() {
  if (state.timer) { clearInterval(state.timer); state.timer = null; }
  const name = (location.hash.replace('#/', '') || 'dashboard').split('?')[0];
  const fn = views[name] || views.dashboard;
  document.querySelectorAll('.side nav a').forEach((a) => {
    const active = a.dataset.view === name;
    a.classList.toggle('active', active);
    if (active) a.setAttribute('aria-current', 'page');
    else a.removeAttribute('aria-current');
  });
  $('#title').textContent = name[0].toUpperCase() + name.slice(1);
  $('#ranges').style.display = name === 'dashboard' ? '' : 'none';

  fn().catch((e) => { $('#view').innerHTML = `<p class="err">${esc(e.message)}</p>`; });

  if (name === 'dashboard' && state.range === 'live') {
    state.timer = setInterval(() => { fn().catch(() => {}); }, 5000);
  }
}

let rt;
window.addEventListener('resize', () => {
  clearTimeout(rt);
  rt = setTimeout(() => {
    if ((location.hash.replace('#/', '') || 'dashboard') === 'dashboard') route();
  }, 200);
});

document.addEventListener('DOMContentLoaded', boot);
