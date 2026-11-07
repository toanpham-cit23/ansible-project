// dns.go — a tiny authoritative DNS server for the lab zone.
//
// The dashboard can hand out hostnames for the addresses it already tracks,
// so http://lab.local:8080 works instead of an IP. This is deliberately a
// small, single-zone server: it answers A and PTR for the records the
// operator maintains in the dns_records table, and it forwards everything it
// does not know to the upstream resolver rather than guessing.
//
// It only runs when LAB_DNS_LISTEN is set, because port 53 needs privilege
// that the container does not otherwise want.
package main

import (
	"encoding/binary"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	dnsA     = 1
	dnsCNAME = 5
	dnsPTR   = 12
	dnsMX    = 15
	dnsTXT   = 16
	dnsAAAA  = 28
	dnsTypeA = 1
	dnsClass = 1
)

// dnsRecord is what the server needs from a row; the rest of the table is UI
// metadata and does not affect an answer.
type dnsRecord struct {
	Name string
	IP   string
	TTL  uint32
}

type dnsCache struct {
	mu    sync.RWMutex
	byFQN map[string]dnsRecord // "r1.lab.local." -> record
	byRev map[string]string    // "20.23.168.192.in-addr.arpa." -> "r1.lab.local."
	zone  string               // "lab.local"
}

var dnsCacheStore = &dnsCache{byFQN: map[string]dnsRecord{}, byRev: map[string]string{}}

// dnsFQN lowercases a name and guarantees the trailing dot.
func dnsFQN(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}

// reverseName turns an IPv4 address into its in-addr.arpa form.
func reverseName(ip string) string {
	p := net.ParseIP(strings.TrimSpace(ip))
	if p == nil || p.To4() == nil {
		return ""
	}
	b := p.To4()
	return dnsFQN(itoa(int(b[3])) + "." + itoa(int(b[2])) + "." +
		itoa(int(b[1])) + "." + itoa(int(b[0])) + ".in-addr.arpa")
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// reloadDNS refreshes the in-memory view from the database. It is called at
// startup and after every write, so a record change is visible immediately
// instead of after a TTL.
func reloadDNS() {
	zone := dnsFQN(setting("dns_zone", "lab.local"))
	next := &dnsCache{byFQN: map[string]dnsRecord{}, byRev: map[string]string{}, zone: zone}

	rs, err := db.Query(`SELECT name, ip, ttl FROM dns_records`)
	if err == nil {
		defer rs.Close()
		for rs.Next() {
			var name, ip string
			var ttl int
			if rs.Scan(&name, &ip, &ttl) != nil {
				continue
			}
			fqn := dnsFQN(name)
			// a bare label belongs to the zone; a dotted name is taken as-is
			if !strings.Contains(strings.TrimSuffix(fqn, "."), ".") {
				fqn = dnsFQN(strings.TrimSuffix(fqn, ".") + "." + strings.TrimSuffix(zone, "."))
			}
			if ttl <= 0 {
				ttl = 60
			}
			next.byFQN[fqn] = dnsRecord{Name: fqn, IP: ip, TTL: uint32(ttl)}
			if rev := reverseName(ip); rev != "" {
				next.byRev[rev] = fqn
			}
		}
	}

	dnsCacheStore.mu.Lock()
	dnsCacheStore.byFQN = next.byFQN
	dnsCacheStore.byRev = next.byRev
	dnsCacheStore.zone = next.zone
	dnsCacheStore.mu.Unlock()
}

// ---------------------------------------------------------------- wire format

// dnsQ is one parsed question.
type dnsQ struct {
	name  string
	qtype uint16
}

// parseQName reads a length-prefixed name, following compression pointers.
func parseQName(msg []byte, off int) (string, int, bool) {
	var sb strings.Builder
	jumped := false
	origOff := off
	hops := 0
	for {
		if off >= len(msg) {
			return "", 0, false
		}
		l := int(msg[off])
		if l == 0 {
			off++
			break
		}
		// compression pointer
		if l&0xC0 == 0xC0 {
			if off+1 >= len(msg) {
				return "", 0, false
			}
			ptr := int(binary.BigEndian.Uint16(msg[off:off+2]) & 0x3FFF)
			if !jumped {
				origOff = off + 2
				jumped = true
			}
			off = ptr
			hops++
			if hops > 24 { // malformed loop
				return "", 0, false
			}
			continue
		}
		if off+1+l > len(msg) {
			return "", 0, false
		}
		sb.Write(msg[off+1 : off+1+l])
		sb.WriteByte('.')
		off += 1 + l
	}
	if !jumped {
		origOff = off
	}
	return strings.ToLower(sb.String()), origOff, true
}

// writeName appends a name without compression. Servers are allowed to emit
// uncompressed names; it costs a few bytes and removes a whole class of bugs.
func writeName(buf []byte, name string) []byte {
	name = strings.TrimSuffix(name, ".")
	if name == "" {
		return append(buf, 0)
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) > 63 {
			label = label[:63]
		}
		buf = append(buf, byte(len(label)))
		buf = append(buf, label...)
	}
	return append(buf, 0)
}

