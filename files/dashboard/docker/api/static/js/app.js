const state = { range: 'live', devices: [], me: null, timer: null, term: null, zoom: 1,
                logPage: 1, logLevel: 'all', logQuery: '', logFilterOpen: false,
                leaseQuery: '', leaseOnline: false, analysisHours: 24, shiftEdit: null,
                topoFocus: null, topoIssues: {} };
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

// markTg paints the Telegram field red and writes the reason under it.
function markTg(msg, bad) {
  const f = $('#sh-telegram');
  const n = $('#sh-tg-note');
  if (f) f.style.borderColor = bad ? 'var(--err)' : '';
  if (n) {
    n.textContent = msg || '';
    n.style.color = bad ? 'var(--err)' : 'var(--muted)';
  }
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
  // The Telegram button lives in the persistent status bar, outside #view, so
  // it needs its own delegated listener. It is visible to every signed-in user
  // (self-service); the Bot Token itself stays admin-only (runbook 4.1d/4.1c).
  $('#statusbar').addEventListener('click', async (e) => {
    const b = e.target.closest('[data-act="tg-login"]');
    if (!b) return;
    try {
      const s = await api.get('/api/telegram/status');
      let target = s.login_url || '';
      if (!s.linked) {
        const l = await api.post('/api/telegram/link-token');
        target = l.url || target;
      }
      if (!target) { toast('Admin chưa cấu hình Telegram Bot Username trong Settings', true); return; }
      window.open(target, '_blank', 'noopener,noreferrer');
    } catch (err) { toast(err.message, true); }
  });
  $('#view').addEventListener('click', onClick);
  $('#view').addEventListener('change', onChange);
  $('#view').addEventListener('input', onInput);
  $('#view').addEventListener('keydown', onKey);
  $('#view').addEventListener('focusout', onFocusOut);
  $('#view').addEventListener('wheel', onWheel, { passive: false });
  $('#view').addEventListener('click', onTopoClick);
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
  if (e.target.id === 'sh-telegram') {
    const v = (e.target.value || '').trim().replace(/^@/, '');
    if (!v) { markTg('', false); return; }
    try {
      const r = await api.get('/api/staff/lookup?username=' + encodeURIComponent(v));
      if (r.exists) markTg('Verified in the Telegram group as @' + r.username, false);
      else markTg('Username not exist', true);
    } catch (err) { markTg('', false); }
    return;
  }
  if (e.target.id === 'log-cat') {
    state.logCat = e.target.value; state.logPage = 1; route(); return;
  }
  if (e.target.id === 'log-level') {
    state.logLevel = e.target.value; state.logPage = 1; route(); return;
  }
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

  if (b.dataset.logtab) {
    state.logTab = b.dataset.logtab;
    if (b.dataset.logtab === 'entries') state.logPage = 1;
    route();
    return;
  }

  // ---- Telegram incident alerts (server re-checks role on every write)
  if (b.dataset.act === 'tg-save') {
    try {
      await api.put('/api/telegram/config', {
        bot_token: ($('#tg-token') || {}).value || '',
        chat_id: ($('#tg-chat') || {}).value || '',
        bot_username: ($('#tg-bot') || {}).value || '',
      });
      toast('Telegram configuration saved; token encrypted at rest');
      route();
    } catch (err) { toast(err.message, true); }
    return;
  }
  if (b.dataset.act === 'tg-delete') {
    const ok = await confirmDialog('Delete this bot?',
      'Xoá token và CHAT_ID khỏi dashboard. Bot trên Telegram vẫn tồn tại, nhưng '
      + 'hệ thống sẽ không gửi được cảnh báo cho tới khi cấu hình lại. Thao tác này không hoàn tác được.',
      'Delete bot');
    if (!ok) return;
    try {
      await api.del('/api/telegram/config');
      toast('Telegram bot configuration deleted');
      route();
    } catch (err) { toast(err.message, true); }
    return;
  }
  if (b.dataset.act === 'tg-test') {
    try { await api.post('/api/telegram/test'); toast('Test alert sent to the Telegram group'); }
    catch (err) { toast(err.message, true); }
    return;
  }
  if (b.dataset.act === 'tg-alert') {
    try {
      const r = await api.post('/api/telegram/alert', {
        rule: ($('#tg-rule') || {}).value || 'manual',
        summary: ($('#tg-summary') || {}).value || '',
      });
      toast('Incident ' + r.code + ' sent');
      route();
    } catch (err) { toast(err.message, true); }
    return;
  }
  if (b.dataset.act === 'tg-done') {
    try {
      await api.post('/api/telegram/incident', { code: b.dataset.code, status: 'fixed' });
      toast('Incident marked as fixed');
      route();
    } catch (err) { toast(err.message, true); }
    return;
  }

  // ---- B.9 discovery ledger actions
  if (b.dataset.act === 'disc-ack' || b.dataset.act === 'disc-ignore' ||
      b.dataset.act === 'disc-reopen') {
    const action = b.dataset.act === 'disc-ack' ? 'acknowledge'
      : (b.dataset.act === 'disc-ignore' ? 'ignore' : 'reopen');
    try {
      await api.post('/api/discovery/action', { id: Number(b.dataset.id), action });
      toast('Discovery record updated');
      route();
    } catch (err) { toast(err.message, true); }
    return;
  }

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

  // ---- cutting a host off, and putting it back
  if (b.dataset.act === 'lease-terminate' || b.dataset.act === 'lease-restore') {
    const cut = b.dataset.act === 'lease-terminate';
    const who = b.dataset.who || b.dataset.id;
    if (cut && !confirm(`Terminate the connection for ${who}?\n\n`
      + 'The host is marked offline, the cut is written to the lease history and an '
      + 'event is filed. Restore puts it back.')) return;
    b.disabled = true;
    try {
      await api.post(`/api/leases/${b.dataset.id}/${cut ? 'terminate' : 'restore'}`, {});
      toast(cut ? `${who} terminated` : `${who} restored`);
      route();
    } catch (err) {
      b.disabled = false;
      toast(err.message, true);
    }
    return;
  }

  // ---- operations buttons on the main dashboard
  if (b.dataset.act === 'ops-snapshot') {
    b.disabled = true;
    try {
      const r = await api.post('/api/ops/snapshot', {});
      toast(r.message || 'Snapshot queued');
    } catch (err) { toast(err.message, true); }
    b.disabled = false;
    return;
  }
  if (b.dataset.act === 'ops-retention') {
    const apply = b.dataset.mode === 'apply';
    if (apply && !confirm('Run a real data clearance?\n\nThis deletes history rows older than 180 days '
      + 'and old local files. Inventory, design, configuration and roster tables are protected. '
      + 'Use Dry run first.')) return;
    b.disabled = true;
    try {
      const r = await api.post('/api/ops/retention?mode=' + b.dataset.mode,
        apply ? { confirm: 'APPLY' } : {});
      toast(r.message || 'Queued');
    } catch (err) { toast(err.message, true); }
    b.disabled = false;
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
  if (b.dataset.act === 'shift-edit') {
    state.shiftEdit = Number(b.dataset.id);
    route();
    return;
  }

  if (b.dataset.act === 'shift-cancel') {
    state.shiftEdit = null;
    route();
    return;
  }

  if (b.dataset.act === 'shift-add') {
    const days = [...document.querySelectorAll('.sh-day:checked')].map((x) => x.value);
    const name = ($('#sh-name') || {}).value || '';
    const tgName = (($('#sh-telegram') || {}).value || '').trim().replace(/^@/, '');
    if (!name.trim()) { toast('A name is required', true); return; }
    if (!tgName) {
      markTg('Telegram username is required', true);
      toast('Telegram username is required', true);
      return;
    }
    // Ask the bot before writing: the roster must not contain a name the bot
    // has never seen, because an on-call entry that cannot be pinged is worse
    // than no entry at all.
    try {
      const chk = await api.get('/api/staff/lookup?username=' + encodeURIComponent(tgName));
      if (!chk.exists) {
        markTg('Username not exist', true);
        toast('Username not exist', true);
        return;
      }
      markTg('', false);
    } catch (err) { toast(err.message, true); return; }
    const editing = state.shiftEdit ? Number(state.shiftEdit) : 0;
    try {
      await api.put('/api/shifts', {
        ...(editing ? { id: editing } : {}),
        name: name.trim(), email: (tgName + '@telegram.local'),
        phone: ($('#sh-phone') || {}).value || '',
        telegram: tgName,
        role: ($('#sh-role') || {}).value || 'primary',
        starts_at: ($('#sh-start') || {}).value || '00:00',
        ends_at: ($('#sh-end') || {}).value || '23:59',
        days,
      });
      toast(editing ? 'Administrator updated' : 'Administrator added');
      state.shiftEdit = null;
      route();
    } catch (err) { toast(err.message, true); }
    return;
  }

  if (b.dataset.act === 'device-add') {
    const name = (($('#nd-name') || {}).value || '').trim();
    const ip = (($('#nd-ip') || {}).value || '').trim();
    const kind = (($('#nd-kind') || {}).value || 'router');
    if (!name) { toast('A name is required', true); return; }
    if (!ip) { toast('No free address left in the pool', true); return; }
    try {
      await api.post('/api/devices', { hostname: name, mgmt_ip: ip, kind });
      toast('Node ' + name + ' added at ' + ip);
      route();
    } catch (err) { toast(err.message, true); }
    return;
  }
  if (b.dataset.act === 'device-delete') {
    const ok = await confirmDialog('Delete ' + (b.dataset.who || 'this device') + '?',
      'Xoá thiết bị khỏi danh mục. Đây là thiết bị chưa nhận diện được nên không có dữ liệu '
      + 'lịch sử đáng giữ. Thao tác không hoàn tác được.', 'Delete');
    if (!ok) return;
    try {
      await api.del('/api/devices/' + b.dataset.id);
      toast('Device deleted');
      route();
    } catch (err) { toast(err.message, true); }
    return;
  }

  if (b.dataset.act === 'staff-approve' || b.dataset.act === 'staff-decline') {
    const approve = b.dataset.act === 'staff-approve';
    const ok = await confirmDialog(approve ? 'Approve this person?' : 'Decline this request?',
      approve
        ? 'Thêm người này vào lịch trực. Chỉ làm được khi bot đã thấy họ trong nhóm Telegram.'
        : 'Từ chối yêu cầu. Người này có thể gửi lại /addstaff sau.',
      approve ? 'Approve' : 'Decline');
    if (!ok) return;
    try {
      await api.post('/api/staff/requests/' + b.dataset.id,
        { action: approve ? 'approve' : 'decline' });
      toast(approve ? 'Added to the roster' : 'Request declined');
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
      // Re-render either way: the theme changes the page it is on, and the
      // layout has to redraw this card so the preview strip disappears the
      // moment Simple is chosen. Without this the button only wrote a setting
      // and looked dead.
      route();
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

// ------------------------------------------------------------- lab topology
// The picture follows the lab as it is wired: a controller on top, a redundant
// core, three access switches on 802.1Q trunks, and the PCs that hang off them.
// Which links exist comes from the design below; what state they are in comes
// from the database. A PC that holds no lease says "no address yet" instead of
// borrowing an address it never had, and a PC whose connection was terminated
// is drawn with its drop cut, because that is what happened to it.
const LAB = {
  switches: ['Switch 1', 'Switch 2', 'Switch 3'],
  // Two uplinks per router carry traffic; the third is the spare that only
  // lights up when a primary dies. Those spares are the redundancy the old
  // drawing was missing - without them the "fabric" is three single points of
  // failure wearing a switch icon.
  // One router carries one switch; the spare goes to the neighbouring switch so
  // the three pairs form a ring. An earlier version gave each router two
  // primaries, which quietly made r1 the carrier for Switch 1 *and* Switch 2 -
  // r1 ended up owning half the PCs while r3 owned none, and the picture said
  // the opposite. One primary each makes the drawing and the routing agree.
  uplinks: [
    { primary: [0], backup: [2] },   // r1 carries Switch 1, spare to Switch 3
    { primary: [1], backup: [0] },   // r2 carries Switch 2, spare to Switch 1
    { primary: [2], backup: [1] },   // r3 carries Switch 3, spare to Switch 2
  ],
  // MAC addresses are the join key: the poller only ever sees a MAC and an
  // address, so the design has to name the MAC it expects to see. The Docker
  // user simulator uses exactly these values.
  hosts: [
    { sw: 0, name: 'Accounting PC', vlan: 10, mac: '02:00:00:00:0A:01' },
    { sw: 0, name: 'HR PC',         vlan: 20, mac: '02:00:00:00:14:01' },
    { sw: 1, name: 'Sales PC',      vlan: 40, mac: '02:00:00:00:28:01' },
    { sw: 1, name: 'Access point',  vlan: 60, mac: '02:00:00:00:3C:01' },
    { sw: 1, name: 'File server',   vlan: 70, mac: '02:00:00:00:46:01' },
    { sw: 2, name: 'Director PC',   vlan: 50, mac: '02:00:00:00:32:01' },
    { sw: 2, name: 'NVR',           vlan: 95, mac: '02:00:00:00:5F:01' },
    { sw: 2, name: 'Camera',        vlan: 90, mac: '02:00:00:00:5A:01' },
  ],
};

function subnetOf(ip) {
  const p = String(ip || '').split('.');
  return p.length === 4 ? p.slice(0, 3).join('.') + '.0/24' : '';
}

// Where a line should stop so it touches a card's border instead of its centre.
function edgeOf(cx, cy, w, h, tx, ty) {
  const dx = tx - cx, dy = ty - cy;
  if (!dx && !dy) return [cx, cy];
  const s = Math.min(dx ? (w / 2) / Math.abs(dx) : Infinity,
                     dy ? (h / 2) / Math.abs(dy) : Infinity);
  return [cx + dx * s, cy + dy * s];
}

function labTopology(t, leases) {
  const ctrl = t.nodes.find((n) => n.effective_kind === 'server');
  const routers = t.nodes.filter((n) => n.effective_kind !== 'server' && n.effective_kind !== 'switch');

  // A router's own management address shows up in its neighbours' ARP tables,
  // so without this the diagram draws r1, r2, r3 and the controller a second
  // time as if they were workstations.
  const infra = new Set(t.nodes.map((n) => n.mgmt_ip));
  const designMac = new Set(LAB.hosts.map((h) => h.mac.toUpperCase()));

  // Every lease we hold for a designed PC, whatever state it is in. `online`
  // and `blocked` then decide which of three states the card is drawn in.
  const known = new Map();
  const unplaced = [];
  for (const l of leases) {
    const mac = String(l.mac || '').toUpperCase();
    if (designMac.has(mac)) {
      known.set(mac, l);
    } else if (Number(l.online) === 1 && !l.blocked && !infra.has(l.ip)) {
      unplaced.push(l);
    }
  }
  const isUp = (mac) => {
    const l = known.get(mac);
    return !!l && Number(l.online) === 1 && !l.blocked;
  };

  // Everything wrong with everything, keyed so a click can find it again.
  // Nothing here is invented: each entry is a row in the database that says so.
  const issues = {};
  const note = (key, title, sev, text) => {
    if (!issues[key]) issues[key] = { title, sev: 'warn', list: [] };
    issues[key].list.push({ sev, text });
    if (sev === 'err') issues[key].sev = 'err';
  };

  const W = 980;
  const CW = 178, CH = 48;          // controller
  const RW = 150, RH = 52;          // router
  const SW = 150, SH = 48;          // switch
  const HW = 150, HH = 46;          // host
  const CY = 52, RY = 176, SY = 306, H0 = 392, HGAP = 58;

  const cols = LAB.switches.map((_, i) => LAB.hosts.filter((h) => h.sw === i));
  const rows = Math.max(1, ...cols.map((c) => c.length));
  const H = H0 + (rows - 1) * HGAP + HH / 2 + 34;

  const out = [];
  const card = (cx, cy, w, h, cls, name, sub, tip, extra) => {
    const x = (cx - w / 2).toFixed(1), y = (cy - h / 2).toFixed(1);
    return `<g class="tp-dev" data-tip="${esc(tip)}"${extra || ''}>`
      + `<rect x="${x}" y="${y}" width="${w}" height="${h}" rx="7" class="tp-card ${cls}"/>`
      + `<text x="${cx.toFixed(1)}" y="${(cy + (sub ? -4 : 4)).toFixed(1)}" text-anchor="middle" class="tp-name">${esc(name)}</text>`
      + (sub ? `<text x="${cx.toFixed(1)}" y="${(cy + 11).toFixed(1)}" text-anchor="middle" class="tp-ip">${esc(sub)}</text>` : '')
      + '</g>';
  };
  // A ring outside the card, so a device with a problem reads as flagged even
  // at a glance and even when the card's own border already means something.
  const halo = (cx, cy, w, h, sev) => {
    const x = (cx - w / 2 - 4).toFixed(1), y = (cy - h / 2 - 4).toFixed(1);
    return `<rect x="${x}" y="${y}" width="${w + 8}" height="${h + 8}" rx="10"
             class="tp-halo ${sev}"/>`;
  };
  // The X. Sits on the card's top-right corner, half outside it, so it reads as
  // an annotation on the device rather than part of its label.
  const flag = (cx, cy, w, h, sev, key, tip) => {
    const x = cx + w / 2 - 3, y = cy - h / 2 + 3;
    const mark = sev === 'err'
      ? `<path d="M${(x - 3.9).toFixed(1)} ${(y - 3.9).toFixed(1)}L${(x + 3.9).toFixed(1)} ${(y + 3.9).toFixed(1)}`
        + `M${(x - 3.9).toFixed(1)} ${(y + 3.9).toFixed(1)}L${(x + 3.9).toFixed(1)} ${(y - 3.9).toFixed(1)}"/>`
      : `<path d="M${x.toFixed(1)} ${(y - 4.3).toFixed(1)}V${(y + 0.9).toFixed(1)}"/>`
        + `<circle cx="${x.toFixed(1)}" cy="${(y + 4.3).toFixed(1)}" r="1.35" class="tp-dot"/>`;
    return `<g class="tp-flag ${sev}" data-issue-key="${esc(key)}" data-tip="${esc(tip)}" role="button">`
      + `<circle cx="${x.toFixed(1)}" cy="${y.toFixed(1)}" r="9.4" class="tp-flag-bg"/>`
      + mark + '</g>';
  };
  const line = (ax, ay, bx, by, cls, path) => out.push(
    `<line x1="${ax.toFixed(1)}" y1="${ay.toFixed(1)}" x2="${bx.toFixed(1)}" y2="${by.toFixed(1)}" class="${cls}"`
    + (path ? ` data-path="${esc(path)}"` : '') + '/>');
  // A cut link is drawn with a cross on it. Two short strokes are enough to
  // read as "blocked" at any zoom, and they survive being printed in mono.
  const cross = (x, y, path) => {
    const s = 5;
    out.push(`<g class="tp-x"${path ? ` data-path="${esc(path)}"` : ''}>`
      + `<line x1="${(x - s).toFixed(1)}" y1="${(y - s).toFixed(1)}" x2="${(x + s).toFixed(1)}" y2="${(y + s).toFixed(1)}"/>`
      + `<line x1="${(x - s).toFixed(1)}" y1="${(y + s).toFixed(1)}" x2="${(x + s).toFixed(1)}" y2="${(y - s).toFixed(1)}"/>`
      + '</g>');
  };
  // Peering links between non-adjacent routers would otherwise run straight
  // through the card in the middle of the row.
  const arc = (ax, ay, bx, by, cls) => out.push(
    `<path d="M${ax.toFixed(1)} ${ay.toFixed(1)} Q${((ax + bx) / 2).toFixed(1)} ${(RY - 64).toFixed(1)} ${bx.toFixed(1)} ${by.toFixed(1)}" class="${cls}" fill="none"/>`);

  const ctrlPt = [W / 2, CY];
  const nR = Math.max(routers.length, 1);
  const rPt = routers.map((_, i) => [W * (i + 1) / (nR + 1), RY]);
  const sPt = LAB.switches.map((_, i) => [200 + i * 290, SY]);

  // Packets, drawn between the links and the cards so they disappear into a
  // switch and come out the other side instead of sliding over its label.
  // A host with no live lease emits nothing, which is the point: silence is
  // what "offline" looks like.
  const calm = typeof window !== 'undefined' && window.matchMedia
    && window.matchMedia('(prefers-reduced-motion: reduce)').matches;
  const packets = [];
  const packet = (path, delay, alt) => {
    if (calm) return;
    packets.push(`<circle class="tp-pkt${alt ? ' alt' : ''}" r="4">`
      + `<animateMotion dur="3.6s" begin="${delay.toFixed(2)}s" repeatCount="indefinite"`
      + ` path="${path}"/></circle>`);
  };
  // No modulo here. A router that is not part of the designed fabric (r4 is
  // added by hand) used to inherit uplinks[0] by wrapping around, which made
  // the diagram claim "Switch 1 loses its primary uplink" for a box that was
  // never cabled to it. An undesigned router now has no uplinks, so it draws
  // no switch links and reports no uplink fallout.
  const upOf = (i) => LAB.uplinks[i] || { primary: [], backup: [] };
  const macsOn = (si) => cols[si].map((h) => h.mac.toUpperCase());
  // Which router actually carries each switch right now: its live primary, or
  // - when that primary is down - the live spare. Tracing has to follow the
  // link the traffic really uses, or the highlight points at a dead cable.
  const via = LAB.switches.map((_, si) => {
    const carriers = (kind) => routers.map((r, i) => i).filter((i) => upOf(i)[kind].includes(si));
    const prim = carriers('primary');
    const upPrim = prim.filter((i) => routers[i].last_state === 'up');
    if (upPrim.length) return upPrim[0];
    const upSpare = carriers('backup').filter((i) => routers[i].last_state === 'up');
    return upSpare.length ? upSpare[0] : prim[0];
  });

  // ---- core to access: two live uplinks plus the spare that makes it a fabric
  routers.forEach((r, i) => {
    const plan = upOf(i);
    const [rx, ry] = rPt[i];
    const dead = r.last_state !== 'up';
    // A device we cannot identify has no known cabling. Drawing guessed links
    // to the switches would be an assertion the data does not support, so the
    // only path kept is the one that is measured: the controller's poll.
    if (!dead) {
      const draw = (list, cls, traced) => list.forEach((si) => {
        const [sx, sy] = sPt[si];
        const [ax, ay] = edgeOf(rx, ry, RW, RH, sx, sy);
        const [bx, by] = edgeOf(sx, sy, SW, SH, rx, ry);
        const path = traced && via[si] === i ? macsOn(si).join(' ') : '';
        line(ax, ay, bx, by, cls, path);
      });
      // the spare is a real path when the primary dies, so it is traced too
      draw(plan.backup, 'tp-backup', true);
      draw(plan.primary, 'tp-primary', true);
    }
  });

  // ---- control plane: the controller polls every router, so every router gets
  // a dashed link to it. Drawn before the cards so the cards sit on top.
  if (ctrl) {
    routers.forEach((r, i) => {
      const p = rPt[i];
      const path = LAB.switches.map((_, si) => si)
        .filter((si) => via[si] === i).flatMap((si) => macsOn(si)).join(' ');
      const [ax, ay] = edgeOf(ctrlPt[0], ctrlPt[1], CW, CH, p[0], p[1]);
      const [bx, by] = edgeOf(p[0], p[1], RW, RH, ctrlPt[0], ctrlPt[1]);
      if (r.last_state !== 'up') {
        // blinking, not crossed: the poll is still being attempted, it is just
        // not answered. A cross means "cut on purpose".
        line(ax, ay, bx, by, 'tp-ctrl-blink', path);
      } else {
        line(ax, ay, bx, by, 'tp-ctrl', path);
      }
    });
  }
  // ---- router to router, from the OSPF adjacencies that were accepted
  for (const l of t.links) {
    const ia = routers.findIndex((r) => r.id === l.a_device_id);
    const ib = routers.findIndex((r) => r.id === l.b_device_id);
    if (ia < 0 || ib < 0) continue;
    const a = rPt[ia], b = rPt[ib];
    const bad = routers[ia].last_state !== 'up' || routers[ib].last_state !== 'up';
    const cls = bad ? 'tp-blocked' : 'tp-peer';
    if (Math.abs(ia - ib) === 1) {
      const [ax, ay] = edgeOf(a[0], a[1], RW, RH, b[0], b[1]);
      const [bx, by] = edgeOf(b[0], b[1], RW, RH, a[0], a[1]);
      line(ax, ay, bx, by, cls);
      if (bad) cross((ax + bx) / 2, (ay + by) / 2);
    } else {
      const [ax, ay] = edgeOf(a[0], a[1], RW, RH, b[0], b[1]);
      const [bx, by] = edgeOf(b[0], b[1], RW, RH, a[0], a[1]);
      arc(ax, ay, bx, by, cls);
    }
  }

  // ---- access drops: one spine per switch, so the drops never cross a card
  LAB.switches.forEach((_, i) => {
    const [sx] = sPt[i];
    const list = cols[i];
    if (!list.length) return;
    const spine = sx + SW / 2 + 13;
    const dropY = SY + SH / 2;
    const lastY = H0 + (list.length - 1) * HGAP;
    const all = macsOn(i).join(' ');
    line(sx, dropY, sx, dropY + 16, 'tp-access', all);
    line(sx, dropY + 16, spine, dropY + 16, 'tp-access', all);
    line(spine, dropY + 16, spine, lastY, 'tp-access', all);
    list.forEach((h, j) => {
      const y = H0 + j * HGAP;
      const mac = h.mac.toUpperCase();
      const dead = known.has(mac) && known.get(mac).blocked;
      line(spine, y, sx + HW / 2, y, dead ? 'tp-blocked' : 'tp-access', mac);
      if (dead) cross((spine + sx + HW / 2) / 2, y, mac);
    });
  });

  // ---- cards on top of the lines. Rings go in their own layer: a halo
  // pushed after its card would paint over it instead of behind it.
  const marks = [];
  const rings = [];
  if (ctrl) {
    const all = LAB.switches.map((_, si) => macsOn(si)).flat().join(' ');
    marks.push(card(ctrlPt[0], ctrlPt[1], CW, CH, 'ctrl', ctrl.hostname, ctrl.mgmt_ip,
      ctrl.hostname + ' \u00b7 controller \u00b7 ' + ctrl.mgmt_ip,
      ` data-path="${esc(all)}"`));
  }
  routers.forEach((r, i) => {
    const [x, y] = rPt[i];
    const plan = upOf(i);
    const spare = plan.backup.map((si) => LAB.switches[si]).join(', ');
    const path = LAB.switches.map((_, si) => si)
      .filter((si) => via[si] === i).flatMap((si) => macsOn(si)).join(' ');
    const key = 'dev:' + r.id;
    const down = r.last_state !== 'up';
    if (down) {
      const carried = plan.primary.map((si) => LAB.switches[si]);
      const spared = plan.backup.map((si) => LAB.switches[si]);
      note(key, r.hostname, 'err', 'Not answering SNMP. The dashboard has no health data for it, '
        + 'and every uplink it carries is drawn blocked.');
      if (carried.length) {
        note(key, r.hostname, 'warn', carried.join(', ') + ' loses its primary uplink and fails over '
          + 'to the spare. Check that the spare is up before the traffic moves.');
      }
      if (spared.length) {
        note(key, r.hostname, 'warn', 'The spare path it gave ' + spared.join(', ')
          + ' is gone, so ' + (spared.length === 1 ? 'that switch has' : 'those switches have')
          + ' one fewer backup.');
      }
    }
    marks.push(card(x, y, RW, RH, down ? 'down' : 'rt', r.hostname, r.mgmt_ip,
      r.hostname + ' \u00b7 ' + (r.last_state || 'unknown') + ' \u00b7 ' + r.mgmt_ip
      + (spare ? ' \u00b7 spare uplink to ' + spare : ''),
      path ? ` data-path="${esc(path)}"` : ''));
    if (issues[key]) rings.push(halo(x, y, RW, RH, issues[key].sev));
    if (issues[key]) marks.push(flag(x, y, RW, RH, issues[key].sev, key, r.hostname + ' - click for details'));
  });
  LAB.switches.forEach((name, i) => {
    const [x, y] = sPt[i];
    // Every router with a cable to this switch, primary or spare. Counting only
    // the primaries reported "no uplink left" while a perfectly good spare was
    // still carrying traffic.
    const plan = routers.map((r, ri) => ({ r, ri }))
      .filter((q) => upOf(q.ri).primary.includes(i) || upOf(q.ri).backup.includes(i));
    const lost = plan.filter((q) => q.r.last_state !== 'up');
    const live = plan.length - lost.length;
    const key = 'sw:' + i;
    if (lost.length) {
      note(key, name, live === 0 ? 'err' : 'warn',
        lost.length + ' of ' + plan.length + ' uplinks down: '
        + lost.map((q) => q.r.hostname).join(', ') + '. '
        + (live === 0
          ? 'No path left to the core - everything behind this switch is cut off.'
          : live === 1 ? 'Traffic moves to the remaining uplink.'
          : 'Traffic moves to the ' + live + ' remaining uplinks.'));
    }
    const vlans = LAB.hosts.filter((h) => h.sw === i).map((h) => 'VLAN ' + h.vlan);
    marks.push(card(x, y, SW, SH, live === 0 ? 'sw down' : 'sw', name,
      lost.length ? lost.length + ' uplink' + (lost.length === 1 ? '' : 's') + ' down' : '802.1Q trunk',
      name + ' \u00b7 trunk \u00b7 ' + (vlans.join(', ') || 'no access port')
      + (lost.length ? ' \u00b7 ' + lost.length + ' uplink(s) down' : ''),
      ` data-path="${esc(macsOn(i).join(' '))}"`));
    if (issues[key]) {
      rings.push(halo(x, y, SW, SH, issues[key].sev));
      marks.push(flag(x, y, SW, SH, issues[key].sev, key, name + ' - click for details'));
    }
  });
  LAB.switches.forEach((_, i) => {
    const [sx] = sPt[i];
    cols[i].forEach((h, j) => {
      const y = H0 + j * HGAP;
      const mac = h.mac.toUpperCase();
      const l = known.get(mac);
      const live = isUp(mac);
      const dead = !!l && !!l.blocked;
      const key = 'host:' + mac;
      if (dead) {
        note(key, h.name, 'err', 'Connection terminated'
          + (l.blocked_at ? ' at ' + l.blocked_at + ' UTC' : '')
          + (l.blocked_by ? ' by ' + l.blocked_by : '')
          + '. The address ' + l.ip + ' is still reserved for it.');
        note(key, h.name, 'err', 'Restore it from Leases to put the host back on the wire.');
      } else if (l && !live) {
        note(key, h.name, 'warn', 'Not answering. Last seen ' + (l.last_seen || '?')
          + (l.seen_on ? ' on ' + l.seen_on : '') + ' at ' + l.ip + '.');
      } else if (!l) {
        note(key, h.name, 'warn', 'No address. This PC has never answered on the segment, '
          + 'so there is nothing to report about it yet.');
      }
      const cls = dead ? 'host cut' : (live ? 'host on' : 'host idle');
      const sub = dead ? 'terminated' : (live ? l.ip : (l ? l.ip : 'no address yet'));
      const tip = h.name + ' \u00b7 VLAN ' + h.vlan + ' \u00b7 ' + h.mac
        + (dead ? ' \u00b7 connection terminated'
          : live ? ' \u00b7 ' + l.ip + ' seen on ' + (l.seen_on || '?')
          : ' \u00b7 no live lease');
      marks.push(card(sx, y, HW, HH, cls, h.name, sub, tip,
        ` data-mac="${mac}" data-path="${mac}"`));
      // A packet climbing host -> switch -> router -> controller.
      // It follows the wire, not the straight line between the boxes: out of
      // the card, right along its stub, up the spine, then into the switch.
      // Cutting the corner made the dot float beside the cable it was supposed
      // to be travelling on.
      if (live && ctrl) {
        const ri = via[i];
        const rp = rPt[ri] || ctrlPt;
        const alt = !upOf(ri).primary.includes(i);
        const spine = sx + SW / 2 + 13;
        const dropY = SY + SH / 2;
        packet(`M${sx.toFixed(1)} ${y.toFixed(1)} L${(sx + HW / 2).toFixed(1)} ${y.toFixed(1)} `
          + `L${spine.toFixed(1)} ${y.toFixed(1)} L${spine.toFixed(1)} ${(dropY + 16).toFixed(1)} `
          + `L${sx.toFixed(1)} ${(dropY + 16).toFixed(1)} L${sx.toFixed(1)} ${SY} `
          + `L${rp[0].toFixed(1)} ${rp[1].toFixed(1)} L${ctrlPt[0].toFixed(1)} ${ctrlPt[1].toFixed(1)}`,
          // phases spread evenly over the cycle, so the picture always has
          // traffic on it instead of eight dots that all leave at once
          ((i * 3 + j) * 0.45) % 3.6, alt);
      }
      if (issues[key]) {
        rings.push(halo(sx, y, HW, HH, issues[key].sev));
        marks.push(flag(sx, y, HW, HH, issues[key].sev, key, h.name + ' - click for details'));
      }
    });
  });

  const svg = `<svg viewBox="0 0 ${W} ${H}" style="width:${state.zoom * 100}%" role="img" aria-label="Lab topology">`
    + out.join('') + packets.join('') + rings.join('') + marks.join('') + '</svg>';

  const rowsOut = [];
  for (const l of leases) {
    if (infra.has(l.ip)) continue;
    const mac = String(l.mac || '').toUpperCase();
    rowsOut.push({ ip: l.ip, mac: l.mac, name: l.device_name || l.hostname || '',
      subnet: l.subnet || subnetOf(l.ip), online: Number(l.online) === 1,
      blocked: !!l.blocked, seen: l.seen_on || '',
      key: designMac.has(mac) ? 'host:' + mac : '' });
  }
  const online = LAB.hosts.filter((h) => isUp(h.mac.toUpperCase())).length;
  const terminated = LAB.hosts.filter((h) => {
    const l = known.get(h.mac.toUpperCase());
    return l && l.blocked;
  }).length;
  return {
    svg, rows: rowsOut, issues,
    hosts: online, planned: LAB.hosts.length,
    terminated, idle: LAB.hosts.length - online - terminated,
    unplaced: unplaced.length,
    routersDown: routers.filter((r) => r.last_state !== 'up').length,
    flagged: Object.keys(issues).length,
  };
}

// ------------------------------------------------------------- issue popover
// One panel, reused. Anchored to whatever was clicked - an SVG badge or a table
// row - because the SVG is replaced on every poll and a popover parented to it
// would vanish mid-read.
function closeIssues() {
  const p = document.querySelector('.tp-pop');
  if (p) p.remove();
}

function showIssues(key, anchor) {
  const already = document.querySelector('.tp-pop');
  const same = already && already.dataset.key === key;
  closeIssues();
  if (same) return;
  const entry = (state.topoIssues || {})[key];
  if (!entry) return;
  const pop = document.createElement('div');
  pop.className = 'tp-pop ' + entry.sev;
  pop.dataset.key = key;
  pop.innerHTML = `<div class="tp-pop-h"><i></i>${esc(entry.title)}</div>`
    + entry.list.map((i) => `<div class="tp-pop-r ${i.sev}"><span>${esc(i.text)}</span></div>`).join('');
  document.body.appendChild(pop);

  const r = anchor.getBoundingClientRect();
  const pw = pop.offsetWidth, ph = pop.offsetHeight;
  const pad = 12;
  // Under the badge, not beside it: a panel that opens to the right covers the
  // card it is describing, and the first thing the operator does is look for
  // that card to confirm which device they clicked.
  let left = r.left + r.width / 2 - pw / 2;
  let top = r.bottom + 8;
  if (left < pad) left = pad;
  if (left + pw > window.innerWidth - pad) left = window.innerWidth - pw - pad;
  if (top + ph > window.innerHeight - pad) top = Math.max(pad, r.top - ph - 8);
  pop.style.left = left + 'px';
  pop.style.top = top + 'px';
}

// Clicking a PC traces its route. Handled here rather than on the card itself
// because the SVG is replaced on every poll.
function onTopoClick(e) {
  const flag = e.target.closest && e.target.closest('[data-issue-key]');
  if (flag) {
    showIssues(flag.dataset.issueKey, flag);
    return;
  }
  closeIssues();
  const dev = e.target.closest && e.target.closest('.tp-dev[data-mac]');
  if (!dev) {
    if (state.topoFocus) focusPath(state.topoFocus);
    return;
  }
  focusPath(dev.dataset.mac);
}

// Clicking a PC lights the route it actually takes - controller, the router
// carrying its uplink, its switch, its drop - and dims everything else. Same
// click again clears it. No re-render, so it works while the poller is
// replacing the data underneath.
function focusPath(mac, keep) {
  const svg = document.querySelector('.topo-box svg');
  if (!svg) return;
  // `keep` re-applies the current trace after a refresh has replaced the SVG;
  // without it every 5 s tick would clear what the operator just clicked.
  const on = keep ? mac : (state.topoFocus === mac ? null : mac);
  state.topoFocus = on;
  svg.querySelectorAll('[data-path]').forEach((el) => {
    el.classList.toggle('lit', !!on && (' ' + el.dataset.path + ' ').includes(' ' + on + ' '));
  });
  svg.classList.toggle('tp-focus', !!on);
}

// Ten fixed categories, matching the SQL predicates in handleLogs. Kept here so
// the label and the filter cannot drift apart.
const LOG_CATS = [
  ['all', 'All categories'],
  ['state', '1. Device state changes'],
  ['reach', '2. Unreachable'],
  ['reboot', '3. Restart / power loss'],
  ['config', '4. Config & routing'],
  ['security', '5. Security & login'],
  ['terminal', '6. Terminal commands'],
  ['lease', '7. Leases & ARP'],
  ['alert', '8. Alerts & delivery'],
  ['discovery', '9. Discovery'],
  ['other', '10. Other'],
];

function logTabs(active) {
  const t = (k, l) => `<button class="act${k === active ? ' primary' : ''}" data-logtab="${k}">${l}</button>`;
  return `<div class="row" style="margin-bottom:10px;gap:6px">`
    + t('entries', 'Log entries') + t('analysis', 'Analysis') + t('alerts', 'Alerts & delivery')
    + `</div>`;
}

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

    // Operational data comes from the controller's SSH-deployed scripts, read
    // through the API. One round of three calls keeps the 5 s tick cheap.
    const [ops, tele, act] = await Promise.all([
      api.get('/api/ops').catch(() => ({})),
      api.get('/api/ops/telemetry').catch(() => ({ nodes: [] })),
      api.get('/api/ops/activity').catch(() => ({ events: [], deliveries: [], rules: [] })),
    ]);
    const sshByIp = new Map((tele.nodes || []).map((n) => [n.ip, n]));

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
      const ssh = sshByIp.get(d.mgmt_ip);
      const sshUp = !!(ssh && ssh.ssh_reachable);
      // This card IS the controller. A TCP/22 probe of the host from inside
      // its own container says nothing useful, and opening a terminal to the
      // box running the dashboard is not an operation we want one click away.
      const isLocal = (ssh && ssh.kind === 'server') || d.hostname === 'controller';
      const badge = isLocal
        ? '<span class="ssh-chip local" title="This is the machine running the dashboard">Local host</span>'
        : `<span class="ssh-chip ${sshUp ? 'ok' : 'err'}" title="TCP/22 probe from the controller${ssh ? ' · ' + ssh.ssh_probe_ms + ' ms' : ''}">SSH ${sshUp ? 'up' : 'down'}</span>`;
      // A box that never answered is not "down": we do not know what it is.
      // It gets a pulsing yellow question mark instead of a red failure badge,
      // and no terminal button, because a shell to a device we cannot identify
      // is a guess, not an operation.
      const unknown = d.last_state !== 'up';
      const foot = isLocal
        ? '<span class="muted" style="font-size:11.5px">Managed locally - no remote console</span>'
        : (unknown
            ? '<span class="muted" style="font-size:11.5px">Unidentified - console disabled</span>'
            : `<button class="act" data-act="term" data-id="${d.id}">Terminal console</button>`);
      return `<div class="dev-card${unknown ? ' down unknown' : ''}" data-id="${d.id}">
        <div class="dev-head"><b>${esc(d.hostname)}</b>
          <span class="row" style="gap:6px">
            ${badge}
            ${unknown
              ? '<span class="pill q" title="No response - device not identified">?</span>'
              : '<span class="pill up">up</span>'}
          </span></div>
        <canvas class="mini" data-dev="${d.id}"></canvas>
        <div class="dev-nums">
          <span>CPU <b>${c == null ? '–' : c + '%'}</b></span>
          <span>MEM <b>${m == null ? '–' : m + '%'}</b></span>
          <span>DSK <b>${k == null ? '–' : k + '%'}</b></span>
        </div>
        <div class="dev-foot">
          ${foot}
          <span class="muted mono" style="font-size:11px">${esc(d.mgmt_ip)}</span>
        </div>
      </div>`;
    }).join('');

    const devices = `
      <div class="stat-grid">
        <div class="panel"><div class="p-title">Number of devices</div><div class="p-big">${devs.length}</div></div>
        <div class="panel ${down ? 'err' : 'ok'}"><div class="p-title">Unavailable</div><div class="p-big">${down}</div></div>
      </div>
      <div class="dev-grid">${cards}</div>`;

    const bk = ops.backup || {}, ret = ops.retention || {};
    const duck = ops.duckdns || {}, tls = ops.tls || {}, fw = ops.firewall || {};
    const net = ops.network || {};
    const tun = ops.cloudflared || {};
    const tunCell = tun.url
      ? `<a href="${esc(tun.url)}" target="_blank" rel="noopener">${esc(String(tun.url).replace('https://', ''))}</a>`
      : '—';
    const kv = (k, v, cls) => `<div class="op-kv"><span>${esc(k)}</span>`
      + `<b class="${cls || ''}">${esc(v == null || v === '' ? '—' : v)}</b></div>`;
    const feedRow = (e) => `<div class="feed-row ${esc(e.severity || '')}">`
      + `<span class="mono">${esc(String(e.ts || '').slice(5, 16))}</span> `
      + `<span>${esc(e.message || e.status)}</span></div>`;
    const uptime = (s) => s == null ? '—'
      : (s > 86400 ? Math.floor(s / 86400) + 'd ' + Math.floor(s % 86400 / 3600) + 'h'
        : Math.floor(s / 3600) + 'h ' + Math.floor(s % 3600 / 60) + 'm');

    const operations = `
      <div class="op-grid">
        <div class="panel op">
          <div class="p-title">Trust &amp; access</div>
          ${kv('DuckDNS', duck.ok ? duck.domain + ' · ' + duck.public_ip : duck.state, duck.ok ? 'ok' : 'err')}
          ${kv('HTTPS', tls.ok ? 'issued · ' + String(tls.expires || '').slice(0, 22) : tls.state, tls.ok ? 'ok' : 'err')}
          ${kv('Firewall', fw.state, fw.state === 'active' ? 'ok' : 'err')}
          ${kv('Rollback guard', fw.deadline ? 'armed' : (fw.state === 'active' ? 'confirmed' : 'idle'))}
          ${kv('SQLite', ops.db_size)}
          ${kv('Lab LAN / upstream', (net.lan_ip || '—') + ' / ' + (net.wan_ip || '—'))}
          ${kv('Local DNS override', net.local_override)}
          <div class="op-kv tunnel-link"><span>Cloudflare tunnel</span>${tunCell}</div>
          ${kv('Tunnel state', tun.state, tun.ok ? 'ok' : 'err')}
          ${kv('Inventory', (ops.inventory || []).map((n) => n.hostname).join(', '))}
          <p class="muted" style="font-size:11.5px;margin:7px 0 0">${esc(net.forward_hint || 'Public IP changes are picked up automatically. Port forwarding is configured on the router/VMware host, outside SSH reach.')}</p>
        </div>
        <div class="panel op">
          <div class="p-title">Backup &amp; restore</div>
          ${kv('Last snapshot', bk.last_snapshot_at || bk.last_snapshot)}
          ${kv('Size', bk.last_snapshot_bytes ? (bk.last_snapshot_bytes / 1024).toFixed(1) + ' KB' : null)}
          ${kv('Snapshots', bk.snapshot_count)}
          ${kv('Last restore', bk.last_restore)}
          <div class="op-actions"><button class="act" data-act="ops-snapshot">Take snapshot</button></div>
          <p class="muted" style="font-size:11.5px;margin:7px 0 0">Queued to the SSH-deployed controller worker. Restore stays a separate, manually confirmed playbook.</p>
        </div>
        <div class="panel op">
          <div class="p-title">Maintenance &amp; retention</div>
          ${kv('Cycle', '6 months · 1 Jan / 1 Jul, 03:00 UTC')}
          ${kv('Next run', ops.next_clearance)}
          ${kv('Last run', ret.state || 'not run')}
          ${kv('Eligible rows', ret.rows_total)}
          ${kv('Keep window', (ops.retention_days || 180) + ' days')}
          <div class="op-actions">
            <button class="act" data-act="ops-retention" data-mode="dry-run">Dry run</button>
            <button class="act danger" data-act="ops-retention" data-mode="apply">Run clearance</button>
          </div>
          <p class="muted" style="font-size:11.5px;margin:7px 0 0">Inventory, design, config and roster tables are protected. Dry run deletes nothing.</p>
        </div>
        <div class="panel op">
          <div class="p-title">Node telemetry</div>
          <table class="op-table"><thead><tr>
            <th>Node</th><th>SSH</th><th>CPU</th><th>RAM</th><th>RTT</th><th>Uptime</th>
          </tr></thead><tbody>${(tele.nodes || []).map((n) => `<tr>
            <td><b>${esc(n.hostname)}</b></td>
            <td><span class="dot ${n.ssh_reachable ? 'ok' : 'err'}"></span>${n.ssh_reachable ? 'up' : 'down'}</td>
            <td>${n.cpu_pct == null ? '—' : Math.round(n.cpu_pct) + '%'}</td>
            <td>${n.mem_pct == null ? '—' : Math.round(n.mem_pct) + '%'}</td>
            <td>${n.rtt_ms == null ? '—' : n.rtt_ms + ' ms'}</td>
            <td>${uptime(n.uptime_s)}</td>
          </tr>`).join('')}</tbody></table>
        </div>
      </div>
      <div class="panel op" style="margin-top:9px">
        <div class="p-title">Alerts &amp; audit feed</div>
        <div class="op-feed">
          <div class="op-feed-col"><h4>Recent system errors</h4>
            ${(act.events || []).slice(0, 7).map(feedRow).join('') || '<span class="muted">No events recorded</span>'}
          </div>
          <div class="op-feed-col"><h4>Alert pipeline &amp; deliveries</h4>
            ${(act.rules || []).map((r) => `<div class="feed-row"><span class="dot ${r.enabled ? 'ok' : ''}"></span>${esc(r.name)} <span class="muted">≥ ${esc(r.severity)}</span></div>`).join('')}
            ${(act.deliveries || []).slice(0, 4).map((d) => `<div class="feed-row"><span class="mono">${esc(String(d.ts || '').slice(5, 16))}</span> ${esc(d.status)} · ${esc(d.channel)} → ${esc(d.recipient)}</div>`).join('') || '<span class="muted">No deliveries yet</span>'}
          </div>
        </div>
      </div>`;

    $('#view').innerHTML =
      section('operations', 'Operations', operations, true) +
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
    const admin = state.me.role === 'admin';
    const [t, ld, pool] = await Promise.all([
      api.get('/api/topology'),
      // every lease, not only the online ones: a terminated host is offline by
      // definition, and filtering here would erase the very state the diagram
      // is supposed to show
      api.get('/api/leases?limit=300').catch(() => ({ leases: [] })),
      // Free addresses for the "add a node" form. Read-only and cheap; only an
      // admin sees the form, but a viewer's page should not 403 in the console.
      api.get('/api/devices/pool').catch(() => ({ ips: [] })),
    ]);
    const built = labTopology(t, (ld && ld.leases) || []);
    state.topoIssues = built.issues;

    const errs = Object.values(built.issues).filter((i) => i.sev === 'err').length;
    const warns = Object.keys(built.issues).length - errs;
    const stats = [
      `<span class="tp-stat ok"><b>${built.hosts}</b> online</span>`,
      built.terminated ? `<span class="tp-stat err"><b>${built.terminated}</b> terminated</span>` : '',
      built.idle ? `<span class="tp-stat"><b>${built.idle}</b> no address</span>` : '',
      built.routersDown ? `<span class="tp-stat err"><b>${built.routersDown}</b> router${built.routersDown === 1 ? '' : 's'} down</span>` : '',
      errs ? `<span class="tp-stat err"><b>${errs}</b> fault${errs === 1 ? '' : 's'}</span>` : '',
      warns ? `<span class="tp-stat warn"><b>${warns}</b> warning${warns === 1 ? '' : 's'}</span>` : '',
    ].filter(Boolean).join('');

    const topo = `
      <div class="row" style="justify-content:space-between;margin-bottom:9px">
        <div class="zoom">
          <button data-zoom="out" title="Zoom out">−</button>
          <span data-zlabel>${Math.round(state.zoom * 100)}%</span>
          <button data-zoom="in" title="Zoom in">+</button>
          <button data-zoom="reset" title="Reset">⟲</button>
        </div>
        <div class="tp-stats">${stats}</div>
      </div>
      <div class="legend" style="margin:0 0 9px">
        <span><i style="background:var(--accent)"></i>live uplink</span>
        <span><i class="dash ok"></i>spare uplink (redundancy)</span>
        <span><i class="dash accent"></i>controller poll</span>
        <span><i class="dash"></i>OSPF adjacency</span>
        <span><i style="background:var(--ok)"></i>PC holding a lease</span>
        <span><i class="dash err"></i>cut / blocked path</span>
      </div>
      <div class="topo-box">${built.svg}</div>
      <p class="muted" style="font-size:12.5px;margin-top:9px">
        Switches, trunks and the spare uplinks are the lab's wiring; the routers come from
        discovery and every PC address comes from the <a href="#/leases">leases</a> table.
        ${built.hosts} of ${built.planned} planned PC${built.planned === 1 ? '' : 's'} hold an address${built.hosts === 1 ? '' : 's'} right now.
        ${built.terminated ? `A terminated PC is drawn with its drop cut until it is restored from the leases view.` : ''}
        ${built.unplaced ? `${built.unplaced} host${built.unplaced === 1 ? '' : 's'} on the wire ${built.unplaced === 1 ? 'is' : 'are'} not in the design and appear only in the table below.` : ''}
        Click a PC to trace its route.
      </p>`;

    // The same X the diagram uses, as real SVG so it stays crisp in a table row.
    // Clicking it opens the identical panel: one description of a fault, two
    // places to reach it from.
    const xIcon = (sev, key) => `<button class="issue-x ${sev}" data-issue-key="${esc(key)}"
        title="${sev === 'err' ? 'fault - click for details' : 'warning - click for details'}"
        aria-label="show the problem"><svg viewBox="0 0 16 16" width="15" height="15">
        <circle cx="8" cy="8" r="7.3" class="ix-bg"/>${sev === 'err'
          ? '<path d="M5.3 5.3l5.4 5.4M10.7 5.3l-5.4 5.4" class="ix-mark"/>'
          : '<path d="M8 4.1v4.5" class="ix-mark"/><circle cx="8" cy="11.3" r=".95" class="ix-mark-f"/>'}
        </svg></button>`;

    const nodes = `${admin ? `<div class="panel" style="margin-bottom:9px;padding:11px 13px">
      <div class="p-title" style="text-align:left;margin-bottom:8px">Add a node</div>
      <div style="display:grid;grid-template-columns:repeat(auto-fit,minmax(170px,1fr));gap:9px">
        <label class="muted" style="font-size:12px">Name<br>
          <input id="nd-name" placeholder="lab-pc-01" style="width:100%;margin-top:4px"></label>
        <label class="muted" style="font-size:12px">Address<br>
          <select id="nd-ip" style="width:100%;margin-top:4px">
            ${(pool.ips || []).map((ip) => `<option value="${ip}">${ip}</option>`).join('') || '<option value="">no free address</option>'}
          </select></label>
        <label class="muted" style="font-size:12px">Class<br>
          <select id="nd-kind" style="width:100%;margin-top:4px">
            <option value="router">router</option><option value="switch">switch</option>
            <option value="l3switch">l3switch</option><option value="server">server</option>
          </select></label>
      </div>
      <div class="row" style="margin-top:10px;justify-content:flex-end">
        <button class="act primary" data-act="device-add">Add node</button>
      </div>
      <p class="muted" style="font-size:11.5px;margin-top:7px">
        ${esc(pool.note || '')} Thêm node mô phỏng thiết bị ngoài; node chưa từng trả lời sẽ
        hiện dấu hỏi vàng và bị xoá sau 180 giây nếu không phản hồi.
      </p>
    </div>` : ''}<div class="panel"><table>
      <thead><tr><th>Host</th><th>Address</th><th>Class</th><th>State</th><th></th><th></th><th></th></tr></thead>
      <tbody>${t.nodes.map((n) => {
        const issue = built.issues['dev:' + n.id];
        return `<tr${issue ? ' class="cut"' : ''}>
        <td><b>${esc(n.hostname)}</b></td><td class="mono">${esc(n.mgmt_ip)}</td><td>${esc(n.effective_kind)}</td>
        <td><span class="pill ${n.last_state === 'up' ? 'up' : 'down'}">${esc(n.last_state || 'unknown')}</span></td>
        <td>${issue ? xIcon(issue.sev, 'dev:' + n.id) : ''}</td>
        <td>${n.effective_kind === 'server' ? '' : `<button class="act" data-act="term" data-id="${n.id}">terminal</button>`}</td>
        <td>${n.protected || n.effective_kind === 'server'
          ? '<span class="muted" style="font-size:11.5px" title="infrastructure - never auto-deleted">protected</span>'
          : (n.last_state !== 'up'
            ? `<button class="act danger" data-act="device-delete" data-id="${n.id}"
                       data-who="${esc(n.hostname)}">Delete</button>`
            : '<span class="muted" style="font-size:11.5px">in use</span>')}</td>
      </tr>`;
      }).join('')}</tbody></table></div>`;

    const map = `<div class="panel"><table>
      <thead><tr><th>Host</th><th>Address</th><th>Physical address</th><th>Subnet</th><th>State</th><th></th></tr></thead>
      <tbody>${built.rows.length ? built.rows.map((r) => {
        const issue = r.key ? built.issues[r.key] : null;
        return `<tr${r.blocked ? ' class="cut"' : ''}>
        <td><b>${esc(r.name || '\u2014')}</b></td><td class="mono">${esc(r.ip)}</td>
        <td class="mono" style="font-size:11.5px">${esc(r.mac)}</td>
        <td class="mono" style="font-size:11.5px">${esc(r.subnet)}</td>
        <td>${r.blocked
          ? '<span class="pill err">terminated</span>'
          : `<span class="pill ${r.online ? 'up' : 'down'}">${r.online ? 'online' : 'offline'}</span>`}</td>
        <td>${issue ? xIcon(issue.sev, r.key) : ''}</td>
      </tr>`;
      }).join('') : '<tr><td colspan="6" class="muted" style="text-align:center;padding:22px">No lease recorded yet. A PC appears here after the poller reads its router\u2019s ARP table.</td></tr>'}</tbody></table></div>`;

    // The view redraws every 5 s, so anything the operator set has to survive
    // the redraw: where the diagram was scrolled to, and which route they
    // traced. Losing either on a tick would make the page unusable.
    const prev = $('.topo-box');
    const scroll = prev ? [prev.scrollLeft, prev.scrollTop] : [0, 0];

    $('#view').innerHTML = section('topo', 'Topology', topo, true)
      + section('nodes', 'Nodes', nodes, true)
      + section('map', 'Address map', map, false);

    const box = $('.topo-box');
    if (box) { box.scrollLeft = scroll[0]; box.scrollTop = scroll[1]; }
    if (state.topoFocus) focusPath(state.topoFocus, true);
  },


  async logs() {
    // Logs, Analysis and Alerts are one section now: they all answer "what
    // happened and did anyone hear about it". The tab bar is the only place
    // that knows they used to be separate pages.
    const tab = state.logTab || 'entries';
    if (tab === 'analysis' || tab === 'alerts') {
      if (tab === 'analysis') await views.analysis(); else await views.alerts();
      const inner = $('#view').innerHTML;
      $('#view').innerHTML = logTabs(tab) + inner;
      return;
    }
    const page = state.logPage || 1;
    const level = state.logLevel || 'all';
    const cat = state.logCat || 'all';
    const q = state.logQuery || '';
    const data = await api.get('/api/logs?page=' + page + '&per_page=10&level=' + level +
                               '&cat=' + encodeURIComponent(cat) + '&q=' + encodeURIComponent(q));
    state.logPage = data.page;

    const CHIP = { info: 'chip-info', warning: 'chip-warn', critical: 'chip-crit' };
    // Only a critical entry gets an action. An informational line is a record of
    // something that happened, not a decision waiting to be made, and offering
    // Block/Ignore on it invited people to silence events they had not read.
    // Warnings stay actionable only through the alert rules, not from here.
    const rows = data.items.length
      ? data.items.map((it) => `
        <tr>
          <td class="muted mono">${esc(it.ts)}</td>
          <td>${esc(it.device || '\u2014')}</td>
          <td><span class="chip ${CHIP[it.level] || 'chip-info'}">${esc(it.level)}</span></td>
          <td class="log-msg">${esc(it.message)}</td>
          <td>${it.level === 'critical' ? `
            <select class="log-act" data-sig="${esc(it.sig)}">
              <option value="none"${it.action === '' ? ' selected' : ''}>\u2014</option>
              <option value="allow"${it.action === 'allow' ? ' selected' : ''}>Allow</option>
              <option value="ignore"${it.action === 'ignore' ? ' selected' : ''}>Ignore</option>
              <option value="block"${it.action === 'block' ? ' selected' : ''}>Block</option>
            </select>` : '<span class="muted">\u2014</span>'}</td>
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
        <select id="log-cat" style="height:34px;min-width:190px">
          ${LOG_CATS.map(([k, l]) => `<option value="${k}"${k === cat ? ' selected' : ''}>${l}</option>`).join('')}
        </select>
        <select id="log-level" style="height:34px">
          <option value="all"${level === 'all' ? ' selected' : ''}>All levels</option>
          <option value="info"${level === 'info' ? ' selected' : ''}>Info</option>
          <option value="warning"${level === 'warning' ? ' selected' : ''}>Warning</option>
          <option value="critical"${level === 'critical' ? ' selected' : ''}>Critical</option>
        </select>
        <input id="log-q" placeholder="search message or device" value="${esc(q)}" style="width:240px">
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
        Restarts and power loss are logged here too \u2014 look for <i>uptime \u2026 restart</i> or
        <i>power loss</i> in the message. Only a <b>critical</b> entry offers an action:
        <b>Block</b> suppresses that event class from now on &nbsp;\u00b7&nbsp;
        <b>Ignore</b> hides it from this view &nbsp;\u00b7&nbsp;
        <b>Allow</b> marks it as expected.
      </p>`;

    $('#view').innerHTML = logTabs('entries') + section('logs', 'Logs', body, true);
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
          <p class="muted" style="font-size:12px;margin-top:9px">Simple hides the capacity charts and the per-device sparklines, and tightens the device cards so more hosts fit on one screen.</p>
          <div class="grid3 capacity" style="margin-top:10px">
            <div class="card" style="padding:8px"><div class="k" style="font-size:11px">In</div>
              <div class="muted" style="font-size:12px">42.1 Mbps</div></div>
            <div class="card" style="padding:8px"><div class="k" style="font-size:11px">Out</div>
              <div class="muted" style="font-size:12px">8.4 Mbps</div></div>
            <div class="card" style="padding:8px"><div class="k" style="font-size:11px">CPU</div>
              <div class="muted" style="font-size:12px">17 %</div></div>
          </div>
          <canvas class="mini" height="26" style="width:100%;margin-top:8px"></canvas>
          <p class="muted" style="font-size:11.5px;margin-top:6px">
            ${m.layout === 'simple' ? 'Simple is on: the strip above is hidden everywhere.' : 'Detailed is on: charts and sparklines are shown.'}
          </p>
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

    // ---- Telegram incident alerts (runbook 4.1 / 4.5)
    // Admin gets the self-input form; everyone else gets a read-only summary
    // with no input, no button and no token value. The server also enforces
    // 403 on PUT /api/telegram/config (4.1c defense in depth).
    const t = await api.get('/api/telegram/status').catch(() => ({}));
    const chip = (ok, label) => `<span class="chip ${ok ? 'chip-ok' : 'chip-warn'}">${label}</span>`;
    // Once a bot is configured the inputs disappear: leaving them on screen
    // invites someone to paste a token into a form that no longer needs one,
    // and the token is the bot's password. Only "send a test" and the
    // destructive "delete this bot" remain.
    const configured = !!(t.token_set && t.chat_id_set);
    const tgForm = !admin ? '' : (configured ? `
      <div class="panel" style="margin-top:9px">
        <div class="p-title" style="text-align:left;margin-bottom:9px">Bot configuration (admin only)</div>
        <p class="muted" style="font-size:12.5px;margin:0 0 10px">
          Bot đã được cấu hình. Token và CHAT_ID không hiển thị lại.
          ${t.env_override ? 'Giá trị đang đọc từ biến môi trường nên không xoá được ở đây.' : ''}
        </p>
        <div class="row" style="justify-content:flex-end">
          <button class="act" data-act="tg-test">Send test alert</button>
          <button class="act danger" data-act="tg-delete" ${t.env_override ? 'disabled title="Token đang lấy từ env"' : ''}>Delete this bot</button>
        </div>
      </div>` : `
      <div class="panel" style="margin-top:9px">
        <div class="p-title" style="text-align:left;margin-bottom:9px">Bot configuration (admin only)</div>
        <div style="display:grid;grid-template-columns:repeat(auto-fit,minmax(200px,1fr));gap:9px">
          <label class="muted" style="font-size:12px">Telegram Bot Token<br>
            <input id="tg-token" type="password" autocomplete="off"
                   placeholder="YOUR_BOT_TOKEN_HERE" style="width:100%;margin-top:4px"></label>
          <label class="muted" style="font-size:12px">Group CHAT_ID<br>
            <input id="tg-chat" placeholder="YOUR_CHAT_ID_HERE" style="width:100%;margin-top:4px"></label>
          <label class="muted" style="font-size:12px">Bot username<br>
            <input id="tg-bot" placeholder="YOUR_BOT_USERNAME_HERE" style="width:100%;margin-top:4px"></label>
        </div>
        <p class="muted" style="font-size:11.5px;margin-top:8px">
          Giá trị thật lấy từ BotFather; token được mã hoá AES-GCM trước khi lưu.
          Sau khi lưu, các ô nhập sẽ được ẩn và chỉ còn nút gửi thử và nút xoá bot.
        </p>
        <div class="row" style="margin-top:11px;justify-content:flex-end">
          <button class="act primary" data-act="tg-save">Save configuration</button>
        </div>
      </div>`);
    const tgBody = `
      <div class="panel" style="padding:12px 14px">
        <div class="row" style="gap:8px;flex-wrap:wrap">
          ${chip(t.token_set, t.token_set ? 'Token configured (' + esc(t.token_source || 'set') + ')' : 'Token not configured')}
          ${chip(t.chat_id_set, t.chat_id_set ? 'CHAT_ID configured' : 'CHAT_ID not configured')}
          ${chip(!!t.bot_username, t.bot_username ? '@' + esc(t.bot_username) : 'Bot username not configured')}
          ${chip((t.followers || []).length > 0, (t.followers || []).length + ' follower(s)')}
        </div>
        <p class="muted" style="font-size:12.5px;margin-top:9px">
          <b>Join nhóm Telegram chung của team để nhận cảnh báo sự cố.</b>
          Mỗi on-call admin tự khai báo <span class="mono">@username</span> trong
          form <i>Add administrator</i>; hệ thống ping đúng người đang trực, và
          chỉ ping toàn bộ follower khi không có ca trực. Telegram không có
          <span class="mono">@all</span> tự nhiên.
        </p>
        <p class="muted" style="font-size:12px;margin-top:6px">
          Bot token chỉ đọc từ env (<span class="mono">TELEGRAM_BOT_TOKEN</span>) hoặc
          ô nhập của admin, được mã hoá AES-GCM trước khi lưu vào SQLite, không
          ghi plaintext vào code/DB và không bao giờ trả về API đọc. Mọi cuộc
          gọi tới Bot API dùng HTTPS. Telegram thay mail server cho
          <b>cảnh báo nội bộ</b>; email giao dịch cho người dùng cuối vẫn dùng
          luồng mail riêng của dashboard.
        </p>
        <p class="muted" style="font-size:12px;margin-top:6px">
          ${t.linked ? 'Tài khoản của bạn đã liên kết @' + esc(t.telegram_username || '') + '.'
                     : 'Bạn chưa liên kết Telegram — dùng nút ở góc phải trên màn hình.'}
          ${t.env_override ? ' TELEGRAM_BOT_TOKEN từ env đang được ưu tiên.' : ''}
        </p>
      </div>` + tgForm;

    $('#view').innerHTML =
      section('appearance', 'Appearance', appearance, true) +
      section('account', 'Account', account, true) +
      section('telegram', 'Telegram incident alerts', tgBody, true) +
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

    const rows = data.leases.length ? data.leases.map((l) => {
      const who = l.device_name || l.ip;
      const state = l.blocked
        ? '<span class="pill err" title="connection cut - the address is still known">terminated</span>'
        : `<span class="pill ${l.online ? 'up' : 'down'}">${l.online ? 'online' : 'offline'}</span>`;
      const act = !admin ? ''
        // The controller runs this dashboard and the routers are what it
        // watches. Cutting either is not an operation, it is a way to take the
        // tool down with it - so the button is not offered, and the API refuses
        // it anyway.
        : l.protected
          ? '<span class="muted" style="font-size:11.5px" title="managed device - cutting this would take the monitoring path down">protected</span>'
          : (l.blocked
            ? `<button class="act" data-act="lease-restore" data-id="${l.id}"
                       data-who="${esc(who)}" title="put this host back on the wire">restore</button>`
            : `<button class="act danger" data-act="lease-terminate" data-id="${l.id}"
                       data-who="${esc(who)}" title="cut this host off and file an event">terminate</button>`);
      return `
      <tr data-lease="${l.id}"${l.blocked ? ' class="cut"' : ''}>
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
        <td>${state}</td>
        <td class="muted mono" style="font-size:11.5px">${esc((l.last_seen || '').slice(11))}</td>
        <td>${act}</td>
      </tr>`;
    }).join('')
      : `<tr><td colspan="10" class="muted" style="text-align:center;padding:22px">
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
            <th>Subnet</th><th>VLAN</th><th>Seen on</th><th>State</th><th>Last seen</th><th></th>
          </tr></thead>
          <tbody>${rows}</tbody>
        </table>
      </div>`;
    $('#view').innerHTML = section('leases', 'Address leases', body, true);
  },

  // --------------------------------------------------------------- shifts
  async shifts() {
    const [d, sr] = await Promise.all([
      api.get('/api/shifts'),
      api.get('/api/staff/requests').catch(() => ({ requests: [], pending: 0 })),
    ]);
    const admin = state.me.role === 'admin';
    const cur = d.current;
    const pending = (sr.requests || []).filter((x) => x.status === 'pending');

    const banner = cur
      ? `<div class="panel" style="margin-bottom:10px;border-left:3px solid var(--accent)">
           <div class="row" style="justify-content:space-between">
             <div>
               <div class="muted" style="font-size:11.5px;text-transform:uppercase;letter-spacing:.06em">
                 On call now</div>
               <div style="font-size:17px;margin-top:3px"><b>${esc(cur.name)}</b>
                 ${cur.telegram ? `<span class="muted" style="font-size:13px">@${esc(cur.telegram)}</span>` : ''}</div>
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
    // The person on duty right now is marked in the table itself, not only in
    // the banner above it: "who holds the pager" is the question this page
    // exists to answer, so it has to be visible where the names are.
    const onId = cur ? cur.id : null;
    const rows = d.admins.length ? d.admins.map((a) => `
      <tr data-shift="${a.id}" class="${a.id === onId ? 'oncall' : ''}">
        <td><b>${esc(a.name)}</b>${a.id === onId ? ' <span class="chip chip-ok">on call</span>' : ''}${a.active ? '' : ' <span class="muted">(inactive)</span>'}</td>
        <td>${esc(a.role)}</td>
        <td class="mono">${esc(a.starts_at)}\u2013${esc(a.ends_at)}</td>
        <td class="mono" style="font-size:11.5px">${esc((a.days || []).join(' '))}</td>
        <td style="text-align:right">
          ${admin ? `<button class="act" data-act="shift-edit" data-id="${a.id}">Edit</button>
                     <button class="act${a.id === onId ? '' : ' primary'}" data-act="shift-handover" data-id="${a.id}"
                       ${a.id === onId ? 'disabled title="Already on call"' : ''}>
                       ${a.id === onId ? 'On call' : 'Make on-call'}</button>` : ''}
        </td>
      </tr>`).join('')
      : `<tr><td colspan="5" class="muted" style="text-align:center;padding:22px">No administrators yet.</td></tr>`;

    const ed = state.shiftEdit ? d.admins.find((a) => a.id === Number(state.shiftEdit)) : null;
    const v = (k, fb) => esc(ed ? (ed[k] ?? fb) : fb);
    const form = admin ? `
      <div class="panel" style="margin-top:10px" id="shift-form">
        <div class="p-title" style="text-align:left;margin-bottom:9px">
          ${ed ? 'Edit ' + esc(ed.name) : 'Add an administrator'}</div>
        <div style="display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:9px">
          <label class="muted" style="font-size:12px">Name<br>
            <input id="sh-name" style="width:100%;margin-top:4px" value="${v('name', '')}"></label>
          <label class="muted" style="font-size:12px">Telegram (@username)<br>
            <input id="sh-telegram" placeholder="@username" autocomplete="off"
                   style="width:100%;margin-top:4px" value="${v('telegram', '')}">
            <span id="sh-tg-note" style="display:block;font-size:11.5px;margin-top:3px"></span></label>
          <label class="muted" style="font-size:12px">Phone<br>
            <input id="sh-phone" style="width:100%;margin-top:4px" value="${v('phone', '')}"></label>
          <label class="muted" style="font-size:12px">Role<br>
            <select id="sh-role" style="width:100%;margin-top:4px">
              <option value="primary"${ed && ed.role === 'backup' ? '' : ' selected'}>primary</option>
              <option value="backup"${ed && ed.role === 'backup' ? ' selected' : ''}>backup</option>
            </select></label>
          <label class="muted" style="font-size:12px">From<br>
            <input id="sh-start" style="width:100%;margin-top:4px" value="${v('starts_at', '08:00')}"></label>
          <label class="muted" style="font-size:12px">To<br>
            <input id="sh-end" style="width:100%;margin-top:4px" value="${v('ends_at', '17:00')}"></label>
        </div>
        <div class="row" style="margin-top:9px">
          <span class="muted" style="font-size:12px">Days</span>
          ${DAYS.map((x) => `<label class="muted" style="font-size:12px">
            <input type="checkbox" class="sh-day" value="${x}"${ed ? ((ed.days || []).includes(x) ? ' checked' : '') : ' checked'}> ${x}</label>`).join('')}
        </div>
        <div class="row" style="margin-top:11px;justify-content:flex-end">
          ${ed ? '<button class="act" data-act="shift-cancel">Cancel</button>' : ''}
          <button class="act primary" data-act="shift-add">${ed ? 'Save changes' : 'Add'}</button>
        </div>
      </div>` : '';

    const reqRows = (sr.requests || []).slice(0, 20).map((x) => `
      <tr>
        <td><b>@${esc(x.username || '?')}</b>${x.display_name && x.display_name !== x.username ? ' <span class="muted">(' + esc(x.display_name) + ')</span>' : ''}</td>
        <td class="mono" style="font-size:11.5px">${esc(x.telegram_id)}</td>
        <td class="muted mono" style="font-size:11.5px">${esc(x.created_at)}</td>
        <td class="status-col"><span class="pill ${x.status === 'approved' ? 'up' : (x.status === 'declined' ? 'down' : 'q')}">${esc(x.status)}</span></td>
        <td style="text-align:right">${admin && x.status === 'pending' ? `
          <button class="act primary" data-act="staff-approve" data-id="${x.id}">Approve</button>
          <button class="act" data-act="staff-decline" data-id="${x.id}">Decline</button>` : ''}</td>
      </tr>`).join('');

    const reqPanel = `
      <div class="panel" style="margin-top:10px;padding:4px 8px">
        <div class="p-title" style="text-align:left;margin:8px 4px 4px">
          Staff requests from the Telegram group${pending.length ? ` <span class="chip chip-warn">${pending.length} pending</span>` : ''}</div>
        <table>
          <thead><tr><th>Telegram</th><th>ID</th><th>Requested</th><th>Status</th><th></th></tr></thead>
          <tbody>${reqRows || '<tr><td colspan="5" class="muted" style="text-align:center;padding:18px">Nobody has asked yet. A group member sends <span class="mono">/addstaff</span> to the bot.</td></tr>'}</tbody>
        </table>
        <p class="muted" style="font-size:12px;margin:8px 4px 6px">
          Thành viên nhóm gửi <span class="mono">/addstaff &lt;mã&gt;</span> cho bot; bot tự lấy
          <span class="mono">@username</span> từ chính tin nhắn nên không ai mạo danh được.
          Quản trị viên duyệt ở đây trước khi người đó vào lịch trực.
        </p>
      </div>`;

    const body = banner + `
      <div class="panel" style="padding:4px 8px">
        <table>
          <thead><tr><th>Name</th><th>Role</th><th>Hours</th>
            <th>Days</th><th></th></tr></thead>
          <tbody>${rows}</tbody>
        </table>
      </div>
      <p class="muted" style="font-size:12.5px;margin-top:9px">
        The green row is the person holding the pager right now. \u201cMake on-call\u201d records a
        handover: use it when a shift changes early, or when someone works overtime and forgets to
        hand over \u2014 every unresolved alert is re-sent to the new person immediately. A handover
        stays in effect for ${esc(d.valid_h)} hours, after which the timetable above takes over again.
      </p>
      <p class="muted" style="font-size:12px;margin-top:6px">
        <b>Join nhóm Telegram chung của team để nhận cảnh báo sự cố.</b>
        Mỗi admin tự khai báo <span class="mono">@username</span> ở ô
        <i>Telegram</i> và tự join group; hệ thống chỉ mention đúng người đang
        trực (hoặc toàn bộ follower nếu không có ca trực). Bot không lấy được
        danh sách thành viên group qua Bot API, nên roster là nguồn duy nhất.
      </p>` + form;
    $('#view').innerHTML = section('shifts', 'Shift roster and on-call', body + reqPanel, true);
    if (ed) {
      const f = $('#shift-form');
      if (f && f.scrollIntoView) f.scrollIntoView({ block: 'nearest' });
    }
  },

  // --------------------------------------------------------------- alerts
  async alerts() {
    const [d, t, inc, tgDeliv] = await Promise.all([
      api.get('/api/alerts'),
      api.get('/api/telegram/status').catch(() => ({})),
      api.get('/api/telegram/incidents').catch(() => ({ incidents: [] })),
      api.get('/api/telegram/deliveries').catch(() => ({ deliveries: [] })),
    ]);
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
        ${admin ? `
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

    // Telegram is the new internal incident channel. Mail above stays for
    // end-user transactional email - the two are not interchangeable.
    const tgRows = (inc.incidents || []).map((x) => `
      <tr>
        <td class="mono">${esc(x.code)}</td>
        <td>${esc(x.summary)}</td>
        <td class="status-col"><span class="pill ${x.status === 'Đã sửa' ? 'up' : 'down'}">${esc(x.status)}</span></td>
        <td class="muted">${esc(x.by_user || '—')}</td>
        <td class="muted mono" style="font-size:11px">${esc(x.updated_at)}</td>
        <td style="text-align:right">${admin && x.status !== 'Đã sửa'
          ? `<button class="act" data-act="tg-done" data-code="${esc(x.code)}">Mark fixed</button>` : ''}</td>
      </tr>`).join('');
    const tgSection = section('telegram-channel', 'Telegram channel (internal alerts)', `
      <div class="row" style="margin-bottom:9px;gap:10px;flex-wrap:wrap">
        <span class="chip ${t.configured ? 'chip-ok' : 'chip-warn'}">${t.configured ? 'configured' : 'not configured'}</span>
        <span class="muted" style="font-size:12.5px">${(t.followers || []).length} follower(s) ·
          on-call mentioned first, else all followers · "Đang chờ lệnh" → "Đã sửa"</span>
        ${admin ? '<span style="margin-left:auto"></span><button class="act" data-act="tg-test">Send test alert</button>' : ''}
      </div>
      <div class="panel" style="padding:4px 8px;margin-bottom:9px">
        <table><thead><tr><th>Code</th><th>Summary</th><th class="status-col">Status</th>
          <th>By</th><th>Updated</th><th></th></tr></thead>
        <tbody>${tgRows || '<tr><td colspan="6" class="muted" style="text-align:center;padding:18px">No Telegram incidents yet.</td></tr>'}</tbody></table>
      </div>
      ${admin ? `<div class="panel" style="padding:10px 12px">
        <div class="p-title" style="text-align:left;margin-bottom:8px">Raise an incident</div>
        <div style="display:grid;grid-template-columns:repeat(auto-fit,minmax(180px,1fr));gap:9px">
          <label class="muted" style="font-size:12px">Rule<br>
            <input id="tg-rule" value="manual" style="width:100%;margin-top:4px"></label>
          <label class="muted" style="font-size:12px">Summary<br>
            <input id="tg-summary" placeholder="[AVERAGE] r1: Device unreachable — 3 alert đang mở"
                   style="width:100%;margin-top:4px"></label>
        </div>
        <div class="row" style="margin-top:10px;justify-content:flex-end">
          <button class="act primary" data-act="tg-alert">Send alert</button>
        </div>
      </div>` : ''}
      <p class="muted" style="font-size:12px;margin-top:9px">
        Telegram thay mail server cho <b>cảnh báo sự cố nội bộ</b>: push tức thì,
        không cần domain/DNS/SPF/DKIM, miễn phí. Bảng mail ở trên vẫn phục vụ
        email giao dịch cho người dùng cuối. Thiếu token hoặc CHAT_ID thì API
        trả lỗi rõ ràng, không crash.
        ${(tgDeliv.deliveries || []).length
          ? ' Lần gửi gần nhất: ' + esc(tgDeliv.deliveries[0].status) + ' · ' + esc(tgDeliv.deliveries[0].ts) : ''}
      </p>`, true);

    $('#view').innerHTML = section('alerts', 'Alert rules and delivery', body, true) + tgSection;
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

  // ------------------------------------------------------- B.9 discovery
  // HONEST HISTORY: the "Discovery" nav entry used to point at #/setup, whose
  // content is devices + OSPF link proposals + DNS (and, on the older build,
  // a copy of the Dashboard with CPU/disk gauges, cleanup drills and a command
  // console). There was NO presentation for newly seen hosts. This view is
  // that missing layer: it reads only what poller.py actually collects, and
  // states plainly what is still not implemented.
  async discovery() {
    const d = await api.get('/api/discovery').catch(() => ({}));
    const admin = state.me.role === 'admin';
    const rows = d.candidates || [];
    const c = d.counts || {};

    const table = rows.length ? rows.map((x) => `
      <tr>
        <td class="mono"><b>${esc(x.mac)}</b></td>
        <td class="mono">${esc(x.ip)}</td>
        <td>${esc(x.device_name || x.hostname || '—')}</td>
        <td class="muted">${esc(x.vendor || 'unknown OUI')}</td>
        <td class="mono" style="font-size:11.5px">${esc(x.seen_on || '—')}${x.if_name ? ' / ' + esc(x.if_name) : ''}</td>
        <td class="mono" style="font-size:11.5px">${esc(x.first_seen)}</td>
        <td class="status-col"><span class="pill ${x.state === 'acknowledged' ? 'up' : (x.state === 'ignored' ? 'down' : 'err')}">${esc(x.state)}</span></td>
        <td class="status-col">${x.online ? '<span class="pill up">online</span>' : '<span class="pill down">offline</span>'}</td>
        <td style="text-align:right">${admin ? `
          <button class="act" data-act="disc-ack" data-id="${x.id}">Acknowledge</button>
          <button class="act" data-act="disc-ignore" data-id="${x.id}">Ignore</button>` : ''}</td>
      </tr>`).join('')
      : `<tr><td colspan="9" class="muted" style="text-align:center;padding:22px">
           No unknown device observed yet. A new IP/MAC pair seen in a router ARP
           table and absent from the inventory will appear here automatically.</td></tr>`;

    const events = (d.events || []).length
      ? (d.events || []).map((e) => `<div class="feed-row">
           <span class="muted mono" style="font-size:11.5px">${esc(e.ts)}</span>
           <span class="mono">${esc(e.ip)}</span>
           <span class="mono muted">${esc(e.mac)}</span>
           <span class="muted">${esc(e.detail || 'new binding')}</span></div>`).join('')
      : '<p class="muted" style="font-size:12.5px">No appearance/move events recorded yet.</p>';

    const src = (d.sources || []).map((s) => `<li>${esc(s)}</li>`).join('');
    const lim = (d.limitations || []).map((s) => `<li>${esc(s)}</li>`).join('');

    const body = `
      <div class="panel" style="padding:12px 14px;margin-bottom:10px">
        <b>B.9 — New device discovery</b>
        <span class="muted" style="font-size:12.5px">
          Passive detection of hosts newly plugged into the lab network, recorded
          into the <span class="mono">discovered_devices</span> ledger.
          <b>${c.unknown || 0}</b> unknown · <b>${c.new || 0}</b> new ·
          <b>${c.acknowledged || 0}</b> acknowledged · <b>${c.ignored || 0}</b> ignored ·
          <b>${c.known_inventory || 0}</b> already in inventory
          (ledger ${c.ledger || 0}).
          ${d.sync && d.sync !== 'ok' ? '<span class="chip chip-crit">' + esc(d.sync) + '</span>' : ''}
        </span>
      </div>
      <div class="panel" style="padding:4px 8px">
        <table>
          <thead><tr><th>MAC</th><th>IP</th><th>Name</th><th>Vendor (OUI)</th>
            <th>Seen on</th><th>First seen</th><th class="status-col">State</th>
            <th class="status-col">Live</th><th></th></tr></thead>
          <tbody>${table}</tbody>
        </table>
      </div>
      <div class="grid" style="margin-top:11px">
        <div class="panel" style="padding:12px 14px">
          <div class="p-title" style="text-align:left">What actually feeds this view</div>
          <ul class="gap-recs">${src}</ul>
          <p class="muted" style="font-size:12px;margin-top:8px">
            Không còn hiển thị CPU/disk/memory, cleanup drill hay command console
            ở trang Discovery — đó là nội dung Dashboard bị lặp lại trước đây.
            Để quản lý thiết bị / DNS / OSPF link proposals, dùng
            <a href="#/setup">Setup</a>.
          </p>
        </div>
        <div class="panel" style="padding:12px 14px">
          <div class="p-title" style="text-align:left">Still partial — not implemented</div>
          <ul class="gap-recs">${lim}</ul>
          <p class="muted" style="font-size:12px;margin-top:8px">
            Ghi nhận vào ledger là tự động; việc đưa thiết bị vào inventory vẫn
            do admin thực hiện. Các nút Acknowledge / Ignore chỉ hiển thị với
            <span class="mono">admin</span> và được kiểm tra lại ở server (403).
          </p>
        </div>
      </div>
      <div class="panel" style="margin-top:11px;padding:12px 14px">
        <div class="p-title" style="text-align:left;margin-bottom:8px">Recent appearance timeline</div>
        ${events}
      </div>`;
    $('#view').innerHTML = section('discovery', 'New device discovery (B.9)', body, true);
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
  },

  // ------------------------------------------------------- gap analysis
  // A separate section, not a widget: the audit is a written record of what
  // the dashboard was missing and which of those gaps were actually closed.
  async gapanalysis() {
    const [g, ops] = await Promise.all([
      api.get('/api/ops/gaps').catch(() => ({ gaps: [] })),
      api.get('/api/ops').catch(() => ({})),
    ]);
    const gaps = g.gaps || [];
    const closed = gaps.filter((x) => x.status === 'closed').length;
    const rows = gaps.map((x) => `<tr>
      <td><b>${esc(x.area)}</b></td>
      <td class="muted">${esc(x.before)}</td>
      <td>${esc(x.after)}</td>
      <td class="status-col"><span class="pill ${x.status === 'closed' ? 'up' : (x.status === 'blocked' ? 'err' : 'down')}">${esc(x.status).replace(/^./, (ch) => ch.toUpperCase())}</span></td>
      <td class="muted mono" style="font-size:11px">${esc(x.evidence)}</td>
    </tr>`).join('');
    const body = `
      <div class="panel" style="padding:12px 14px;margin-bottom:10px">
        <b>Dashboard gap analysis</b>
        <span class="muted" style="font-size:12.5px">A structural audit of what
        the dashboard collected versus what it actually showed. ${closed} of
        ${gaps.length} findings are closed; the rest are listed honestly rather
        than marked done.</span>
      </div>
      <div class="panel"><table>
        <thead><tr><th>Area</th><th>Before</th><th>After</th><th class="status-col">Status</th><th>Evidence</th></tr></thead>
        <tbody>${rows}</tbody></table></div>
      <div class="panel" style="margin-top:10px;padding:12px 14px">
        <div class="p-title">Further recommendations</div>
        <ul class="gap-recs">
          <li><b>Interface error counters.</b> Walk <span class="mono">ifInErrors / ifOutErrors</span> in the poller and store them in <span class="mono">health_samples</span>. The routers currently expose neither, so this needs an SNMP capability check first.</li>
          <li><b>Per-node disk samples.</b> Disk is served by the health endpoint but not stored per node; add a column so the telemetry table can show history rather than a single reading.</li>
          <li><b>DuckDNS token.</b> The TLS listener on :8443 is ready and DNS-01 automation is installed, but a trusted certificate needs a real subdomain and token. Until then the HTTP listener stays up.</li>
          <li><b>Port forwarding.</b> A trusted certificate only helps outside the lab if the public router forwards 443 to the controller. That is outside SSH reach and needs the hypervisor/network owner.</li>
          <li><b>Retention drill.</b> Run the dry-run button once, compare eligible rows with the live table, then confirm the semi-annual cron. Inventory tables are protected either way.</li>
        </ul>
      </div>`;
    $('#view').innerHTML = body;
  },

  // ------------------------------------------------------- experimental
  // A sandbox, not a preview. The simulator inside the frame has its own
  // devices, its own addressing and its own faults; it never calls this API.
  // Keeping it in an iframe is the isolation, not a styling choice - its
  // script would otherwise share a global scope with this one and its CSS
  // would leak into every panel on the page.
  async experimental() {
    const theme = document.documentElement.dataset.theme === 'dark' ? 'dark' : 'light';
    const src = `/static/sim/experiment.html?theme=${theme}`;

    // The one thing taken from Monitor: a live reading of the real lab, so the
    // model can be compared against reality side by side. It is display only -
    // the sandbox never sees it.
    const [t, ld] = await Promise.all([
      api.get('/api/topology').catch(() => ({ nodes: [], links: [] })),
      api.get('/api/leases?limit=300').catch(() => ({ leases: [] })),
    ]);
    const nodes = t.nodes || [];
    const leases = (ld && ld.leases) || [];
    const up = nodes.filter((n) => n.last_state === 'up').length;
    const online = leases.filter((l) => l.online && !l.blocked).length;
    const cut = leases.filter((l) => l.blocked).length;
    const live = [
      `<span class="tp-stat ok"><b>${up}/${nodes.length}</b> devices up</span>`,
      `<span class="tp-stat ok"><b>${online}</b> hosts online</span>`,
      cut ? `<span class="tp-stat err"><b>${cut}</b> terminated</span>` : '',
    ].filter(Boolean).join('');

    const body = `
      <div class="panel" style="padding:12px 14px;margin-bottom:10px">
        <div class="row" style="justify-content:space-between;align-items:flex-start">
          <div style="max-width:720px">
            <b>Sandbox</b>
            <span class="muted" style="font-size:12.5px">
              A full model of the same lab - packets, DHCP, virtual gateway, loop
              protection and a catalogue of faults - running entirely in the page.
              Nothing here reads or writes the database, the routers or the
              simulator containers, so it is safe to break. Rehearse a change here
              first; the live view stays untouched in
              <a href="#/monitoring">Monitor</a>.
            </span>
          </div>
          <a class="act" href="${src}" target="_blank" rel="noopener"
             style="text-decoration:none;white-space:nowrap">Open in a new tab</a>
        </div>
        <div class="row" style="margin-top:10px;gap:14px;align-items:center">
          <span class="muted" style="font-size:12px">Live lab, for comparison</span>
          ${live}
        </div>
      </div>
      <div class="panel" style="padding:0;overflow:hidden">
        <iframe src="${src}" title="Network sandbox"
                style="display:block;width:100%;height:calc(100vh - 230px);min-height:600px;border:0"></iframe>
      </div>`;
    $('#view').innerHTML = body;
  }
};

// --------------------------------------------------------- status bar
// Persists across routes because it lives outside #view. Everything it shows
// comes from status files the controller scripts write; it never guesses.
function sbItem(label, ok, text, tip) {
  const cls = ok === null ? '' : (ok ? 'ok' : 'err');
  return `<span class="sb-item ${cls}" title="${esc(tip || '')}"><i></i>`
    + `<b>${esc(label)}</b> ${esc(text || '')}</span>`;
}

async function renderStatusbar() {
  const el = $('#statusbar');
  if (!el) return;
  let d, tg;
  try {
    [d, tg] = await Promise.all([
      api.get('/api/ops'),
      api.get('/api/telegram/status').catch(() => ({})),
    ]);
  } catch (err) {
    el.innerHTML = '<span class="sb-item err"><i></i>Operations status unavailable</span>';
    return;
  }
  tg = tg || {};
  const duck = d.duckdns || {}, tls = d.tls || {}, fw = d.firewall || {}, bk = d.backup || {};
  const tun = d.cloudflared || {};
  const duckOk = duck.ok === true ? true : (duck.state === 'unconfigured' ? null : false);
  const tlsOk = tls.ok === true ? true : (tls.state === 'unconfigured' ? null : false);
  const fwOk = fw.state === 'active' ? true
    : (fw.state === 'pending' || fw.state === 'inactive' || fw.state === 'rolled_back' ? null : false);
  const expiry = tls.expires ? String(tls.expires).slice(4, 16) : 'trusted';
  el.innerHTML = [
    sbItem('DuckDNS', duckOk, duck.ok ? (duck.public_ip || duck.state) : (duck.state || 'unknown'), duck.message),
    sbItem('HTTPS', tlsOk, tls.ok ? 'valid to ' + expiry : (tls.state || 'not issued'), tls.message),
    sbItem('Firewall', fwOk, fw.state || 'unknown', fw.message),
    sbItem('Tunnel', tun.ok === true ? true : (tun.state === 'down' ? false : null),
      tun.ok ? 'connected' : (tun.state || 'unknown'), tun.url || tun.message),
    sbItem('SQLite', true, d.db_size || '—', 'Database file size on the controller'),
    sbItem('Backups', bk.ok === true ? true : null, bk.last_snapshot || 'none', bk.message),
    sbItem('Next clearance', true, d.next_clearance || '—', '1 Jan / 1 Jul at 03:00 UTC'),
    // Top-right is now the group invitation, not a per-user login. Joining the
    // group is what makes the bot able to reach a person; the per-user link
    // only recorded a username, which the roster no longer asks for.
    `<a class="sb-tg" href="https://t.me/+RJDUhlPh_GQ2MTRl" target="_blank"
        rel="noopener noreferrer" title="Tham gia nhóm Telegram của team">
        Join Telegram group</a>`,
  ].join('');
}

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
  // Route names are single words, so the old naive capitalisation produced
  // "Gapanalysis". Multi-word labels are mapped explicitly (task 3).
  const TITLES = { gapanalysis: 'Gap analysis', discovery: 'Discovery' };
  $('#title').textContent = TITLES[name] || (name[0].toUpperCase() + name.slice(1));
  $('#ranges').style.display = name === 'dashboard' ? '' : 'none';

  fn().catch((e) => { $('#view').innerHTML = `<p class="err">${esc(e.message)}</p>`; });
  renderStatusbar();

  // The poller runs every 5 s, so the views that show live state redraw at the
  // same cadence - otherwise the page is always up to a poll behind and the
  // 5 s interval only shows up in the logs.
  const every5 = name === 'dashboard'
    ? state.range === 'live'
    : (name === 'monitoring' || name === 'leases' || name === 'discovery');
  if (every5) {
    state.timer = setInterval(() => {
      // never yank the fault panel out from under someone reading it
      if (document.querySelector('.tp-pop')) return;
      fn().catch(() => {});
      renderStatusbar();
    }, 5000);
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
