package vm

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/runcrateai/runcrate-host/internal/sysexec"
)

// MetadataIP is the link-local metadata address every VM sees.
const MetadataIP = "169.254.169.254"

// MetadataPort is where the agent's metadata server listens on the host. The
// firewall redirects 169.254.169.254:80 from VMs to it and drops it from
// anywhere else.
const MetadataPort = 16925

// Network gives each VM its own routed tap device with a /30: no shared L2
// between VMs, egress-only NAT to the internet, nothing toward the host or
// the partner's private networks except the metadata server.
type Network struct {
	Subnet        *net.IPNet
	ForwardWasOff bool
	Run           sysexec.Runner
	// ProcRoot is "/" in production (sysctl writes go under proc/sys).
	ProcRoot string
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

// blocked are destinations VMs may never reach: private, shared, loopback,
// link-local, benchmark, multicast and reserved ranges. The metadata server
// is reached by redirect before routing, so it isn't affected.
var blocked = []string{
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
	"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15",
	"198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
}

// Ruleset is the nftables table. It only ever matches our rc-* interfaces,
// except for the optional forwarding guard (see ForwardWasOff).
func (n *Network) Ruleset() string {
	var b strings.Builder
	b.WriteString("table inet runcrate_host\ndelete table inet runcrate_host\n")
	b.WriteString("table inet runcrate_host {\n")
	fmt.Fprintf(&b, "\tset blocked4 {\n\t\ttype ipv4_addr\n\t\tflags interval\n\t\telements = { %s }\n\t}\n", strings.Join(blocked, ", "))
	fmt.Fprintf(&b, `	chain prerouting {
		type nat hook prerouting priority dstnat; policy accept;
		iifname "rc-*" ip daddr %s tcp dport 80 redirect to :%d
	}
	chain input {
		type filter hook input priority filter - 10; policy accept;
		iifname "rc-*" ip saddr != %s drop
		iifname "rc-*" tcp dport %d ct original ip daddr %s accept
		iifname "rc-*" ct state established,related accept
		iifname "rc-*" drop
		tcp dport %d drop
	}
	chain forward {
		type filter hook forward priority filter - 10; policy accept;
		iifname "rc-*" oifname "rc-*" drop
		iifname "rc-*" meta nfproto ipv6 drop
		oifname "rc-*" meta nfproto ipv6 drop
		iifname "rc-*" ip saddr != %s drop
		iifname "rc-*" ip daddr @blocked4 drop
		iifname "rc-*" accept
		oifname "rc-*" ct state established,related accept
		oifname "rc-*" drop
`, MetadataIP, MetadataPort, n.Subnet, MetadataPort, MetadataIP, MetadataPort, n.Subnet)
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

// Setup turns on forwarding and installs the firewall. Idempotent.
func (n *Network) Setup(ctx context.Context) error {
	if err := n.sysctl("net.ipv4.ip_forward", "1"); err != nil {
		return fmt.Errorf("enable forwarding: %w", err)
	}
	if _, err := n.Run.RunInput(ctx, []byte(n.Ruleset()), "nft", "-f", "-"); err != nil {
		return fmt.Errorf("firewall: %w", err)
	}
	n.allowThroughIptables(ctx)
	return nil
}

// allowThroughIptables: Docker and ufw set the iptables FORWARD policy to
// DROP, which would drop VM egress in their chain even though ours accepts
// it. Insert narrow accepts for rc-* (our nft drops still apply first).
func (n *Network) allowThroughIptables(ctx context.Context) {
	out, err := n.Run.Run(ctx, "iptables", "-S", "FORWARD")
	if err != nil || !strings.Contains(string(out), "-P FORWARD DROP") {
		return
	}
	for _, rule := range iptablesRules {
		if _, err := n.Run.Run(ctx, "iptables", append([]string{"-C", "FORWARD"}, rule...)...); err == nil {
			continue
		}
		_, _ = n.Run.Run(ctx, "iptables", append([]string{"-I", "FORWARD", "1"}, rule...)...)
	}
}

var iptablesRules = [][]string{
	{"-i", "rc-+", "-j", "ACCEPT"},
	{"-o", "rc-+", "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
}

// AddTap creates (or re-addresses) a VM's tap device.
func (n *Network) AddTap(ctx context.Context, name string, slot int) error {
	host, _, err := n.Slot(slot)
	if err != nil {
		return err
	}
	if _, err := n.Run.Run(ctx, "ip", "link", "show", "dev", name); err != nil {
		if _, err := n.Run.Run(ctx, "ip", "tuntap", "add", "dev", name, "mode", "tap"); err != nil {
			return err
		}
	}
	// Strict reverse-path filtering pins each VM to its own /30, so one VM
	// can't pose as another to the metadata server.
	_ = n.sysctl("net.ipv6.conf."+name+".disable_ipv6", "1")
	if err := n.sysctl("net.ipv4.conf."+name+".rp_filter", "1"); err != nil {
		return fmt.Errorf("rp_filter on %s: %w", name, err)
	}
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

// Teardown removes everything Setup and AddTap made. Used by uninstall.
func (n *Network) Teardown(ctx context.Context) {
	_, _ = n.Run.Run(ctx, "nft", "delete", "table", "inet", "runcrate_host")
	for _, rule := range iptablesRules {
		for {
			if _, err := n.Run.Run(ctx, "iptables", append([]string{"-D", "FORWARD"}, rule...)...); err != nil {
				break
			}
		}
	}
	if ifs, err := net.Interfaces(); err == nil {
		for _, i := range ifs {
			if strings.HasPrefix(i.Name, "rc-") {
				n.DelTap(ctx, i.Name)
			}
		}
	}
	if n.ForwardWasOff {
		_ = n.sysctl("net.ipv4.ip_forward", "0")
	}
}
