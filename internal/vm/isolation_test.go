package vm

import (
	"fmt"
	"strings"
	"testing"

	"github.com/runcrateai/runcrate-host/internal/api"
)

func TestTunnelGatewaysDropWhatIsNotWellFormed(t *testing.T) {
	in := []api.Gateway{
		{ID: "iad", IPv4: "203.0.113.7", Port: 443},
		{ID: "IAD", IPv4: "203.0.113.8", Port: 443},                   // upper case id
		{ID: "x$(reboot)", IPv4: "203.0.113.8", Port: 443},            // shell in the id
		{ID: strings.Repeat("a", 33), IPv4: "203.0.113.8", Port: 443}, // id too long
		{ID: "", IPv4: "203.0.113.8", Port: 443},                      // no id
		{ID: "v6", IPv4: "2001:db8::1", Port: 443},                    // not IPv4
		{ID: "host", IPv4: "gw.runcrate.ai", Port: 443},               // a name, not an address
		{ID: "inj", IPv4: "203.0.113.8;reboot", Port: 443},            // shell in the address
		{ID: "p0", IPv4: "203.0.113.8", Port: 0},                      // port out of range
		{ID: "pbig", IPv4: "203.0.113.8", Port: 65536},                // port out of range
		{ID: "iad", IPv4: "203.0.113.9", Port: 22},                    // repeated id
		{ID: "mapped", IPv4: "::ffff:198.51.100.4", Port: 2222},       // IPv4 in IPv6 form
	}
	for i := 1; i <= 8; i++ {
		in = append(in, api.Gateway{ID: fmt.Sprintf("g%d", i), IPv4: fmt.Sprintf("198.51.100.%d", i), Port: 443})
	}
	got := TunnelGatewaysFrom(in)
	if len(got) != maxTunnelGateways {
		t.Fatalf("kept %d gateways, want %d: %+v", len(got), maxTunnelGateways, got)
	}
	if got[0] != (TunnelGateway{ID: "iad", IP: "203.0.113.7", Port: 443}) {
		t.Fatalf("first = %+v", got[0])
	}
	// The mapped address is re-formatted from the parsed value.
	if got[1] != (TunnelGateway{ID: "mapped", IP: "198.51.100.4", Port: 2222}) {
		t.Fatalf("second = %+v", got[1])
	}
	// Two valid ones came first, so g7 and g8 (the 9th and 10th) are dropped.
	if got[7].ID != "g6" {
		t.Fatalf("last kept = %+v, want g6", got[7])
	}

	script := isolationScript(IsolationTargets{TunnelGateways: got})
	for _, want := range []string{
		`ssh_banner() { timeout 8 bash -c "exec 3<>/dev/tcp/$1/$2; head -c 4 <&3" 2>/dev/null | grep -q '^SSH-'; }`,
		"must gateway_iad \"203.0.113.7:443 answers SSH\" ssh_banner 203.0.113.7 443\n",
		"must gateway_mapped \"198.51.100.4:2222 answers SSH\" ssh_banner 198.51.100.4 2222\n",
		"must gateway_g6 \"198.51.100.6:443 answers SSH\" ssh_banner 198.51.100.6 443\n",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script lacks %q:\n%s", want, script)
		}
	}
	for _, bad := range []string{"reboot", "203.0.113.8", "203.0.113.9", "2001:db8", "gw.runcrate.ai", "::ffff", "gateway_g7", "gateway_g8", "gateway_IAD"} {
		if strings.Contains(script, bad) {
			t.Fatalf("script carries %q", bad)
		}
	}
	if n := strings.Count(script, "must gateway_"); n != maxTunnelGateways {
		t.Fatalf("%d gateway checks, want %d", n, maxTunnelGateways)
	}
}

// isolationScript itself rechecks what it is given, whoever built it.
func TestIsolationScriptRechecksTunnelGateways(t *testing.T) {
	var tg []TunnelGateway
	tg = append(tg, TunnelGateway{ID: "ok", IP: "203.0.113.7", Port: 22},
		TunnelGateway{ID: "a'b", IP: "203.0.113.8", Port: 22},
		TunnelGateway{ID: "c", IP: "203.0.113.9 `id`", Port: 22},
		TunnelGateway{ID: "d", IP: "203.0.113.10", Port: -1})
	for i := range 10 {
		tg = append(tg, TunnelGateway{ID: fmt.Sprintf("n%d", i), IP: "192.0.2.1", Port: 22})
	}
	script := isolationScript(IsolationTargets{TunnelGateways: tg})
	if !strings.Contains(script, "must gateway_ok \"203.0.113.7:22 answers SSH\" ssh_banner 203.0.113.7 22\n") {
		t.Fatalf("script lacks the valid gateway:\n%s", script)
	}
	for _, bad := range []string{"a'b", "`id`", "203.0.113.8", "203.0.113.10", "gateway_n7"} {
		if strings.Contains(script, bad) {
			t.Fatalf("script carries %q", bad)
		}
	}
	if n := strings.Count(script, "must gateway_"); n != maxTunnelGateways {
		t.Fatalf("%d gateway checks, want %d", n, maxTunnelGateways)
	}
	// No gateways, no gateway checks.
	if strings.Contains(isolationScript(IsolationTargets{}), "must gateway_") {
		t.Fatal("gateway check without gateways")
	}
}
