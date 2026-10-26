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

// gpuCacheFile holds what the host's NVIDIA driver said about each GPU before
// the installer moved it to vfio-pci.
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

// snapshotGPUs asks the host driver (if any) for names, VRAM, UUIDs and the
// NVLink matrix while it still owns the GPUs.
func snapshotGPUs(ctx context.Context, cfg *config.Config) (gpuCache, error) {
	c := loadGPUCache(cfg)
	smi, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return c, nil
	}
	out, err := exec.CommandContext(ctx, smi, "--query-gpu=pci.bus_id,name,memory.total,uuid", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return c, nil // driver present but no GPU on it (already on vfio-pci)
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
	if t, err := exec.CommandContext(ctx, smi, "topo", "-m").Output(); err == nil {
		c.Topo = string(t)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
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
	return bench.Peers(ctx, h.cfg.HostID, topo.PrivateAddrs, peers, rdma, func(addrs []string) (func(), error) {
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
