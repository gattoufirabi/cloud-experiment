package main

import (
	"encoding/binary"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
)

// A tiny authoritative DNS server for the end-to-end test: it answers TXT queries from
// records set over HTTP (POST /dns/set?name=&txt=, POST /dns/clear?name=), so email
// domain verification can be tested without a real zone.

type txtStore struct {
	mu   sync.Mutex
	recs map[string][]string
}

func normName(n string) string { return strings.TrimSuffix(strings.ToLower(n), ".") }

func (s *txtStore) get(name string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.recs[normName(name)]...)
}

func (s *txtStore) handlers() {
	http.HandleFunc("/dns/set", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		n := normName(r.URL.Query().Get("name"))
		s.recs[n] = append(s.recs[n], r.URL.Query().Get("txt"))
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

func dnsAnswer(q []byte, store *txtStore) []byte {
	name, qtype, end, ok := parseQuestion(q)
	if !ok {
		return nil
	}
	var answers [][]byte
	if qtype == 16 { // TXT
		for _, txt := range store.get(name) {
			var rdata []byte
			for len(txt) > 255 {
				rdata = append(append(rdata, 255), txt[:255]...)
				txt = txt[255:]
			}
			rdata = append(append(rdata, byte(len(txt))), txt...)
			rr := []byte{0xC0, 0x0C, 0, 16, 0, 1, 0, 0, 0, 1}
			rr = binary.BigEndian.AppendUint16(rr, uint16(len(rdata)))
			answers = append(answers, append(rr, rdata...))
		}
	}
	resp := make([]byte, 12, 512)
	copy(resp, q[:2])
	flags := uint16(0x8500) // response, authoritative, recursion desired echoed below
	flags |= binary.BigEndian.Uint16(q[2:4]) & 0x0100
	if len(answers) == 0 && len(store.get(name)) == 0 {
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
