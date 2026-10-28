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
  message   TEXT NOT NULL
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