// buildDNSResponse answers one query. A nil body means "not my zone" and the
// caller falls through to the upstream resolver.
// NODATA/NXDOMAIN replies. These matter more than they look: a resolver that
// gets SERVFAIL for AAAA or for an unused name retries and then reports the
// whole lookup as broken, so "lab.lab.local resolves" would still read as a
// failure to the user.
func dnsRcode(query []byte, rcode byte) ([]byte, bool) {
	if len(query) < 12 {
		return nil, false
	}
	out := make([]byte, 0, 64)
	out = append(out, query[0], query[1])
	out = append(out, 0x84|(query[2]&0x01), rcode)
	out = append(out, 0x00, 0x01) // QDCOUNT
	out = append(out, 0x00, 0x00) // ANCOUNT
	out = append(out, 0x00, 0x00)
	out = append(out, 0x00, 0x00)
	qEnd := 12
	if _, e, ok := parseQName(query, 12); ok {
		qEnd = e + 4
	}
	if qEnd > len(query) {
		qEnd = len(query)
	}
	out = append(out, query[12:qEnd]...)
	return out, true
}

// buildDNSResponse answers one query. A nil body means "not my zone" and the
// caller falls through to the upstream resolver.
func buildDNSResponse(query []byte, q dnsQ) ([]byte, bool) {
	dnsCacheStore.mu.RLock()
	rec, haveFQN := dnsCacheStore.byFQN[q.name]
	ptr, haveRev := dnsCacheStore.byRev[q.name]
	zone := dnsCacheStore.zone
	dnsCacheStore.mu.RUnlock()

	// Only names inside our zone are ours to answer, including the negative
	// answers. Anything else is left to the resolver.
	inZone := zone != "" && strings.HasSuffix(q.name, zone)
	inRev := strings.HasSuffix(q.name, ".in-addr.arpa.")

	var answerIP, answerName string
	var answerTTL uint32 = 60
	switch q.qtype {
	case dnsA:
		if !haveFQN {
			if inZone {
				// the name exists under the zone but has no address, or does
				// not exist at all; both are authoritative negatives
				return dnsRcode(query, 0x03)
			}
			return nil, false
		}
		answerIP, answerName, answerTTL = rec.IP, rec.Name, rec.TTL
	case dnsPTR:
		if !haveRev {
			if inRev {
				return dnsRcode(query, 0x03)
			}
			return nil, false
		}
		answerName, answerTTL = ptr, 60
	default:
		// AAAA/MX/TXT/SOA for a name we do own: "no data", not an error.
		if haveFQN || (inZone && zone != "") {
			return dnsRcode(query, 0x00) // NOERROR, zero answers
		}
		return nil, false
	}

	// echo the header, then the question, then one answer
	out := make([]byte, 0, 512)
	if len(query) < 12 {
		return nil, false
	}
	out = append(out, query[0], query[1]) // id
	// Byte 2 is QR(0x80) | opcode | AA(0x04) | TC | RD(0x01); byte 3 is
	// RA | Z | RCODE. Copying RD into byte 3 instead of byte 2 silently sets
	// RCODE=1 (FORMERR) even though the answer body is correct.
	rd := query[2] & 0x01
	out = append(out, 0x84|rd, 0x00)
	out = append(out, 0x00, 0x01) // QDCOUNT 1
	out = append(out, 0x00, 0x01) // ANCOUNT 1
	out = append(out, 0x00, 0x00) // NSCOUNT
	out = append(out, 0x00, 0x00) // ARCOUNT

	// question section, copied verbatim so the client can match it
	qEnd := 12
	if _, e, ok := parseQName(query, 12); ok {
		qEnd = e + 4
	}
	if qEnd > len(query) {
		return nil, false
	}
	out = append(out, query[12:qEnd]...)

	// answer: name pointer to the question at offset 12
	out = append(out, 0xC0, 0x0C)
	if q.qtype == dnsA {
		out = append(out, 0x00, byte(dnsA))
		out = append(out, 0x00, byte(dnsClass))
		var t [4]byte
		binary.BigEndian.PutUint32(t[:], answerTTL)
		out = append(out, t[:]...)
		ip := net.ParseIP(answerIP).To4()
		if ip == nil {
			return nil, false
		}
		out = append(out, 0x00, 0x04)
		out = append(out, ip...)
	} else {
		out = append(out, 0x00, byte(dnsPTR))
		out = append(out, 0x00, byte(dnsClass))
		var t [4]byte
		binary.BigEndian.PutUint32(t[:], answerTTL)
		out = append(out, t[:]...)
		body := writeName(nil, answerName)
		out = append(out, byte(len(body)>>8), byte(len(body)))
		out = append(out, body...)
	}
	_ = zone
	return out, true
}

