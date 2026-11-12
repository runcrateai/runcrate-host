package vm

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/runcrateai/runcrate-host/internal/hw"
)

func goldenQemuSpec() QemuSpec {
	return QemuSpec{
		Name: "vm-a", Dir: "/var/lib/runcrate-host/vms/vm-a", VCPUs: 8, RAMGB: 64,
		Disk: "/d/disk.qcow2", Base: "/d/images/v1.qcow2", Seed: "/d/seed.iso",
		Tap: "rc-a", MAC: "52:54:00:00:00:01",
		Firmware:    Firmware{Code: "/usr/share/OVMF/OVMF_CODE_4M.fd", Vars: "/usr/share/OVMF/OVMF_VARS_4M.fd"},
		PassThrough: []string{"0000:41:00.0", "0000:41:00.1"}, MMIO64MB: 65536,
	}
}

// qemuGolden is the command line of goldenQemuSpec as it was before Gaudi
// existed (the NVIDIA VM's arguments must stay exactly this).
var qemuGolden = []string{
	"-name", "guest=vm-a,debug-threads=on",
	"-machine", "q35,accel=kvm,kernel-irqchip=on,vmport=off,dump-guest-core=off,mem-merge=off",
	"-cpu", "host,host-phys-bits=on,-vmx,-svm",
	"-smp", "8,sockets=1,cores=8,threads=1",
	"-m", "64G",
	"-nodefaults", "-no-user-config",
	"-display", "none",
	"-vga", "none",
	"-sandbox", "on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny",
	"-msg", "timestamp=on",
	"-rtc", "base=utc",
	"-drive", "if=pflash,format=raw,unit=0,readonly=on,file=/usr/share/OVMF/OVMF_CODE_4M.fd",
	"-drive", "if=pflash,format=raw,unit=1,file=/var/lib/runcrate-host/vms/vm-a/OVMF_VARS.fd",
	"-global", "q35-pcihost.pci-hole64-size=65536M",
	"-fw_cfg", "name=opt/ovmf/X-PciMmio64Mb,string=65536",
	"-blockdev", "driver=file,filename=/d/images/v1.qcow2,node-name=base-file,read-only=on",
	"-blockdev", "driver=qcow2,file=base-file,node-name=base,read-only=on,backing=",
	"-blockdev", "driver=file,filename=/d/disk.qcow2,node-name=disk-file,cache.direct=on,aio=threads,discard=unmap",
	"-blockdev", "driver=qcow2,file=disk-file,node-name=disk0,backing=base,discard=unmap,detect-zeroes=unmap",
	"-device", "virtio-blk-pci,drive=disk0,bootindex=1",
	"-blockdev", "driver=file,filename=/d/seed.iso,node-name=seed-file,read-only=on",
	"-blockdev", "driver=raw,file=seed-file,node-name=seed0,read-only=on",
	"-device", "virtio-blk-pci,drive=seed0",
	"-netdev", "tap,id=net0,ifname=rc-a,script=no,downscript=no,vhost=on",
	"-device", "virtio-net-pci,netdev=net0,mac=52:54:00:00:00:01",
	"-device", "virtio-rng-pci",
	"-chardev", "ringbuf,id=serial0,size=1048576",
	"-serial", "chardev:serial0",
	"-qmp", "unix:/var/lib/runcrate-host/vms/vm-a/qmp.sock,server=on,wait=off",
	"-device", "pcie-root-port,id=rp0,chassis=1",
	"-device", "vfio-pci,host=0000:41:00.0,bus=rp0",
	"-device", "pcie-root-port,id=rp1,chassis=2",
	"-device", "vfio-pci,host=0000:41:00.1,bus=rp1",
}

func TestQemuArgsNvidiaUnchanged(t *testing.T) {
	for _, v := range []hw.Vendor{hw.VendorNvidia, ""} {
		s := goldenQemuSpec()
		s.ForVendor(v)
		if got := s.Args(); strings.Join(got, "\n") != strings.Join(qemuGolden, "\n") {
			t.Fatalf("vendor %q: qemu arguments changed:\n%s", v, strings.Join(got, "\n"))
		}
	}
}

