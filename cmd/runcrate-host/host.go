package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/runcrateai/runcrate-host/internal/api"
	"github.com/runcrateai/runcrate-host/internal/bench"
	"github.com/runcrateai/runcrate-host/internal/config"
	"github.com/runcrateai/runcrate-host/internal/devmode"
	"github.com/runcrateai/runcrate-host/internal/hw"
	"github.com/runcrateai/runcrate-host/internal/sysexec"
	"github.com/runcrateai/runcrate-host/internal/vm"
)

// gpuCacheFile holds what the host's vendor tool (nvidia-smi, hl-smi) said
// about each GPU before the installer moved it to vfio-pci.
const gpuCacheFile = "gpus-cache.json"

type gpuCache struct {
	GPUs map[string]hw.Cached `json:"gpus"`
	Topo string               `json:"topo"`
}

func loadGPUCache(cfg *config.Config) gpuCache {
	var c gpuCache
	b, err := os.ReadFile(filepath.Join(cfg.DataDir, gpuCacheFile))
	if err == nil {
		_ = json.Unmarshal(b, &c)
	}
	if c.GPUs == nil {
		c.GPUs = map[string]hw.Cached{}
	}
	return c
}

// gpuInfo is a vendor's host-side tool, run while the host driver still owns
// the GPUs.
type gpuInfo interface {
	// snapshot records in c what the tool says about each GPU it sees. It
	// reports false when the tool is missing or sees no GPU, which leaves the
	// cache as it was.
	snapshot(ctx context.Context, c *gpuCache) bool
}

// smiTool finds and runs a vendor tool. The zero value uses the real system;
// tests set the fields.
type smiTool struct {
	look func(file string) (string, error)
	run  func(ctx context.Context, bin string, args ...string) ([]byte, error)
}

func (t smiTool) path(file string) (string, error) {
	if t.look != nil {
		return t.look(file)
	}
	return exec.LookPath(file)
}

func (t smiTool) output(ctx context.Context, bin string, args ...string) ([]byte, error) {
	if t.run != nil {
		return t.run(ctx, bin, args...)
	}
	return exec.CommandContext(ctx, bin, args...).Output()
}

// nvidiaSMI reads names, VRAM, UUIDs and the NVLink matrix from nvidia-smi.
type nvidiaSMI struct{ smiTool }

func (n nvidiaSMI) snapshot(ctx context.Context, c *gpuCache) bool {
	smi, err := n.path("nvidia-smi")
	if err != nil {
		return false
	}
	out, err := n.output(ctx, smi, "--query-gpu=pci.bus_id,name,memory.total,uuid", "--format=csv,noheader,nounits")
	if err != nil {
		return false // driver present but no GPU on it (already on vfio-pci)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, ",")
		if len(f) != 4 {
			continue
		}
		addr := normalizeBusID(strings.TrimSpace(f[0]))
		mib, _ := strconv.Atoi(strings.TrimSpace(f[2]))
		c.GPUs[addr] = hw.Cached{Name: strings.TrimSpace(f[1]), VRAMGB: (mib + 512) / 1024, UUID: strings.TrimSpace(f[3])}
	}
	if t, err := n.output(ctx, smi, "topo", "-m"); err == nil {
		c.Topo = string(t)
	}
	return true
}

// habanaSMI reads names and VRAM from hl-smi, when the host has it. The query
// and its output format are not verified against a real Gaudi host (see
// GAUDI-NOTES.md): lines that don't start with a PCI address (a header, a
// note) are skipped, and any failure leaves the cache as it was, so the GPU
// keeps its pci.ids name and the labelled 128 GB fallback from hw.GPUs. hl-smi
// gives no UUID here; one learned earlier is kept. c.Topo stays empty: Gaudi
// scale-up is direct OAM-to-OAM links, not NVLink, and nothing reads it.
type habanaSMI struct{ smiTool }

func (h habanaSMI) snapshot(ctx context.Context, c *gpuCache) bool {
	smi, err := h.path("hl-smi")
	if err != nil {
		return false
	}
	out, err := h.output(ctx, smi, "--query-aip=bus_id,name,memory.total", "--format=csv,noheader,nounits")
	if err != nil {
		return false // driver gone from the GPUs (already on vfio-pci)
	}
	seen := false
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, ",")
		if len(f) != 3 {
			continue
		}
		addr := normalizeBusID(strings.TrimSpace(f[0]))
		if !hw.ValidPCIAddress(addr) {
			continue
		}
		c.GPUs[addr] = hw.Cached{Name: strings.TrimSpace(f[1]), VRAMGB: memoryGB(f[2]), UUID: c.GPUs[addr].UUID}
		seen = true
	}
	return seen
}

