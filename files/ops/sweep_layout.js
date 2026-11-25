#!/usr/bin/env node
/**
 * Layout sweep: proves no view overflows or squeezes text at any width.
 *
 * Three failure modes are checked, because all three have shipped before:
 *
 *   1. horizontal overflow - documentElement.scrollWidth exceeds innerWidth,
 *      so the page scrolls sideways;
 *   2. stacked text - a leaf element narrower than 60 px yet taller than
 *      90 px means its text wrapped one character (or one word) per line.
 *      This is the visible "chữ rớt dòng" symptom;
 *   3. clipped cells - a table cell whose scrollWidth exceeds its
 *      clientWidth, i.e. content cut off with no way to read it.
 *
 * Usage:
 *   node sweep_layout.js                  # defaults below
 *   VIEWS=leases,monitoring WIDTHS=390,1476 node sweep_layout.js
 *
 * Requires a browser with the DevTools protocol open and a Node with a
 * built-in WebSocket (Node 22+).
 *
 *   msedge --headless=new --remote-debugging-port=9333 \
 *          --user-data-dir=/tmp/edgeprof about:blank
 *   CDP=http://127.0.0.1:9333 LAB=http://192.168.23.10:8080 \
 *   LAB_PASS=labadmin node sweep_layout.js
 *
 * Exit code is the number of bad rows, capped at 125, so it works in CI.
 */
const http = require('http');

const CDP = process.env.CDP || 'http://127.0.0.1:9333';
const BASE = process.env.LAB || 'http://192.168.23.10:8080';
const USER = process.env.LAB_USER || 'admin';
const PASS = process.env.LAB_PASS || 'labadmin';
const VIEWS = (process.env.VIEWS ||
  'leases,monitoring,database,dashboard,logs,compliance').split(',');
const WIDTHS = (process.env.WIDTHS || '390,768,1024,1280,1476').split(',').map(Number);
const HEIGHT = parseInt(process.env.VH || '900', 10);

function getJson(path) {
  return new Promise((res, rej) => {
    http.get(CDP + path, (r) => {
      let b = '';
      r.on('data', (d) => (b += d));
      r.on('end', () => { try { res(JSON.parse(b)); } catch (e) { rej(e); } });
    }).on('error', rej);
  });
}

function loginPost() {
  const u = new URL(BASE + '/api/login');
  const body = JSON.stringify({ username: USER, password: PASS });
  return new Promise((res, rej) => {
    const req = http.request({
      hostname: u.hostname, port: u.port || 80, path: u.pathname, method: 'POST',
      headers: { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(body) },
    }, (r) => {
      let b = '';
      r.on('data', (d) => (b += d));
      r.on('end', () => res({ status: r.statusCode, cookies: r.headers['set-cookie'] || [], body: b }));
    });
    req.on('error', rej);
    req.end(body);
  });
}

class Cdp {
  constructor(url) { this.url = url; this.id = 0; this.waiters = new Map(); }

  open() {
    return new Promise((res, rej) => {
      this.ws = new WebSocket(this.url);
      this.ws.addEventListener('open', () => res());
      this.ws.addEventListener('error', () => rej(new Error('websocket error')));
      this.ws.addEventListener('message', (ev) => {
        let m;
        try { m = JSON.parse(ev.data); } catch { return; }
        if (m.id && this.waiters.has(m.id)) {
          const w = this.waiters.get(m.id);
          this.waiters.delete(m.id);
          m.error ? w.rej(new Error(JSON.stringify(m.error))) : w.res(m.result);
        }
      });
    });
  }

  send(method, params = {}) {
    const id = ++this.id;
    return new Promise((res, rej) => {
      this.waiters.set(id, { res, rej });
      this.ws.send(JSON.stringify({ id, method, params }));
    });
  }

  async eval(expression) {
    const r = await this.send('Runtime.evaluate', {
      expression, returnByValue: true, awaitPromise: true,
    });
    if (r.exceptionDetails) {
      const d = r.exceptionDetails.exception && r.exceptionDetails.exception.description;
      throw new Error(r.exceptionDetails.text + ' ' + (d || ''));
    }
    return r.result.value;
  }
}

