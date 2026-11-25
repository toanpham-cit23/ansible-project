package main

// SSH access pool — the central place an operator looks up how to reach a
// switch, router or PC. Passwords are never returned by this endpoint: the
// browser shows only that a credential is stored, and the existing
// PUT /api/devices/{id}/credentials route is the only way to replace it.
//
// GET /api/ssh/pool
//   -> { devices:[{id, hostname, ip, kind, username, port, has_credential}],
//       total:N, with_credential:M }

import (
	"database/sql"
	"net/http"
	"sort"
)

type sshPoolEntry struct {
	ID            int64  `json:"id"`
	Hostname      string `json:"hostname"`
	IP            string `json:"ip"`
	Kind          string `json:"kind"`
	Username      string `json:"username"`
	Port          int    `json:"port"`
	HasCredential bool   `json:"has_credential"`
}

func handleSSHPool(w http.ResponseWriter, r *http.Request, u *User) {
	rows, err := db.Query(`SELECT id, hostname, mgmt_ip,
	                              COALESCE(kind_override, kind),
	                              COALESCE(username,''),
	                              COALESCE(ssh_port,22),
	                              CASE WHEN password_enc IS NULL OR length(password_enc)=0
	                                   THEN 0 ELSE 1 END
	                       FROM devices ORDER BY hostname`)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	out := []sshPoolEntry{}
	withCred := 0
	for rows.Next() {
		var e sshPoolEntry
		var port sql.NullInt64
		var has int
		if rows.Scan(&e.ID, &e.Hostname, &e.IP, &e.Kind, &e.Username, &port, &has) != nil {
			continue
		}
		e.Port = 22
		if port.Valid && port.Int64 > 0 {
			e.Port = int(port.Int64)
		}
		e.HasCredential = has == 1
		if e.HasCredential {
			withCred++
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hostname < out[j].Hostname })

	writeJSON(w, http.StatusOK, map[string]any{
		"devices":        out,
		"total":          len(out),
		"with_credential": withCred,
	})
}
