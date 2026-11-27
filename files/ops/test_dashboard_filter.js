#!/usr/bin/env node
/**
 * Prove the dashboard draws infrastructure only, and never fakes health.
 *
 * Two rules shipped as a fix and both are easy to undo by accident:
 *
 *   1. isInfra() decides which cards are drawn. If it ever returns true for a
 *      workstation, the dashboard goes back to 16 cards with 12 empty ones -
 *      the exact thing that was reported as "nhìn rối".
 *   2. hasHealth() decides whether a card prints CPU/MEM/DSK. If it returns
 *      true for a device with no samples, the card shows a flat line and three
 *      em dashes that read like a real measurement of zero.
 *
 * The functions are lifted out of app.js rather than copied, so this test
 * fails if the shipped logic changes - not if a duplicate in this file does.
 *
 * Usage:  node files/ops/test_dashboard_filter.js
 * Exit code is the number of failures, so it works in CI.
 */
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const SRC = path.join(__dirname, '..', 'dashboard', 'docker', 'api',
                      'static', 'js', 'app.js');
const src = fs.readFileSync(SRC, 'utf8');

// Pull a top-level function or const declaration out of the file by name.
function extract(name) {
  const fn = new RegExp('^function ' + name + '\\s*\\(', 'm');
  const m = fn.exec(src);
  if (m) {
    // Walk braces from the first "{" to its match.
    let i = src.indexOf('{', m.index);
    let depth = 0;
    for (let j = i; j < src.length; j++) {
      if (src[j] === '{') depth++;
      else if (src[j] === '}') {
        depth--;
        if (depth === 0) return src.slice(m.index, j + 1);
      }
    }
  }
  const cn = new RegExp('^const ' + name + '\\s*=', 'm');
  const c = cn.exec(src);
  if (c) {
    const end = src.indexOf(';', c.index);
    return src.slice(c.index, end + 1);
  }
  throw new Error('cannot find ' + name + ' in ' + SRC);
}

const pieces = [extract('INFRA_KINDS'), extract('isInfra'), extract('hasHealth')];
// devLiveness is what hasHealth depends on. Take it too, so the test sees the
// same four-state model the dashboard uses.
pieces.push(extract('devLiveness'));

const ctx = { Set, Array, String };
vm.createContext(ctx);
vm.runInContext(pieces.join('\n'), ctx);
const { isInfra, hasHealth } = ctx;

let fails = 0;
function check(label, got, want) {
  const ok = got === want;
  if (!ok) fails++;
  console.log((ok ? 'PASS  ' : 'FAIL  ') + label +
              (ok ? '' : '  (got ' + JSON.stringify(got) +
                         ', want ' + JSON.stringify(want) + ')'));
}

// --- Rule 1: only plumbing gets a card -----------------------------------
check('router is infrastructure',
      isInfra({ hostname: 'r1', effective_kind: 'router' }), true);
check('switch is infrastructure',
      isInfra({ hostname: 'test-switch', effective_kind: 'switch' }), true);
check('l3switch is infrastructure',
      isInfra({ hostname: 'test-l3switch', effective_kind: 'l3switch' }), true);
check('firewall is infrastructure',
      isInfra({ hostname: 'fw1', effective_kind: 'firewall' }), true);
check('controller is infrastructure even though it is a server',
      isInfra({ hostname: 'controller', effective_kind: 'server' }), true);

check('workstation server is NOT infrastructure',
      isInfra({ hostname: 'sim-fileserver', effective_kind: 'server' }), false);
check('access point is NOT infrastructure',
      isInfra({ hostname: 'sim-ap', effective_kind: 'unknown' }), false);
check('camera is NOT infrastructure',
      isInfra({ hostname: 'sim-camera', effective_kind: 'unknown' }), false);
check('unclassified device is NOT infrastructure',
      isInfra({ hostname: 'sim-hr', effective_kind: '' }), false);
check('null is NOT infrastructure', isInfra(null), false);
check('kind is case-insensitive',
      isInfra({ hostname: 'x', effective_kind: 'ROUTER' }), true);
check('a router named like a host still counts (class beats name)',
      isInfra({ hostname: 'sim-ap', effective_kind: 'router' }), true);

// --- Rule 2: health numbers only when a sample exists --------------------
check('up with samples has health',
      hasHealth({ last_state: 'up', _h: [{ cpu_pct: 4 }] }), true);
check('up with no samples has NO health',
      hasHealth({ last_state: 'up', _h: [] }), false);
check('up with _h missing has NO health',
      hasHealth({ last_state: 'up' }), false);
check('blind has NO health',
      hasHealth({ last_state: 'blind', last_reachable: 'up',
                  _h: [{ cpu_pct: 4 }] }), false);
check('down has NO health',
      hasHealth({ last_state: 'down', _h: [{ cpu_pct: 4 }] }), false);
check('unknown has NO health',
      hasHealth({ _h: [{ cpu_pct: 4 }] }), false);

// --- The combination that caused the report ------------------------------
// A blind endpoint must be excluded twice over: no card, and no health call.
const blindEndpoint = { hostname: 'sim-ap', effective_kind: 'unknown',
                        last_reachable: 'up', _h: [] };
check('blind endpoint: not drawn', isInfra(blindEndpoint), false);
check('blind endpoint: no health', hasHealth(blindEndpoint), false);

const liveRouter = { hostname: 'r2', effective_kind: 'router',
                     last_state: 'up', _h: [{ cpu_pct: 7 }] };
check('live router: drawn', isInfra(liveRouter), true);
check('live router: has health', hasHealth(liveRouter), true);

// --- Rule 3: the dashboard render loop actually calls the filter ----------
// The two helpers above can be perfect and still be ignored. Assert the call
// site, so deleting `all.filter(isInfra)` fails here instead of silently
// putting 16 cards back on the page.
check('dashboard filters the device list through isInfra',
      /const\s+devs\s*=\s*all\.filter\(isInfra\)/.test(src), true);
check('health is only requested for devices that can answer',
      /devs\.filter\(\(d\)\s*=>\s*devLiveness\(d\)\s*!==\s*'blind'\)/.test(src), true);

console.log('\n' + (fails ? fails + ' FAILED' : 'all checks passed'));
process.exit(fails);
