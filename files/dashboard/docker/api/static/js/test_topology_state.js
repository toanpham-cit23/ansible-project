// Regression tests for the topology's device-state model.
//
// These load the *shipped* app.js into a sandbox rather than re-implementing
// its rules, so a test can only pass if the code the browser runs is correct.
// Run:  node files/dashboard/docker/api/static/js/test_topology_state.js
//
// Two defects are locked down here.
//
// 1. The core row must contain only devices classified as routers. A test
//    device guessed as a router used to be drawn as part of the designed
//    fabric, and counted as "routers down" when it was simply offline.
//
// 2. A device's state was derived from the SNMP uptime reply alone. A router
//    with a stopped snmpd was stored as "down", drawn as a red outage, and
//    every switch hanging off it was told it had lost uplinks - while the
//    router was forwarding traffic the whole time.

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const SRC = path.join(__dirname, 'app.js');

// ---------------------------------------------------------------- sandbox
// Only what app.js touches at load time. Every accessor is a stub that returns
// nothing useful: the file must not need a real DOM to define its functions.
function load() {
  const noop = () => {};
  const el = () => ({
    dataset: {}, style: {}, classList: { add: noop, remove: noop, toggle: noop },
    appendChild: noop, addEventListener: noop, setAttribute: noop, remove: noop,
    querySelector: () => null, querySelectorAll: () => [], innerHTML: '', textContent: '',
  });
  const ctx = {
    console,
    setTimeout, clearTimeout, setInterval, clearInterval,
    Date, Math, JSON, Number, String, Object, Array, Map, Set, Promise, RegExp, Error,
    location: { hash: '#/dashboard' },
    localStorage: { getItem: () => null, setItem: noop, removeItem: noop },
    document: {
      querySelector: () => null,
      querySelectorAll: () => [],
      getElementById: () => null,
      addEventListener: noop,
      createElement: el,
      documentElement: { dataset: {} },
      body: el(),
    },
    window: {
      addEventListener: noop,
      matchMedia: () => ({ matches: false, addEventListener: noop, addListener: noop }),
    },
    matchMedia: () => ({ matches: false, addEventListener: noop, addListener: noop }),
    fetch: () => Promise.reject(new Error('no network in test')),
    navigator: { userAgent: 'node' },
    requestAnimationFrame: noop,
    cancelAnimationFrame: noop,
  };
  ctx.globalThis = ctx;
  const src = fs.readFileSync(SRC, 'utf8')
    // top-level `const` lives in the script's lexical scope, not on the context
    // object, so export the functions under test explicitly.
    + '\n;globalThis.__devLiveness = devLiveness;'
    + '\n;globalThis.__devStatePill = devStatePill;'
    + '\n;globalThis.__labTopology = labTopology;';
  vm.createContext(ctx);
  vm.runInContext(src, ctx, { filename: SRC });
  return ctx;
}

// ---------------------------------------------------------------- harness
let failed = 0;
let passed = 0;
function check(name, cond, detail) {
  if (cond) { passed += 1; console.log('  ok   ' + name); }
  else { failed += 1; console.log('  FAIL ' + name + (detail ? '\n       ' + detail : '')); }
}

const app = load();
const liveness = app.__devLiveness;
const devStatePill = app.__devStatePill;
const labTopology = app.__labTopology;

// ---------------------------------------------------------------- predicate
console.log('devLiveness()');
check('SNMP answering is up',
  liveness({ last_state: 'up', last_reachable: 'up' }) === 'up');

check('SNMP silent but ICMP answers is blind, not down',
  liveness({ last_state: 'down', last_reachable: 'up', reach_method: 'icmp' }) === 'blind',
  'got ' + liveness({ last_state: 'down', last_reachable: 'up', reach_method: 'icmp' }));

check('nothing answering is down',
  liveness({ last_state: 'down', last_reachable: 'down' }) === 'down');

// Absence of data is not evidence of an outage. A row that has never been
// measured must not be drawn as a fault.
check('a device never measured is unknown, not down',
  liveness({ last_state: null, last_reachable: null }) === 'unknown',
  'got ' + liveness({ last_state: null, last_reachable: null }));

check('rows written before the reachability columns existed keep their SNMP verdict',
  liveness({ last_state: 'down', last_reachable: '' }) === 'down');

check('an unmeasured row with empty reachability is unknown',
  liveness({ last_state: '', last_reachable: '' }) === 'unknown');

console.log('\ndevStatePill()');
check('up renders an up pill', /class="pill up"/.test(devStatePill({ last_state: 'up' })));
check('blind renders an unmonitored pill, never a down pill',
  /class="pill warn"/.test(devStatePill({ last_state: 'down', last_reachable: 'up' }))
  && !/pill down/.test(devStatePill({ last_state: 'down', last_reachable: 'up' })),
  devStatePill({ last_state: 'down', last_reachable: 'up' }));
check('a real outage renders a down pill',
  /class="pill down"/.test(devStatePill({ last_state: 'down', last_reachable: 'down' })));

