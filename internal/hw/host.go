package hw

import (
	"bufio"

	"github.com/runcrateai/runcrate-host/internal/api"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// CPU reads the CPU model and core/thread counts from /proc/cpuinfo.
func (s Sys) CPU() (model string, cores, threads int) {
	f, err := os.Open(s.p("proc/cpuinfo"))
	if err != nil {
		return "", runtime.NumCPU(), runtime.NumCPU()
	}
	defer f.Close()
	physCores := map[string]bool{}
	var phys, core string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "processor":
			threads++
		case "model name":
			if model == "" {
				model = v
			}
		case "physical id":
			phys = v
		case "core id":
			core = v
			physCores[phys+"/"+core] = true
		}
	}
	cores = len(physCores)
	if cores == 0 {
		cores = threads
	}
	return model, cores, threads
}

// Mem returns total and available memory in bytes from /proc/meminfo.
func (s Sys) Mem() (total, avail uint64) {
	f, err := os.Open(s.p("proc/meminfo"))
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) < 2 {
			continue
		}
		n, _ := strconv.ParseUint(fs[1], 10, 64)
		switch fs[0] {
		case "MemTotal:":
			total = n * 1024
		case "MemAvailable:":
			avail = n * 1024
		}
	}
	return total, avail
}

// Load1 returns the one-minute load average.
func (s Sys) Load1() float64 {
	f := strings.Fields(readTrim(s.p("proc/loadavg")))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}

// DiskFree returns free and total bytes of the filesystem holding path.
func DiskFree(path string) (free, total uint64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0
	}
	return st.Bavail * uint64(st.Bsize), st.Blocks * uint64(st.Bsize)
}

// NIC is one physical network interface.
type NIC = api.NIC

// NICs lists physical NICs (those backed by a device) and RDMA ports.
// InfiniBand and RoCE come from /sys/class/infiniband, so neither ibstat nor
// rdma-core is needed.
func (s Sys) NICs() []NIC {
	// netdev -> rdma device and link layer.
	type rd struct {
		dev, layer string
		rate       float64
		up         bool
	}
	rdma := map[string]rd{}
	var ibOnly []NIC
	ibDevs, _ := os.ReadDir(s.p("sys/class/infiniband"))
	for _, d := range ibDevs {
		ports, _ := os.ReadDir(s.p("sys/class/infiniband", d.Name(), "ports"))
		for _, p := range ports {
			pd := s.p("sys/class/infiniband", d.Name(), "ports", p.Name())
			layer := readTrim(filepath.Join(pd, "link_layer"))
			rate := parseRate(readTrim(filepath.Join(pd, "rate")))
			up := strings.Contains(readTrim(filepath.Join(pd, "state")), "ACTIVE")
			ndevs, _ := os.ReadDir(s.p("sys/class/infiniband", d.Name(), "device", "net"))
			if len(ndevs) == 0 {
				typ := "infiniband"
				if strings.EqualFold(layer, "Ethernet") {
					typ = "roce"
				}
				ibOnly = append(ibOnly, NIC{Name: d.Name() + "/" + p.Name(), Type: typ, SpeedGbps: rate, RDMADevice: d.Name(), LinkUp: up})
				continue
			}
			for _, nd := range ndevs {
				rdma[nd.Name()] = rd{dev: d.Name(), layer: layer, rate: rate, up: up}
			}
		}
	}
	var out []NIC
	ents, _ := os.ReadDir(s.p("sys/class/net"))
	for _, e := range ents {
		base := s.p("sys/class/net", e.Name())
		if _, err := os.Stat(filepath.Join(base, "device")); err != nil {
			continue // virtual: lo, bridges, taps, docker0, our rc-*
		}
		n := NIC{Name: e.Name(), Type: "ethernet", LinkUp: readTrim(filepath.Join(base, "operstate")) == "up"}
		if mbps, err := strconv.ParseFloat(readTrim(filepath.Join(base, "speed")), 64); err == nil && mbps > 0 {
			n.SpeedGbps = mbps / 1000
		}
		if r, ok := rdma[e.Name()]; ok {
			n.RDMADevice = r.dev
			if strings.EqualFold(r.layer, "InfiniBand") {
				n.Type = "infiniband"
			} else {
				n.Type = "roce"
			}
			if n.SpeedGbps == 0 {
				n.SpeedGbps = r.rate
			}
			n.LinkUp = n.LinkUp || r.up
		} else if readTrim(filepath.Join(base, "type")) == "32" {
			n.Type = "infiniband" // ARPHRD_INFINIBAND (IPoIB)
		}
		out = append(out, n)
	}
	out = append(out, ibOnly...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// parseRate reads "400 Gb/sec (4X NDR)".
func parseRate(s string) float64 {
	f := strings.Fields(s)
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}

// PrivateAddrs lists RFC 1918 IPv4 addresses on physical interfaces,
// skipping exclude (our own VM network).
func PrivateAddrs(exclude *net.IPNet) []string {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagLoopback != 0 || ifc.Flags&net.FlagUp == 0 || strings.HasPrefix(ifc.Name, "rc-") {
			continue
		}
		if _, err := os.Stat(filepath.Join("/sys/class/net", ifc.Name, "device")); err != nil {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil || !ipn.IP.IsPrivate() {
				continue
			}
			if exclude != nil && exclude.Contains(ipn.IP) {
				continue
			}
			out = append(out, ipn.IP.String())
		}
	}
	sort.Strings(out)
	return out
}
