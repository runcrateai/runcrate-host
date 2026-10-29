package vm

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/runcrateai/runcrate-host/internal/hw"
	"github.com/runcrateai/runcrate-host/internal/metadata"
	"github.com/runcrateai/runcrate-host/internal/vfio"
)

// fakeHost fakes systemd, qemu-img, genisoimage and ip.
type fakeHost struct {
	mu      sync.Mutex
	units   map[string]string
	calls   []string
	started []string // vm ids in start order
	onStart func(id string)
	failRun bool
}

func (f *fakeHost) RunInput(ctx context.Context, _ []byte, name string, args ...string) ([]byte, error) {
	return f.Run(ctx, name, args...)
}

func (f *fakeHost) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	f.mu.Unlock()
	switch name {
	case "systemd-run":
		if f.failRun {
			return nil, fmt.Errorf("systemd-run failed")
		}
		u := strings.TrimPrefix(args[0], "--unit=")
		id := strings.TrimSuffix(strings.TrimPrefix(u, "runcrate-vm-"), ".service")
		f.mu.Lock()
		f.units[u] = "active"
		f.started = append(f.started, id)
		cb := f.onStart
		f.mu.Unlock()
		if cb != nil {
			go cb(id)
		}
	case "systemctl":
		f.mu.Lock()
		defer f.mu.Unlock()
		switch args[0] {
		case "show":
			if s, ok := f.units[args[len(args)-1]]; ok {
				return []byte(s + "\n"), nil
			}
			return []byte("inactive\n"), nil
		case "stop":
			f.units[args[1]] = "inactive"
		}
	case "qemu-img":
		for i, a := range args {
			if a == "-b" {
				return nil, os.WriteFile(args[i+2], []byte("qcow2 overlay"), 0o600)
			}
		}
	case "genisoimage":
		for i, a := range args {
			if a == "-output" {
				return nil, os.WriteFile(args[i+1], []byte("iso"), 0o600)
			}
		}
	}
	return nil, nil
}

func (f *fakeHost) startedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.started...)
}

type fakeImages struct{ path string }

func (f fakeImages) Local(ctx context.Context, v string) (string, string, error) {
	return f.Ensure(ctx, v)
}

func (f fakeImages) Ensure(ctx context.Context, v string) (string, string, error) {
	if v == "" {
		v = "v1"
	}
	return f.path, v, nil
}

type fixture struct {
	m    *Manager
	host *fakeHost
	root string
}

// newFixture builds a manager over a fake sysfs with two GPUs already on
// vfio-pci (each with an audio companion).
func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	for i, a := range []string{"0000:41:00.0", "0000:42:00.0"} {
		for _, fn := range []string{a, a[:len(a)-1] + "1"} {
			d := filepath.Join(root, "sys/bus/pci/devices", fn)
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
			_ = os.Symlink(filepath.Join(root, "sys/bus/pci/drivers/vfio-pci"), filepath.Join(d, "driver"))
			_ = os.Symlink(filepath.Join(root, "sys/kernel/iommu_groups", fmt.Sprint(20+i)), filepath.Join(d, "iommu_group"))
			_ = os.WriteFile(filepath.Join(d, "resource"), []byte(fmt.Sprintf("0x%x 0x%x 0x14220c\n", 0x200000000000+i*0x10000000000, 0x200000000000+i*0x10000000000+0x1fffffffff)), 0o644)
		}
	}
	gpus := []hw.GPU{
		{Index: 0, Address: "0000:41:00.0", Companions: []string{"0000:41:00.1"}},
		{Index: 1, Address: "0000:42:00.0", Companions: []string{"0000:42:00.1"}},
	}
	_, subnet, _ := net.ParseCIDR("10.254.0.0/16")
	host := &fakeHost{units: map[string]string{}}
	img := filepath.Join(root, "data/images/v1.qcow2")
	_ = os.MkdirAll(filepath.Dir(img), 0o711)
	_ = os.WriteFile(img, []byte("base"), 0o444)
	vars := filepath.Join(root, "OVMF_VARS.fd")
	_ = os.WriteFile(vars, []byte("vars"), 0o644)
	m := &Manager{
		DataDir: filepath.Join(root, "data"), Net: &Network{Subnet: subnet, Run: host, ProcRoot: root},
		Meta: metadata.New(subnet), Run: host, Sys: hw.Sys{Root: root}, VFIO: vfio.Sysfs{Root: root},
		Images: fakeImages{img}, Firmware: Firmware{Code: "/fw/CODE.fd", Vars: vars},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), GPUs: gpus,
		HostThreads: 64, HostRAMGB: 512, BootWait: time.Millisecond,
	}
	// ProcRoot: let sysctl writes land.
	_ = os.MkdirAll(filepath.Join(root, "proc/sys/net/ipv4"), 0o755)
	return &fixture{m: m, host: host, root: root}
}

