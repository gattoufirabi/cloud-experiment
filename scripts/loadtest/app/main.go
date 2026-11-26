// Load test app: tiny static binary, a few MB of memory.
//
//	/            answers at once ("ok <host>"): proxy and sandbox overhead only
//	/work        hashes a buffer ROUNDS times: CPU-bound (about 2-3 ms of one core at 4000)
//	/sleep?ms=N  waits N ms (max 30000) before answering: holds connections open
//	/bytes?kb=N  sends N KiB (max 10240): bandwidth
package main

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"
)

func intParam(r *http.Request, name string, def, max int) int {
	n, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil || n < 0 {
		return def
	}
	return min(n, max)
}

func main() {
	rounds := 4000
	if v, err := strconv.Atoi(os.Getenv("ROUNDS")); err == nil && v > 0 {
		rounds = v
	}
	host, _ := os.Hostname()
	chunk := make([]byte, 1024)
	for i := range chunk {
		chunk[i] = 'x'
	}
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok", host) })
	http.HandleFunc("/work", func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 256)
		var h [32]byte
		for i := 0; i < rounds; i++ {
			h = sha256.Sum256(b)
			copy(b, h[:])
		}
		fmt.Fprintf(w, "%x %s\n", h[:4], host)
	})
	http.HandleFunc("/sleep", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(time.Duration(intParam(r, "ms", 1000, 30000)) * time.Millisecond):
			fmt.Fprintln(w, "ok", host)
		case <-r.Context().Done():
		}
	})
	http.HandleFunc("/bytes", func(w http.ResponseWriter, r *http.Request) {
		n := intParam(r, "kb", 100, 10240)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(n*1024))
		for i := 0; i < n; i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	})
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	srv := &http.Server{Addr: ":" + port, ReadHeaderTimeout: 10 * time.Second}
	_ = srv.ListenAndServe()
}
