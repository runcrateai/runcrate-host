package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/runcrateai/runcrate-host/internal/config"
	"github.com/runcrateai/runcrate-host/internal/hw"
)

const (
	nvidiaQuery = "nvidia-smi --query-gpu=pci.bus_id,name,memory.total,uuid --format=csv,noheader,nounits"
	nvidiaTopo  = "nvidia-smi topo -m"
	habanaQuery = "hl-smi --query-aip=bus_id,name,memory.total --format=csv,noheader,nounits"
)

// fakeTools answers "<tool> <args>" with canned output; a tool with no entry
// is not installed, and a command with no entry fails.
type fakeTools struct {
	out   map[string]string
	calls []string
	// deadlines is the time left on each command's context, by command.
	deadlines map[string]time.Duration
}

func (f *fakeTools) tool() smiTool {
	return smiTool{
		look: func(file string) (string, error) {
			for k := range f.out {
				if strings.HasPrefix(k, file+" ") {
					return "/fake/" + file, nil
				}
			}
			return "", errors.New("not found")
		},
		run: func(ctx context.Context, bin string, args ...string) ([]byte, error) {
			key := filepath.Base(bin) + " " + strings.Join(args, " ")
			f.calls = append(f.calls, key)
			if dl, ok := ctx.Deadline(); ok {
				if f.deadlines == nil {
					f.deadlines = map[string]time.Duration{}
				}
				f.deadlines[key] = time.Until(dl)
			}
			if o, ok := f.out[key]; ok {
				return []byte(o), nil
			}
			return nil, errors.New("exit status 1")
		},
	}
}

func (f *fakeTools) snapshot(t *testing.T, cfg *config.Config) (gpuCache, error) {
	t.Helper()
	return snapshotWith(context.Background(), cfg, nvidiaSMI{f.tool()}, habanaSMI{f.tool()})
}

func newCfg(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{DataDir: filepath.Join(t.TempDir(), "data")}
}

func cacheFile(cfg *config.Config) (string, error) {
	b, err := os.ReadFile(filepath.Join(cfg.DataDir, gpuCacheFile))
	return string(b), err
}

