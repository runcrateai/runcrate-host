package vfio

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const addr = "0000:82:00.0"

// fakeDev is one function with the given vendor:device and reset_method ("" = no file),
// already on vfio-pci when bound is set.
func fakeDev(t *testing.T, vendor, device, resetMethod string, bound bool) (Sysfs, string) {
	t.Helper()
	root := t.TempDir()
	dev := filepath.Join(root, "sys/bus/pci/devices", addr)
	drv := filepath.Join(root, "sys/bus/pci/drivers", Driver)
	for _, d := range []string{dev, drv} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join(dev, "vendor"):                     vendor + "\n",
		filepath.Join(dev, "device"):                     device + "\n",
		filepath.Join(dev, "driver_override"):            "",
		filepath.Join(root, "sys/bus/pci/drivers_probe"): "",
		filepath.Join(drv, "unbind"):                     "",
	}
	if resetMethod != "" {
		files[filepath.Join(dev, "reset_method")] = resetMethod
	}
	for p, c := range files {
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if bound {
		if err := os.Symlink(drv, filepath.Join(dev, "driver")); err != nil {
			t.Fatal(err)
		}
	}
	return Sysfs{Root: root}, filepath.Join(dev, "reset_method")
}

func readMethod(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A function not yet on vfio-pci: Bind probes it, and the fake kernel binds it once probed.
func TestFreshBindPutsBusFirstForH100PCIe(t *testing.T) {
	s, path := fakeDev(t, "0x10de", "0x2331", "flr bus\n", false)
	dev := filepath.Join(s.Root, "sys/bus/pci/devices", addr)
	go func() {
		probe := filepath.Join(s.Root, "sys/bus/pci/drivers_probe")
		for range 400 {
			if b, _ := os.ReadFile(probe); string(b) == addr {
				_ = os.Symlink(filepath.Join(s.Root, "sys/bus/pci/drivers", Driver), filepath.Join(dev, "driver"))
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	if err := s.Bind(addr); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dev, "driver_override")); string(b) != Driver {
		t.Fatalf("driver_override %q", b)
	}
	if got := readMethod(t, path); got != "bus flr" {
		t.Fatalf("reset_method %q, want %q (bus first, FLR kept as the fallback)", got, "bus flr")
	}
}

func TestBindAllowlist(t *testing.T) {
	for _, tc := range []struct{ name, device, have, want string }{
		{"H100 PCIe, FLR first", "0x2331", "flr bus\n", "bus flr"},
		{"H100 PCIe, every method kept after bus", "0x2331", "pm flr bus\n", "bus pm flr"},
		{"H100 PCIe already bus first", "0x2331", "bus flr\n", "bus flr\n"},
		{"H100 PCIe sharing its bus (no bus method)", "0x2331", "flr pm\n", "flr pm\n"},
		{"H100 SXM5, not proven", "0x2330", "flr bus\n", "flr bus\n"},
		{"NVSwitch", "0x22a3", "flr bus\n", "flr bus\n"},
		{"RTX 4090", "0x2684", "flr bus\n", "flr bus\n"},
	} {
		s, path := fakeDev(t, "0x10de", tc.device, tc.have, true)
		if err := s.Bind(addr); err != nil {
			t.Fatalf("%s: Bind: %v", tc.name, err)
		}
		if got := readMethod(t, path); got != tc.want {
			t.Errorf("%s: reset_method %q, want %q", tc.name, got, tc.want)
		}
	}
	// Another vendor's 2331 is not the H100.
	s, path := fakeDev(t, "0x8086", "0x2331", "flr bus\n", true)
	if err := s.Bind(addr); err != nil {
		t.Fatal(err)
	}
	if got := readMethod(t, path); got != "flr bus\n" {
		t.Errorf("8086:2331: reset_method %q, want it untouched", got)
	}
}

func TestBusResetRefusedEvenIfAllowlisted(t *testing.T) {
	for _, device := range []string{"0x1eb8", "0x2340", "0x2342", "0x237f"} {
		key := "10de:" + device[2:]
		busResetIDs[key] = true
		s, path := fakeDev(t, "0x10de", device, "flr bus\n", true)
		err := s.Bind(addr)
		got := readMethod(t, path)
		delete(busResetIDs, key)
		if err != nil {
			t.Fatalf("%s: Bind: %v", key, err)
		}
		if got != "flr bus\n" {
			t.Errorf("%s: reset_method %q, want it untouched", key, got)
		}
	}
	// Just outside the refused range is the allowlist's call again.
	if busResetRefused(0x10de, 0x233f) || busResetRefused(0x10de, 0x2380) || !busResetRefused(0x10de, 0x1eb8) {
		t.Fatal("refused range bounds")
	}
}

func TestBindWithoutResetMethod(t *testing.T) {
	s, path := fakeDev(t, "0x10de", "0x2331", "", true)
	if err := s.Bind(addr); err != nil {
		t.Fatalf("Bind on a kernel without reset_method: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("reset_method created: %v", err)
	}
}

func TestReleaseRestoresDefaultOnlyForAllowlisted(t *testing.T) {
	s, path := fakeDev(t, "0x10de", "0x2331", "bus flr\n", true)
	if err := s.Release(addr); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got := readMethod(t, path); got != "default" {
		t.Fatalf("H100 PCIe reset_method after Release %q, want %q", got, "default")
	}
	s, path = fakeDev(t, "0x10de", "0x2330", "bus\n", true)
	if err := s.Release(addr); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got := readMethod(t, path); got != "bus\n" {
		t.Fatalf("other GPU reset_method after Release %q, want it untouched", got)
	}
}

func TestReleaseWithoutResetMethod(t *testing.T) {
	s, path := fakeDev(t, "0x10de", "0x2331", "", true)
	if err := s.Release(addr); err != nil {
		t.Fatalf("Release on a kernel without reset_method: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("reset_method created: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(s.Root, "sys/bus/pci/devices", addr, "driver_override")); string(b) != "\n" {
		t.Fatalf("driver_override after Release %q", b)
	}
}