// Gaudi: the guest kernel arguments as an SMBIOS OEM string (commas doubled
// for QEMU) and a 64-bit window for 8 x 256 GiB, and nothing else differs.
func TestQemuArgsGaudi(t *testing.T) {
	s := goldenQemuSpec()
	s.PassThrough = nil
	for i := range 8 {
		s.PassThrough = append(s.PassThrough, "0000:"+string(rune('1'+i))+"9:00.0")
	}
	base := s.Args()
	s.ForVendor(hw.VendorHabana)
	got := s.Args()
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "-smbios type=11,value=runcrate.kernel_args=pci=nocrs,,realloc,,assign-busses -device") {
		t.Fatalf("no kernel arguments in the command line:\n%s", joined)
	}
	if s.MMIO64MB != 2<<20 || !strings.Contains(joined, "q35-pcihost.pci-hole64-size=2097152M") || !strings.Contains(joined, "string=2097152") {
		t.Fatalf("64-bit window = %d MB:\n%s", s.MMIO64MB, joined)
	}
	// Taking out those three changes gives the NVIDIA arguments back.
	var rest []string
	for i := 0; i < len(got); i++ {
		switch {
		case got[i] == "-smbios":
			i++
		case strings.HasPrefix(got[i], "q35-pcihost.pci-hole64-size=") || strings.HasPrefix(got[i], "name=opt/ovmf/X-PciMmio64Mb"):
		default:
			rest = append(rest, got[i])
		}
	}
	var baseRest []string
	for _, a := range base {
		if !strings.HasPrefix(a, "q35-pcihost.pci-hole64-size=") && !strings.HasPrefix(a, "name=opt/ovmf/X-PciMmio64Mb") {
			baseRest = append(baseRest, a)
		}
	}
	if strings.Join(rest, "\n") != strings.Join(baseRest, "\n") {
		t.Fatalf("Gaudi changed more than the kernel arguments and the window:\n%s", strings.Join(rest, "\n"))
	}
}

func TestGaudiWindow(t *testing.T) {
	for n, want := range map[int]int{0: 32 << 10, 1: 256 << 10, 3: 1 << 20, 8: 2 << 20} {
		if got := gaudiMMIO64MB(n); got != want {
			t.Errorf("gaudiMMIO64MB(%d) = %d, want %d", n, got, want)
		}
	}
	// A window the host's own BARs already need is never made smaller.
	s := goldenQemuSpec()
	s.MMIO64MB = 4 << 20
	s.ForVendor(hw.VendorHabana)
	if s.MMIO64MB != 4<<20 {
		t.Fatalf("window shrank to %d", s.MMIO64MB)
	}
}

