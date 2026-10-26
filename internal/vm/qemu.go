package vm

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Firmware is the OVMF pair. Secure Boot stays off: the image's NVIDIA
// modules are built by DKMS and unsigned.
type Firmware struct{ Code, Vars string }

// FindFirmware looks for OVMF where Ubuntu and Debian put it.
func FindFirmware() (Firmware, error) {
	for _, p := range []Firmware{
		{"/usr/share/OVMF/OVMF_CODE_4M.fd", "/usr/share/OVMF/OVMF_VARS_4M.fd"},
		{"/usr/share/OVMF/OVMF_CODE.fd", "/usr/share/OVMF/OVMF_VARS.fd"},
		{"/usr/share/qemu/OVMF_CODE.fd", "/usr/share/qemu/OVMF_VARS.fd"},
	} {
		if _, err := os.Stat(p.Code); err == nil {
			if _, err := os.Stat(p.Vars); err == nil {
				return p, nil
			}
		}
	}
	return Firmware{}, fmt.Errorf("OVMF firmware not found (install the ovmf package)")
}

// QemuSpec is everything needed to start one VM.
type QemuSpec struct {
	Name  string
	Dir   string
	VCPUs int
	RAMGB int
	Disk  string
	// Base is the read-only image under Disk, named explicitly so the
	// overlay header's backing reference is never followed.
	Base     string
	Seed     string
	Tap      string
	MAC      string
	Firmware Firmware
	// PassThrough are the PCI functions handed to the VM (GPUs, then any
	// NVSwitches), one PCIe root port each.
	PassThrough []string
	// MMIO64MB is the 64-bit MMIO window: big enough for every BAR.
	MMIO64MB int
}

// Args is the qemu-system-x86_64 command line.
func (s QemuSpec) Args() []string {
	a := []string{
		"-name", "guest=" + s.Name + ",debug-threads=on",
		"-machine", "q35,accel=kvm,kernel-irqchip=on,vmport=off,dump-guest-core=off,mem-merge=off",
		// No nested virtualisation for guests.
		"-cpu", "host,host-phys-bits=on,-vmx,-svm",
		"-smp", fmt.Sprintf("%d,sockets=1,cores=%d,threads=1", s.VCPUs, s.VCPUs),
		"-m", fmt.Sprintf("%dG", s.RAMGB),
		"-nodefaults", "-no-user-config",
		"-display", "none", "-vga", "none",
		"-sandbox", "on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny",
		"-msg", "timestamp=on",
		"-rtc", "base=utc",
		"-drive", "if=pflash,format=raw,unit=0,readonly=on,file=" + s.Firmware.Code,
		"-drive", "if=pflash,format=raw,unit=1,file=" + filepath.Join(s.Dir, "OVMF_VARS.fd"),
		"-global", fmt.Sprintf("q35-pcihost.pci-hole64-size=%dM", s.MMIO64MB),
		"-fw_cfg", fmt.Sprintf("name=opt/ovmf/X-PciMmio64Mb,string=%d", s.MMIO64MB),
		"-blockdev", "driver=file,filename=" + s.Base + ",node-name=base-file,read-only=on",
		"-blockdev", "driver=qcow2,file=base-file,node-name=base,read-only=on,backing=",
		"-blockdev", "driver=file,filename=" + s.Disk + ",node-name=disk-file,cache.direct=on,aio=threads,discard=unmap",
		"-blockdev", "driver=qcow2,file=disk-file,node-name=disk0,backing=base,discard=unmap,detect-zeroes=unmap",
		"-device", "virtio-blk-pci,drive=disk0,bootindex=1",
		"-blockdev", "driver=file,filename=" + s.Seed + ",node-name=seed-file,read-only=on",
		"-blockdev", "driver=raw,file=seed-file,node-name=seed0,read-only=on",
		"-device", "virtio-blk-pci,drive=seed0",
		"-netdev", "tap,id=net0,ifname=" + s.Tap + ",script=no,downscript=no,vhost=on",
		"-device", "virtio-net-pci,netdev=net0,mac=" + s.MAC,
		"-device", "virtio-rng-pci",
		"-chardev", "ringbuf,id=serial0,size=1048576",
		"-serial", "chardev:serial0",
		"-qmp", "unix:" + filepath.Join(s.Dir, "qmp.sock") + ",server=on,wait=off",
	}
	for i, addr := range s.PassThrough {
		rp := fmt.Sprintf("rp%d", i)
		a = append(a,
			"-device", fmt.Sprintf("pcie-root-port,id=%s,chassis=%d", rp, i+1),
			"-device", fmt.Sprintf("vfio-pci,host=%s,bus=%s", addr, rp),
		)
	}
	return a
}

// MMIO64For sizes the 64-bit MMIO window: twice the sum of the devices'
// 64-bit BARs, as a power of two, at least 32 GiB.
func MMIO64For(sysRoot string, addrs []string) int {
	var total uint64
	for _, a := range addrs {
		total += barBytes(filepath.Join(sysRoot, "sys/bus/pci/devices", a, "resource"))
	}
	mb := uint64(32 << 10)
	for mb*(1<<20) < total*2 {
		mb *= 2
	}
	return int(mb)
}

func barBytes(path string) uint64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	var sum uint64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) != 3 {
			continue
		}
		start, err1 := strconv.ParseUint(strings.TrimPrefix(fs[0], "0x"), 16, 64)
		end, err2 := strconv.ParseUint(strings.TrimPrefix(fs[1], "0x"), 16, 64)
		if err1 != nil || err2 != nil || end <= start {
			continue
		}
		sum += end - start + 1
	}
	return sum
}
