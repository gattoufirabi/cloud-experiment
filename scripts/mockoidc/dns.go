package main

import (
	"encoding/binary"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
)

// A tiny authoritative DNS server for the end-to-end test. Records are set over HTTP:
//
//	POST /dns/set?name=&txt=   a TXT record (email domain verification)
//	POST /dns/set?name=&mx=    an MX record (direct email delivery); name "*" answers any name
//	POST /dns/set?name=&a=     an A record (the mail server's address)
//	POST /dns/clear?name=      removes a name's records

type rec struct{ typ, val string }

type txtStore struct {
	mu   sync.Mutex
	recs map[string][]rec
}

func normName(n string) string { return strings.TrimSuffix(strings.ToLower(n), ".") }

func (s *txtStore) records(name, typ string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, r := range s.recs[normName(name)] {
		if r.typ == typ {
			out = append(out, r.val)
		}
	}
	if len(out) == 0 && typ == "MX" {
		for _, r := range s.recs["*"] {
			if r.typ == "MX" {
				out = append(out, r.val)
			}
		}
	}
	return out
}

func (s *txtStore) known(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.recs[normName(name)]) > 0 || len(s.recs["*"]) > 0
}

// get returns the TXT records of a name.
func (s *txtStore) get(name string) []string { return s.records(name, "TXT") }

func (s *txtStore) handlers() {
	http.HandleFunc("/dns/set", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		n := normName(r.URL.Query().Get("name"))
		if s.recs == nil {
			s.recs = map[string][]rec{}
		}
		for _, typ := range []string{"txt", "mx", "a"} {
			if v := r.URL.Query().Get(typ); v != "" {
				s.recs[n] = append(s.recs[n], rec{strings.ToUpper(typ), v})
			}
		}
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	http.HandleFunc("/dns/clear", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		delete(s.recs, normName(r.URL.Query().Get("name")))
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
}

// parseQuestion returns the query name, type and the end offset of the question.
func parseQuestion(m []byte) (string, uint16, int, bool) {
	if len(m) < 12 || binary.BigEndian.Uint16(m[4:6]) != 1 {
		return "", 0, 0, false
	}
	i := 12
	var labels []string
	for {
		if i >= len(m) {
			return "", 0, 0, false
		}
		l := int(m[i])
		i++
		if l == 0 {
			break
		}
		if l > 63 || i+l > len(m) {
			return "", 0, 0, false
		}
		labels = append(labels, string(m[i:i+l]))
		i += l
	}
	if i+4 > len(m) {
		return "", 0, 0, false
	}
	return strings.Join(labels, "."), binary.BigEndian.Uint16(m[i : i+2]), i + 4, true
}

func encodeName(name string) []byte {
	var b []byte
	for _, l := range strings.Split(normName(name), ".") {
		if l == "" {
			continue
		}
		b = append(append(b, byte(len(l))), l...)
	}
	return append(b, 0)
}

func rr(typ uint16, rdata []byte) []byte {
	r := []byte{0xC0, 0x0C}
	r = binary.BigEndian.AppendUint16(r, typ)
	r = append(r, 0, 1, 0, 0, 0, 1)
	r = binary.BigEndian.AppendUint16(r, uint16(len(rdata)))
	return append(r, rdata...)
}

func dnsAnswer(q []byte, store *txtStore) []byte {
	name, qtype, end, ok := parseQuestion(q)
	if !ok {
		return nil
	}
	var answers [][]byte
	switch qtype {
	case 16: // TXT
		for _, txt := range store.records(name, "TXT") {
			var rdata []byte
			for len(txt) > 255 {
				rdata = append(append(rdata, 255), txt[:255]...)
				txt = txt[255:]
			}
			answers = append(answers, rr(16, append(append(rdata, byte(len(txt))), txt...)))
		}
	case 15: // MX
		for _, host := range store.records(name, "MX") {
			answers = append(answers, rr(15, append([]byte{0, 10}, encodeName(host)...)))
		}
	case 1: // A
		for _, a := range store.records(name, "A") {
			if ip := net.ParseIP(a).To4(); ip != nil {
				answers = append(answers, rr(1, ip))
			}
		}
	}
	resp := make([]byte, 12, 512)
	copy(resp, q[:2])
	flags := uint16(0x8500) // response, authoritative, recursion desired echoed below
	flags |= binary.BigEndian.Uint16(q[2:4]) & 0x0100
	if len(answers) == 0 && !store.known(name) {
		flags |= 3 // NXDOMAIN
	}
	binary.BigEndian.PutUint16(resp[2:], flags)
	binary.BigEndian.PutUint16(resp[4:], 1)
	binary.BigEndian.PutUint16(resp[6:], uint16(len(answers)))
	resp = append(resp, q[12:end]...)
	for _, a := range answers {
		resp = append(resp, a...)
	}
	return resp
}

func serveDNS(addr string, store *txtStore) {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("mock DNS on %s/udp", addr)
	buf := make([]byte, 1500)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			continue
		}
		if resp := dnsAnswer(buf[:n], store); resp != nil {
			_, _ = pc.WriteTo(resp, from)
		}
	}
}