// A VM's vendor comes from its GPU records: a Gaudi VM gets the kernel
// arguments, an NVIDIA VM on the same host doesn't, and one VM can't mix.
func TestMixedHostVMs(t *testing.T) {
	f := newFixture(t)
	f.m.GPUs[1].Vendor = hw.VendorHabana
	f.m.GPUs[0].Vendor = hw.VendorNvidia
	f.prepTap(t, "vm-n")
	f.prepTap(t, "vm-g")
	ctx := context.Background()
	if _, err := f.m.Create(ctx, spec("vm-n", 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Create(ctx, spec("vm-g", 1)); err != nil {
		t.Fatal(err)
	}
	var nvidiaRun, gaudiRun string
	for _, c := range f.host.calls {
		switch {
		case strings.HasPrefix(c, "systemd-run") && strings.Contains(c, "host=0000:41:00.0"):
			nvidiaRun = c
		case strings.HasPrefix(c, "systemd-run") && strings.Contains(c, "host=0000:42:00.0"):
			gaudiRun = c
		}
	}
	if nvidiaRun == "" || strings.Contains(nvidiaRun, "smbios") {
		t.Fatalf("the NVIDIA VM must get no kernel arguments:\n%s", nvidiaRun)
	}
	if !strings.Contains(gaudiRun, "-smbios type=11,value=runcrate.kernel_args=pci=nocrs,,realloc,,assign-busses") || !strings.Contains(gaudiRun, "pci-hole64-size=262144M") {
		t.Fatalf("the Gaudi VM lacks its kernel arguments or window:\n%s", gaudiRun)
	}
	if v := f.m.GPUVendor(1); v != hw.VendorHabana {
		t.Fatalf("GPUVendor(1) = %q", v)
	}
	if v := f.m.GPUVendor(9); v != "" {
		t.Fatalf("GPUVendor of an unknown GPU = %q", v)
	}
	if _, err := f.m.Create(ctx, spec("vm-x", 0, 1)); err == nil || !strings.Contains(err.Error(), "one vendor") {
		t.Fatalf("a VM with NVIDIA and Gaudi GPUs: %v", err)
	}
}

// After a Gaudi VM, the scrub VM runs the Gaudi script and its result frees the GPU.
func TestGaudiScrubRunsTheGaudiScript(t *testing.T) {
	f := newFixture(t)
	f.m.GPUs[1].Vendor = hw.VendorHabana
	f.m.Scrub = true
	f.answerEphemeral(`{"ok":true,"wiped":false,"gpus":[{"uuid":"","ok":true}]}`)
	var mu sync.Mutex
	var scrubScript string
	inner := f.host.onStart
	f.host.onStart = func(id string) {
		f.m.mu.Lock()
		if r, ok := f.m.vms[id]; ok && strings.HasPrefix(id, "scrub-") {
			mu.Lock()
			scrubScript = r.CloudInit
			mu.Unlock()
		}
		f.m.mu.Unlock()
		inner(id)
	}
	for _, id := range []string{"vm-a", "vm-b", "scrub-vm-a"} {
		f.prepTap(t, id)
	}
	ctx := context.Background()
	if _, err := f.m.Create(ctx, spec("vm-a", 1)); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Destroy(ctx, "vm-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Create(ctx, spec("vm-b", 1)); err != nil {
		t.Fatal(err)
	}
	f.m.Wait()
	mu.Lock()
	defer mu.Unlock()
	if scrubScript != GaudiScrubScript {
		t.Fatalf("the scrub VM ran:\n%s", scrubScript)
	}
	if started := f.host.startedIDs(); len(started) != 3 || !strings.HasPrefix(started[1], "scrub-") || started[2] != "vm-b" {
		t.Fatalf("start order = %v, want vm-a, a scrub, then vm-b", started)
	}
}

func TestScriptsByVendor(t *testing.T) {
	tg := IsolationTargets{HostIPs: []string{"192.0.2.10"}}
	for _, v := range []hw.Vendor{hw.VendorNvidia, ""} {
		if ScrubScriptFor(v) != ScrubScript {
			t.Errorf("vendor %q: the NVIDIA scrub script changed", v)
		}
		if BenchScriptFor(v, tg, true) != BenchScript(tg) {
			t.Errorf("vendor %q: the NVIDIA benchmark script changed", v)
		}
		if got := BenchScriptFor(v, tg, false); got != ephemeralPrelude+benchGPUs || strings.Contains(got, "iso.txt\"; :") {
			t.Errorf("vendor %q: the NVIDIA script without isolation checks is wrong", v)
		}
	}
	if ScrubScriptFor(hw.VendorHabana) != GaudiScrubScript {
		t.Error("Gaudi scrub script not chosen")
	}
	with, without := BenchScriptFor(hw.VendorHabana, tg, true), BenchScriptFor(hw.VendorHabana, tg, false)
	if !strings.Contains(with, "closed host_ip") || strings.Contains(without, "closed host_ip") || !strings.Contains(without, "gaudi_pci") {
		t.Errorf("Gaudi benchmark script: isolation checks in %v / %v", strings.Contains(with, "closed host_ip"), strings.Contains(without, "closed host_ip"))
	}
	// No NVIDIA tool and no CUDA in a Gaudi VM, and no Intel download: hl-smi
	// is used only when the customer's own stack put it there.
	for name, s := range map[string]string{"scrub": GaudiScrubScript, "bench": with} {
		// "nvidia_smi" is the result's evidence field, not a tool.
		bare := strings.ReplaceAll(s, `"nvidia_smi"`, "")
		for _, bad := range []string{"nvidia", "rc-gpubench", "CUDA", "apt", "pip ", "wget"} {
			if strings.Contains(bare, bad) {
				t.Errorf("Gaudi %s script contains %q", name, bad)
			}
		}
		if !strings.Contains(s, "hl-smi -L") || !strings.Contains(s, "0x1da3") {
			t.Errorf("Gaudi %s script doesn't count with hl-smi -L and the 1da3 PCI functions", name)
		}
	}
}

// The scripts are valid bash.
func TestGaudiScriptsParse(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	for name, s := range map[string]string{
		"scrub":        GaudiScrubScript,
		"bench":        BenchScriptFor(hw.VendorHabana, IsolationTargets{}, true),
		"bench-no-iso": BenchScriptFor(hw.VendorHabana, IsolationTargets{}, false),
	} {
		cmd := exec.Command(bash, "-n")
		cmd.Stdin = strings.NewReader(s)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s script: %v: %s", name, err, out)
		}
	}
}
