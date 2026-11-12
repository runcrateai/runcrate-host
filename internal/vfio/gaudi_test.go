package vfio

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// gaudiHabana is the driver Gaudi modules sit on while the host owns them.
const gaudiHabana = "habanalabs"

// gaudiKernel is a fake sysfs with Gaudi 3 OAM modules (1da3:1060) and a
// goroutine playing the kernel's part: a driver's unbind file detaches the
// function written to it, and drivers_probe attaches an unbound function to
// vfio-pci when its driver_override says so, else to habanalabs (the driver a
// real probe finds for it). Every detach and attach is logged in order.
type gaudiKernel struct {
	Sysfs
	mu  sync.Mutex
	log []string
}

// newGaudiKernel makes one module per entry of onDriver, each at 0000:10+i:00.0
// on that driver, and returns the kernel and the addresses.
func newGaudiKernel(t *testing.T, onDriver ...string) (*gaudiKernel, []string) {
	t.Helper()
	root := t.TempDir()
	write := func(p, c string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range append([]string{gaudiHabana, Driver}, onDriver...) {
		write(filepath.Join(root, "sys/bus/pci/drivers", d, "unbind"), "")
	}
	write(filepath.Join(root, "sys/bus/pci/drivers_probe"), "")
	var addrs []string
	for i, d := range onDriver {
		a := fmt.Sprintf("0000:%02x:00.0", 0x10+i)
		addrs = append(addrs, a)
		dev := filepath.Join(root, "sys/bus/pci/devices", a)
		write(filepath.Join(dev, "vendor"), "0x1da3\n")
		write(filepath.Join(dev, "device"), "0x1060\n")
		write(filepath.Join(dev, "driver_override"), "")
		write(filepath.Join(dev, "reset_method"), "flr bus\n")
		if err := os.Symlink(filepath.Join(root, "sys/bus/pci/drivers", d), filepath.Join(dev, "driver")); err != nil {
			t.Fatal(err)
		}
	}
	k := &gaudiKernel{Sysfs: Sysfs{Root: root}}
	stop, done := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(stop); <-done })
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
				k.tick()
			}
		}
	}()
	return k, addrs
}

func (k *gaudiKernel) note(format string, args ...any) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.log = append(k.log, fmt.Sprintf(format, args...))
}

// events is what the kernel did so far, in order.
func (k *gaudiKernel) events() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.log)
}

func (k *gaudiKernel) tick() {
	drivers := filepath.Join(k.Root, "sys/bus/pci/drivers")
	devs := filepath.Join(k.Root, "sys/bus/pci/devices")
	ents, _ := os.ReadDir(drivers)
	for _, e := range ents {
		p := filepath.Join(drivers, e.Name(), "unbind")
		b, _ := os.ReadFile(p)
		a := strings.TrimSpace(string(b))
		if a == "" {
			continue
		}
		link := filepath.Join(devs, a, "driver")
		if l, err := os.Readlink(link); err == nil && filepath.Base(l) == e.Name() {
			k.note("unbind %s %s", e.Name(), a)
			_ = os.Remove(link)
		}
		_ = os.WriteFile(p, nil, 0o644)
	}
	probe := filepath.Join(k.Root, "sys/bus/pci/drivers_probe")
	b, _ := os.ReadFile(probe)
	a := strings.TrimSpace(string(b))
	if a == "" {
		return
	}
	link := filepath.Join(devs, a, "driver")
	if _, err := os.Readlink(link); err == nil {
		return // still bound: the probe waits for the unbind
	}
	drv := gaudiHabana
	if ov, _ := os.ReadFile(filepath.Join(devs, a, "driver_override")); strings.TrimSpace(string(ov)) == Driver {
		drv = Driver
	}
	k.note("bind %s %s", drv, a)
	// The probe is consumed before the driver shows, so a caller that sees the
	// driver can write its next probe without it being wiped.
	_ = os.WriteFile(probe, nil, 0o644)
	_ = os.Symlink(filepath.Join(drivers, drv), link)
}

func (k *gaudiKernel) file(a, name string) string {
	b, _ := os.ReadFile(k.dev(a, name))
	return string(b)
}