// An NVIDIA host: the same two nvidia-smi calls, the same cache file, byte for byte.
func TestSnapshotNvidiaUnchanged(t *testing.T) {
	cfg := newCfg(t)
	f := &fakeTools{out: map[string]string{
		nvidiaQuery: "00000000:41:00.0, NVIDIA H100 80GB HBM3, 81559, GPU-1\n00000000:42:00.0, NVIDIA H100 80GB HBM3, 81559, GPU-2\n",
		nvidiaTopo:  "GPU0 GPU1\nGPU0 X NV18\n",
	}}
	c, err := f.snapshot(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{nvidiaQuery, nvidiaTopo}; !slices.Equal(f.calls, want) {
		t.Fatalf("commands run: %v, want %v", f.calls, want)
	}
	if c.GPUs["0000:41:00.0"].VRAMGB != 80 || c.GPUs["0000:42:00.0"].UUID != "GPU-2" || c.Topo != "GPU0 GPU1\nGPU0 X NV18\n" {
		t.Fatalf("cache = %+v", c)
	}
	want := `{
  "gpus": {
    "0000:41:00.0": {
      "name": "NVIDIA H100 80GB HBM3",
      "vram_gb": 80,
      "uuid": "GPU-1"
    },
    "0000:42:00.0": {
      "name": "NVIDIA H100 80GB HBM3",
      "vram_gb": 80,
      "uuid": "GPU-2"
    }
  },
  "topo": "GPU0 GPU1\nGPU0 X NV18\n"
}`
	if got, err := cacheFile(cfg); err != nil || got != want {
		t.Fatalf("gpus-cache.json (%v):\n%s\nwant:\n%s", err, got, want)
	}
}

// No vendor tool, or one that fails: nothing is written and nothing breaks.
func TestSnapshotWithoutToolsWritesNothing(t *testing.T) {
	for name, out := range map[string]map[string]string{
		"no tools":              {},
		"nvidia-smi failing":    {"nvidia-smi --other": ""},
		"hl-smi failing":        {"hl-smi --other": ""},
		"hl-smi output unknown": {habanaQuery: "Error: no devices\n"},
	} {
		cfg := newCfg(t)
		f := &fakeTools{out: out}
		c, err := f.snapshot(t, cfg)
		if err != nil || len(c.GPUs) != 0 || c.Topo != "" {
			t.Fatalf("%s: cache %+v, err %v", name, c, err)
		}
		if _, err := os.Stat(filepath.Join(cfg.DataDir, gpuCacheFile)); !os.IsNotExist(err) {
			t.Fatalf("%s: gpus-cache.json written (%v)", name, err)
		}
	}
}

// A Gaudi host with hl-smi: names and memory by PCI address, no nvidia-smi
// needed, no topology, a header line skipped, a UUID learned earlier kept.
func TestSnapshotGaudiWithHlSmi(t *testing.T) {
	cfg := newCfg(t)
	if err := config.ClaimDataDir(cfg.DataDir, false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.DataDir, gpuCacheFile), []byte(`{"gpus":{"0000:19:00.0":{"name":"old","vram_gb":0,"uuid":"AIP-1"}},"topo":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeTools{out: map[string]string{
		habanaQuery: "bus_id, name, memory.total\n00000000:19:00.0, HL-325L, 131072\n0000:3b:00.0, HL-325L, 131072\n",
	}}
	c, err := f.snapshot(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{habanaQuery}; !slices.Equal(f.calls, want) {
		t.Fatalf("commands run: %v, want %v", f.calls, want)
	}
	if len(c.GPUs) != 2 || c.Topo != "" {
		t.Fatalf("cache = %+v", c)
	}
	if g := c.GPUs["0000:19:00.0"]; g.Name != "HL-325L" || g.VRAMGB != 128 || g.UUID != "AIP-1" {
		t.Fatalf("0000:19:00.0 = %+v", g)
	}
	if g := c.GPUs["0000:3b:00.0"]; g.Name != "HL-325L" || g.VRAMGB != 128 || g.UUID != "" {
		t.Fatalf("0000:3b:00.0 = %+v", g)
	}
	onDisk := loadGPUCache(cfg)
	if onDisk.GPUs["0000:3b:00.0"].VRAMGB != 128 || onDisk.GPUs["0000:19:00.0"].UUID != "AIP-1" {
		t.Fatalf("on disk = %+v", onDisk)
	}
}

// NVIDIA and Gaudi on one host: both tools run, each fills its own GPUs, and
// the NVLink matrix is NVIDIA's.
func TestSnapshotMixedHost(t *testing.T) {
	cfg := newCfg(t)
	f := &fakeTools{out: map[string]string{
		nvidiaQuery: "00000000:41:00.0, NVIDIA H100 80GB HBM3, 81559, GPU-1\n",
		nvidiaTopo:  "GPU0 X\n",
		habanaQuery: "0000:19:00.0, HL-325L, 131072\n",
	}}
	c, err := f.snapshot(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.GPUs) != 2 || c.GPUs["0000:41:00.0"].UUID != "GPU-1" || c.GPUs["0000:19:00.0"].Name != "HL-325L" || c.Topo != "GPU0 X\n" {
		t.Fatalf("cache = %+v", c)
	}
}

func TestMemoryGB(t *testing.T) {
	for in, want := range map[string]int{
		" 131072":   128,
		"126976":    124,
		"128 GiB":   128,
		"126 GB":    126,
		"98304 MiB": 96,
		"":          0,
		"abc":       0,
		"0":         0,
		"-5":        0,
		"NaN":       0,
		"nan MiB":   0,
		"Inf":       0,
		"+Inf GiB":  0,
		"-Inf":      0,
		"1e308":     0,
		"2000 GiB":  0, // above 1 TiB
		"1024 GiB":  1024,
		"1048576":   1024, // 1 TiB in MiB
		"2097152":   0,
	} {
		if got := memoryGB(in); got != want {
			t.Errorf("memoryGB(%q) = %d, want %d", in, got, want)
		}
	}
}

// hl-smi gets a deadline (a hung Habana driver must not hang the agent's
// start); nvidia-smi's calls are as they were, with none.
func TestHlSmiHasADeadline(t *testing.T) {
	f := &fakeTools{out: map[string]string{
		habanaQuery: "0000:19:00.0, HL-325L, 131072\n",
		nvidiaQuery: "00000000:41:00.0, NVIDIA H100 80GB HBM3, 81559, GPU-1\n",
		nvidiaTopo:  "GPU0 X\n",
	}}
	if _, err := f.snapshot(t, newCfg(t)); err != nil {
		t.Fatal(err)
	}
	dl, ok := f.deadlines[habanaQuery]
	if !ok || dl <= 0 || dl > 30*time.Second {
		t.Fatalf("hl-smi deadline = %v, %v; want one of at most 30s", dl, ok)
	}
	if len(f.deadlines) != 1 {
		t.Fatalf("only hl-smi may get a deadline: %v", f.deadlines)
	}
}

// Gaudi OAM modules sit on one baseboard: a Gaudi host is only rented whole.
func TestTopologyPartialVMOK(t *testing.T) {
	nv := hw.GPU{Vendor: hw.VendorNvidia}
	gd := hw.GPU{Vendor: hw.VendorHabana}
	for _, tc := range []struct {
		name       string
		gpus       []hw.GPU
		nvswitches []string
		want       bool
	}{
		{"nvidia", []hw.GPU{nv, nv}, nil, true},
		{"nvidia without a recorded vendor", []hw.GPU{{}, {}}, nil, true},
		{"nvswitch node", []hw.GPU{nv, nv}, []string{"0000:e1:00.0"}, false},
		{"gaudi baseboard", []hw.GPU{gd, gd, gd, gd, gd, gd, gd, gd}, nil, false},
		{"mixed", []hw.GPU{nv, gd}, nil, false},
	} {
		h := &host{gpus: tc.gpus, nvswitches: tc.nvswitches, sys: hw.Sys{Root: t.TempDir()}}
		if got := h.Topology().PartialVMOK; got != tc.want {
			t.Errorf("%s: PartialVMOK = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Registration carries a Gaudi's reset_method (explicit "" when it has none);
// an NVIDIA GPU's entry is byte-identical to what it was, whatever its reset
// method is.
func TestRegisterGPUCarriesResetMethodForGaudiOnly(t *testing.T) {
	for name, tc := range map[string]struct {
		gpu  hw.GPU
		want string
	}{
		"nvidia": {hw.GPU{Index: 0, ModelRaw: "H100", VRAMGB: 80, UUID: "GPU-1", Address: "0000:41:00.0", Vendor: hw.VendorNvidia, ResetMethod: "flr"},
			`{"index":0,"model_raw":"H100","vram_gb":80,"uuid":"GPU-1","pcie_address":"0000:41:00.0"}`},
		"nvidia unrecorded vendor": {hw.GPU{Index: 1, ModelRaw: "H100", VRAMGB: 80, Address: "0000:42:00.0", ResetMethod: "flr bus"},
			`{"index":1,"model_raw":"H100","vram_gb":80,"uuid":"","pcie_address":"0000:42:00.0"}`},
		"gaudi": {hw.GPU{Index: 2, ModelRaw: "Intel Gaudi 3 OAM", VRAMGB: 128, Address: "0000:19:00.0", Vendor: hw.VendorHabana, ResetMethod: "flr bus"},
			`{"index":2,"model_raw":"Intel Gaudi 3 OAM","vram_gb":128,"uuid":"","pcie_address":"0000:19:00.0","reset_method":"flr bus"}`},
		"gaudi without a reset method": {hw.GPU{Index: 3, ModelRaw: "Intel Gaudi 3 OAM", VRAMGB: 128, Address: "0000:3b:00.0", Vendor: hw.VendorHabana},
			`{"index":3,"model_raw":"Intel Gaudi 3 OAM","vram_gb":128,"uuid":"","pcie_address":"0000:3b:00.0","reset_method":""}`},
	} {
		raw, err := json.Marshal(registerGPU(tc.gpu))
		if err != nil || string(raw) != tc.want {
			t.Errorf("%s: payload %s (%v), want %s", name, raw, err, tc.want)
		}
	}
}
