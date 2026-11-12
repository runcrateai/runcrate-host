package hw

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// A pci.ids with both vendors, a decoy "1060" under NVIDIA and another vendor,
// and a subsystem line (two tabs) that must be ignored.
const bothVendorsIDs = "10de  NVIDIA Corporation\n\t1060  NVIDIA with Gaudi's device id\n\t2330  GH100 [H100 SXM5 80GB]\n\t\t10de 16c1  H100 SXM5 80GB\n" +
	"1da3  Habana Labs Ltd.\n\t1060  Gaudi3 [HL-325L]\n\t\t1da3 1060  a subsystem\n\t1063  Gaudi3 PCIe\n" +
	"15b3  Mellanox\n\t1060  not a GPU\n"

// gaudiOAMHost lays out eight Gaudi 3 OAM modules (1da3:1060), each alone in
// its own IOMMU group on the habanalabs driver, plus a bridge in group 0.
func gaudiOAMHost(tr tree) {
	for i := range 8 {
		tr.dev(fmt.Sprintf("0000:%02x:00.0", 0x10+i), "0x1da3", "0x1060", "0x120000", "habanalabs", fmt.Sprint(i))
	}
	tr.dev("0000:0f:01.0", "0x1022", "0x1483", "0x060400", "pcieport", "0")
}