// Eight Gaudi 3 OAM modules on habanalabs go to vfio-pci, each unbound from
// habanalabs first, and come back to habanalabs on Release.
func TestGaudiBindAndRestore(t *testing.T) {
	var on []string
	for range 8 {
		on = append(on, gaudiHabana)
	}
	k, addrs := newGaudiKernel(t, on...)
	for _, a := range addrs {
		if err := k.Bind(a); err != nil {
			t.Fatalf("Bind %s: %v", a, err)
		}
	}
	if !k.Bound(addrs...) {
		t.Fatal("not every module is on vfio-pci")
	}
	var want []string
	for _, a := range addrs {
		want = append(want, "unbind habanalabs "+a, "bind vfio-pci "+a)
		if got := k.file(a, "driver_override"); got != Driver {
			t.Errorf("%s: driver_override %q", a, got)
		}
		// Gaudi's reset method is unknown: the kernel's own list stays as it is.
		if got := k.file(a, "reset_method"); got != "flr bus\n" {
			t.Errorf("%s: reset_method %q, want it untouched", a, got)
		}
	}
	if got := k.events(); !slices.Equal(got, want) {
		t.Fatalf("kernel events on bind:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Binding again is a no-op.
	if err := k.Bind(addrs[0]); err != nil {
		t.Fatalf("second Bind: %v", err)
	}
	if got := k.events(); !slices.Equal(got, want) {
		t.Fatalf("a second Bind touched the kernel: %v", got)
	}
	for _, a := range addrs {
		if err := k.Release(a); err != nil {
			t.Fatalf("Release %s: %v", a, err)
		}
		// The real kernel finishes the probe inside the write; the fake does it
		// within a tick, and must not see the next module's writes before that.
		for range 500 {
			if k.driver(a) == gaudiHabana {
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		want = append(want, "unbind vfio-pci "+a, "bind habanalabs "+a)
	}
	if got := k.events(); !slices.Equal(got, want) {
		t.Fatalf("kernel events after release:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, a := range addrs {
		if d := k.driver(a); d != gaudiHabana {
			t.Errorf("%s: driver %q after Release, want %s", a, d, gaudiHabana)
		}
		if got := k.file(a, "driver_override"); got != "\n" {
			t.Errorf("%s: driver_override %q after Release", a, got)
		}
		if got := k.file(a, "reset_method"); got != "flr bus\n" {
			t.Errorf("%s: reset_method %q after Release, want it untouched", a, got)
		}
	}
	if k.Bound(addrs...) {
		t.Fatal("modules still on vfio-pci after Release")
	}
}

// Whatever driver holds the module is unbound first, not only habanalabs.
func TestGaudiBindUnbindsAnyDriver(t *testing.T) {
	k, addrs := newGaudiKernel(t, "some_other_driver")
	if err := k.Bind(addrs[0]); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	want := []string{"unbind some_other_driver " + addrs[0], "bind vfio-pci " + addrs[0]}
	if got := k.events(); !slices.Equal(got, want) {
		t.Fatalf("kernel events: %v, want %v", got, want)
	}
}

// A module whose unbind is refused (something holds it) is reported with its
// driver, and is never bound.
func TestGaudiBindReportsBusyDriver(t *testing.T) {
	k, addrs := newGaudiKernel(t, gaudiHabana)
	if err := os.Remove(filepath.Join(k.Root, "sys/bus/pci/drivers", gaudiHabana, "unbind")); err != nil {
		t.Fatal(err)
	}
	err := k.Bind(addrs[0])
	if err == nil || !strings.Contains(err.Error(), "unbind from "+gaudiHabana) {
		t.Fatalf("Bind with a failing unbind: %v", err)
	}
	if k.Bound(addrs[0]) {
		t.Fatal("bound despite the failed unbind")
	}
}

// busResetIDs has no Gaudi entry until one is proven on the box, and the hook
// works the way it does for NVIDIA when one is added.
func TestGaudiBusResetHookIsEmptyAndWorks(t *testing.T) {
	for id := range busResetIDs {
		if strings.HasPrefix(id, "1da3:") {
			t.Fatalf("busResetIDs lists %s: add Gaudi only with a run on the box behind it", id)
		}
	}
	s, path := fakeDev(t, "0x1da3", "0x1060", "flr bus\n", true)
	if err := s.Bind(addr); err != nil {
		t.Fatal(err)
	}
	if got := readMethod(t, path); got != "flr bus\n" {
		t.Fatalf("Gaudi reset_method %q, want it untouched", got)
	}
	busResetIDs["1da3:1060"] = true
	defer delete(busResetIDs, "1da3:1060")
	if err := s.Bind(addr); err != nil {
		t.Fatal(err)
	}
	if got := readMethod(t, path); got != "bus flr" {
		t.Fatalf("with a 1da3:1060 entry reset_method %q, want %q", got, "bus flr")
	}
}

// An NVIDIA host's boot file is what it was; Gaudi ids add the habanalabs softdep.
func TestModprobeConfHabana(t *testing.T) {
	nv := ModprobeConf([]string{"10de:2330"})
	want := "# Written by runcrate-host. Removed by `runcrate-host uninstall`.\n" +
		"options vfio-pci ids=10de:2330 disable_vga=1\n" +
		"softdep nvidia pre: vfio-pci\nsoftdep nvidia_drm pre: vfio-pci\nsoftdep nvidia_modeset pre: vfio-pci\n" +
		"softdep nouveau pre: vfio-pci\nsoftdep snd_hda_intel pre: vfio-pci\nsoftdep xhci_pci pre: vfio-pci\n"
	if nv != want {
		t.Fatalf("NVIDIA modprobe file changed:\n%s", nv)
	}
	for _, ids := range [][]string{{"1da3:1060"}, {"10de:2330", "1da3:1060", "1da3:1063"}} {
		got := ModprobeConf(ids)
		if !strings.Contains(got, "options vfio-pci ids="+strings.Join(ids, ",")+" ") || !strings.HasSuffix(got, "softdep habanalabs pre: vfio-pci\n") || !strings.Contains(got, "softdep nvidia pre: vfio-pci\n") {
			t.Fatalf("ids %v:\n%s", ids, got)
		}
	}
}
