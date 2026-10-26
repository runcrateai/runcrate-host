package metadata

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func req(t *testing.T, s *Server, method, path, from, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = from + ":40000"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func newServer() *Server {
	_, n, _ := net.ParseCIDR("10.254.0.0/16")
	return New(n)
}

func TestOnlyKnownVMsInsideTheSubnet(t *testing.T) {
	s := newServer()
	s.Set("10.254.0.2", &Entry{VMID: "vm-a"})
	if w := req(t, s, "GET", "/runcrate/v1/instance", "192.168.1.5", ""); w.Code != http.StatusForbidden {
		t.Fatalf("outside subnet: %d", w.Code)
	}
	if w := req(t, s, "GET", "/runcrate/v1/instance", "10.254.0.6", ""); w.Code != http.StatusNotFound {
		t.Fatalf("unknown vm: %d", w.Code)
	}
	w := req(t, s, "GET", "/runcrate/v1/instance", "10.254.0.2", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"vm-a"`) {
		t.Fatalf("known vm: %d %s", w.Code, w.Body)
	}
}

func TestReclaimNoticeIsPerVM(t *testing.T) {
	s := newServer()
	s.Set("10.254.0.2", &Entry{VMID: "vm-a"})
	s.Set("10.254.0.6", &Entry{VMID: "vm-b"})
	if w := req(t, s, "GET", "/runcrate/v1/reclaim-notice", "10.254.0.2", ""); w.Code != 404 {
		t.Fatalf("no notice yet: %d", w.Code)
	}
	if w := req(t, s, "GET", "/latest/meta-data/spot/instance-action", "10.254.0.2", ""); w.Code != 404 {
		t.Fatalf("aws shape, no notice: %d", w.Code)
	}
	deadline := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	if !s.SetReclaim("vm-a", deadline) {
		t.Fatal("SetReclaim")
	}
	w := req(t, s, "GET", "/runcrate/v1/reclaim-notice", "10.254.0.2", "")
	var got struct {
		Reclaim  bool   `json:"reclaim"`
		Deadline string `json:"deadline_at"`
		Left     int    `json:"seconds_remaining"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != 200 || !got.Reclaim || got.Deadline != deadline.Format(time.RFC3339) || got.Left < 500 {
		t.Fatalf("notice: %d %s", w.Code, w.Body)
	}
	if w := req(t, s, "GET", "/latest/meta-data/spot/instance-action", "10.254.0.2", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"terminate"`) {
		t.Fatalf("aws shape: %d %s", w.Code, w.Body)
	}
	if w := req(t, s, "GET", "/runcrate/v1/reclaim-notice", "10.254.0.6", ""); w.Code != 404 {
		t.Fatalf("other vm must not see the notice: %d", w.Code)
	}
}

func TestEphemeralResultOnlyFromThrowawayVMs(t *testing.T) {
	s := newServer()
	s.Set("10.254.0.2", &Entry{VMID: "customer"})
	s.Set("10.254.0.6", &Entry{VMID: "bench-1", Ephemeral: true})
	if w := req(t, s, "POST", "/runcrate/v1/ephemeral-result", "10.254.0.2", `{"ok":true}`); w.Code != http.StatusForbidden {
		t.Fatalf("customer vm posted a result: %d", w.Code)
	}
	if w := req(t, s, "POST", "/runcrate/v1/ephemeral-result", "10.254.0.6", `not json`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad json: %d", w.Code)
	}
	if w := req(t, s, "POST", "/runcrate/v1/ephemeral-result", "10.254.0.6", `{"ok":true}`); w.Code != 204 {
		t.Fatalf("result: %d", w.Code)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	b, err := s.Result(ctx, "10.254.0.6")
	if err != nil || string(b) != `{"ok":true}` {
		t.Fatalf("Result = %s, %v", b, err)
	}
}
