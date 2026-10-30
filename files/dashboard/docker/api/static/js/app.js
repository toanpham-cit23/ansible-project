const state = { range: 'live', devices: [], me: null, timer: null, term: null, zoom: 1,
                logPage: 1, logLevel: 'all', logQuery: '', logFilterOpen: false,
                leaseQuery: '', leaseOnline: false, analysisHours: 24 };
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
  $('#view').addEventListener('focusout', onFocusOut);
  $('#view').addEventListener('wheel', onWheel, { passive: false });
  window.addEventListener('hashchange', route);
  route();
}

async function onFocusOut(e) {
  // Saving on blur rather than on a button keeps the lease table to one row
  // per address; there is nothing to submit and no extra click.
  const inp = e.target.closest && e.target.closest('.lease-name');
  if (!inp) return;
  const name = inp.value.trim();
  if (!name) return;
  try {
    await api.patch(`/api/leases/${inp.dataset.id}`, { device_name: name });
    toast(`Named ${name}`);
    route();
  } catch (err) { toast(err.message, true); }
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

  // ---- leases
  if (b.dataset.act === 'lease-search') {
    state.leaseQuery = ($('#lease-q') || {}).value || '';
    route();
    return;
  }

  if (b.dataset.act === 'lease-online') {
    state.leaseOnline = !state.leaseOnline;
    route();
    return;
  }

  // ---- log analysis window
  if (b.dataset.act === 'analysis-hours') {
    state.analysisHours = Number(b.dataset.h);
    route();
    return;
  }

  if (b.dataset.act === 'ack-finding') {
    try {
      await api.post(`/api/logs/analysis/${b.dataset.id}/ack`, {});
      toast('Acknowledged');
      route();
    } catch (err) { toast(err.message, true); }
    return;
  }

  // ---- shifts
  if (b.dataset.act === 'shift-add') {
    const days = [...document.querySelectorAll('.sh-day:checked')].map((x) => x.value);
    const name = ($('#sh-name') || {}).value || '';
    const email = ($('#sh-email') || {}).value || '';
    if (!name.trim() || !email.includes('@')) {
      toast('A name and a valid email are required', true);
      return;
    }
    try {
      await api.put('/api/shifts', {
        name: name.trim(), email: email.trim(),
        role: ($('#sh-role') || {}).value || 'primary',
        starts_at: ($('#sh-start') || {}).value || '00:00',
        ends_at: ($('#sh-end') || {}).value || '23:59',
        days,
      });
      toast('Administrator added');
      route();
    } catch (err) { toast(err.message, true); }
    return;
  }

  if (b.dataset.act === 'shift-handover') {
    const adminId = Number(b.dataset.id);
    const ok = await confirmDialog('Make on-call',
      'Hand the on-call duty to this administrator now, and re-send every ' +
      'unresolved alert to them? Use this when a shift changes early, or when ' +
      'someone worked overtime and did not hand over.', 'Hand over');
    if (!ok) return;
    try {
      const r = await api.post('/api/handover', {
        to_admin: adminId, reason: 'manual',
        note: 'handover from the dashboard', redispatch: true,
      });
      toast(r.mailed ? `Handed over, ${r.mailed} alert(s) re-sent` : 'Handed over');
      route();
    } catch (err) { toast(err.message, true); }
    return;
  }

  // ---- alerts
  if (b.dataset.act === 'rule-toggle') {
    const on = b.dataset.on === '1';
    try {
      await api.patch(`/api/alerts/rules/${b.dataset.id}`, { enabled: !on });
      route();
    } catch (err) { toast(err.message, true); }
    return;
  }

  if (b.dataset.act === 'alert-test') {
    try {
      const r = await api.post('/api/alerts/test', {});
      toast(`Test mail written for ${r.to} (${r.transport})`);
      route();
    } catch (err) { toast(err.message, true); }
    return;
  }

  if (b.dataset.act === 'alert-redispatch') {
    try {
      const r = await api.post('/api/alerts/redispatch', {});
      toast(r.sent ? `Re-sent ${r.sent} mail(s) to ${r.recipients.join(', ')}`
                   : 'Nothing open to re-send');
      route();
    } catch (err) { toast(err.message, true); }
    return;
  }

  // ---- setup / discovery
  if (b.dataset.act === 'discover') {
    try {
      const r = await api.post('/api/topology/discover', {});
      toast(`Scanned ${r.routers} router(s): ${r.links.length} adjacency(ies), ` +
            `${r.new} new proposal(s)`);
      route();
    } catch (err) { toast(err.message, true); }
    return;
  }

  if (b.dataset.act === 'prop-accept' || b.dataset.act === 'prop-reject') {
    const action = b.dataset.act === 'prop-accept' ? 'accept' : 'reject';
    try {
      await api.post(`/api/topology/proposals/${b.dataset.id}`, { action });
      toast(action === 'accept' ? 'Link added to the topology' : 'Proposal rejected');
      route();
    } catch (err) { toast(err.message, true); }
    return;
  }

  if (b.dataset.act === 'dev-add') {
    try {
      await api.post('/api/devices', {
        hostname: (($('#nd-host') || {}).value || '').trim(),
        mgmt_ip: (($('#nd-ip') || {}).value || '').trim(),
        kind: ($('#nd-kind') || {}).value || 'router',
        username: (($('#nd-user') || {}).value || '').trim(),
        password: ($('#nd-pass') || {}).value || '',
      });
      toast('Device added');
      route();
    } catch (err) { toast(err.message, true); }
    return;
  }

  if (b.dataset.act === 'dev-del') {
    const ok = await confirmDialog('Remove device',
      `Remove <b>${esc(b.dataset.host)}</b> from the inventory? Its links and ` +
      'lease history go with it.', 'Remove');
    if (!ok) return;
    try {
      await api.call('DELETE', `/api/devices/${b.dataset.id}`);
      toast('Device removed');
      route();
    } catch (err) { toast(err.message, true); }
    return;
  }

  // ---- dns
  if (b.dataset.act === 'dns-sug') {
    // a suggestion fills the form rather than saving straight away: the
    // operator still gets to change the label before it goes live
    if ($('#dns-name')) $('#dns-name').value = b.dataset.name;
    if ($('#dns-ip')) $('#dns-ip').value = b.dataset.ip;
    if ($('#dns-name')) $('#dns-name').focus();
    return;
  }

  if (b.dataset.act === 'dns-add') {
    const name = (($('#dns-name') || {}).value || '').trim();
    const ip = (($('#dns-ip') || {}).value || '').trim();
    if (!name || !ip) { toast('A label and an address are required', true); return; }
    try {
      await api.put('/api/dns', {
        name, ip,
        ttl: Number((($('#dns-ttl') || {}).value || 60)) || 60,
      });
      toast(`Name ${name} added`);
      route();
    } catch (err) { toast(err.message, true); }
    return;
  }

  if (b.dataset.act === 'dns-del') {
    const ok = await confirmDialog('Remove name',
      `Stop answering for <b>${esc(b.dataset.name)}</b>?`, 'Remove');
    if (!ok) return;
    try {
      await api.call('DELETE', `/api/dns/${b.dataset.id}`);
      toast('Name removed');
      route();
    } catch (err) { toast(err.message, true); }
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
    return;
  }
  // Enter in either search box should search, not reload the page
  if ((e.target.id === 'log-q' || e.target.id === 'lease-q') && e.key === 'Enter') {
    e.preventDefault();
    e.target.closest('#view').querySelector(
      e.target.id === 'log-q' ? '[data-act="log-search"]' : '[data-act="lease-search"]').click();
    return;
  }
  // name a discovered device without leaving the row
  if (e.target.classList && e.target.classList.contains('lease-name') && e.key === 'Enter') {
    e.preventDefault();
    e.target.blur();
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
  },

  // ---------------------------------------------------------------- leases
  // "Which PC holds this IP, and what is its MAC and name." The poller fills
  // this from every router's ARP table, so a device shows up here even if it
  // was never added to the inventory.
  async leases() {
    const q = state.leaseQuery || '';
    const only = state.leaseOnline ? '&online=1' : '';
    const data = await api.get('/api/leases?limit=500&q=' + encodeURIComponent(q) + only);
    const admin = state.me.role === 'admin';
    const c = data.counts || {};

    const rows = data.leases.length ? data.leases.map((l) => `
      <tr data-lease="${l.id}">
        <td class="mono">${esc(l.ip)}</td>
        <td class="mono">${esc(l.mac)}</td>
        <td>${l.mac_vendor ? `<span class="muted" style="font-size:11.5px">${esc(l.mac_vendor)}</span>` : '\u2014'}</td>
        <td>${l.device_name
          ? esc(l.device_name)
          : (admin
              ? `<input class="lease-name" data-id="${l.id}" placeholder="name this device"
                        value="" style="width:100%;max-width:190px">`
              : '<span class="muted">unnamed</span>')}</td>
        <td class="muted mono">${esc(l.subnet || '\u2014')}</td>
        <td class="muted">${l.vlan_id ? esc(l.vlan_id) : '\u2014'}</td>
        <td class="muted">${esc(l.seen_on || '\u2014')}</td>
        <td><span class="pill ${l.online ? 'up' : 'down'}">${l.online ? 'online' : 'offline'}</span></td>
        <td class="muted mono" style="font-size:11.5px">${esc((l.last_seen || '').slice(11))}</td>
      </tr>`).join('')
      : `<tr><td colspan="9" class="muted" style="text-align:center;padding:22px">
           No address has been seen yet. The poller reads each router's ARP table every 5 seconds.</td></tr>`;

    const body = `
      <div class="row" style="margin-bottom:10px;gap:10px">
        <input id="lease-q" placeholder="Search IP, MAC or device name" value="${esc(q)}"
               style="padding:8px 10px;background:var(--bg);border:1px solid var(--line);
                      border-radius:4px;color:var(--ink);font-size:13.5px;min-width:280px">
        <button class="act" data-act="lease-search">Search</button>
        <button class="act ${state.leaseOnline ? 'on' : ''}" data-act="lease-online">
          Online only</button>
        <span class="muted" style="margin-left:auto;font-size:12.5px">
          ${c.online ?? 0} online of ${c.total ?? 0} address${(c.total ?? 0) === 1 ? '' : 'es'} known
        </span>
      </div>
      <div class="panel" style="padding:4px 8px">
        <table>
          <thead><tr>
            <th>Address</th><th>MAC</th><th>Vendor</th><th>Device</th>
            <th>Subnet</th><th>VLAN</th><th>Seen on</th><th>State</th><th>Last seen</th>
          </tr></thead>
          <tbody>${rows}</tbody>
        </table>
      </div>`;
    $('#view').innerHTML = section('leases', 'Address leases', body, true);
  },

  // -------------------------------------------------------------- reboots
  // Power cuts and restarts, inferred from an SNMP uptime going backwards.
  async reboots() {
    const rows = await api.get('/api/reboots?limit=100');
    const KIND = { 'power-loss': 'chip-crit', restart: 'chip-warn', crash: 'chip-crit' };
    const items = rows.length ? rows.map((r) => `
      <tr>
        <td class="muted mono">${esc(r.ts)}</td>
        <td><b>${esc(r.hostname || '\u2014')}</b></td>
        <td><span class="chip ${KIND[r.kind] || 'chip-info'}">${esc(r.kind)}</span></td>
        <td class="mono">${esc(r.prev_uptime_s)}s</td>
        <td class="mono">${esc(r.new_uptime_s)}s</td>
        <td class="mono">${r.gap_s ? esc(r.gap_s) + 's' : '\u2014'}</td>
        <td class="muted">${esc(r.note || '')}</td>
      </tr>`).join('')
      : `<tr><td colspan="7" class="muted" style="text-align:center;padding:22px">
           No restart recorded. A device that reboots appears here within one poll.</td></tr>`;

    const body = `
      <p class="muted" style="font-size:12.5px;margin-bottom:10px">
        A restart is detected when a device's SNMP uptime goes backwards. If it was also
        unreachable for 20&nbsp;s or more beforehand, it is reported as a power loss rather
        than a quick restart.
      </p>
      <div class="panel" style="padding:4px 8px">
        <table>
          <thead><tr><th>When</th><th>Device</th><th>Kind</th>
            <th>Uptime before</th><th>Uptime after</th><th>Gap</th><th>Detail</th></tr></thead>
          <tbody>${items}</tbody>
        </table>
      </div>`;
    $('#view').innerHTML = section('reboots', 'Restarts and power loss', body, true);
  },

  // --------------------------------------------------------------- shifts
  async shifts() {
    const d = await api.get('/api/shifts');
    const admin = state.me.role === 'admin';
    const cur = d.current;

    const banner = cur
      ? `<div class="panel" style="margin-bottom:10px;border-left:3px solid var(--accent)">
           <div class="row" style="justify-content:space-between">
             <div>
               <div class="muted" style="font-size:11.5px;text-transform:uppercase;letter-spacing:.06em">
                 On call now</div>
               <div style="font-size:17px;margin-top:3px"><b>${esc(cur.name)}</b>
                 <span class="muted" style="font-size:13px">&lt;${esc(cur.email)}&gt;</span></div>
               <div class="muted" style="font-size:12.5px;margin-top:2px">
                 ${esc(cur.role)} \u00b7 chosen by ${esc(cur.source)}
                 ${d.handover ? `\u00b7 handover at ${esc(d.handover.ts)} (${esc(d.handover.reason)})` : ''}
               </div>
             </div>
             ${d.overdue ? '<span class="chip chip-crit">handover overdue</span>' : ''}
           </div>
         </div>`
      : `<div class="panel" style="margin-bottom:10px"><span class="muted">
           No roster yet. Add an administrator below so alerts have somewhere to go.</span></div>`;

    const DAYS = ['mon', 'tue', 'wed', 'thu', 'fri', 'sat', 'sun'];
    const rows = d.admins.length ? d.admins.map((a) => `
      <tr data-shift="${a.id}">
        <td><b>${esc(a.name)}</b>${a.active ? '' : ' <span class="muted">(inactive)</span>'}</td>
        <td class="mono">${esc(a.email)}</td>
        <td>${esc(a.role)}</td>
        <td class="mono">${esc(a.starts_at)}\u2013${esc(a.ends_at)}</td>
        <td class="mono" style="font-size:11.5px">${esc((a.days || []).join(' '))}</td>
        <td class="muted mono" style="font-size:11.5px">${esc((a.days_mask || 0).toString(2))}</td>
        <td style="text-align:right">
          ${admin ? `<button class="act" data-act="shift-edit" data-id="${a.id}">Edit</button>
                     <button class="act primary" data-act="shift-handover" data-id="${a.id}">
                       Make on-call</button>` : ''}
        </td>
      </tr>`).join('')
      : `<tr><td colspan="7" class="muted" style="text-align:center;padding:22px">No administrators yet.</td></tr>`;

    const form = admin ? `
      <div class="panel" style="margin-top:10px">
        <div class="p-title" style="text-align:left;margin-bottom:9px">Add an administrator</div>
        <div style="display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:9px">
          <label class="muted" style="font-size:12px">Name<br>
            <input id="sh-name" style="width:100%;margin-top:4px"></label>
          <label class="muted" style="font-size:12px">Email<br>
            <input id="sh-email" style="width:100%;margin-top:4px"></label>
          <label class="muted" style="font-size:12px">Role<br>
            <select id="sh-role" style="width:100%;margin-top:4px">
              <option value="primary">primary</option><option value="backup">backup</option>
            </select></label>
          <label class="muted" style="font-size:12px">From<br>
            <input id="sh-start" value="08:00" style="width:100%;margin-top:4px"></label>
          <label class="muted" style="font-size:12px">To<br>
            <input id="sh-end" value="17:00" style="width:100%;margin-top:4px"></label>
        </div>
        <div class="row" style="margin-top:9px">
          <span class="muted" style="font-size:12px">Days</span>
          ${DAYS.map((x) => `<label class="muted" style="font-size:12px">
            <input type="checkbox" class="sh-day" value="${x}" checked> ${x}</label>`).join('')}
        </div>
        <div class="row" style="margin-top:11px;justify-content:flex-end">
          <button class="act primary" data-act="shift-add">Add</button>
        </div>
      </div>` : '';

    const body = banner + `
      <div class="panel" style="padding:4px 8px">
        <table>
          <thead><tr><th>Name</th><th>Email</th><th>Role</th><th>Hours</th>
            <th>Days</th><th>Mask</th><th></th></tr></thead>
          <tbody>${rows}</tbody>
        </table>
      </div>
      <p class="muted" style="font-size:12.5px;margin-top:9px">
        \u201cMake on-call\u201d records a handover. Use it when a shift changes early, or
        when someone works overtime and forgets to hand over: the next person presses it and
        every unresolved alert is re-sent to them immediately. A handover stays in effect for
        ${esc(d.valid_h)} hours, after which the timetable above takes over again.
      </p>` + form;
    $('#view').innerHTML = section('shifts', 'Shift roster and on-call', body, true);
  },

  // --------------------------------------------------------------- alerts
  async alerts() {
    const d = await api.get('/api/alerts');
    const admin = state.me.role === 'admin';
    const ST = { sent: 'chip-info', failed: 'chip-crit', suppressed: 'chip-warn', queued: 'chip-info' };

    const rules = d.rules.map((r) => `
      <tr>
        <td><b>${esc(r.name)}</b></td>
        <td class="mono">${esc(r.min_severity)}</td>
        <td class="mono" style="font-size:11.5px">${esc(r.match_like || '\u2014')}</td>
        <td class="mono">${esc(r.recipients)}</td>
        <td class="mono">${esc(r.cooldown_s)}s</td>
        <td><span class="pill ${r.enabled ? 'up' : 'down'}">${r.enabled ? 'on' : 'off'}</span></td>
        <td style="text-align:right">${admin
          ? `<button class="act" data-act="rule-toggle" data-id="${r.id}"
                     data-on="${r.enabled ? 1 : 0}">${r.enabled ? 'Disable' : 'Enable'}</button>` : ''}</td>
      </tr>`).join('');

    const deliv = d.deliveries.length ? d.deliveries.map((x) => `
      <tr>
        <td class="muted mono">${esc(x.ts)}</td>
        <td>${esc(x.kind === 'redispatch' ? 'handover re-send' : (x.kind === 'test' ? 'test' : x.rule))}</td>
        <td class="mono">${esc(x.to || '\u2014')}</td>
        <td><span class="chip ${ST[x.status] || 'chip-info'}">${esc(x.status)}</span></td>
        <td class="muted">${esc(x.subject)}</td>
        <td class="muted" style="font-size:11.5px">${esc(x.error || '')}</td>
      </tr>`).join('')
      : `<tr><td colspan="6" class="muted" style="text-align:center;padding:22px">No mail sent yet.</td></tr>`;

    const body = `
      <div class="row" style="margin-bottom:10px;gap:14px">
        <span class="muted" style="font-size:12.5px">
          ${d.counts.sent} sent \u00b7 ${d.counts.failed} failed \u00b7
          ${d.counts.suppressed} suppressed by cooldown</span>
        <span style="margin-left:auto"></span>
        ${admin ? `<button class="act" data-act="alert-test">Send test mail</button>
                   <button class="act" data-act="alert-redispatch">Re-send open alerts</button>` : ''}
      </div>
      <div class="panel" style="padding:4px 8px;margin-bottom:10px">
        <table>
          <thead><tr><th>Rule</th><th>Min severity</th><th>Match</th>
            <th>Recipients</th><th>Cooldown</th><th>State</th><th></th></tr></thead>
          <tbody>${rules}</tbody>
        </table>
      </div>
      <div class="panel" style="padding:4px 8px">
        <table>
          <thead><tr><th>When</th><th>Rule</th><th>To</th><th>Status</th>
            <th>Subject</th><th>Note</th></tr></thead>
          <tbody>${deliv}</tbody>
        </table>
      </div>
      <p class="muted" style="font-size:12.5px;margin-top:9px">
        With no SMTP relay configured, each message is written as a real <span
        class="mono">.eml</span> file under <span class="mono">db/outbox/</span> on the
        controller, so the routing can be inspected even in an isolated lab.
      </p>`;
    $('#view').innerHTML = section('alerts', 'Alert rules and delivery', body, true);
  },

  // ---------------------------------------------------------- log analysis
  async analysis() {
    const d = await api.get('/api/logs/analysis?hours=' + (state.analysisHours || 24));
    const S = { disaster: 'chip-crit', high: 'chip-crit', average: 'chip-warn',
                warning: 'chip-warn', info: 'chip-info' };
    const sev = d.by_severity || {};
    const cat = d.by_category || {};

    const chips = Object.keys(sev).sort().map((k) =>
      `<span class="chip ${S[k] || 'chip-info'}">${esc(k)}: ${sev[k]}</span>`).join(' ');
    const catChips = Object.keys(cat).sort().map((k) =>
      `<span class="pill">${esc(k)}: ${cat[k]}</span>`).join(' ');

    const rows = d.findings.length ? d.findings.map((f) => `
      <tr>
        <td class="muted mono" style="font-size:11.5px">${esc(f.last_ts)}</td>
        <td><b>${esc(f.hostname || '\u2014')}</b></td>
        <td><span class="chip ${S[f.severity] || 'chip-info'}">${esc(f.severity)}</span></td>
        <td class="muted">${esc(f.category || '\u2014')}</td>
        <td class="mono" style="text-align:center">${esc(f.hits)}</td>
        <td class="log-msg mono" style="font-size:11.5px">${esc(f.line)}</td>
        <td class="muted" style="font-size:11.5px">${esc(f.hint || '')}</td>
        <td style="text-align:right">${state.me.role === 'admin' && !f.acked
          ? `<button class="act" data-act="ack-finding" data-id="${f.id}">Ack</button>`
          : (f.acked ? '<span class="muted" style="font-size:11.5px">acked</span>' : '')}</td>
      </tr>`).join('')
      : `<tr><td colspan="8" class="muted" style="text-align:center;padding:22px">
           Nothing matched a known error pattern in this window.</td></tr>`;

    const body = `
      <div class="row" style="margin-bottom:10px;gap:8px">
        <span class="muted" style="font-size:12.5px">Window</span>
        ${[1, 6, 24, 168].map((h) => `<button class="act ${String(state.analysisHours || 24) === String(h) ? 'on' : ''}"
            data-act="analysis-hours" data-h="${h}">${h === 168 ? '7d' : h + 'h'}</button>`).join('')}
        <span style="margin-left:auto"></span>
        <span style="font-size:12px">${chips}</span>
      </div>
      <div class="row" style="margin-bottom:10px;gap:6px">${catChips}</div>
      <div class="panel" style="padding:4px 8px">
        <table>
          <thead><tr><th>Last seen</th><th>Device</th><th>Severity</th><th>Category</th>
            <th style="text-align:center">Hits</th><th>Log line</th><th>What it means</th><th></th></tr></thead>
          <tbody>${rows}</tbody>
        </table>
      </div>
      <p class="muted" style="font-size:12.5px;margin-top:9px">
        The poller reads the tail of each router's <span class="mono">/var/log/messages</span>
        once a minute and matches it against the patterns in <span class="mono">log_patterns</span>.
        Identical lines collapse into one row with a hit count, so a flapping link reads as a
        single problem rather than a hundred notifications.
      </p>`;
    $('#view').innerHTML = section('analysis', 'Automated log analysis', body, true);
  },

  // ---------------------------------------------------------------- setup
  // Devices, discovery and DNS in one place: this is where a new box is
  // introduced and where the discovered topology is confirmed.
  async setup() {
    const admin = state.me.role === 'admin';
    const [devs, props, poll] = await Promise.all([
      api.get('/api/devices'),
      api.get('/api/topology/proposals').catch(() => []),
      api.get('/api/poll').catch(() => ({})),
    ]);
    state.devices = devs;

    const devRows = devs.map((d) => `
      <tr>
        <td><b>${esc(d.hostname)}</b></td>
        <td class="mono">${esc(d.mgmt_ip)}</td>
        <td>${esc(d.effective_kind)}</td>
        <td><span class="pill ${d.last_state === 'up' ? 'up' : 'down'}">${esc(d.last_state || '?')}</span></td>
        <td class="muted mono" style="font-size:11.5px">${esc(d.last_seen || '')}</td>
        <td style="text-align:right">${admin
          ? `<button class="act" data-act="dev-del" data-id="${d.id}" data-host="${esc(d.hostname)}">Remove</button>` : ''}</td>
      </tr>`).join('');

    const propRows = props.length ? props.map((p) => `
      <tr>
        <td><b>${esc(p.a)}</b> \u2194 <b>${esc(p.b)}</b></td>
        <td class="mono">${esc(p.proto)}</td>
        <td class="muted" style="font-size:11.5px">${esc(p.evidence || '')}</td>
        <td class="mono">${Math.round((p.confidence || 0) * 100)}%</td>
        <td style="text-align:right">${admin
          ? `<button class="act" data-act="prop-accept" data-id="${p.id}">Accept</button>
             <button class="act" data-act="prop-reject" data-id="${p.id}">Reject</button>` : ''}</td>
      </tr>`).join('')
      : `<tr><td colspan="5" class="muted" style="text-align:center;padding:18px">
           No pending proposals. Run discovery to read the live OSPF adjacencies.</td></tr>`;

    const pollBadge = poll.healthy
      ? `<span class="chip chip-info">polling every ${esc(poll.interval_s)}s \u00b7 last ${esc(poll.age_s)}s ago</span>`
      : `<span class="chip chip-crit">no sample for ${esc(poll.age_s)}s</span>`;

    const add = admin ? `
      <div class="panel" style="margin-top:10px">
        <div class="p-title" style="text-align:left;margin-bottom:9px">Add a device</div>
        <div style="display:grid;grid-template-columns:repeat(auto-fit,minmax(140px,1fr));gap:9px">
          <label class="muted" style="font-size:12px">Hostname<br>
            <input id="nd-host" style="width:100%;margin-top:4px"></label>
          <label class="muted" style="font-size:12px">Management IP<br>
            <input id="nd-ip" style="width:100%;margin-top:4px"></label>
          <label class="muted" style="font-size:12px">Kind<br>
            <select id="nd-kind" style="width:100%;margin-top:4px">
              <option>router</option><option>switch</option>
              <option>l3switch</option><option>server</option></select></label>
          <label class="muted" style="font-size:12px">SSH user<br>
            <input id="nd-user" value="root" style="width:100%;margin-top:4px"></label>
          <label class="muted" style="font-size:12px">SSH password<br>
            <input id="nd-pass" type="password" style="width:100%;margin-top:4px"></label>
        </div>
        <div class="row" style="margin-top:11px;justify-content:flex-end">
          <button class="act" data-act="discover">Run discovery</button>
          <button class="act primary" data-act="dev-add">Add device</button>
        </div>
      </div>` : '';

    const body = `
      <div class="row" style="margin-bottom:10px">
        ${pollBadge}
        <span class="muted" style="font-size:12.5px;margin-left:auto">
          ${devs.length} device${devs.length === 1 ? '' : 's'} in the inventory</span>
      </div>
      <div class="panel" style="padding:4px 8px">
        <table>
          <thead><tr><th>Hostname</th><th>Address</th><th>Kind</th><th>State</th>
            <th>Last seen</th><th></th></tr></thead>
          <tbody>${devRows}</tbody>
        </table>
      </div>
      <div style="margin-top:12px">
        <div class="row" style="margin-bottom:6px">
          <b style="font-size:13.5px">Discovered links</b>
          <span class="muted" style="font-size:12.5px">read from live OSPF adjacencies</span>
        </div>
        <div class="panel" style="padding:4px 8px">
          <table>
            <thead><tr><th>Pair</th><th>Proto</th><th>Evidence</th>
              <th>Confidence</th><th></th></tr></thead>
            <tbody>${propRows}</tbody>
          </table>
        </div>
      </div>` + add;

    // ---- DNS, folded into this panel because it is part of "setting a box up"
    const dns = await api.get('/api/dns').catch(() => ({ records: [], zone: 'lab.local', suggest: [] }));
    const dnsRows = dns.records.length ? dns.records.map((r) => `
      <tr>
        <td class="mono"><b>${esc(r.name)}</b>.${esc(dns.zone)}</td>
        <td class="mono">${esc(r.ip)}</td>
        <td class="mono">${esc(r.ttl)}s</td>
        <td class="muted" style="font-size:11.5px">${esc(r.note || '')}</td>
        <td style="text-align:right">${admin
          ? `<button class="act" data-act="dns-del" data-id="${r.id}" data-name="${esc(r.name)}">Remove</button>` : ''}</td>
      </tr>`).join('')
      : `<tr><td colspan="5" class="muted" style="text-align:center;padding:18px">
           No names yet. Add one below and <span class="mono">http://name.${esc(dns.zone)}:8080</span> starts working.</td></tr>`;

    const dnsState = dns.listening
      ? `<span class="chip chip-info">serving on udp/${esc(String(dns.listening))}</span>`
      : `<span class="chip chip-warn">not serving \u00b7 records only</span>`;
    const sug = (dns.suggest || []).map((s) =>
      `<button class="act" data-act="dns-sug" data-name="${esc(s.name)}"
               data-ip="${esc(s.ip)}" style="font-size:11.5px">${esc(s.name)} \u2192 ${esc(s.ip)}</button>`).join(' ');

    const dnsPanel = `
      <div style="margin-top:12px">
        <div class="row" style="margin-bottom:6px;gap:9px">
          <b style="font-size:13.5px">DNS for the web UI</b>
          <span class="muted" style="font-size:12.5px">optional \u00b7 zone ${esc(dns.zone)}</span>
          <span style="margin-left:auto"></span>${dnsState}
        </div>
        <div class="panel" style="padding:4px 8px">
          <table>
            <thead><tr><th>Name</th><th>Address</th><th>TTL</th><th>Note</th><th></th></tr></thead>
            <tbody>${dnsRows}</tbody>
          </table>
        </div>
        ${admin ? `<div class="panel" style="margin-top:9px">
          <div class="p-title" style="text-align:left;margin-bottom:9px">Add a name</div>
          <div style="display:grid;grid-template-columns:repeat(auto-fit,minmax(140px,1fr));gap:9px">
            <label class="muted" style="font-size:12px">Label<br>
              <input id="dns-name" placeholder="lab" style="width:100%;margin-top:4px"></label>
            <label class="muted" style="font-size:12px">Address<br>
              <input id="dns-ip" placeholder="192.168.23.10" style="width:100%;margin-top:4px"></label>
            <label class="muted" style="font-size:12px">TTL (s)<br>
              <input id="dns-ttl" value="60" style="width:100%;margin-top:4px"></label>
          </div>
          ${sug ? `<div class="row" style="margin-top:9px;gap:6px;flex-wrap:wrap">
            <span class="muted" style="font-size:11.5px">from the lease table:</span>${sug}</div>` : ''}
          <div class="row" style="margin-top:11px;justify-content:flex-end">
            <button class="act primary" data-act="dns-add">Add name</button>
          </div>
        </div>` : ''}
      </div>`;

    $('#view').innerHTML = section('setup', 'Devices, discovery and DNS', body + dnsPanel, true);
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
