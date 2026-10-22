// Command fakelocalapi stands in for tailscaled's LocalAPI in the e2e
// harness: it answers GET /localapi/v0/whois on a Unix socket from a JSON
// file mapping source IPs to peers. Test-only; never part of a release.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"time"
)

// peer is the WhoIs node, in tailscaled's field names.
//
//nolint:tagliatelle // tailscaled's wire format
type peer struct {
	Name         string   `json:"Name"`
	ComputedName string   `json:"ComputedName"`
	Tags         []string `json:"Tags"`
}

func main() {
	socket := flag.String("socket", "/run/fake-tailscaled.sock", "Unix socket to listen on")
	peersFile := flag.String("peers", "/e2e/peers.json", "JSON object: IP -> peer")
	flag.Parse()

	_ = os.Remove(*socket)
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "unix", *socket)
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /localapi/v0/whois", func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.URL.Query().Get("addr"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Read on every call, so a test can change identities mid-run.
		raw, err := os.ReadFile(*peersFile)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var peers map[string]peer
		if err := json.Unmarshal(raw, &peers); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		p, ok := peers[host]
		if !ok {
			http.Error(w, "no peer", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]peer{"Node": p})
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.Serve(ln))
}
