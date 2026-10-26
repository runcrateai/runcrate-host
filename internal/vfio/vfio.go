// Package vfio moves PCI functions between the host's driver and vfio-pci
// with driver_override, so only the chosen GPUs move and they can be given
// back without a reboot.
package vfio

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/runcrateai/runcrate-host/internal/hw"
)

// Driver is the passthrough driver.
const Driver = "vfio-pci"

// Sysfs writes under a root ("/" in production).
type Sysfs struct{ Root string }

func (s Sysfs) dev(addr, f string) string {
	return filepath.Join(s.Root, "sys/bus/pci/devices", addr, f)
}

func (s Sysfs) driver(addr string) string {
	l, err := os.Readlink(s.dev(addr, "driver"))
	if err != nil {
		return ""
	}
	return filepath.Base(l)
}

func write(path, val string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(val)
	return err
}

// Bind puts one function on vfio-pci. Idempotent.
func (s Sysfs) Bind(addr string) error {
	if !hw.ValidPCIAddress(addr) {
		return fmt.Errorf("bad pci address %q", addr)
	}
	if s.driver(addr) == Driver {
		return nil
	}
	if err := write(s.dev(addr, "driver_override"), Driver); err != nil {
		return fmt.Errorf("%s: driver_override: %w", addr, err)
	}
	if cur := s.driver(addr); cur != "" {
		// Unbinding from the NVIDIA driver blocks while something holds the
		// GPU (nvidia-persistenced, a display server, a CUDA process).
		if err := write(s.dev(addr, "driver/unbind"), addr); err != nil {
			return fmt.Errorf("%s: unbind from %s (is something using the GPU?): %w", addr, cur, err)
		}
	}
	if err := write(filepath.Join(s.Root, "sys/bus/pci/drivers_probe"), addr); err != nil {
		// Older kernels: bind directly.
		if err2 := write(filepath.Join(s.Root, "sys/bus/pci/drivers", Driver, "bind"), addr); err2 != nil {
			return fmt.Errorf("%s: bind to vfio-pci: %w", addr, errors.Join(err, err2))
		}
	}
	for i := 0; i < 50; i++ {
		if s.driver(addr) == Driver {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("%s: still on %q after binding to vfio-pci", addr, s.driver(addr))
}

// Release gives one function back to the host: clears the override and lets
// the kernel pick the normal driver (nvidia or nouveau, if installed).
func (s Sysfs) Release(addr string) error {
	if !hw.ValidPCIAddress(addr) {
		return fmt.Errorf("bad pci address %q", addr)
	}
	if err := write(s.dev(addr, "driver_override"), "\n"); err != nil {
		return fmt.Errorf("%s: clear driver_override: %w", addr, err)
	}
	if s.driver(addr) == Driver {
		if err := write(s.dev(addr, "driver/unbind"), addr); err != nil {
			return fmt.Errorf("%s: unbind from vfio-pci: %w", addr, err)
		}
	}
	// No host driver is fine: the GPU is free either way.
	_ = write(filepath.Join(s.Root, "sys/bus/pci/drivers_probe"), addr)
	return nil
}

// Bound reports whether every address is on vfio-pci.
func (s Sysfs) Bound(addrs ...string) bool {
	for _, a := range addrs {
		if s.driver(a) != Driver {
			return false
		}
	}
	return true
}

// ModprobeConf is /etc/modprobe.d content that makes vfio-pci claim the
// given vendor:device ids at boot, before the NVIDIA driver loads. Used only
// when Runcrate takes every NVIDIA GPU on the host.
func ModprobeConf(ids []string) string {
	var b strings.Builder
	b.WriteString("# Written by runcrate-host. Removed by `runcrate-host uninstall`.\n")
	fmt.Fprintf(&b, "options vfio-pci ids=%s disable_vga=1\n", strings.Join(ids, ","))
	for _, m := range []string{"nvidia", "nvidia_drm", "nvidia_modeset", "nouveau", "snd_hda_intel", "xhci_pci"} {
		fmt.Fprintf(&b, "softdep %s pre: vfio-pci\n", m)
	}
	return b.String()
}
