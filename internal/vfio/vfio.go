// Package vfio moves PCI functions between the host's driver and vfio-pci
// with driver_override, so only the chosen GPUs move and they can be given
// back without a reboot.
package vfio

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
	// O_TRUNC as a shell's > does: sysfs ignores it, a test's plain file gets only val.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(val)
	return err
}

// Bind puts one function on vfio-pci, with a bus reset first for the device
// IDs that need one (preferBusReset). Idempotent. For an Intel Gaudi (vendor
// 1da3) a failure at any step puts the device back as it was found: the
// override cleared and the original driver probed again, so a module whose
// unbind was refused (busy) is not left with an override that sends it to
// vfio-pci on the next probe or boot. NVIDIA keeps its behaviour: nothing is
// rolled back.
func (s Sysfs) Bind(addr string) error {
	if !hw.ValidPCIAddress(addr) {
		return fmt.Errorf("bad pci address %q", addr)
	}
	if s.driver(addr) == Driver {
		return s.preferBusReset(addr)
	}
	orig := s.driver(addr)
	// fail reports err and, for a Gaudi, restores the device as found.
	fail := func(err error) error {
		if v, _, perr := s.pciID(addr); perr != nil || v != gaudiVendor {
			return err
		}
		if rerr := s.rollback(addr, orig); rerr != nil {
			return errors.Join(err, fmt.Errorf("%s: rollback to %q: %w", addr, orig, rerr))
		}
		return err
	}
	if err := write(s.dev(addr, "driver_override"), Driver); err != nil {
		return fmt.Errorf("%s: driver_override: %w", addr, err)
	}
	if cur := s.driver(addr); cur != "" {
		// Unbinding from the host driver (nvidia, habanalabs, ...) blocks while
		// something holds the GPU (nvidia-persistenced, a display server, a
		// CUDA process, a Gaudi runtime).
		if err := write(s.dev(addr, "driver/unbind"), addr); err != nil {
			return fail(fmt.Errorf("%s: unbind from %s (is something using the GPU?): %w", addr, cur, err))
		}
	}
	if err := write(filepath.Join(s.Root, "sys/bus/pci/drivers_probe"), addr); err != nil {
		// Older kernels: bind directly.
		if err2 := write(filepath.Join(s.Root, "sys/bus/pci/drivers", Driver, "bind"), addr); err2 != nil {
			return fail(fmt.Errorf("%s: bind to vfio-pci: %w", addr, errors.Join(err, err2)))
		}
	}
	for i := 0; i < 50; i++ {
		if s.driver(addr) == Driver {
			if err := s.preferBusReset(addr); err != nil {
				return fail(err)
			}
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fail(fmt.Errorf("%s: still on %q after binding to vfio-pci", addr, s.driver(addr)))
}

// gaudiVendor is the PCI vendor id of Intel Gaudi (Habana Labs), as pciID reads it.
const gaudiVendor = 0x1da3

// rollback undoes a failed Bind: clears the override, takes the function off
// vfio-pci if it got there, and lets the kernel probe the original driver
// again (drivers_probe, else a direct bind to it).
func (s Sysfs) rollback(addr, orig string) error {
	errs := []error{}
	if err := write(s.dev(addr, "driver_override"), "\n"); err != nil {
		errs = append(errs, fmt.Errorf("clear driver_override: %w", err))
	}
	if s.driver(addr) == Driver {
		if err := write(s.dev(addr, "driver/unbind"), addr); err != nil {
			errs = append(errs, fmt.Errorf("unbind from vfio-pci: %w", err))
		}
	}
	if s.driver(addr) == "" {
		if err := write(filepath.Join(s.Root, "sys/bus/pci/drivers_probe"), addr); err != nil {
			if orig == "" {
				errs = append(errs, fmt.Errorf("reprobe: %w", err))
			} else if err2 := write(filepath.Join(s.Root, "sys/bus/pci/drivers", orig, "bind"), addr); err2 != nil {
				errs = append(errs, fmt.Errorf("reprobe: %w", errors.Join(err, err2)))
			}
		}
	}
	return errors.Join(errs...)
}

// busResetIDs are the vendor:device IDs proven to need a secondary bus reset
// instead of FLR. 10de:2331, H100 PCIe (2026-11-01 e2e on a bare-metal H100
// PCIe test server, host kernel 7.0): reset by FLR when vfio-pci opened it, the
// card came back with its BARs cleared and memory decode off. The guest
// driver read 0xbadf0200 from BOOT_0 and called the card unsupported. Once
// the BARs were written back, the first CUDA job hit Xid 95 (uncontained)
// and hung. With a bus reset, the same card passed the benchmark and served
// a VM. Add an ID only with a run like that behind it.
var busResetIDs = map[string]bool{
	"10de:2331": true,
	// 1da3 (Intel Gaudi 3: 1da3:1060 OAM, 1da3:1063 PCIe): none yet. Fill from
	// the box (reset_method), as above; until then Gaudi keeps the kernel's
	// own reset methods, like any NVIDIA id not listed.
}

// busResetRefused says a bus reset is known to break the device: T4
// (10de:1eb8) and the 10de:2340-237f range. Checked before busResetIDs, so
// adding one of them there by mistake still sets nothing.
func busResetRefused(vendor, device uint64) bool {
	return vendor == 0x10de && (device == 0x1eb8 || (device >= 0x2340 && device <= 0x237f))
}

// pciID reads a function's vendor and device IDs from sysfs.
func (s Sysfs) pciID(addr string) (vendor, device uint64, err error) {
	for _, f := range []struct {
		name string
		v    *uint64
	}{{"vendor", &vendor}, {"device", &device}} {
		b, err := os.ReadFile(s.dev(addr, f.name))
		if err != nil {
			return 0, 0, fmt.Errorf("%s: read %s: %w", addr, f.name, err)
		}
		if *f.v, err = strconv.ParseUint(strings.TrimPrefix(strings.TrimSpace(string(b)), "0x"), 16, 16); err != nil {
			return 0, 0, fmt.Errorf("%s: bad %s %q", addr, f.name, strings.TrimSpace(string(b)))
		}
	}
	return vendor, device, nil
}

// wantsBusReset reports whether the function is one of busResetIDs and not
// refused. Every other function (NVSwitches included) keeps its methods.
func (s Sysfs) wantsBusReset(addr string) (bool, error) {
	vendor, device, err := s.pciID(addr)
	if err != nil {
		return false, err
	}
	return !busResetRefused(vendor, device) && busResetIDs[fmt.Sprintf("%04x:%04x", vendor, device)], nil
}

// preferBusReset puts "bus" first in the function's reset_method and keeps
// the kernel's other methods after it, so FLR stays the fallback. Only for
// wantsBusReset, and only when the kernel lists "bus" (it does when the
// function is alone on its bus). Kernels without reset_method (before 5.15)
// have nothing to set.
func (s Sysfs) preferBusReset(addr string) error {
	b, err := os.ReadFile(s.dev(addr, "reset_method"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s: read reset_method: %w", addr, err)
	}
	methods := strings.Fields(string(b))
	if len(methods) == 0 || methods[0] == "bus" || !slices.Contains(methods, "bus") {
		return nil
	}
	if want, err := s.wantsBusReset(addr); err != nil || !want {
		return err
	}
	order := strings.Join(append([]string{"bus"}, slices.DeleteFunc(methods, func(m string) bool { return m == "bus" })...), " ")
	if err := write(s.dev(addr, "reset_method"), order); err != nil {
		return fmt.Errorf("%s: set reset_method to %q: %w", addr, order, err)
	}
	return nil
}

// defaultReset gives a function preferBusReset may have changed the
// kernel's own reset methods back. Others are left alone.
func (s Sysfs) defaultReset(addr string) error {
	if _, err := os.Stat(s.dev(addr, "reset_method")); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if want, err := s.wantsBusReset(addr); err != nil || !want {
		return err
	}
	if err := write(s.dev(addr, "reset_method"), "default"); err != nil {
		return fmt.Errorf("%s: restore reset_method: %w", addr, err)
	}
	return nil
}

// Release gives one function back to the host: its default reset methods,
// clears the override and lets the kernel pick the normal driver (nvidia,
// nouveau or habanalabs, whichever is installed).
func (s Sysfs) Release(addr string) error {
	if !hw.ValidPCIAddress(addr) {
		return fmt.Errorf("bad pci address %q", addr)
	}
	if err := s.defaultReset(addr); err != nil {
		return err
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
// given vendor:device ids at boot, before the NVIDIA driver (or, for Intel
// Gaudi ids, 1da3:..., habanalabs) loads. Used only when Runcrate takes every
// GPU on the host.
func ModprobeConf(ids []string) string {
	var b strings.Builder
	b.WriteString("# Written by runcrate-host. Removed by `runcrate-host uninstall`.\n")
	fmt.Fprintf(&b, "options vfio-pci ids=%s disable_vga=1\n", strings.Join(ids, ","))
	for _, m := range []string{"nvidia", "nvidia_drm", "nvidia_modeset", "nouveau", "snd_hda_intel", "xhci_pci"} {
		fmt.Fprintf(&b, "softdep %s pre: vfio-pci\n", m)
	}
	if slices.ContainsFunc(ids, func(id string) bool { return strings.HasPrefix(id, "1da3:") }) {
		b.WriteString("softdep habanalabs pre: vfio-pci\n")
	}
	return b.String()
}