// dnsServe answers one UDP query: own the zone, otherwise forward upstream.
func dnsServe(conn *net.UDPConn, upstream string) {
	buf := make([]byte, 1500)
	for {
		n, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if n < 12 {
			continue
		}
		query := make([]byte, n)
		copy(query, buf[:n])

		name, _, ok := parseQName(query, 12)
		qtype := uint16(0)
		if ok {
			// question name, then QTYPE/QCLASS
			if _, e, ok2 := parseQName(query, 12); ok2 && e+4 <= len(query) {
				qtype = binary.BigEndian.Uint16(query[e : e+2])
			}
		}

		if ok {
			if resp, handled := buildDNSResponse(query, dnsQ{name: name, qtype: qtype}); handled {
				conn.WriteToUDP(resp, addr)
				continue
			}
		}

		// not ours: forward, preserving the client's transaction id
		if upstream == "" {
			// SERVFAIL so the client retries elsewhere instead of hanging
			fail := make([]byte, 12)
			copy(fail, query[:2])
			fail[2] = 0x84
			fail[3] = 0x02 // SERVFAIL
			fail[4], fail[5] = 0x00, 0x01
			conn.WriteToUDP(fail, addr)
			continue
		}
		up, err := net.DialTimeout("udp", upstream, 2*time.Second)
		if err != nil {
			continue
		}
		up.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := up.Write(query); err != nil {
			up.Close()
			continue
		}
		rb := make([]byte, 1500)
		if rn, err := up.Read(rb); err == nil {
			conn.WriteToUDP(rb[:rn], addr)
		}
		up.Close()
	}
}

// startDNS runs the responder when LAB_DNS_LISTEN is set, e.g. ":53".
// A failure to bind is logged and ignored: the dashboard must still start,
// the web UI simply stays reachable by address.
func startDNS() {
	listen := strings.TrimSpace(os.Getenv("LAB_DNS_LISTEN"))
	if listen == "" {
		return
	}
	upstream := strings.TrimSpace(os.Getenv("LAB_DNS_UPSTREAM"))
	if upstream != "" && !strings.Contains(upstream, ":") {
		upstream += ":53"
	}
	reloadDNS()

	udpAddr, err := net.ResolveUDPAddr("udp", listen)
	if err != nil {
		log.Printf("dns: bad listen address %q: %v", listen, err)
		return
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		log.Printf("dns: cannot bind %s: %v", listen, err)
		return
	}
	log.Printf("dns: serving zone %q on %s (upstream %s)",
		dnsCacheStore.zone, listen, upstream)
	go dnsServe(conn, upstream)
}