func TestGaudi3OAMx8Discovered(t *testing.T) {
	tr := tree{t, t.TempDir()}
	gaudiOAMHost(tr)
	tr.file("pci.ids", bothVendorsIDs)
	s := Sys{Root: tr.root}
	ids := LoadPCIIDs(filepath.Join(tr.root, "pci.ids"))
	gpus, err := s.GPUs(nil, nil, ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(gpus) != 8 {
		t.Fatalf("gpus = %+v", gpus)
	}
	for i, g := range gpus {
		if g.Index != i || g.Address != fmt.Sprintf("0000:%02x:00.0", 0x10+i) || g.DeviceID != "0x1060" {
			t.Errorf("gpu %d numbered or addressed wrong: %+v", i, g)
		}
		if g.Vendor != VendorHabana || g.Family != "gaudi3" || g.Driver != "habanalabs" || g.IOMMUGroup != fmt.Sprint(i) {
			t.Errorf("gpu %d = %+v", i, g)
		}
		// The device's own pci.ids name wins; the decoys under other vendors don't.
		if g.ModelRaw != "Gaudi3 [HL-325L]" {
			t.Errorf("gpu %d name %q", i, g.ModelRaw)
		}
		// No host tool reported the memory: the table's 128 GB, labelled.
		if g.VRAMGB != 128 || !g.VRAMFallback {
			t.Errorf("gpu %d vram %d fallback %v", i, g.VRAMGB, g.VRAMFallback)
		}
		if g.Blocker != "" || len(g.Companions) != 0 {
			t.Errorf("gpu %d must be usable alone: %+v", i, g)
		}
	}
	if sw, _ := s.NVSwitches(); len(sw) != 0 {
		t.Fatalf("Gaudi is not an NVSwitch: %+v", sw)
	}
	only, _ := s.GPUs([]string{"0000:13:00.0"}, nil, ids)
	if len(only) != 1 || only[0].Index != 0 || only[0].Address != "0000:13:00.0" {
		t.Fatalf("--gpus filter = %+v", only)
	}
}

func TestGaudiNamesAndMemory(t *testing.T) {
	tr := tree{t, t.TempDir()}
	tr.dev("0000:10:00.0", "0x1da3", "0x1060", "0x120000", "habanalabs", "1")
	tr.dev("0000:11:00.0", "0x1da3", "0x1063", "0x120000", "habanalabs", "2")
	tr.dev("0000:12:00.0", "0x1da3", "0x1ffe", "0x120000", "", "3") // a device we don't know
	s := Sys{Root: tr.root}
	// No pci.ids: our table's names, then the vendor label and the id.
	gpus, _ := s.GPUs(nil, map[string]Cached{"0000:10:00.0": {Name: "HL-325L", VRAMGB: 126, UUID: "AIP-1"}}, LoadPCIIDs(filepath.Join(tr.root, "absent")))
	if len(gpus) != 3 {
		t.Fatalf("gpus = %+v", gpus)
	}
	// The host tool's name and memory beat the table, and aren't marked as a fallback.
	if g := gpus[0]; g.ModelRaw != "HL-325L" || g.VRAMGB != 126 || g.VRAMFallback || g.UUID != "AIP-1" || g.Family != "gaudi3" {
		t.Fatalf("gpu 0 = %+v", g)
	}
	if g := gpus[1]; g.ModelRaw != "Intel Gaudi 3 PCIe" || g.VRAMGB != 128 || !g.VRAMFallback {
		t.Fatalf("gpu 1 = %+v", g)
	}
	if g := gpus[2]; g.ModelRaw != "Intel Habana 1ffe" || g.VRAMGB != 0 || g.VRAMFallback || g.Family != "" {
		t.Fatalf("unknown Habana device = %+v", g)
	}
}

func TestGaudiIsNotGPUOutsideItsClasses(t *testing.T) {
	for _, tc := range []struct {
		class string
		gpu   bool
	}{
		{"0x120000", true},  // processing accelerator
		{"0x128000", true},  // any 0x12xx
		{"0x030200", true},  // 3D controller
		{"0x020000", false}, // a NIC function of the same vendor
		{"0x060400", false}, // a bridge
	} {
		d := PCIDevice{Vendor: HabanaVendor, Device: "0x1060", Class: tc.class}
		if d.IsGPU() != tc.gpu {
			t.Errorf("class %s: IsGPU = %v", tc.class, d.IsGPU())
		}
		if d.IsNVSwitch() {
			t.Errorf("class %s: a Gaudi is never an NVSwitch", tc.class)
		}
	}
	// 0x0680 under Gaudi's vendor is not an NVSwitch rule either.
	if (PCIDevice{Vendor: HabanaVendor, Class: "0x068000"}).IsNVSwitch() {
		t.Error("NVSwitch rule applied to Gaudi")
	}
}

func TestGaudiCompanionsAndBlockers(t *testing.T) {
	tr := tree{t, t.TempDir()}
	// A module with another function of its own (same slot, same vendor): bound along with it.
	tr.dev("0000:10:00.0", "0x1da3", "0x1060", "0x120000", "habanalabs", "1")
	tr.dev("0000:10:00.1", "0x1da3", "0x1061", "0x118000", "habanalabs", "1")
	// A module sharing its group with a NIC: blocked.
	tr.dev("0000:20:00.0", "0x1da3", "0x1060", "0x120000", "habanalabs", "2")
	tr.dev("0000:21:00.0", "0x15b3", "0x1021", "0x020000", "mlx5_core", "2")
	// A module sharing its slot's group with an NVIDIA function: blocked, another vendor's.
	tr.dev("0000:30:00.0", "0x1da3", "0x1060", "0x120000", "habanalabs", "3")
	tr.dev("0000:30:00.1", "0x10de", "0x22ba", "0x040300", "snd_hda_intel", "3")
	// A module with no IOMMU group: blocked.
	tr.dev("0000:40:00.0", "0x1da3", "0x1060", "0x120000", "habanalabs", "")
	// A module behind a bridge that shares its group: fine.
	tr.dev("0000:50:00.0", "0x1da3", "0x1060", "0x120000", "", "5")
	tr.dev("0000:4f:01.0", "0x1022", "0x1483", "0x060400", "pcieport", "5")
	gpus, _ := Sys{Root: tr.root}.GPUs(nil, nil, nil)
	if len(gpus) != 5 {
		t.Fatalf("gpus = %+v", gpus)
	}
	if g := gpus[0]; g.Blocker != "" || len(g.Companions) != 1 || g.Companions[0] != "0000:10:00.1" {
		t.Fatalf("own function = %+v", g)
	}
	if g := gpus[1]; !strings.Contains(g.Blocker, "IOMMU group 2 also holds 0000:21:00.0") {
		t.Fatalf("NIC in group = %+v", g)
	}
	if g := gpus[2]; !strings.Contains(g.Blocker, "IOMMU group 3 also holds 0000:30:00.1") || len(g.Companions) != 0 {
		t.Fatalf("NVIDIA function in group = %+v", g)
	}
	if g := gpus[3]; g.Blocker != "IOMMU is off" {
		t.Fatalf("no IOMMU group = %+v", g)
	}
	if g := gpus[4]; g.Blocker != "" || len(g.Companions) != 0 {
		t.Fatalf("bridge in group = %+v", g)
	}
}

// NVIDIA and Gaudi on one host: every GPU is reported, numbered by address,
// with its vendor, and the NVSwitch rule stays NVIDIA's.
func TestMixedNvidiaAndGaudiHost(t *testing.T) {
	tr := tree{t, t.TempDir()}
	tr.dev("0000:19:00.0", "0x1da3", "0x1060", "0x120000", "habanalabs", "1")
	tr.dev("0000:41:00.0", "0x10de", "0x2330", "0x030200", "nvidia", "2")
	tr.dev("0000:ca:00.0", "0x1da3", "0x1060", "0x120000", "habanalabs", "3")
	tr.dev("0000:e1:00.0", "0x10de", "0x22a3", "0x068000", "", "4")
	tr.file("pci.ids", bothVendorsIDs)
	s := Sys{Root: tr.root}
	gpus, err := s.GPUs(nil, map[string]Cached{"0000:41:00.0": {Name: "NVIDIA H100 80GB HBM3", VRAMGB: 80, UUID: "GPU-1"}}, LoadPCIIDs(filepath.Join(tr.root, "pci.ids")))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, g := range gpus {
		got = append(got, fmt.Sprintf("%d %s %s %q %d %v", g.Index, g.Address, g.Vendor, g.ModelRaw, g.VRAMGB, g.VRAMFallback))
	}
	want := []string{
		`0 0000:19:00.0 intel-habana "Gaudi3 [HL-325L]" 128 true`,
		`1 0000:41:00.0 nvidia "NVIDIA H100 80GB HBM3" 80 false`,
		`2 0000:ca:00.0 intel-habana "Gaudi3 [HL-325L]" 128 true`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("gpus:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if sw, _ := s.NVSwitches(); len(sw) != 1 || sw[0].Address != "0000:e1:00.0" {
		t.Fatalf("nvswitches = %+v", sw)
	}
}

func TestPCIIDsKeysByVendor(t *testing.T) {
	tr := tree{t, t.TempDir()}
	tr.file("pci.ids", bothVendorsIDs)
	ids := LoadPCIIDs(filepath.Join(tr.root, "pci.ids"))
	for _, tc := range []struct{ vendor, device, want string }{
		{"0x10de", "0x1060", "NVIDIA with Gaudi's device id"},
		{"0x1da3", "0x1060", "Gaudi3 [HL-325L]"},
		{"0x1da3", "0x1063", "Gaudi3 PCIe"},
		{"0x10de", "0x2330", "GH100 [H100 SXM5 80GB]"},
		{"0x10de", "0x9999", "NVIDIA 9999"},
		{"0x1da3", "0x1ffe", "Intel Habana 1ffe"},
		{"0x15b3", "0x1060", "PCI 15b3:1060"}, // Mellanox is not a GPU vendor: not read
	} {
		if got := ids.Name(tc.vendor, tc.device); got != tc.want {
			t.Errorf("Name(%s, %s) = %q, want %q", tc.vendor, tc.device, got, tc.want)
		}
	}
	// No database at all: our own table.
	var none *PCIIDs
	if got := none.Name("0x1da3", "0x1060"); got != "Intel Gaudi 3 OAM" {
		t.Errorf("nil database: %q", got)
	}
}

// An NVIDIA-only host reads exactly as before the Gaudi vendor existed: the
// same GPUs, numbering, names, companions and blockers (the fixture of
// TestGPUsWithCompanionsAndBlockers), rendered with the fields GPU had then.
func TestNvidiaOnlyHostGolden(t *testing.T) {
	tr := tree{t, t.TempDir()}
	tr.dev("0000:41:00.0", "0x10de", "0x2684", "0x030000", "nvidia", "20")
	tr.dev("0000:41:00.1", "0x10de", "0x22ba", "0x040300", "snd_hda_intel", "20")
	tr.dev("0000:81:00.0", "0x10de", "0x2330", "0x030200", "vfio-pci", "30")
	tr.dev("0000:82:00.0", "0x15b3", "0x1021", "0x020000", "mlx5_core", "30")
	tr.dev("0000:c1:00.0", "0x10de", "0x2330", "0x030200", "", "40")
	tr.dev("0000:c0:01.0", "0x1022", "0x1483", "0x060400", "pcieport", "40")
	tr.dev("0000:e1:00.0", "0x10de", "0x22a3", "0x068000", "", "50")
	tr.file("pci.ids", "10de  NVIDIA Corporation\n\t2330  GH100 [H100 SXM5 80GB]\n\t\t10de 16c1  H100 SXM5 80GB\n\t2684  AD102 [GeForce RTX 4090]\n15b3  Mellanox\n\t1021  MT2910\n")
	s := Sys{Root: tr.root}
	gpus, err := s.GPUs(nil, map[string]Cached{"0000:81:00.0": {Name: "NVIDIA H100 80GB HBM3", VRAMGB: 80, UUID: "GPU-1"}}, LoadPCIIDs(filepath.Join(tr.root, "pci.ids")))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, g := range gpus {
		got = append(got, fmt.Sprintf("%d %s %s %q %d %q %q %q %v %q", g.Index, g.Address, g.DeviceID, g.ModelRaw, g.VRAMGB, g.UUID, g.Driver, g.IOMMUGroup, g.Companions, g.Blocker))
		if g.Vendor != VendorNvidia || g.Family != "" || g.VRAMFallback {
			t.Errorf("NVIDIA GPU got Gaudi-side fields: %+v", g)
		}
	}
	golden := strings.Join([]string{
		`0 0000:41:00.0 0x2684 "AD102 [GeForce RTX 4090]" 0 "" "nvidia" "20" [0000:41:00.1] ""`,
		`1 0000:81:00.0 0x2330 "NVIDIA H100 80GB HBM3" 80 "GPU-1" "vfio-pci" "30" [] "IOMMU group 30 also holds 0000:82:00.0 (0x020000); turn on ACS or move the card"`,
		`2 0000:c1:00.0 0x2330 "GH100 [H100 SXM5 80GB]" 0 "" "" "40" [] ""`,
	}, "\n")
	if g := strings.Join(got, "\n"); g != golden {
		t.Fatalf("NVIDIA-only host changed:\n%s\nwant:\n%s", g, golden)
	}
	// Without pci.ids an NVIDIA card keeps its old fallback name, and no VRAM table applies.
	bare, _ := s.GPUs([]string{"0000:41:00.0"}, nil, nil)
	if len(bare) != 1 || bare[0].ModelRaw != "NVIDIA 2684" || bare[0].VRAMGB != 0 {
		t.Fatalf("bare NVIDIA = %+v", bare)
	}
}
