package vm

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/runcrateai/runcrate-host/internal/sysexec"
)

// MetadataIP is the link-local metadata address every VM sees.
const MetadataIP = "169.254.169.254"

// MetadataPort is where the agent's metadata server listens on the host. It
// is below 1024 so only root can bind it; the firewall redirects
// 169.254.169.254:80 from VMs to it and drops it from anywhere else.
const MetadataPort = 980

// Network gives each VM its own routed tap device with a /30: no shared L2
// between VMs, egress-only NAT to the internet, nothing toward the host, the
// partner's networks or other VMs except the metadata server.
type Network struct {
	Subnet        *net.IPNet
	ForwardWasOff bool
	// Blocked are extra destinations VMs may never reach (config
	// blocked_cidrs: management VLANs, BMC ranges).
	Blocked []string
	Run     sysexec.Runner
	// ProcRoot is "/" in production (sysctl writes go under proc/sys).
	ProcRoot string
	// Connected lists the host's own IPv4 networks; nil reads the host.
	Connected func() []string

	mu      sync.Mutex
	applied string // sha256 of `nft list table` right after our last apply
	partner []string
}

// Slot addresses: host side .1, guest .2 of the slot's /30.
func (n *Network) Slot(slot int) (host, guest net.IP, err error) {
	ones, bits := n.Subnet.Mask.Size()
	if slot < 0 || slot >= 1<<(bits-ones)/4 {
		return nil, nil, fmt.Errorf("slot %d outside %s", slot, n.Subnet)
	}
	base := binary.BigEndian.Uint32(n.Subnet.IP.To4())
	h := make(net.IP, 4)
	g := make(net.IP, 4)
	binary.BigEndian.PutUint32(h, base+uint32(slot)*4+1)
	binary.BigEndian.PutUint32(g, base+uint32(slot)*4+2)
	return h, g, nil
}

// MaxSlots is how many VMs the subnet holds.
func (n *Network) MaxSlots() int {
	ones, bits := n.Subnet.Mask.Size()
	return 1 << (bits - ones) / 4
}

// bogons are destinations VMs may never reach: private, shared, loopback,
// link-local, documentation, benchmark, multicast and reserved ranges. The
// metadata server is reached by redirect before routing, so it isn't
// affected.
var bogons = []string{
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
	"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15",
	"198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
}

// Ruleset is the nftables table for the given partner networks (the host's
// own connected networks, its gateway and configured extras). It only ever
// matches our rc-* interfaces, except for the optional forwarding guard.
func (n *Network) Ruleset(partner []string) string {
	var b strings.Builder
	b.WriteString("table inet runcrate_host\ndelete table inet runcrate_host\n")
	b.WriteString("table inet runcrate_host {\n")
	fmt.Fprintf(&b, "\tset bogons4 {\n\t\ttype ipv4_addr\n\t\tflags interval\n\t\tauto-merge\n\t\telements = { %s }\n\t}\n", strings.Join(bogons, ", "))
	b.WriteString("\tset partner4 {\n\t\ttype ipv4_addr\n\t\tflags interval\n\t\tauto-merge\n")
	if len(partner) > 0 {
		fmt.Fprintf(&b, "\t\telements = { %s }\n", strings.Join(partner, ", "))
	}
	b.WriteString("\t}\n")
	fmt.Fprintf(&b, `	chain antispoof {
		type filter hook prerouting priority raw; policy accept;
		iifname "rc-*" meta nfproto ipv6 drop
		iifname "rc-*" fib saddr . iif oif missing drop
		iifname "rc-*" ip saddr != %[3]s drop
	}
	chain prerouting {
		type nat hook prerouting priority dstnat; policy accept;
		iifname "rc-*" ip daddr %[1]s tcp dport 80 redirect to :%[2]d
	}
	chain input {
		type filter hook input priority filter - 10; policy accept;
		iifname "rc-*" tcp dport %[2]d ct original ip daddr %[1]s accept
		iifname "rc-*" ct state established,related accept
		iifname "rc-*" drop
		tcp dport %[2]d drop
	}
	chain forward {
		type filter hook forward priority filter - 10; policy accept;
		iifname "rc-*" oifname "rc-*" drop
		iifname "rc-*" meta nfproto ipv6 drop
		oifname "rc-*" meta nfproto ipv6 drop
		iifname "rc-*" ip daddr @bogons4 drop
		iifname "rc-*" ip daddr @partner4 drop
		iifname "rc-*" accept
		oifname "rc-*" ct state established,related accept
		oifname "rc-*" drop
`, MetadataIP, MetadataPort, n.Subnet)
	if n.ForwardWasOff {
		// Forwarding was off before we turned it on for our VMs: keep the
		// host from routing anything else.
		b.WriteString("\t\tdrop\n")
	}
	fmt.Fprintf(&b, `	}
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		ip saddr %s oifname != "rc-*" masquerade
	}
}
`, n.Subnet)
	return b.String()
}

