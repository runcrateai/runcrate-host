// Command fakecp is a stand-in control plane for testing the agent on real
// hardware before the real host API is reachable. It listens on loopback
// only and queues commands posted to /admin/queue.
//
//	go run ./hack/fakecp -image-version v1 -image-sha <sha256>
//	curl -XPOST localhost:8787/admin/queue -d '{"kind":"create_vm","payload":{...}}'
//	curl localhost:8787/admin/state
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"
)

type command struct {
	ID      int64           `json:"id"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8787", "listen address (loopback)")
	imgURL := flag.String("image-url", "https://host-images.runcrate.ai/vm/runcrate-vm.qcow2", "image url")
	imgSHA := flag.String("image-sha", "", "image sha256")
	imgVer := flag.String("image-version", "", "image version")
	flag.Parse()

	var mu sync.Mutex
	var next int64 = 1
	var queue []command
	results := map[int64]json.RawMessage{}
	var lastHB json.RawMessage
	var benches []json.RawMessage
	mux := http.NewServeMux()
	read := func(r *http.Request) json.RawMessage {
		var m json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&m)
		return m
	}
	out := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	auth := func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer fake-cred" }
	mux.HandleFunc("POST /register", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("register: %s", read(r))
		out(w, map[string]any{"host_id": "fake-host", "host_credential": "fake-cred", "poll_interval_s": 10})
	})
	mux.HandleFunc("POST /heartbeat", func(w http.ResponseWriter, r *http.Request) {
		if !auth(r) {
			w.WriteHeader(401)
			return
		}
		hb := read(r)
		mu.Lock()
		defer mu.Unlock()
		lastHB = hb
		var cmds []command
		for _, c := range queue {
			if _, done := results[c.ID]; !done {
				cmds = append(cmds, c)
			}
		}
		out(w, map[string]any{"poll_interval_s": 10, "commands": cmds})
	})
	mux.HandleFunc("POST /commands/{id}/result", func(w http.ResponseWriter, r *http.Request) {
		if !auth(r) {
			w.WriteHeader(401)
			return
		}
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		res := read(r)
		log.Printf("result %d: %s", id, res)
		mu.Lock()
		results[id] = res
		mu.Unlock()
		w.WriteHeader(204)
	})
	mux.HandleFunc("POST /benchmark", func(w http.ResponseWriter, r *http.Request) {
		b := read(r)
		log.Printf("benchmark: %d bytes", len(b))
		mu.Lock()
		benches = append(benches, b)
		mu.Unlock()
		out(w, map[string]any{"verified": true, "reasons": []string{}})
	})
	mux.HandleFunc("GET /image", func(w http.ResponseWriter, r *http.Request) {
		out(w, map[string]any{"url": *imgURL, "sha256": *imgSHA, "version": *imgVer})
	})
	mux.HandleFunc("POST /admin/queue", func(w http.ResponseWriter, r *http.Request) {
		var c command
		_ = json.NewDecoder(r.Body).Decode(&c)
		mu.Lock()
		c.ID = next
		next++
		queue = append(queue, c)
		mu.Unlock()
		out(w, c)
	})
	mux.HandleFunc("GET /admin/state", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		out(w, map[string]any{"at": time.Now().UTC(), "heartbeat": lastHB, "results": results, "benchmarks": benches})
	})
	log.Printf("fakecp on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}
