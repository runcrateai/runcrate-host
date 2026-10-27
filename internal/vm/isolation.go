package vm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/runcrateai/runcrate-host/internal/api"
)

// The benchmark's isolation checks prove the firewall on this host, not in
// theory: the benchmark VM tries to reach what a customer VM must never reach
// (the host, its LAN, private ranges) and what it must (the internet, DNS,
// the metadata server), and the host tries to open a connection to the VM
// the way a machine on its LAN would.

// IsolationTargets are this host's addresses the benchmark VM must not reach.
type IsolationTargets struct {
	HostIPs      []string // the host's own IPv4 addresses
	Gateways     []string // its default gateways (the partner's LAN)
	DockerBridge string   // docker0's address, when Docker is installed
}

// IsolationTargets reads the host's addresses, gateways and Docker bridge.
func (n *Network) IsolationTargets() IsolationTargets {
	var t IsolationTargets
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if strings.HasPrefix(i.Name, "rc-") || strings.HasPrefix(i.Name, lanProbeVeth) || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil || n.Subnet.Contains(ipn.IP) {
				continue
			}
			if i.Name == "docker0" {
				t.DockerBridge = ipn.IP.String()
			} else {
				t.HostIPs = append(t.HostIPs, ipn.IP.String())
			}
		}
	}
	for _, c := range hostNetworks(n.ProcRoot) {
		if ip, ok := strings.CutSuffix(c, "/32"); ok && net.ParseIP(ip) != nil && !n.Subnet.Contains(net.ParseIP(ip)) {
			t.Gateways = append(t.Gateways, ip)
		}
	}
	t.HostIPs, t.Gateways = dedupe(t.HostIPs, 6), dedupe(t.Gateways, 3)
	return t
}

func dedupe(in []string, max int) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	if len(out) > max {
		out = out[:max]
	}
	return out
}

// isolationScript is the benchmark VM's part: one "name<TAB>ok<TAB>detail"
// line per check in $out/iso.txt. Every address in it comes from net.IP
// formatting, never from free text.
func isolationScript(t IsolationTargets) string {
	var b strings.Builder
	b.WriteString(`iso="$out/iso.txt"; : > "$iso"
# Exit 0 = connected; anything else (refused, dropped, timed out) = closed.
tcp() { timeout 4 bash -c "exec 3<>/dev/tcp/$1/$2" 2>/dev/null; }
# must NAME DETAIL CMD...: passes when CMD succeeds.
must() { n=$1; d=$2; shift 2; if "$@" >/dev/null 2>&1; then r=true; s=works; else r=false; s=failed; fi; printf '%s\t%s\t%s: %s\n' "$n" "$r" "$d" "$s" >> "$iso"; }
# closed NAME DETAIL HOST:PORT...: passes when none of them connects.
closed() { n=$1; d=$2; shift 2; o=""; for hp in "$@"; do tcp "${hp%:*}" "${hp##*:}" && o="$o $hp"; done; if [ -z "$o" ]; then printf '%s\ttrue\t%s: all closed\n' "$n" "$d" >> "$iso"; else printf '%s\tfalse\t%s: connected to%s\n' "$n" "$d" "$o" >> "$iso"; fi; }
gw=$(ip route | awk '/^default/ {print $3; exit}')
must egress "https://www.runcrate.ai" curl -sS -m 8 -o /dev/null https://www.runcrate.ai
must dns "resolve www.runcrate.ai" getent hosts www.runcrate.ai
must metadata "169.254.169.254" curl -sS -m 4 -o /dev/null http://169.254.169.254/runcrate/v1/reclaim-notice
`)
	fmt.Fprintf(&b, "closed host_tap_gateway \"host side of the VM link $gw\" \"$gw:22\" \"$gw:%d\"\n", MetadataPort)
	closed := func(name, detail string, ips []string, ports ...int) {
		var hp []string
		for _, ip := range ips {
			if p := net.ParseIP(ip); p == nil || p.To4() == nil {
				continue
			}
			for _, port := range ports {
				hp = append(hp, net.JoinHostPort(ip, strconv.Itoa(port)))
			}
		}
		if len(hp) > 0 {
			fmt.Fprintf(&b, "closed %s %q %s\n", name, detail, strings.Join(hp, " "))
		}
	}
	closed("host_ip", "the host's own addresses", t.HostIPs, 22)
	closed("host_lan_gateway", "the host's default gateway", t.Gateways, 22, 80, 443)
	if t.DockerBridge != "" {
		closed("docker_bridge", "the Docker bridge", []string{t.DockerBridge}, 22)
	}
	closed("private_ranges", "10/8, 172.16/12, 192.168/16", []string{"10.0.0.1", "172.16.0.1", "192.168.0.1"}, 22, 80)
	closed("cgnat", "100.64/10", []string{"100.64.0.1"}, 80)
	closed("link_local", "169.254/16 other than metadata", []string{"169.254.1.1"}, 80)
	return b.String()
}

