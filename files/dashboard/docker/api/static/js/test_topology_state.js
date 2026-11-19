// Regression test for the monitoring topology's device-state classification.
//
// The dashboard is a browser app with no JS test runner in this repo, so this
// file extracts the exact predicates from app.js and exercises them with Node.
// Run:  node files/dashboard/docker/api/static/js/test_topology_state.js
const fs = require('fs');
const path = require('path');

const app = fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8');
const start = app.indexOf('function labTopology');
const end = app.indexOf('const svg = `<svg', start);
if (start < 0 || end < 0) throw new Error('labTopology not found in app.js');
const seg = app.slice(start, end);

const routerFilter = /const routers = t\.nodes\.filter\(\(n\) => ([\s\S]*?)\);\n/.exec(seg);
const isDownSrc = /const isDown = \(r\) => ([^;]+);/.exec(seg);
const isUnknownSrc = /const isUnknown = \(r\) => ([^;]+);/.exec(seg);
if (!routerFilter || !isDownSrc || !isUnknownSrc) {
  throw new Error('topology predicates not found');
}

const isDown = eval('(r) => ' + isDownSrc[1]);
const isUnknown = eval('(r) => ' + isUnknownSrc[1]);
const keep = eval('(n, ctrl) => ' + routerFilter[1]);

const nodes = [
  { id: 1, hostname: 'controller', effective_kind: 'server', last_state: 'up' },
  { id: 2, hostname: 'r1', effective_kind: 'router', last_state: 'down' },
  { id: 3, hostname: 'r2', effective_kind: 'router', last_state: '' },
  { id: 4, hostname: 'r3', effective_kind: 'router', last_state: 'up' },
  { id: 5, hostname: 'test-router', effective_kind: 'router', last_state: 'down' },
  { id: 6, hostname: 'test-switch', effective_kind: 'switch', last_state: 'down' },
  { id: 7, hostname: 'test-laptop', effective_kind: 'unknown', last_state: 'down' },
];
const ctrl = nodes[0];
const routers = nodes.filter(n => keep(n, ctrl));
const names = routers.map(n => n.hostname);
const failures = [];
const eq = (got, want, label) => {
  if (JSON.stringify(got) !== JSON.stringify(want))
    failures.push(`${label}: got ${JSON.stringify(got)}, want ${JSON.stringify(want)}`);
};

eq(names, ['r1', 'r2', 'r3', 'test-router'], 'core row only contains router-kind devices');
eq(routers.filter(isDown).map(n => n.hostname), ['r1', 'test-router'], 'only confirmed down devices count as down');
eq(routers.filter(isUnknown).map(n => n.hostname), ['r2'], 'never-polled device is unknown, not down');
eq(isDown({ last_state: 'up' }), false, 'up is not down');
eq(isUnknown({ last_state: 'up' }), false, 'up is not unknown');

if (failures.length) {
  console.error('topology state test FAILED');
  failures.forEach(f => console.error('  ' + f));
  process.exit(1);
}
console.log('topology state test: 5/5 checks passed');