// ---------------------------------------------------------------- core row
// Only router-kind devices belong in the designed fabric. A test device added
// by hand is frequently guessed as a router, and drawing it there produced two
// wrong statements at once: it counted as "routers down" when it was simply
// offline, and it inherited an X as though a designed uplink had failed.
console.log('\nthe core row contains only routers');
const mixed = [
  { id: 1, hostname: 'controller', mgmt_ip: '192.168.23.10',
    effective_kind: 'server', last_state: 'up' },
  { id: 2, hostname: 'r1', mgmt_ip: '192.168.23.11',
    effective_kind: 'router', last_state: 'down', last_reachable: 'down' },
  { id: 3, hostname: 'r2', mgmt_ip: '192.168.23.12',
    effective_kind: 'router', last_state: '', last_reachable: '' },
  { id: 4, hostname: 'r3', mgmt_ip: '192.168.23.13',
    effective_kind: 'router', last_state: 'up' },
  { id: 5, hostname: 'test-router', mgmt_ip: '192.168.23.51',
    effective_kind: 'router', last_state: 'down', last_reachable: 'down' },
  { id: 6, hostname: 'test-switch', mgmt_ip: '192.168.23.52',
    effective_kind: 'switch', last_state: 'down', last_reachable: 'down' },
  { id: 7, hostname: 'test-laptop', mgmt_ip: '192.168.23.53',
    effective_kind: 'unknown', last_state: 'down', last_reachable: 'down' },
];
const mixBuilt = labTopology({ nodes: mixed, links: [] }, []);
// r1 and test-router are confirmed dead. The switch and the laptop are dead
// too, but they are not part of the core row, so they must not be counted.
check('only router-kind devices are counted as down', mixBuilt.routersDown === 2,
  'routersDown = ' + mixBuilt.routersDown);
check('a never-polled router draws no fault flag',
  liveness(mixed[2]) === 'unknown');

// ---------------------------------------------------------------- topology
// Three routers, all answering ICMP, none answering SNMP: exactly the state the
// live lab is in. Nothing here may be drawn or described as a cut path.
const nodes = [
  { id: 10, hostname: 'controller', mgmt_ip: '192.168.23.10',
    effective_kind: 'server', last_state: 'up', last_reachable: 'up' },
  { id: 1, hostname: 'r1', mgmt_ip: '192.168.23.11',
    effective_kind: 'router', last_state: 'down', last_reachable: 'up', reach_method: 'icmp' },
  { id: 2, hostname: 'r2', mgmt_ip: '192.168.23.12',
    effective_kind: 'router', last_state: 'down', last_reachable: 'up', reach_method: 'icmp' },
  { id: 3, hostname: 'r3', mgmt_ip: '192.168.23.13',
    effective_kind: 'router', last_state: 'down', last_reachable: 'up', reach_method: 'icmp' },
];
const links = [
  { a_device_id: 1, b_device_id: 2, if_a: 'eth1', if_b: 'eth1', proto: 'ospf' },
  { a_device_id: 2, b_device_id: 3, if_a: 'eth1', if_b: 'eth1', proto: 'ospf' },
];
const built = labTopology({ nodes, links }, []);

console.log('\nlabTopology() with the whole core SNMP-silent but pingable');
check('no router is counted as down', built.routersDown === 0,
  'routersDown = ' + built.routersDown);
check('all three routers are counted as reachable-but-unmonitored',
  built.routersBlind === 3, 'routersBlind = ' + built.routersBlind);

// The specific sentence the operator complained about.
const allText = Object.values(built.issues)
  .map((i) => i.list.map((l) => l.text).join(' ')).join(' ');
check('no switch is told it lost an uplink',
  !/uplinks? down/i.test(allText), 'issue text: ' + allText);
check('no switch is told there is no path left',
  !/No path left/i.test(allText), 'issue text: ' + allText);
check('the monitor gap is described as such',
  /reachable but unmonitored/i.test(allText), 'issue text: ' + allText);

// A routing device that answers keeps its cabling. Cutting the links was the
// visible half of the false red.
const cutPeerLinks = (built.svg.match(/class="tp-blocked"/g) || []).length;
check('no router-to-router adjacency is drawn cut', cutPeerLinks === 0,
  'tp-blocked occurrences: ' + cutPeerLinks);
check('router cards are not drawn in the down style',
  !/class="tp-card down"/.test(built.svg), 'svg still contains a down card');
check('router cards are drawn in the blind style',
  (built.svg.match(/tp-card rt blind/g) || []).length === 3,
  'blind cards: ' + (built.svg.match(/tp-card rt blind/g) || []).length);
check('the blind card says why, in words',
  /SNMP silent/i.test(built.svg), 'card subtitle missing the explanation');

// ---------------------------------------------------------------- contrast
// The same topology with a genuinely dead router must still be red, or the fix
// would have simply hidden faults instead of classifying them.
console.log('\nlabTopology() with one router genuinely unreachable');
const deadNodes = nodes.map((n) => (n.id === 2
  ? { ...n, last_state: 'down', last_reachable: 'down', reach_method: 'icmp' }
  : n));
const dead = labTopology({ nodes: deadNodes, links }, []);
const deadText = Object.values(dead.issues)
  .map((i) => i.list.map((l) => l.text).join(' ')).join(' ');

check('the dead router is counted as down', dead.routersDown === 1,
  'routersDown = ' + dead.routersDown);
check('the two live routers are still classified blind',
  dead.routersBlind === 2, 'routersBlind = ' + dead.routersBlind);
check('a real outage still reports lost uplinks', /uplinks? down/i.test(deadText),
  'issue text: ' + deadText);
check('a real outage cuts the peer adjacency',
  (dead.svg.match(/class="tp-blocked"/g) || []).length > 0);
check('a real outage draws a down card',
  /class="tp-card down"/.test(dead.svg));

// ---------------------------------------------------------------- result
console.log('\n' + (failed ? 'FAILED ' + failed + ' of ' + (failed + passed)
  : 'all ' + passed + ' assertions passed'));
process.exit(failed ? 1 : 0);
