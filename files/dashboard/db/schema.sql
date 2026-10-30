PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS sites (
  id          INTEGER PRIMARY KEY,
  name        TEXT NOT NULL UNIQUE,
  location    TEXT
);

CREATE TABLE IF NOT EXISTS devices (
  id                INTEGER PRIMARY KEY,
  hostname          TEXT NOT NULL UNIQUE,
  mgmt_ip           TEXT NOT NULL,
  site_id           INTEGER REFERENCES sites(id),
  kind              TEXT NOT NULL DEFAULT 'unknown'
                    CHECK (kind IN ('router','l3switch','switch','server','unknown')),
  kind_confidence   REAL NOT NULL DEFAULT 0,
  kind_override     TEXT CHECK (kind_override IN ('router','l3switch','switch','server')),
  vendor            TEXT,
  model             TEXT,
  serial            TEXT,
  asset_tag         TEXT,
  ssh_port          INTEGER NOT NULL DEFAULT 22,
  telnet_port       INTEGER NOT NULL DEFAULT 23,
  snmp_version      TEXT NOT NULL DEFAULT '2c' CHECK (snmp_version IN ('2c','3')),
  snmp_community_enc BLOB,
  snmp_v3_user      TEXT,
  snmp_v3_auth_enc  BLOB,
  snmp_v3_priv_enc  BLOB,
  snmp_v3_level     TEXT NOT NULL DEFAULT 'authPriv'
                    CHECK (snmp_v3_level IN ('noAuthNoPriv','authNoPriv','authPriv')),
  username          TEXT,
  password_enc      BLOB,
  notes             TEXT,
  last_seen         TEXT,
  last_state        TEXT,
  created_at        TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at        TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS ix_devices_kind ON devices(kind);
CREATE INDEX IF NOT EXISTS ix_devices_site ON devices(site_id);

CREATE TABLE IF NOT EXISTS vlans (
  id        INTEGER PRIMARY KEY,
  vlan_id   INTEGER NOT NULL CHECK (vlan_id BETWEEN 1 AND 4094),
  name      TEXT NOT NULL,
  site_id   INTEGER REFERENCES sites(id),
  UNIQUE (site_id, vlan_id)
);

CREATE TABLE IF NOT EXISTS subnets (
  id          INTEGER PRIMARY KEY,
  cidr        TEXT NOT NULL UNIQUE,
  gateway     TEXT,
  vlan_id     INTEGER,
  site_id     INTEGER REFERENCES sites(id),
  description TEXT
);

CREATE TABLE IF NOT EXISTS interfaces (
  id        INTEGER PRIMARY KEY,
  device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  name      TEXT NOT NULL,
  vlan_id   INTEGER,
  ip        TEXT,
  prefix    INTEGER,
  uplink    TEXT,
  UNIQUE (device_id, name)
);

CREATE TABLE IF NOT EXISTS health_samples (
  id           INTEGER PRIMARY KEY,
  device_id    INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  ts           TEXT NOT NULL DEFAULT (datetime('now')),
  cpu_pct      REAL,
  mem_pct      REAL,
  uptime_ticks INTEGER,
  if_up        INTEGER,
  if_total     INTEGER,
  state        TEXT NOT NULL CHECK (state IN ('up','down','unknown'))
);

CREATE INDEX IF NOT EXISTS ix_health ON health_samples(device_id, ts DESC);

CREATE TABLE IF NOT EXISTS events (
  id        INTEGER PRIMARY KEY,
  device_id INTEGER REFERENCES devices(id) ON DELETE CASCADE,
  ts        TEXT NOT NULL DEFAULT (datetime('now')),
  severity  TEXT NOT NULL CHECK (severity IN ('info','warning','average','high','disaster')),
  message   TEXT NOT NULL,
  -- stable fingerprint so an identical condition is not re-filed every poll
  sig       TEXT,
  -- set by the operator once they have dealt with it
  acked     INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS ix_events_ts ON events(ts DESC);

CREATE VIEW IF NOT EXISTS v_inventory AS
SELECT d.id,
       d.hostname,
       d.mgmt_ip,
       COALESCE(d.kind_override, d.kind) AS effective_kind,
       d.kind_confidence,
       d.vendor,
       d.model,
       d.asset_tag,
       s.name AS site,
       d.last_seen,
       d.last_state
FROM devices d
LEFT JOIN sites s ON s.id = d.site_id;

CREATE TRIGGER IF NOT EXISTS trg_devices_touch
AFTER UPDATE ON devices
FOR EACH ROW
BEGIN
  UPDATE devices SET updated_at = datetime('now') WHERE id = OLD.id;
END;

-- ---------------------------------------------------------------- v3 dashboard
ALTER TABLE health_samples ADD COLUMN disk_pct   REAL;
ALTER TABLE health_samples ADD COLUMN net_in_bps REAL;
ALTER TABLE health_samples ADD COLUMN net_out_bps REAL;

CREATE TABLE IF NOT EXISTS users (
  id INTEGER PRIMARY KEY,
  username TEXT NOT NULL UNIQUE,
  pw_hash  TEXT NOT NULL,
  role     TEXT NOT NULL DEFAULT 'viewer' CHECK (role IN ('viewer','admin')),
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS sessions (
  jti TEXT PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at TEXT NOT NULL,
  ip TEXT, ua TEXT,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS links (
  id INTEGER PRIMARY KEY,
  a_device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  b_device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  if_a TEXT, if_b TEXT,
  proto TEXT CHECK (proto IN ('ospf','lldp','static')),
  UNIQUE (a_device_id, b_device_id, if_a)
);

CREATE TABLE IF NOT EXISTS settings (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

-- ================================================================ v4
-- Everything below is additive: an existing v3 database gains the new
-- tables/columns without losing collected history.

-- ---------------------------------------------------------------- leases
-- "Which PC holds which IP, with which MAC and which device name."
-- One row per (ip, mac) pair, kept until the lease disappears from the
-- routers for LEASE_GRACE seconds, so the table is a history, not just a
-- snapshot. `online` is what the poller saw on the most recent cycle.
CREATE TABLE IF NOT EXISTS ip_leases (
  id          INTEGER PRIMARY KEY,
  ip          TEXT NOT NULL,
  mac         TEXT NOT NULL,
  hostname    TEXT,
  device_name TEXT NOT NULL DEFAULT '',        -- friendly label shown in the UI
  vlan_id     INTEGER,
  subnet      TEXT,
  -- where the binding was observed
  seen_on     TEXT NOT NULL DEFAULT '',        -- router hostname that reported it
  switch_name TEXT NOT NULL DEFAULT '',        -- bridge/interface it was learned on
  if_index    INTEGER,
  source      TEXT NOT NULL DEFAULT 'arp'
              CHECK (source IN ('arp','dhcp','dhcp-static','manual','snmp')),
  is_static   INTEGER NOT NULL DEFAULT 0 CHECK (is_static IN (0,1)),
  online      INTEGER NOT NULL DEFAULT 1 CHECK (online IN (0,1)),
  first_seen  TEXT NOT NULL DEFAULT (datetime('now')),
  last_seen   TEXT NOT NULL DEFAULT (datetime('now')),
  -- a MAC can move: remember the previous bindings
  moved_from  TEXT,
  UNIQUE (ip, mac)
);

CREATE INDEX IF NOT EXISTS ix_leases_ip    ON ip_leases(ip);
CREATE INDEX IF NOT EXISTS ix_leases_mac   ON ip_leases(mac);
CREATE INDEX IF NOT EXISTS ix_leases_seen  ON ip_leases(last_seen DESC);
CREATE INDEX IF NOT EXISTS ix_leases_online ON ip_leases(online, last_seen DESC);

-- Full observation history: one row per poller cycle per binding would be
-- enormous at a 5 s interval, so we only write a row when something changes
-- (appear / disappear / move / rename).
CREATE TABLE IF NOT EXISTS lease_events (
  id        INTEGER PRIMARY KEY,
  ts        TEXT NOT NULL DEFAULT (datetime('now')),
  ip        TEXT NOT NULL,
  mac       TEXT NOT NULL,
  kind      TEXT NOT NULL CHECK (kind IN ('new','gone','moved','renamed','static')),
  detail    TEXT
);
CREATE INDEX IF NOT EXISTS ix_lease_events_ts ON lease_events(ts DESC);

-- ---------------------------------------------------------------- power
-- A device that reboots has its SNMP uptime reset to a small number.
-- That is the only reliable way to see a power cut from the network side,
-- because during the outage the box simply stops answering.
CREATE TABLE IF NOT EXISTS device_reboots (
  id            INTEGER PRIMARY KEY,
  device_id     INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  ts            TEXT NOT NULL DEFAULT (datetime('now')),
  prev_uptime_s INTEGER,      -- uptime seen on the previous good poll
  new_uptime_s  INTEGER,      -- uptime seen on the first poll after the restart
  gap_s         INTEGER,      -- seconds with no successful poll before it came back
  kind          TEXT NOT NULL DEFAULT 'restart'
                CHECK (kind IN ('restart','power-loss','crash','first-seen')),
  note          TEXT
);
CREATE INDEX IF NOT EXISTS ix_reboots_ts ON device_reboots(ts DESC);
CREATE INDEX IF NOT EXISTS ix_reboots_dev ON device_reboots(device_id, ts DESC);

-- Raw reachability timeline. Needed to tell "was off for 40 s" (power) from
-- "was off for 3 s" (a single dropped SNMP reply).
CREATE TABLE IF NOT EXISTS reach_samples (
  id        INTEGER PRIMARY KEY,
  device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  ts        TEXT NOT NULL DEFAULT (datetime('now')),
  reachable INTEGER NOT NULL CHECK (reachable IN (0,1)),
  rtt_ms    INTEGER
);
CREATE INDEX IF NOT EXISTS ix_reach ON reach_samples(device_id, ts DESC);

-- ---------------------------------------------------------------- shifts
-- Who receives alert mail right now. `starts_at`/`ends_at` are local wall
-- clock (HH:MM) so a roster survives daylight-saving changes.
CREATE TABLE IF NOT EXISTS shift_admins (
  id         INTEGER PRIMARY KEY,
  name       TEXT NOT NULL,
  email      TEXT NOT NULL,
  phone      TEXT,
  -- weekly availability: 0=Monday .. 6=Sunday, stored as a bitmask
  days_mask  INTEGER NOT NULL DEFAULT 127,
  starts_at  TEXT NOT NULL DEFAULT '08:00',
  ends_at    TEXT NOT NULL DEFAULT '17:00',
  timezone   TEXT NOT NULL DEFAULT 'Asia/Ho_Chi_Minh',
  role       TEXT NOT NULL DEFAULT 'primary' CHECK (role IN ('primary','backup')),
  active     INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0,1)),
  sort_order INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Every handover. The newest row is the authoritative on-call admin, which
-- is what lets the *next* shift take over when the outgoing admin forgot.
CREATE TABLE IF NOT EXISTS shift_handovers (
  id          INTEGER PRIMARY KEY,
  ts          TEXT NOT NULL DEFAULT (datetime('now')),
  from_admin  INTEGER REFERENCES shift_admins(id) ON DELETE SET NULL,
  to_admin    INTEGER NOT NULL REFERENCES shift_admins(id) ON DELETE CASCADE,
  by_user     TEXT NOT NULL DEFAULT '',
  reason      TEXT NOT NULL DEFAULT 'scheduled'
              CHECK (reason IN ('scheduled','overtime','manual','escalation','auto')),
  note        TEXT,
  -- when the outgoing admin was supposed to hand over; overdue = now > due_at
  due_at      TEXT
);
CREATE INDEX IF NOT EXISTS ix_handover_ts ON shift_handovers(ts DESC);

-- ---------------------------------------------------------------- alerts
CREATE TABLE IF NOT EXISTS alert_rules (
  id          INTEGER PRIMARY KEY,
  name        TEXT NOT NULL UNIQUE,
  enabled     INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
  -- which severities trigger a mail
  min_severity TEXT NOT NULL DEFAULT 'average'
               CHECK (min_severity IN ('info','warning','average','high','disaster')),
  -- throttle: at most one mail per rule per N seconds
  cooldown_s  INTEGER NOT NULL DEFAULT 300,
  -- optional match on the message, SQL LIKE pattern
  match_like  TEXT,
  recipients  TEXT NOT NULL DEFAULT 'oncall',   -- 'oncall' | 'all' | csv of emails
  created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS alert_deliveries (
  id         INTEGER PRIMARY KEY,
  ts         TEXT NOT NULL DEFAULT (datetime('now')),
  rule_id    INTEGER REFERENCES alert_rules(id) ON DELETE SET NULL,
  event_id   INTEGER,
  admin_id   INTEGER REFERENCES shift_admins(id) ON DELETE SET NULL,
  to_addr    TEXT NOT NULL,
  subject    TEXT NOT NULL,
  status     TEXT NOT NULL DEFAULT 'queued'
             CHECK (status IN ('queued','sent','failed','suppressed')),
  error      TEXT,
  tries      INTEGER NOT NULL DEFAULT 0,
  kind       TEXT NOT NULL DEFAULT 'rule'
             CHECK (kind IN ('rule','redispatch','test'))
);
CREATE INDEX IF NOT EXISTS ix_deliv_ts ON alert_deliveries(ts DESC);

-- ---------------------------------------------------------------- log analysis
-- Patterns the analyser matches against raw device output / events.
-- Kept in the DB so an operator can add one without a redeploy.
CREATE TABLE IF NOT EXISTS log_patterns (
  id          INTEGER PRIMARY KEY,
  name        TEXT NOT NULL UNIQUE,
  -- SQLite-flavoured regex is not available, so these are LIKE patterns with
  -- % wildcards; `regex` holds an optional stricter POSIX pattern used in Go.
  like_pat    TEXT NOT NULL,
  regex       TEXT,
  severity    TEXT NOT NULL DEFAULT 'warning'
              CHECK (severity IN ('info','warning','average','high','disaster')),
  category    TEXT NOT NULL DEFAULT 'general',
  hint        TEXT,          -- what an operator should do about it
  enabled     INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1))
);

CREATE TABLE IF NOT EXISTS log_findings (
  id         INTEGER PRIMARY KEY,
  ts         TEXT NOT NULL DEFAULT (datetime('now')),
  pattern_id INTEGER REFERENCES log_patterns(id) ON DELETE SET NULL,
  device_id  INTEGER REFERENCES devices(id) ON DELETE CASCADE,
  source     TEXT NOT NULL DEFAULT 'poller',
  line       TEXT NOT NULL,
  severity   TEXT NOT NULL,
  category   TEXT NOT NULL DEFAULT 'general',
  hint       TEXT,
  -- grouping so 500 identical lines collapse into one finding
  sig        TEXT NOT NULL,
  hits       INTEGER NOT NULL DEFAULT 1,
  first_ts   TEXT NOT NULL DEFAULT (datetime('now')),
  last_ts    TEXT NOT NULL DEFAULT (datetime('now')),
  acked      INTEGER NOT NULL DEFAULT 0 CHECK (acked IN (0,1))
);
CREATE INDEX IF NOT EXISTS ix_findings_ts  ON log_findings(last_ts DESC);
CREATE INDEX IF NOT EXISTS ix_findings_sig ON log_findings(sig);

-- ---------------------------------------------------------------- discovery
-- What the last automatic mapping run learned, before an operator trusts it.
-- proposal rows are reviewed: pending -> accepted|rejected.
CREATE TABLE IF NOT EXISTS link_proposals (
  id           INTEGER PRIMARY KEY,
  ts           TEXT NOT NULL DEFAULT (datetime('now')),
  a_device_id  INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  b_device_id  INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  if_a         TEXT,
  if_b         TEXT,
  source       TEXT NOT NULL DEFAULT 'ospf'
               CHECK (source IN ('ospf','lldp','arp','route','manual')),
  evidence     TEXT,
  status       TEXT NOT NULL DEFAULT 'pending'
               CHECK (status IN ('pending','accepted','rejected')),
  UNIQUE (a_device_id, b_device_id, if_a)
);
CREATE INDEX IF NOT EXISTS ix_prop_status ON link_proposals(status);

-- ---------------------------------------------------------------- DNS (optional)
CREATE TABLE IF NOT EXISTS dns_records (
  id        INTEGER PRIMARY KEY,
  name      TEXT NOT NULL,
  ip        TEXT NOT NULL,
  kind      TEXT NOT NULL DEFAULT 'A' CHECK (kind IN ('A','CNAME','PTR')),
  ttl       INTEGER NOT NULL DEFAULT 300,
  source    TEXT NOT NULL DEFAULT 'lease'
            CHECK (source IN ('lease','manual','reservation')),
  enabled   INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
  updated_at TEXT NOT NULL DEFAULT (datetime('now')),
  UNIQUE (name, kind)
);
CREATE INDEX IF NOT EXISTS ix_dns_ip ON dns_records(ip);

-- ---------------------------------------------------------------- seed rows
INSERT OR IGNORE INTO settings(key, value) VALUES
  ('poll_interval_s', '5'),
  ('lease_grace_s',   '120'),
  ('dns_enabled',     '0'),
  ('dns_zone',        'lab.local'),
  ('oncall_overdue_h', '1'),
  ('handover_valid_h', '12');

INSERT OR IGNORE INTO alert_rules(name, min_severity, cooldown_s, match_like, recipients) VALUES
  ('Device unreachable', 'average', 300, '%unreachable%', 'oncall'),
  ('Power loss / restart', 'average', 120, '%restart%',   'oncall'),
  ('Critical event', 'high', 600, NULL, 'oncall');

INSERT OR IGNORE INTO log_patterns(name, like_pat, regex, severity, category, hint) VALUES
  ('link down',        '%link down%',        '(?i)link (is )?down',                'average',  'interface', 'Check the cable and the switch port; if it flaps, replace the patch lead.'),
  ('link up',          '%link up%',          '(?i)link (is )?up',                  'info',     'interface', 'Recovery notice, no action needed.'),
  ('ospf neighbour down','%Nbr%down%',       '(?i)neighbor.*(down|Dead)',         'average',  'routing',   'Check the link to that peer and the OSPF timers on both sides.'),
  ('ospf adjacency full','%Full/%',          NULL,                                 'info',     'routing',   'Adjacency formed, no action needed.'),
  ('interface error',  '%errors%',           '(?i)(rx|tx)_errors|input errors',    'warning',  'interface', 'Suspect the cable or duplex mismatch; compare error counters over time.'),
  ('auth failure',     '%authentication fail%','(?i)auth(entication)? fail',       'high',     'security',  'Someone is trying to log in with wrong credentials; check the source address.'),
  ('dhcp exhausted',   '%no free leases%',   '(?i)no (free|available) leases',     'high',     'dhcp',      'The scope is full. Widen the pool or shorten the lease time.'),
  ('disk full',        '%No space left%',    '(?i)no space left',                  'disaster', 'system',    'Free space on the device or rotate logs; writes are already failing.'),
  ('process died',     '%exited%',           '(?i)(process|service).*(exited|terminated)','high','system',   'A daemon stopped. Check why it died before restarting it.'),
  ('kernel panic',     '%Kernel panic%',     '(?i)kernel panic|Oops',              'disaster', 'system',    'The kernel crashed. Capture the trace before rebooting.'),
  ('snmp timeout',     '%Timeout:%',         '(?i)timeout: no response',           'warning',  'monitoring','The device did not answer SNMP. Check reachability and the community string.'),
  ('config changed',   '%Configuration%',    '(?i)config(uration)? (changed|saved)', 'info',    'change',    'Someone edited the configuration. Confirm it was authorised.');
