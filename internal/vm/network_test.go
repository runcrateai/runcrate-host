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
