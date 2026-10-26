package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/runcrateai/runcrate-host/internal/api"
	"github.com/runcrateai/runcrate-host/internal/bench"
	"github.com/runcrateai/runcrate-host/internal/config"
	"github.com/runcrateai/runcrate-host/internal/hw"
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
	cfg        *config.Config
	sys        hw.Sys
	gpus       []hw.GPU
	nvswitches []string
	cacheTopo  string
	subnet     *net.IPNet
	speed      bench.SpeedTest
}

func (h *host) HostID() string { return h.cfg.HostID }

func (h *host) GPUs() []api.GPUStatus {
	out := make([]api.GPUStatus, 0, len(h.gpus))
	for _, g := range h.gpus {
		health := h.sys.Health(g.Address)
		if g.Blocker != "" {
			health = "failed"
		}
		out = append(out, api.GPUStatus{Index: g.Index, Health: health})
	}
	return out
}

func (h *host) GPUAddress(i int) string {
	for _, g := range h.gpus {
		if g.Index == i {
			return g.Address
		}
	}
	return ""
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
	return bench.Peers(ctx, h.cfg.HostID, topo.PrivateAddrs, peers, rdma)
}
