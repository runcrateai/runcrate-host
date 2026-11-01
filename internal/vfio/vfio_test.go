package vfio

import (
	"os"
	"path/filepath"
	"testing"
)

const addr = "0000:82:00.0"

// fakeDev is a function already on vfio-pci, with the given reset_method ("" = no file).
func fakeDev(t *testing.T, resetMethod string) (Sysfs, string) {
	t.Helper()
	root := t.TempDir()
	dev := filepath.Join(root, "sys/bus/pci/devices", addr)
	drv := filepath.Join(root, "sys/bus/pci/drivers", Driver)
	for _, d := range []string{dev, drv} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(drv, filepath.Join(dev, "driver")); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"driver_override", "driver/unbind"} {
		if err := os.WriteFile(filepath.Join(dev, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "sys/bus/pci/drivers_probe"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if resetMethod != "" {
		if err := os.WriteFile(filepath.Join(dev, "reset_method"), []byte(resetMethod), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return Sysfs{Root: root}, filepath.Join(dev, "reset_method")
}

func TestBindPrefersBusReset(t *testing.T) {
	for _, tc := range []struct{ have, want string }{
		{"flr bus\n", "bus"},     // alone on its bus (H100 PCIe): bus reset
		{"flr\n", "flr\n"},       // shares its bus: the kernel offers no bus reset, keep FLR
		{"bus\n", "bus\n"},       // already bus first: untouched
		{"pm flr bus\n", "bus"},  // any order
		{"flr pm\n", "flr pm\n"}, // no bus reset offered
	} {
		s, path := fakeDev(t, tc.have)
		if err := s.Bind(addr); err != nil {
			t.Fatalf("%q: Bind: %v", tc.have, err)
		}
		got, _ := os.ReadFile(path)
		if string(got) != tc.want {
			t.Errorf("reset_method %q: got %q, want %q", tc.have, got, tc.want)
		}
	}
}

func TestBindWithoutResetMethod(t *testing.T) {
	s, path := fakeDev(t, "")
	if err := s.Bind(addr); err != nil {
		t.Fatalf("Bind on a kernel without reset_method: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("reset_method created: %v", err)
	}
}

func TestReleaseRestoresDefaultReset(t *testing.T) {
	s, path := fakeDev(t, "bus\n")
	if err := s.Release(addr); err != nil {
		t.Fatalf("Release: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "default" {
		t.Fatalf("reset_method after Release: %q, want %q", got, "default")
	}
}
