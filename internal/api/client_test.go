package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRegisterSendsNoCredentialAndReturnsOne(t *testing.T) {
	var got RegisterRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/register" || r.Method != http.MethodPost {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("register must not carry a host credential")
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(RegisterResponse{HostID: "h1", HostCredential: "secret", PollIntervalS: 10})
	}))
	defer srv.Close()
	c, err := New(srv.URL, "", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Register(context.Background(), RegisterRequest{RegistrationToken: "tok", RequestID: RequestID("x"),
		Host: RegisterHost{Name: "n", PublicIPMode: "none", GPUs: []RegisterGPU{{Index: 0, ModelRaw: "AD102 [GeForce RTX 4090]", PCIeAddress: "0000:41:00.0"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.HostID != "h1" || resp.HostCredential != "secret" {
		t.Fatalf("resp = %+v", resp)
	}
	if got.RegistrationToken != "tok" || got.Host.GPUs[0].ModelRaw != "AD102 [GeForce RTX 4090]" || got.Host.PublicIPMode != "none" {
		t.Fatalf("sent = %+v", got)
	}
}

func TestRegisterWithoutCredentialIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"host_id":"h1"}`))
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "", "1")
	if _, err := c.Register(context.Background(), RegisterRequest{}); err == nil {
		t.Fatal("want error for an answer without host_credential")
	}
}

func TestUnauthorizedAndRetryable(t *testing.T) {
	code := 401
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "cred", "1")
	if _, err := c.Heartbeat(context.Background(), HeartbeatRequest{}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("401: %v", err)
	}
	code = 503
	_, err := c.Heartbeat(context.Background(), HeartbeatRequest{})
	var he *HTTPError
	if !errors.As(err, &he) || !he.Retryable() {
		t.Fatalf("503: %v", err)
	}
	code = 404
	err = c.Result(context.Background(), 5, CommandResult{})
	if !errors.As(err, &he) || he.Retryable() {
		t.Fatalf("404: %v", err)
	}
}

func TestPlainHTTPRefusedExceptLoopback(t *testing.T) {
	if _, err := New("http://www.runcrate.ai/api/partner-hosts/v1", "", "1"); err == nil {
		t.Fatal("plain http to a remote host must be refused")
	}
	if _, err := New("http://127.0.0.1:9/x", "", "1"); err != nil {
		t.Fatal(err)
	}
}

func TestImageNeedsAllFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("version") != "v2" {
			t.Errorf("version = %q", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"url":"https://x/img","version":"v2"}`))
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "cred", "1")
	if _, err := c.Image(context.Background(), "v2"); err == nil {
		t.Fatal("an image without sha256 must be refused")
	}
}

func TestRequestIDIsStableUUID(t *testing.T) {
	a, b := RequestID("command:7"), RequestID("command:7")
	if a != b || RequestID("command:8") == a {
		t.Fatal("request ids must be stable per name and differ across names")
	}
	if len(a) != 36 || a[14] != '5' || (a[19] != '8' && a[19] != '9' && a[19] != 'a' && a[19] != 'b') {
		t.Fatalf("not a v5 uuid: %s", a)
	}
}
