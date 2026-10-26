// Package metadata serves http://169.254.169.254 to VMs. Each VM is known by
// the guest address of its own /30, which it can't spoof (strict reverse-path
// filtering on its tap, and the firewall only lets redirected traffic in).
package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// Entry is what the server knows about one VM.
type Entry struct {
	VMID      string
	Ephemeral bool
	// Reclaim is the reclaim deadline (zero = none).
	Reclaim time.Time
	result  chan []byte
}

// Server is the metadata server.
type Server struct {
	mu      sync.Mutex
	byIP    map[string]*Entry
	subnet  *net.IPNet
	srv     *http.Server
	maxBody int64
}

// New returns a server for VMs inside subnet.
func New(subnet *net.IPNet) *Server {
	s := &Server{byIP: map[string]*Entry{}, subnet: subnet, maxBody: 4 << 20}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /runcrate/v1/instance", s.instance)
	mux.HandleFunc("GET /runcrate/v1/reclaim-notice", s.reclaim)
	// The AWS spot shape, so existing spot-aware tools work unchanged.
	mux.HandleFunc("GET /latest/meta-data/spot/instance-action", s.awsSpot)
	mux.HandleFunc("POST /runcrate/v1/ephemeral-result", s.ephemeralResult)
	s.srv = &http.Server{
		Handler:           s.guard(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	return s
}

// Handler is the HTTP handler (tests).
func (s *Server) Handler() http.Handler { return s.srv.Handler }

// Serve listens on addr until ctx ends.
func (s *Server) Serve(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp4", addr)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		sh, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(sh)
	}()
	if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Set registers (or replaces) a VM by its guest address.
func (s *Server) Set(guestIP string, e *Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.byIP[guestIP]; ok && old.VMID == e.VMID {
		e.result = old.result
	}
	if e.result == nil {
		e.result = make(chan []byte, 1)
	}
	s.byIP[guestIP] = e
}

// Remove forgets a VM.
func (s *Server) Remove(guestIP string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byIP, guestIP)
}

// SetReclaim sets the reclaim deadline of the VM with vmID.
func (s *Server) SetReclaim(vmID string, deadline time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.byIP {
		if e.VMID == vmID {
			e.Reclaim = deadline
			return true
		}
	}
	return false
}

// Result waits for an ephemeral VM's posted result.
func (s *Server) Result(ctx context.Context, guestIP string) ([]byte, error) {
	s.mu.Lock()
	e, ok := s.byIP[guestIP]
	s.mu.Unlock()
	if !ok {
		return nil, errors.New("no vm at " + guestIP)
	}
	select {
	case b := <-e.result:
		return b, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type ctxKey struct{}

// hostSide is the host address (.1) of the /30 a guest address is in.
func hostSide(guest net.IP) net.IP {
	g := guest.To4()
	if g == nil {
		return nil
	}
	h := make(net.IP, 4)
	copy(h, g)
	h[3] = h[3]&^3 | 1
	return h
}

// guard admits only requests that arrived on a VM tap from a known VM.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		ip := net.ParseIP(host)
		if err != nil || ip == nil || !s.subnet.Contains(ip) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		// The request must have arrived on the VM's own /30: its host side.
		if la, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
			if tcp, ok := la.(*net.TCPAddr); ok && !tcp.IP.Equal(hostSide(ip)) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}
		s.mu.Lock()
		e, ok := s.byIP[ip.String()]
		var snap Entry
		if ok {
			snap = Entry{VMID: e.VMID, Ephemeral: e.Ephemeral, Reclaim: e.Reclaim, result: e.result}
		}
		s.mu.Unlock()
		if !ok {
			http.Error(w, "unknown instance", http.StatusNotFound)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, &snap)))
	})
}

func entryOf(r *http.Request) *Entry { return r.Context().Value(ctxKey{}).(*Entry) }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) instance(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"vm_id": entryOf(r).VMID})
}

func (s *Server) reclaim(w http.ResponseWriter, r *http.Request) {
	e := entryOf(r)
	if e.Reclaim.IsZero() {
		writeJSON(w, http.StatusNotFound, map[string]any{"reclaim": false})
		return
	}
	writeJSON(w, 200, map[string]any{
		"reclaim":           true,
		"vm_id":             e.VMID,
		"deadline_at":       e.Reclaim.UTC().Format(time.RFC3339),
		"seconds_remaining": max(0, int(time.Until(e.Reclaim).Seconds())),
	})
}

func (s *Server) awsSpot(w http.ResponseWriter, r *http.Request) {
	e := entryOf(r)
	if e.Reclaim.IsZero() {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, 200, map[string]string{"action": "terminate", "time": e.Reclaim.UTC().Format(time.RFC3339)})
}

func (s *Server) ephemeralResult(w http.ResponseWriter, r *http.Request) {
	e := entryOf(r)
	if !e.Ephemeral {
		http.Error(w, "not allowed", http.StatusForbidden)
		return
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, s.maxBody+1))
	if err != nil || int64(len(b)) > s.maxBody || !json.Valid(b) {
		http.Error(w, "bad result", http.StatusBadRequest)
		return
	}
	select {
	case e.result <- b:
	default: // already have one
	}
	w.WriteHeader(http.StatusNoContent)
}
