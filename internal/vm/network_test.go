package vm

import "testing"

// Lines as nft 1.0.9's `nft monitor` printed them on Ubuntu 24.04 during a
// `nft flush ruleset` and a blocklist update.
func TestFirewallEvent(t *testing.T) {
	for line, want := range map[string]bool{
		"delete table inet runcrate_host":                              true,
		"delete rule inet runcrate_host forward handle 28":             true,
		"delete chain ip filter DOCKER-USER":                           true,
		"add rule inet t1 c ip saddr @s drop":                          true,
		"delete element inet runcrate_host partner4 { 10.0.0.0/8 }":    true,
		"add element inet f2b-table addr-set-sshd { 1.2.3.4 }":         false,
		"delete element inet crowdsec crowdsec-blacklists { 5.6.7.8 }": false,
		"# new generation 55 by process 12309 (nft)":                   false,
		"": false,
	} {
		if got := firewallEvent(line); got != want {
			t.Errorf("firewallEvent(%q) = %v, want %v", line, got, want)
		}
	}
}

// From `nft -j list chains` on a host with Docker (iptables-nft), a
// default-drop nftables firewall and ours.
const chainsListing = `{"nftables": [{"metainfo": {"version": "1.0.9", "json_schema_version": 1}},
{"chain": {"family": "ip", "table": "filter", "name": "INPUT", "handle": 4, "type": "filter", "hook": "input", "prio": 0, "policy": "drop"}},
{"chain": {"family": "ip", "table": "filter", "name": "DOCKER", "handle": 6}},
{"chain": {"family": "inet", "table": "runcrate", "name": "input", "handle": 2, "type": "filter", "hook": "input", "prio": 0, "policy": "drop"}},
{"chain": {"family": "inet", "table": "runcrate", "name": "ports", "handle": 1}},
{"chain": {"family": "inet", "table": "runcrate", "name": "forward", "handle": 3, "type": "filter", "hook": "forward", "prio": 0, "policy": "accept"}},
{"chain": {"family": "inet", "table": "fw", "name": "fwd", "handle": 1, "type": "filter", "hook": "forward", "prio": 0, "policy": "drop"}},
{"chain": {"family": "inet", "table": "fw", "name": "out", "handle": 2, "type": "filter", "hook": "output", "prio": 0, "policy": "drop"}},
{"chain": {"family": "ip6", "table": "fw6", "name": "in", "handle": 1, "type": "filter", "hook": "input", "prio": 0, "policy": "drop"}},
{"chain": {"family": "inet", "table": "runcrate_host", "name": "input", "handle": 5, "type": "filter", "hook": "input", "prio": -10, "policy": "drop"}}]}`

// Another table's default-drop input or forward chain drops our VMs'
// metadata requests and egress whatever our table accepts: those, and only
// those, get our accepts (iptables' own tables are allowThroughIptables').
func TestDropChains(t *testing.T) {
	got := dropChains([]byte(chainsListing))
	want := [][3]string{{"inet", "runcrate", "input"}, {"inet", "fw", "fwd"}}
	if len(got) != len(want) {
		t.Fatalf("dropChains = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dropChains = %v, want %v", got, want)
		}
	}
}