func (f *fixture) prepTap(t *testing.T, id string) {
	tap := tapName(id)
	for _, d := range []string{"proc/sys/net/ipv4/conf/" + tap, "proc/sys/net/ipv6/conf/" + tap} {
		if err := os.MkdirAll(filepath.Join(f.root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// answerEphemeral makes throwaway VMs post body as their result.
func (f *fixture) answerEphemeral(body string) {
	f.host.onStart = func(id string) {
		if !strings.HasPrefix(id, "scrub-") && !strings.HasPrefix(id, "bench-") {
			return
		}
		for i := 0; i < 200; i++ {
			f.m.mu.Lock()
			r, ok := f.m.vms[id]
			ip := ""
			if ok {
				ip = r.GuestIP
			}
			f.m.mu.Unlock()
			if ip != "" {
				req := httptest.NewRequest("POST", "/runcrate/v1/ephemeral-result", strings.NewReader(body))
				req.RemoteAddr = ip + ":1234"
				f.m.Meta.Handler().ServeHTTP(httptest.NewRecorder(), req)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

func spec(id string, gpus ...int) Spec {
	return Spec{ID: id, GPUIndexes: gpus, VCPUs: 8, RAMGB: 64, DiskGB: 100, CloudInit: "#!/bin/bash\necho launch\n"}
}

func TestCreatePassesTheGPUAndIsIdempotent(t *testing.T) {
	f := newFixture(t)
	f.prepTap(t, "vm-a")
	ctx := context.Background()
	st, err := f.m.Create(ctx, spec("vm-a", 1))
	if err != nil || st != StateRunning {
		t.Fatalf("Create = %q, %v", st, err)
	}
	var run string
	for _, c := range f.host.calls {
		if strings.HasPrefix(c, "systemd-run") {
			run = c
		}
	}
	for _, want := range []string{"vfio-pci,host=0000:42:00.0", "ifname=" + tapName("vm-a"), "-m 64G", "-smp 8,", "LimitMEMLOCK=infinity", "X-PciMmio64Mb"} {
		if !strings.Contains(run, want) {
			t.Fatalf("qemu command lacks %q:\n%s", want, run)
		}
	}
	if strings.Contains(run, "0000:41:00.0") {
		t.Fatal("a GPU the VM didn't ask for was passed through")
	}
	if _, err := f.m.Create(ctx, spec("vm-a", 1)); err != nil {
		t.Fatal(err)
	}
	if n := len(f.host.startedIDs()); n != 1 {
		t.Fatalf("second create started qemu again (%d starts)", n)
	}
	if _, err := f.m.Create(ctx, spec("vm-b", 1)); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("double-booked GPU: %v", err)
	}
	list := f.m.List(ctx)
	if len(list) != 1 || list[0].VMID != "vm-a" || list[0].State != StateRunning {
		t.Fatalf("List = %+v", list)
	}
	if b, err := os.ReadFile(filepath.Join(f.m.DataDir, "vms/vm-a/seed/user-data")); err == nil {
		t.Fatalf("the plain seed (with the launch script) must not be left on disk: %s", b)
	}
}

func TestGuestPowerOffShowsStopped(t *testing.T) {
	f := newFixture(t)
	f.prepTap(t, "vm-a")
	ctx := context.Background()
	if _, err := f.m.Create(ctx, spec("vm-a", 0)); err != nil {
		t.Fatal(err)
	}
	f.host.mu.Lock()
	delete(f.host.units, unit("vm-a"))
	f.host.mu.Unlock()
	if l := f.m.List(ctx); l[0].State != StateStopped {
		t.Fatalf("state = %s", l[0].State)
	}
}

func TestDestroyWipesAndScrubsBeforeReuse(t *testing.T) {
	f := newFixture(t)
	f.m.Scrub = true
	f.answerEphemeral(`{"ok":true,"mode":"scrub","gpus":[{"uuid":"GPU-a","ok":true}]}`)
	for _, id := range []string{"vm-a", "vm-b", "scrub-vm-a"} {
		f.prepTap(t, id)
	}
	ctx := context.Background()
	if _, err := f.m.Create(ctx, spec("vm-a", 0)); err != nil {
		t.Fatal(err)
	}
	disk := filepath.Join(f.m.DataDir, "vms/vm-a/disk.qcow2")
	if _, err := os.Stat(disk); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Destroy(ctx, "vm-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.m.DataDir, "vms/vm-a")); !os.IsNotExist(err) {
		t.Fatal("vm dir still there after destroy")
	}
	if _, err := os.Stat(filepath.Join(f.m.DataDir, "vms/.wipe-vm-a")); !os.IsNotExist(err) {
		t.Fatal("wipe dir still there after destroy")
	}
	// The GPU is not free until the scrub is done; a new VM waits for it.
	if _, err := f.m.Create(ctx, spec("vm-b", 0)); err != nil {
		t.Fatal(err)
	}
	f.m.Wait()
	started := f.host.startedIDs()
	if len(started) != 3 || started[0] != "vm-a" || !strings.HasPrefix(started[1], "scrub-") || started[2] != "vm-b" {
		t.Fatalf("start order = %v, want vm-a, a scrub, then vm-b", started)
	}
	if _, err := os.Stat(f.m.scrubMarker("vm-a")); !os.IsNotExist(err) {
		t.Fatal("scrub marker left behind")
	}
}

func TestFailedScrubHoldsTheGPU(t *testing.T) {
	f := newFixture(t)
	f.m.Scrub = true
	f.answerEphemeral(`{"ok":false,"error":"12 words read back wrong","gpus":[{"uuid":"GPU-a","ok":false}]}`)
	for _, id := range []string{"vm-a", "vm-b", "scrub-vm-a"} {
		f.prepTap(t, id)
	}
	ctx := context.Background()
	if _, err := f.m.Create(ctx, spec("vm-a", 0)); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Destroy(ctx, "vm-a"); err != nil {
		t.Fatal(err)
	}
	f.m.Wait()
	if _, err := f.m.Create(ctx, spec("vm-b", 0)); err == nil || !strings.Contains(err.Error(), "held back") {
		t.Fatalf("held GPU was rented: %v", err)
	}
	if free := f.m.FreeGPUs(false); len(free) != 1 || free[0] != 1 {
		t.Fatalf("FreeGPUs(false) = %v", free)
	}
	if free := f.m.FreeGPUs(true); len(free) != 2 {
		t.Fatalf("FreeGPUs(true) = %v", free)
	}
	f.m.ClearHold([]int{0})
	if _, err := f.m.Create(ctx, spec("vm-b", 0)); err != nil {
		t.Fatal(err)
	}
}

// recImages records which of Ensure and Local each VM asked, and for what.
type recImages struct {
	path  string
	mu    sync.Mutex
	calls []string
}

func (r *recImages) Ensure(ctx context.Context, v string) (string, string, error) {
	r.mu.Lock()
	r.calls = append(r.calls, "ensure "+v)
	r.mu.Unlock()
	return fakeImages{r.path}.Ensure(ctx, v)
}

func (r *recImages) Local(ctx context.Context, v string) (string, string, error) {
	r.mu.Lock()
	r.calls = append(r.calls, "local "+v)
	r.mu.Unlock()
	return fakeImages{r.path}.Local(ctx, v)
}

func TestThrowawayVMBootsTheGivenImage(t *testing.T) {
	f := newFixture(t)
	imgs := &recImages{path: filepath.Join(f.root, "data/images/v1.qcow2")}
	f.m.Images = imgs
	f.answerEphemeral(`{"ok":true,"gpus":[]}`)
	for _, id := range []string{"bench-1", "bench-2"} {
		f.prepTap(t, id)
	}
	ctx := context.Background()
	// With a version (the benchmark's), Ensure: it downloads a missing one.
	if _, v, err := f.m.RunEphemeral(ctx, "bench-1", "", "20261101-1", []int{0}, "#!/bin/bash\n", time.Minute, nil); err != nil || v != "20261101-1" {
		t.Fatalf("RunEphemeral(20261101-1) = %q, %v", v, err)
	}
	// Without one, Local: whatever image is already here.
	if _, v, err := f.m.RunEphemeral(ctx, "bench-2", "", "", []int{0}, "#!/bin/bash\n", time.Minute, nil); err != nil || v != "v1" {
		t.Fatalf("RunEphemeral(\"\") = %q, %v", v, err)
	}
	imgs.mu.Lock()
	defer imgs.mu.Unlock()
	if len(imgs.calls) != 2 || imgs.calls[0] != "ensure 20261101-1" || imgs.calls[1] != "local " {
		t.Fatalf("image calls = %q", imgs.calls)
	}
}

func TestNVSwitchNodeIsRentedWhole(t *testing.T) {
	f := newFixture(t)
	f.m.NVSwitches = []string{"0000:a0:00.0"}
	d := filepath.Join(f.root, "sys/bus/pci/devices/0000:a0:00.0")
	_ = os.MkdirAll(d, 0o755)
	_ = os.Symlink(filepath.Join(f.root, "sys/bus/pci/drivers/vfio-pci"), filepath.Join(d, "driver"))
	_ = os.Symlink(filepath.Join(f.root, "sys/kernel/iommu_groups/90"), filepath.Join(d, "iommu_group"))
	f.prepTap(t, "vm-a")
	ctx := context.Background()
	if _, err := f.m.Create(ctx, spec("vm-a", 0)); err == nil {
		t.Fatal("partial VM on an NVSwitch node was accepted")
	}
	if _, err := f.m.Create(ctx, spec("vm-a", 0, 1)); err != nil {
		t.Fatal(err)
	}
	f.m.mu.Lock()
	pt := strings.Join(f.m.vms["vm-a"].PassThrough, ",")
	f.m.mu.Unlock()
	if pt != "0000:41:00.0,0000:42:00.0,0000:a0:00.0" {
		t.Fatalf("pass-through = %s", pt)
	}
}

func TestCapacityAndBadInput(t *testing.T) {
	f := newFixture(t)
	f.m.HostThreads = 16
	ctx := context.Background()
	s := spec("vm-a", 0)
	s.VCPUs = 15
	if _, err := f.m.Create(ctx, s); err == nil || !strings.Contains(err.Error(), "vcpus") {
		t.Fatalf("over-committed vcpus: %v", err)
	}
	if _, err := f.m.Create(ctx, spec("vm-a", 7)); err == nil {
		t.Fatal("unknown GPU index accepted")
	}
	if _, err := f.m.Create(ctx, spec("a/../../b", 0)); err == nil {
		t.Fatal("path-like vm id accepted")
	}
	if len(f.host.startedIDs()) != 0 {
		t.Fatal("refused specs started qemu")
	}
}

func TestQemuFailureCleansUp(t *testing.T) {
	f := newFixture(t)
	f.host.failRun = true
	f.prepTap(t, "vm-a")
	ctx := context.Background()
	if _, err := f.m.Create(ctx, spec("vm-a", 0)); err == nil {
		t.Fatal("want error")
	}
	if _, err := os.Stat(filepath.Join(f.m.DataDir, "vms/vm-a")); !os.IsNotExist(err) {
		t.Fatal("failed vm left its disk behind")
	}
	if free := f.m.FreeGPUs(false); len(free) != 2 {
		t.Fatalf("GPU not released after a failed create: %v", free)
	}
}

func TestLoadResumesRunningAndDropsHalfMade(t *testing.T) {
	f := newFixture(t)
	f.prepTap(t, "vm-run")
	f.prepTap(t, "vm-half")
	ctx := context.Background()
	if _, err := f.m.Create(ctx, spec("vm-run", 0)); err != nil {
		t.Fatal(err)
	}
	// A create that died part-way.
	half := &Record{Spec: spec("vm-half", 1), Slot: 9, Tap: tapName("vm-half"), State: StateCreating, Want: true}
	_ = os.MkdirAll(f.m.dir("vm-half"), 0o700)
	if err := f.m.save(half); err != nil {
		t.Fatal(err)
	}
	// The VM's own directory belongs to its QEMU: a record planted there
	// (asking to tear down the host's bridge) must not be what Load trusts.
	forged := `{"id":"vm-run","tap":"br0","want":false,"state":"running"}`
	if err := os.WriteFile(filepath.Join(f.m.dir("vm-run"), "vm.json"), []byte(forged), 0o600); err != nil {
		t.Fatal(err)
	}
	// Host reboot: no units, fresh manager.
	host := &fakeHost{units: map[string]string{}}
	m2 := &Manager{
		DataDir: f.m.DataDir, Net: &Network{Subnet: f.m.Net.Subnet, Run: host, ProcRoot: f.root},
		Meta: metadata.New(f.m.Net.Subnet), Run: host, Sys: f.m.Sys, VFIO: f.m.VFIO, Images: f.m.Images,
		Firmware: f.m.Firmware, Log: f.m.Log, GPUs: f.m.GPUs, HostThreads: 64, HostRAMGB: 512, BootWait: time.Millisecond,
	}
	// Uninstall reads the records without resuming anything; it must see both.
	m3 := &Manager{DataDir: f.m.DataDir, Net: m2.Net, Meta: m2.Meta, Run: host, Sys: f.m.Sys, VFIO: f.m.VFIO, Log: f.m.Log}
	if err := m3.Load(ctx, false); err != nil {
		t.Fatal(err)
	}
	if ids := m3.IDs(); len(ids) != 2 || ids[0] != "vm-half" || ids[1] != "vm-run" {
		t.Fatalf("Load without resume found %v, want [vm-half vm-run]", ids)
	}
	if err := m2.Load(ctx, true); err != nil {
		t.Fatal(err)
	}
	if got := host.startedIDs(); len(got) != 1 || got[0] != "vm-run" {
		t.Fatalf("restarted %v, want [vm-run]", got)
	}
	l := m2.List(ctx)
	if len(l) != 1 || l[0].VMID != "vm-run" || l[0].State != StateRunning {
		t.Fatalf("List after reboot = %+v", l)
	}
}

func TestUserDataKeepsTheLaunchScriptVerbatim(t *testing.T) {
	script := "#!/bin/bash\nset -e\necho 'runcrate launch'\n"
	ud, err := UserData(script)
	if err != nil || string(ud) != script {
		t.Fatalf("UserData = %q, %v", ud, err)
	}
	if _, err := UserData("echo no shebang"); err == nil {
		t.Fatal("a launch script without #! was accepted")
	}
}

func TestRulesetIsolatesVMs(t *testing.T) {
	_, subnet, _ := net.ParseCIDR("10.254.0.0/16")
	n := &Network{Subnet: subnet}
	rs := n.Ruleset([]string{"203.0.113.0/26", "198.51.100.1/32"})
	for _, want := range []string{
		`iifname "rc-*" oifname "rc-*" drop`, // VM to VM
		`iifname "rc-*" ip daddr @bogons4 drop`,
		`iifname "rc-*" ip daddr @partner4 drop`,
		`elements = { 203.0.113.0/26, 198.51.100.1/32 }`,
		`fib saddr . iif oif missing drop`,
		`ip saddr != 10.254.0.0/16 drop`,
		`iifname "rc-*" drop`, // VM to host
		`oifname "rc-*" ct state established,related accept`,
		`oifname "rc-*" drop`, // nothing new into a VM
		`redirect to :980`,    // metadata
		`"192.168.0.0/16"`, `10.0.0.0/8`, `172.16.0.0/12`, `169.254.0.0/16`, `100.64.0.0/10`,
		`ip saddr 10.254.0.0/16 oifname != "rc-*" masquerade`,
	} {
		if !strings.Contains(rs, strings.Trim(want, `"`)) {
			t.Fatalf("ruleset lacks %q:\n%s", want, rs)
		}
	}
	if strings.Contains(rs, "\t\tdrop\n\t}\n\tchain postrouting") {
		t.Fatal("forward guard present although forwarding was on")
	}
	n.ForwardWasOff = true
	if !strings.Contains(n.Ruleset(nil), "\t\tdrop\n\t}\n\tchain postrouting") {
		t.Fatal("forward guard missing although forwarding was off before install")
	}
	h, g, _ := n.Slot(3)
	if h.String() != "10.254.0.13" || g.String() != "10.254.0.14" {
		t.Fatalf("slot 3 = %s %s", h, g)
	}
	if _, _, err := n.Slot(n.MaxSlots()); err == nil {
		t.Fatal("slot past the subnet accepted")
	}
}

func TestMMIOWindowFitsBARs(t *testing.T) {
	f := newFixture(t)
	// Each fake GPU has a 128 GiB BAR (and its companion another).
	mb := MMIO64For(f.root, []string{"0000:41:00.0", "0000:42:00.0"})
	if mb < 2*2*128<<10 || mb&(mb-1) != 0 {
		t.Fatalf("mmio = %d MB", mb)
	}
	if MMIO64For(f.root, nil) != 32<<10 {
		t.Fatal("minimum window is 32 GiB")
	}
}
