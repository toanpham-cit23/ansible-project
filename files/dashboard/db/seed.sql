INSERT OR IGNORE INTO sites(id, name, location) VALUES (1, 'Lab', 'VMware');

INSERT OR IGNORE INTO devices(id, hostname, mgmt_ip, site_id, kind, kind_confidence, username, snmp_version)
VALUES (1,'r1','192.168.23.11',1,'router',0.83,'root','2c'),
       (2,'r2','192.168.23.12',1,'router',0.83,'root','2c'),
       (3,'r3','192.168.23.13',1,'router',0.83,'root','2c'),
       (10,'controller','192.168.23.10',1,'server',1.0,'root','2c');

INSERT OR IGNORE INTO links(a_device_id, b_device_id, if_a, if_b, proto)
VALUES (10,1,'eth0','eth1','static'),
       (10,2,'eth0','eth1','static'),
       (10,3,'eth0','eth1','static'),
       (1,2,'eth1','eth1','ospf'),
       (2,3,'eth1','eth1','ospf');
