package hw

import (
	"os"
	"path/filepath"
	"testing"
)

type tree struct {
	t    *testing.T
	root string
}

func (tr tree) file(p, content string) {
	full := filepath.Join(tr.root, p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		tr.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		tr.t.Fatal(err)
	}
}

func (tr tree) link(p, target string) {
	full := filepath.Join(tr.root, p)
	_ = os.MkdirAll(filepath.Dir(full), 0o755)
	if err := os.Symlink(target, full); err != nil {
		tr.t.Fatal(err)
	}
}

func (tr tree) dev(addr, vendor, device, class, driver, group string) {
	base := "sys/bus/pci/devices/" + addr
	tr.file(base+"/vendor", vendor+"\n")
	tr.file(base+"/device", device+"\n")
	tr.file(base+"/class", class+"\n")
	if driver != "" {
		tr.link(base+"/driver", "../../../bus/pci/drivers/"+driver)
	}
	if group != "" {
		tr.link(base+"/iommu_group", "../../../../kernel/iommu_groups/"+group)
		tr.file("sys/kernel/iommu_groups/"+group+"/devices/"+addr, "")
	}
}

func TestGPUsWithCompanionsAndBlockers(t *testing.T) {
	tr := tree{t, t.TempDir()}
	// A 4090 with its audio function in one group: usable.
	tr.dev("0000:41:00.0", "0x10de", "0x2684", "0x030000", "nvidia", "20")
	tr.dev("0000:41:00.1", "0x10de", "0x22ba", "0x040300", "snd_hda_intel", "20")
	// An H100 sharing its group with a NIC: blocked.
	tr.dev("0000:81:00.0", "0x10de", "0x2330", "0x030200", "vfio-pci", "30")
	tr.dev("0000:82:00.0", "0x15b3", "0x1021", "0x020000", "mlx5_core", "30")
	// A bridge in a group is fine.
	tr.dev("0000:c1:00.0", "0x10de", "0x2330", "0x030200", "", "40")
	tr.dev("0000:c0:01.0", "0x1022", "0x1483", "0x060400", "pcieport", "40")
	// An NVSwitch.
	tr.dev("0000:e1:00.0", "0x10de", "0x22a3", "0x068000", "", "50")
	tr.file("pci.ids", "10de  NVIDIA Corporation\n\t2330  GH100 [H100 SXM5 80GB]\n\t\t10de 16c1  H100 SXM5 80GB\n\t2684  AD102 [GeForce RTX 4090]\n15b3  Mellanox\n\t1021  MT2910\n")
	s := Sys{Root: tr.root}
	ids := LoadPCIIDs(filepath.Join(tr.root, "pci.ids"))
	gpus, err := s.GPUs(nil, map[string]Cached{"0000:81:00.0": {Name: "NVIDIA H100 80GB HBM3", VRAMGB: 80, UUID: "GPU-1"}}, ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(gpus) != 3 {
		t.Fatalf("gpus = %+v", gpus)
	}
	g := gpus[0]
	if g.Index != 0 || g.ModelRaw != "AD102 [GeForce RTX 4090]" || g.Blocker != "" || len(g.Companions) != 1 || g.Companions[0] != "0000:41:00.1" {
		t.Fatalf("4090 = %+v", g)
	}
	if gpus[1].Blocker == "" || gpus[1].ModelRaw != "NVIDIA H100 80GB HBM3" || gpus[1].VRAMGB != 80 {
		t.Fatalf("H100 sharing a group = %+v", gpus[1])
	}
	if gpus[2].Blocker != "" || len(gpus[2].Companions) != 0 {
		t.Fatalf("bridge must not block or be bound: %+v", gpus[2])
	}
	only, _ := s.GPUs([]string{"0000:c1:00.0"}, nil, ids)
	if len(only) != 1 || only[0].Index != 0 || only[0].Address != "0000:c1:00.0" {
		t.Fatalf("--gpus filter = %+v", only)
	}
	sw, _ := s.NVSwitches()
	if len(sw) != 1 || sw[0].Address != "0000:e1:00.0" {
		t.Fatalf("nvswitches = %+v", sw)
	}
	if !s.IOMMUEnabled() {
		t.Fatal("iommu groups present")
	}
}

func TestHealth(t *testing.T) {
	tr := tree{t, t.TempDir()}
	tr.dev("0000:41:00.0", "0x10de", "0x2684", "0x030000", "vfio-pci", "1")
	tr.file("sys/bus/pci/devices/0000:41:00.0/current_link_width", "16\n")
	tr.file("sys/bus/pci/devices/0000:41:00.0/max_link_width", "16\n")
	s := Sys{Root: tr.root}
	if h := s.Health("0000:41:00.0"); h != "ok" {
		t.Fatalf("health = %s", h)
	}
	tr.file("sys/bus/pci/devices/0000:41:00.0/current_link_width", "8\n")
	if h := s.Health("0000:41:00.0"); h != "degraded" {
		t.Fatalf("narrow link = %s", h)
	}
	tr.file("sys/bus/pci/devices/0000:41:00.0/aer_dev_fatal", "Undefined 0\nTOTAL_ERR_FATAL 2\n")
	if h := s.Health("0000:41:00.0"); h != "failed" {
		t.Fatalf("fatal AER = %s", h)
	}
	if h := s.Health("0000:99:00.0"); h != "failed" {
		t.Fatalf("missing device = %s", h)
	}
}

func TestNICTypes(t *testing.T) {
	tr := tree{t, t.TempDir()}
	// Ethernet 100G.
	tr.file("sys/class/net/eno1/device/vendor", "0x8086")
	tr.file("sys/class/net/eno1/speed", "100000")
	tr.file("sys/class/net/eno1/operstate", "up")
	// RoCE: mlx5_0 backs ens1f0.
	tr.file("sys/class/net/ens1f0/device/vendor", "0x15b3")
	tr.file("sys/class/net/ens1f0/speed", "200000")
	tr.file("sys/class/net/ens1f0/operstate", "up")
	tr.file("sys/class/infiniband/mlx5_0/ports/1/link_layer", "Ethernet")
	tr.file("sys/class/infiniband/mlx5_0/ports/1/rate", "200 Gb/sec (4X HDR)")
	tr.file("sys/class/infiniband/mlx5_0/ports/1/state", "4: ACTIVE")
	tr.file("sys/class/infiniband/mlx5_0/device/net/ens1f0/x", "")
	// InfiniBand without a netdev.
	tr.file("sys/class/infiniband/mlx5_1/ports/1/link_layer", "InfiniBand")
	tr.file("sys/class/infiniband/mlx5_1/ports/1/rate", "400 Gb/sec (4X NDR)")
	tr.file("sys/class/infiniband/mlx5_1/ports/1/state", "4: ACTIVE")
	// Virtual: skipped.
	tr.file("sys/class/net/docker0/operstate", "up")
	nics := Sys{Root: tr.root}.NICs()
	got := map[string]NIC{}
	for _, n := range nics {
		got[n.Name] = n
	}
	if len(nics) != 3 {
		t.Fatalf("nics = %+v", nics)
	}
	if n := got["eno1"]; n.Type != "ethernet" || n.SpeedGbps != 100 || !n.LinkUp {
		t.Fatalf("eno1 = %+v", n)
	}
	if n := got["ens1f0"]; n.Type != "roce" || n.RDMADevice != "mlx5_0" || n.SpeedGbps != 200 {
		t.Fatalf("ens1f0 = %+v", n)
	}
	if n := got["mlx5_1/1"]; n.Type != "infiniband" || n.SpeedGbps != 400 {
		t.Fatalf("ib = %+v", n)
	}
}