// Runs inside the page.
const CHECK = `(() => {
  const vw = window.innerWidth;
  const docW = document.documentElement.scrollWidth;

  const stacked = [];
  document.querySelectorAll('#view *, .top *').forEach((n) => {
    if (n.children.length) return;                 // leaf text nodes only
    const t = (n.textContent || '').trim();
    if (t.length < 6) return;
    const r = n.getBoundingClientRect();
    if (r.width && r.width < 60 && r.height > 90) {
      stacked.push({ w: Math.round(r.width), h: Math.round(r.height), t: t.slice(0, 34) });
    }
  });

  const clipped = [];
  document.querySelectorAll('#view td, #view th, #view .p-big, #view h1, #view h2')
    .forEach((n) => {
      if (n.scrollWidth > n.clientWidth + 2 && n.clientWidth > 0) {
        clipped.push({ t: (n.textContent || '').trim().slice(0, 28),
                       sw: n.scrollWidth, cw: n.clientWidth });
      }
    });

  return { vw, docW, overflow: docW - vw,
           cls: document.documentElement.className,
           stacked: stacked.slice(0, 5), clipped: clipped.slice(0, 5) };
})()`;

(async () => {
  const auth = await loginPost();
  const m = /sess=([^;]+)/.exec((auth.cookies || []).join('; '));
  if (auth.status !== 200 || !m) {
    console.error('login failed: HTTP', auth.status, (auth.body || '').slice(0, 160));
    process.exit(2);
  }

  const targets = await getJson('/json/list');
  const pages = targets.filter((t) => t.type === 'page' && !/^(edge|chrome|devtools):/.test(t.url));
  const page = pages.find((t) => t.url.includes(new URL(BASE).hostname)) || pages[0];
  if (!page) { console.error('no usable page target on', CDP); process.exit(2); }

  const c = new Cdp(page.webSocketDebuggerUrl);
  await c.open();
  await c.send('Page.enable');
  await c.send('Runtime.enable');
  await c.send('Network.enable');
  await c.send('Network.setCacheDisabled', { cacheDisabled: true });
  await c.send('Network.setCookie', {
    name: 'sess', value: m[1], domain: new URL(BASE).hostname, path: '/', httpOnly: true,
  });

  const rows = [];
  for (const view of VIEWS) {
    for (const w of WIDTHS) {
      const phone = w <= 820;
      const metrics = {
        width: w, height: HEIGHT, deviceScaleFactor: 1, mobile: phone,
        screenWidth: w, screenHeight: HEIGHT, positionX: 0, positionY: 0,
      };
      await c.send('Emulation.setDeviceMetricsOverride', metrics);
      await c.send('Emulation.setTouchEmulationEnabled', { enabled: phone, maxTouchPoints: 5 });

      // A unique query defeats a cached copy of the HTML, which would keep
      // pointing at the previous stylesheet version and hide a fix.
      await c.send('Page.navigate', { url: BASE + '/app?t=' + Date.now() + '#/' + view });
      await new Promise((r) => setTimeout(r, 2200));
      await c.eval(`location.hash = '#/${view}'`);
      await new Promise((r) => setTimeout(r, 1600));
      await c.send('Emulation.setDeviceMetricsOverride', metrics);
      await new Promise((r) => setTimeout(r, 350));

      let r;
      try { r = await c.eval(CHECK); } catch (e) { r = { err: e.message.slice(0, 90) }; }
      rows.push({ view, w, ...r });
      console.log(
        view.padEnd(11) + ' w=' + String(w).padStart(4) +
        '  vw=' + String(r.vw || '?').padStart(4) +
        '  docW=' + String(r.docW || '?').padStart(4) +
        '  ovf=' + String(r.overflow ?? '?').padStart(3) +
        '  cls=' + String(r.cls || '').replace('device-', '') +
        '  stacked=' + (r.stacked || []).length +
        '  clipped=' + (r.clipped || []).length +
        (r.err ? '  ERR=' + r.err : ''));
    }
  }

  const bad = rows.filter((x) =>
    (x.overflow || 0) > 2 || (x.stacked || []).length || (x.clipped || []).length || x.err);
  console.log('\nBAD ROWS: ' + bad.length + ' of ' + rows.length);
  for (const b of bad) console.log('  ' + JSON.stringify(b).slice(0, 240));
  c.ws.close();
  process.exit(Math.min(bad.length, 125));
})().catch((e) => { console.error('ERR', e && e.message); process.exit(2); });