// memoryGB reads hl-smi's memory.total: MiB when it has no unit (nounits), GiB
// or GB when it says so. 0 when unreadable.
func memoryGB(s string) int {
	f := strings.Fields(s)
	if len(f) == 0 {
		return 0
	}
	n, err := strconv.ParseFloat(f[0], 64)
	if err != nil || n <= 0 {
		return 0
	}
	if len(f) > 1 && (f[1] == "GiB" || f[1] == "GB") {
		return int(n + 0.5)
	}
	return int(n/1024 + 0.5)
}

// snapshotGPUs asks the host's vendor tools (if any) for names, VRAM, UUIDs
// and the NVLink matrix while the host drivers still own the GPUs.
func snapshotGPUs(ctx context.Context, cfg *config.Config) (gpuCache, error) {
	return snapshotWith(ctx, cfg, nvidiaSMI{}, habanaSMI{})
}

func snapshotWith(ctx context.Context, cfg *config.Config, tools ...gpuInfo) (gpuCache, error) {
	c := loadGPUCache(cfg)
	learned := false
	for _, t := range tools {
		if t.snapshot(ctx, &c) {
			learned = true
		}
	}
	if !learned {
		return c, nil
	}
	if err := config.ClaimDataDir(cfg.DataDir, false); err != nil {
		return c, err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return c, config.WriteFileAtomic(filepath.Join(cfg.DataDir, gpuCacheFile), b, 0o600)
}

// normalizeBusID turns nvidia-smi's 00000000:41:00.0 into 0000:41:00.0.
func normalizeBusID(s string) string {
	s = strings.ToLower(s)
	if i := strings.Index(s, ":"); i > 4 {
		s = s[i-4:]
	}
	return s
}

// host implements agent.Host.
type host struct {
	mu         sync.Mutex
	mgr        *vm.Manager
	cfg        *config.Config
	sys        hw.Sys
	gpus       []hw.GPU
	nvswitches []string
	cacheTopo  string
	subnet     *net.IPNet
	speed      bench.SpeedTest
}

func (h *host) HostID() string { return h.cfg.HostID }

// GPUs reports every registered GPU; one that is gone from the bus is failed.
func (h *host) GPUs() []api.GPUStatus {
	present := map[int]hw.GPU{}
	for _, g := range h.gpus {
		present[g.Index] = g
	}
	idx := make([]int, 0, len(h.cfg.GPUIndex))
	for _, i := range h.cfg.GPUIndex {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	out := make([]api.GPUStatus, 0, len(idx))
	for _, i := range idx {
		g, ok := present[i]
		health := "failed"
		switch {
		case !ok:
		case devmode.NoPassthrough():
			health = "ok"
		case g.Blocker != "":
		default:
			health = h.sys.Health(g.Address)
		}
		out = append(out, api.GPUStatus{Index: i, Health: health})
	}
	return out
}

func (h *host) GPUUUID(i int) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, g := range h.gpus {
		if g.Index == i {
			return g.UUID
		}
	}
	return ""
}

// LearnUUIDs keeps UUIDs a benchmark saw, in memory, in the manager and in
// the GPU cache, so later scrubs and benchmarks match GPUs exactly.
func (h *host) LearnUUIDs(byIndex map[int]string) {
	h.mu.Lock()
	cache := loadGPUCache(h.cfg)
	for i := range h.gpus {
		if u, ok := byIndex[h.gpus[i].Index]; ok && h.gpus[i].UUID == "" {
			h.gpus[i].UUID = u
			c := cache.GPUs[h.gpus[i].Address]
			c.UUID = u
			cache.GPUs[h.gpus[i].Address] = c
		}
	}
	h.mu.Unlock()
	if h.mgr != nil {
		h.mgr.SetUUIDs(byIndex)
	}
	if b, err := json.MarshalIndent(cache, "", "  "); err == nil {
		_ = config.WriteFileAtomic(filepath.Join(h.cfg.DataDir, gpuCacheFile), b, 0o600)
	}
}

func (h *host) GPUAddress(i int) string {
	for _, g := range h.gpus {
		if g.Index == i {
			return g.Address
		}
	}
	return ""
}

// GPUIdentity is what the host knows of a GPU (devnopassthrough benchmarks).
func (h *host) GPUIdentity(i int) (name, uuid string, vramGB int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, g := range h.gpus {
		if g.Index == i {
			return g.ModelRaw, g.UUID, g.VRAMGB
		}
	}
	return "", "", 0
}

func (h *host) Load() api.Load {
	total, avail := h.sys.Mem()
	free, _ := hw.DiskFree(h.cfg.DataDir)
	gb := func(b uint64) float64 { return float64(b*10>>30) / 10 }
	return api.Load{Load1: h.sys.Load1(), MemUsedGB: gb(total - avail), MemTotalGB: gb(total), DiskFreeGB: gb(free)}
}

func (h *host) Topology() api.Topology {
	t := api.Topology{
		GPUsPerNode:   len(h.gpus),
		Interconnect:  "unknown",
		NVSwitchCount: len(h.nvswitches),
		NVLinkMatrix:  h.cacheTopo,
		PartialVMOK:   len(h.nvswitches) == 0,
		NICs:          h.sys.NICs(),
		PrivateAddrs:  hw.PrivateAddrs(h.subnet),
	}
	switch {
	case len(h.nvswitches) > 0:
		t.Interconnect = "nvswitch"
	case h.cacheTopo != "":
		t.Interconnect = "pcie"
		for _, f := range strings.Fields(h.cacheTopo) {
			if len(f) > 2 && strings.HasPrefix(f, "NV") {
				if _, err := strconv.Atoi(f[2:]); err == nil {
					t.Interconnect = "nvlink"
				}
			}
		}
	case len(h.gpus) <= 1:
		t.Interconnect = "pcie"
	}
	if t.NICs == nil {
		t.NICs = []api.NIC{}
	}
	if t.PrivateAddrs == nil {
		t.PrivateAddrs = []string{}
	}
	return t
}

func (h *host) Disk() api.DiskBench { return bench.Disk(h.cfg.DataDir) }

func (h *host) Net(ctx context.Context) api.NetBench { return h.speed.Net(ctx) }

func (h *host) Peers(ctx context.Context, peers []api.Peer, topo api.Topology) []api.PeerResult {
	rdma := false
	for _, n := range topo.NICs {
		if n.RDMADevice != "" && n.LinkUp {
			rdma = true
		}
	}
	// Peers are dialled only on private addresses inside this host's own
	// connected private networks; anything else the control plane names is
	// dropped.
	nets := hw.PrivateNets(h.subnet)
	onSite := make([]api.Peer, 0, len(peers))
	for _, p := range peers {
		var addrs []string
		for _, a := range p.PrivateAddrs {
			ip := net.ParseIP(a)
			for _, n := range nets {
				if ip != nil && ip.IsPrivate() && n.Contains(ip) {
					addrs = append(addrs, ip.String())
					break
				}
			}
		}
		p.PrivateAddrs = addrs
		onSite = append(onSite, p)
	}
	return bench.Peers(ctx, h.cfg.HostID, topo.PrivateAddrs, onSite, rdma, func(addrs []string) (func(), error) {
		return h.openPeerPorts(ctx, addrs)
	})
}

// openPeerPorts lets only the site's peers reach the benchmark ports while
// the test runs; everything else is dropped.
func (h *host) openPeerPorts(ctx context.Context, addrs []string) (func(), error) {
	var ok []string
	for _, a := range addrs {
		if ip := net.ParseIP(a); ip != nil && ip.To4() != nil {
			ok = append(ok, ip.String())
		}
	}
	set := "elements = { " + strings.Join(ok, ", ") + " }"
	if len(ok) == 0 {
		set = ""
	}
	rules := "table inet runcrate_host_bench\ndelete table inet runcrate_host_bench\ntable inet runcrate_host_bench {\n" +
		"\tset peers { type ipv4_addr; " + set + " }\n" +
		"\tchain input { type filter hook input priority filter - 5; policy accept;\n" +
		"\t\ttcp dport { 5201, 18600-18999 } ip saddr != @peers drop\n" +
		"\t\ttcp dport { 5201, 18600-18999 } meta nfproto ipv6 drop\n\t}\n}\n"
	run := sysexec.Exec{}
	if _, err := run.RunInput(ctx, []byte(rules), "nft", "-f", "-"); err != nil {
		return nil, err
	}
	return func() {
		_, _ = run.Run(context.WithoutCancel(ctx), "nft", "delete", "table", "inet", "runcrate_host_bench")
	}, nil
}