func (n *Network) sysctl(key, val string) error {
	p := filepath.Join(n.ProcRoot, "proc/sys", strings.ReplaceAll(key, ".", "/"))
	return os.WriteFile(p, []byte(val), 0o644)
}

// partnerNets is every IPv4 network the host is on (other than our taps),
// its default gateways, and the configured extras, deduplicated.
func (n *Network) partnerNets() []string {
	var nets []string
	if n.Connected != nil {
		nets = n.Connected()
	} else {
		nets = hostNetworks(n.ProcRoot)
	}
	nets = append(nets, n.Blocked...)
	seen := map[string]bool{}
	var out []string
	for _, c := range nets {
		_, ipn, err := net.ParseCIDR(c)
		if err != nil || ipn.IP.To4() == nil || ipn.String() == "0.0.0.0/0" || n.Subnet.Contains(ipn.IP) {
			continue
		}
		if !seen[ipn.String()] {
			seen[ipn.String()] = true
			out = append(out, ipn.String())
		}
	}
	sort.Strings(out)
	return out
}

// hostNetworks reads the host's IPv4 networks and default gateways.
func hostNetworks(procRoot string) []string {
	var out []string
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if strings.HasPrefix(i.Name, "rc-") || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				out = append(out, (&net.IPNet{IP: ipn.IP.Mask(ipn.Mask), Mask: ipn.Mask}).String())
			}
		}
	}
	// Default gateways from /proc/net/route (hex, little endian).
	if f, err := os.Open(filepath.Join(procRoot, "proc/net/route")); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fs := strings.Fields(sc.Text())
			if len(fs) < 3 || fs[1] != "00000000" || strings.HasPrefix(fs[0], "rc-") {
				continue
			}
			v, err := strconv.ParseUint(fs[2], 16, 32)
			if err != nil || v == 0 {
				continue
			}
			ip := make(net.IP, 4)
			binary.LittleEndian.PutUint32(ip, uint32(v))
			out = append(out, ip.String()+"/32")
		}
	}
	return out
}

// Setup installs the firewall, then turns on forwarding. Idempotent.
func (n *Network) Setup(ctx context.Context) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.apply(ctx); err != nil {
		if n.ForwardWasOff {
			_ = n.sysctl("net.ipv4.ip_forward", "0")
		}
		return err
	}
	if err := n.sysctl("net.ipv4.ip_forward", "1"); err != nil {
		return fmt.Errorf("enable forwarding: %w", err)
	}
	return nil
}

func (n *Network) apply(ctx context.Context) error {
	partner := n.partnerNets()
	if _, err := n.Run.RunInput(ctx, []byte(n.Ruleset(partner)), "nft", "-f", "-"); err != nil {
		return fmt.Errorf("firewall: %w", err)
	}
	out, err := n.Run.Run(ctx, "nft", "list", "table", "inet", "runcrate_host")
	if err != nil {
		return fmt.Errorf("firewall: %w", err)
	}
	n.applied, n.partner = digest(out), partner
	n.allowThroughIptables(ctx)
	return nil
}

func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// Reconcile re-applies the firewall when someone flushed or changed it (a
// partner's `systemctl restart nftables` runs `flush ruleset`) or when the
// host's networks changed. If it can't, every VM is cut off the network
// until it can. Call it every few seconds.
func (n *Network) Reconcile(ctx context.Context) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	out, err := n.Run.Run(ctx, "nft", "list", "table", "inet", "runcrate_host")
	partner := n.partnerNets()
	if err == nil && digest(out) == n.applied && strings.Join(partner, ",") == strings.Join(n.partner, ",") {
		n.allowThroughIptables(ctx)
		return nil
	}
	if err := n.apply(ctx); err != nil {
		n.cutVMs(ctx)
		return fmt.Errorf("firewall missing and can't be restored, VMs cut off: %w", err)
	}
	n.restoreVMs(ctx)
	return nil
}

func (n *Network) taps() []string {
	var out []string
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if strings.HasPrefix(i.Name, "rc-") {
			out = append(out, i.Name)
		}
	}
	return out
}