// The LAN probe's throwaway namespace and veth pair. The names never match
// rc-*, so none of the VM rules apply to them: to the firewall it is just
// another machine on the partner's LAN. 192.0.2.0/30 is TEST-NET-1, never
// routed anywhere.
const (
	lanProbeNS    = "rhlanprobe"
	lanProbeVeth  = "rhlp"
	lanProbeHost  = "192.0.2.1"
	lanProbePeer  = "192.0.2.2"
	lanProbeShort = "/30"
)

// LANToVM checks that a machine on the host's LAN can't open a connection
// to the VM at guestIP: a throwaway namespace routed through the host, like
// a LAN neighbour using it as a router, tries guestIP:22.
func (n *Network) LANToVM(ctx context.Context, guestIP string) api.IsolationCheck {
	c := api.IsolationCheck{Name: "lan_to_vm"}
	if ip := net.ParseIP(guestIP); ip == nil || ip.To4() == nil {
		c.Detail = "no VM address"
		return c
	}
	ns, host, peer := lanProbeNS, lanProbeVeth+"0", lanProbeVeth+"1"
	_, _ = n.Run.Run(ctx, "ip", "netns", "del", ns) // a leftover from a crash
	defer func() { _, _ = n.Run.Run(context.WithoutCancel(ctx), "ip", "netns", "del", ns) }()
	for _, args := range [][]string{
		{"netns", "add", ns},
		{"link", "add", host, "type", "veth", "peer", "name", peer, "netns", ns},
		{"addr", "add", lanProbeHost + lanProbeShort, "dev", host},
		{"link", "set", host, "up"},
		{"-n", ns, "addr", "add", lanProbePeer + lanProbeShort, "dev", peer},
		{"-n", ns, "link", "set", peer, "up"},
		{"-n", ns, "link", "set", "lo", "up"},
		{"-n", ns, "route", "add", "default", "via", lanProbeHost},
	} {
		if _, err := n.Run.Run(ctx, "ip", args...); err != nil {
			c.Detail = clipDetail("inconclusive: could not set up the probe: " + err.Error())
			return c
		}
	}
	// timeout exits 124 when the SYN got no answer (dropped); bash exits 1
	// when it was refused, which means it reached the VM.
	_, err := n.Run.Run(ctx, "ip", "netns", "exec", ns, "timeout", "5", "bash", "-c", fmt.Sprintf("exec 3<>/dev/tcp/%s/22", guestIP))
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		c.Detail = clipDetail("inconclusive: " + err.Error())
		return c
	}
	// The probe reached the host: the namespace resolved its gateway.
	neigh, _ := n.Run.Run(ctx, "ip", "-n", ns, "neigh", "show", lanProbeHost)
	wired := strings.Contains(string(neigh), "lladdr")
	switch {
	case code == 0:
		c.Detail = fmt.Sprintf("a LAN machine connected to the VM (%s:22)", guestIP)
	case code != 124:
		c.Detail = fmt.Sprintf("a LAN machine reached the VM (%s:22 refused, exit %d)", guestIP, code)
	case !wired:
		c.Detail = "inconclusive: the probe never reached the host"
	default:
		c.OK = true
		c.Detail = fmt.Sprintf("a LAN machine routed through the host got no answer from %s:22", guestIP)
	}
	return c
}

func clipDetail(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