func (n *Network) cutVMs(ctx context.Context) {
	for _, t := range n.taps() {
		_, _ = n.Run.Run(ctx, "ip", "link", "set", "dev", t, "down")
	}
}

func (n *Network) restoreVMs(ctx context.Context) {
	for _, t := range n.taps() {
		_, _ = n.Run.Run(ctx, "ip", "link", "set", "dev", t, "up")
	}
}

// allowThroughIptables: Docker and ufw set iptables FORWARD (and ufw INPUT)
// policies to DROP, which would drop VM egress and metadata requests in
// their chains even though ours accept them. Insert narrow accepts for rc-*
// (our nft drops still apply first). Re-checked on every reconcile, since
// `ufw reload` and Docker restarts rewrite those chains.
func (n *Network) allowThroughIptables(ctx context.Context) {
	if _, err := n.Run.Run(ctx, "iptables", "-S", "FORWARD"); err != nil {
		return // no iptables on this host
	}
	for chain, rules := range iptablesRules {
		// Inserted whatever the policy is now: Docker or ufw may switch it
		// to DROP later.
		for _, rule := range rules {
			if _, err := n.Run.Run(ctx, "iptables", append([]string{"-C", chain}, rule...)...); err == nil {
				continue
			}
			_, _ = n.Run.Run(ctx, "iptables", append([]string{"-I", chain, "1"}, rule...)...)
		}
	}
}

var iptablesRules = map[string][][]string{
	"FORWARD": {
		{"-i", "rc-+", "-j", "ACCEPT"},
		{"-o", "rc-+", "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
	},
	"INPUT": {
		{"-i", "rc-+", "-p", "tcp", "--dport", strconv.Itoa(MetadataPort), "-j", "ACCEPT"},
	},
}

// AddTap creates a VM's tap device owned by user (so an unprivileged QEMU
// can open it), replacing any older one.
func (n *Network) AddTap(ctx context.Context, name string, slot int, user string) error {
	host, _, err := n.Slot(slot)
	if err != nil {
		return err
	}
	n.DelTap(ctx, name)
	args := []string{"tuntap", "add", "dev", name, "mode", "tap"}
	if user != "" {
		args = append(args, "user", user)
	}
	if _, err := n.Run.Run(ctx, "ip", args...); err != nil {
		return err
	}
	_ = n.sysctl("net.ipv6.conf."+name+".disable_ipv6", "1")
	_ = n.sysctl("net.ipv4.conf."+name+".rp_filter", "1")
	_ = n.sysctl("net.ipv4.conf."+name+".accept_local", "0")
	if _, err := n.Run.Run(ctx, "ip", "addr", "replace", host.String()+"/30", "dev", name); err != nil {
		return err
	}
	_, err = n.Run.Run(ctx, "ip", "link", "set", "dev", name, "up")
	return err
}

// DelTap removes a VM's tap device (missing is fine).
func (n *Network) DelTap(ctx context.Context, name string) {
	_, _ = n.Run.Run(ctx, "ip", "link", "del", "dev", name)
}

// CutTaps sets every VM tap down. It runs whenever the agent stops (the
// unit's ExecStopPost), so while nothing re-applies the firewall the VMs
// have no network at all; the agent brings the taps back up on start.
func (n *Network) CutTaps(ctx context.Context) error {
	var errs []error
	for _, t := range n.taps() {
		if _, err := n.Run.Run(ctx, "ip", "link", "set", "dev", t, "down"); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t, err))
		}
	}
	return errors.Join(errs...)
}

// UpTap sets a VM's tap up again after the agent restarted.
func (n *Network) UpTap(ctx context.Context, name string) error {
	_, err := n.Run.Run(ctx, "ip", "link", "set", "dev", name, "up")
	return err
}

// Teardown removes everything Setup and AddTap made. Used by uninstall.
func (n *Network) Teardown(ctx context.Context) {
	for _, t := range n.taps() {
		n.DelTap(ctx, t)
	}
	if n.ForwardWasOff {
		_ = n.sysctl("net.ipv4.ip_forward", "0")
	}
	_, _ = n.Run.Run(ctx, "nft", "delete", "table", "inet", "runcrate_host")
	for chain, rules := range iptablesRules {
		for _, rule := range rules {
			for {
				if _, err := n.Run.Run(ctx, "iptables", append([]string{"-D", chain}, rule...)...); err != nil {
					break
				}
			}
		}
	}
}
