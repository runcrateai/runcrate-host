// Package hw reads the host's hardware from sysfs: NVIDIA GPUs and their
// IOMMU groups, NVSwitches, NICs, CPU, memory and disk. Every reader takes a
// root so tests can point it at a fake sysfs tree.
package hw

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// NvidiaVendor is the PCI vendor id of NVIDIA.
const NvidiaVendor = "0x10de"

// Sys is a sysfs/procfs root ("/" in production).
type Sys struct{ Root string }

// Host is the real system.
var Host = Sys{Root: "/"}

func (s Sys) p(parts ...string) string { return filepath.Join(append([]string{s.Root}, parts...)...) }

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// PCIDevice is one PCI function.
type PCIDevice struct {
	Address    string // 0000:41:00.0
	Vendor     string // 0x10de
	Device     string // 0x2684
	Class      string // 0x030000
	Driver     string // vfio-pci, nvidia, ...
	IOMMUGroup string // "" when the IOMMU is off
}

var pciAddrRe = regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$`)

// ValidPCIAddress reports whether s is a full PCI address like 0000:41:00.0.
func ValidPCIAddress(s string) bool { return pciAddrRe.MatchString(s) }

// Device reads one PCI function.
func (s Sys) Device(addr string) (PCIDevice, error) {
	if !ValidPCIAddress(addr) {
		return PCIDevice{}, fmt.Errorf("bad pci address %q", addr)
	}
	base := s.p("sys/bus/pci/devices", addr)
	if _, err := os.Stat(base); err != nil {
		return PCIDevice{}, err
	}
	d := PCIDevice{
		Address: addr,
		Vendor:  readTrim(filepath.Join(base, "vendor")),
		Device:  readTrim(filepath.Join(base, "device")),
		Class:   readTrim(filepath.Join(base, "class")),
	}
	if l, err := os.Readlink(filepath.Join(base, "driver")); err == nil {
		d.Driver = filepath.Base(l)
	}
	if l, err := os.Readlink(filepath.Join(base, "iommu_group")); err == nil {
		d.IOMMUGroup = filepath.Base(l)
	}
	return d, nil
}

// Devices lists every PCI function, sorted by address.
func (s Sys) Devices() ([]PCIDevice, error) {
	ents, err := os.ReadDir(s.p("sys/bus/pci/devices"))
	if err != nil {
		return nil, err
	}
	var out []PCIDevice
	for _, e := range ents {
		if !ValidPCIAddress(e.Name()) {
			continue
		}
		d, err := s.Device(e.Name())
		if err != nil {
			continue
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	return out, nil
}

// IsGPU reports whether the function is an NVIDIA display or 3D controller.
func (d PCIDevice) IsGPU() bool {
	return d.Vendor == NvidiaVendor && (strings.HasPrefix(d.Class, "0x0300") || strings.HasPrefix(d.Class, "0x0302"))
}

// IsNVSwitch reports whether the function is an NVIDIA NVSwitch (bridge, other).
func (d PCIDevice) IsNVSwitch() bool {
	return d.Vendor == NvidiaVendor && strings.HasPrefix(d.Class, "0x0680")
}

// isBridge reports PCI-to-PCI bridges, which may share an IOMMU group with a
// GPU without being assigned.
func (d PCIDevice) isBridge() bool { return strings.HasPrefix(d.Class, "0x0604") }

// Slot is the address without the function (0000:41:00).
func (d PCIDevice) Slot() string { return d.Address[:len(d.Address)-2] }

// GroupMembers lists the functions in an IOMMU group.
func (s Sys) GroupMembers(group string) ([]string, error) {
	if group == "" {
		return nil, fmt.Errorf("no iommu group")
	}
	ents, err := os.ReadDir(s.p("sys/kernel/iommu_groups", group, "devices"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}

// IOMMUEnabled reports whether the kernel has any IOMMU groups.
func (s Sys) IOMMUEnabled() bool {
	ents, err := os.ReadDir(s.p("sys/kernel/iommu_groups"))
	return err == nil && len(ents) > 0
}

// GPU is one NVIDIA GPU on the host, with what must go to vfio-pci with it.
type GPU struct {
	Index      int
	Address    string
	DeviceID   string
	ModelRaw   string
	VRAMGB     int
	UUID       string
	Driver     string
	IOMMUGroup string
	// Companions are the other functions of the same IOMMU group that must
	// also be bound to vfio-pci (the card's audio, USB-C, ...).
	Companions []string
	// Blocker explains why the GPU can't be passed through ("" when it can).
	Blocker string
}

// Cached is what the host's NVIDIA driver said about a GPU before the
// installer took it (the driver is gone once the GPU is on vfio-pci).
type Cached struct {
	Name   string `json:"name"`
	VRAMGB int    `json:"vram_gb"`
	UUID   string `json:"uuid"`
}

// GPUs lists NVIDIA GPUs sorted by address and numbered in that order. only,
// when non-empty, restricts the list to those addresses. cache supplies
// name, VRAM and UUID by address.
func (s Sys) GPUs(only []string, cache map[string]Cached, ids *PCIIDs) ([]GPU, error) {
	devs, err := s.Devices()
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, a := range only {
		want[strings.ToLower(a)] = true
	}
	byAddr := map[string]PCIDevice{}
	for _, d := range devs {
		byAddr[d.Address] = d
	}
	var out []GPU
	for _, d := range devs {
		if !d.IsGPU() || (len(want) > 0 && !want[d.Address]) {
			continue
		}
		g := GPU{Index: len(out), Address: d.Address, DeviceID: d.Device, Driver: d.Driver, IOMMUGroup: d.IOMMUGroup}
		g.ModelRaw = ids.Name(d.Vendor, d.Device)
		if c, ok := cache[d.Address]; ok {
			if c.Name != "" {
				g.ModelRaw = c.Name
			}
			g.VRAMGB, g.UUID = c.VRAMGB, c.UUID
		}
		switch members, err := s.GroupMembers(d.IOMMUGroup); {
		case d.IOMMUGroup == "":
			g.Blocker = "IOMMU is off"
		case err != nil:
			g.Blocker = "can't read IOMMU group: " + err.Error()
		default:
			for _, m := range members {
				if m == d.Address {
					continue
				}
				md, ok := byAddr[m]
				if !ok || md.isBridge() {
					continue
				}
				if md.Vendor != NvidiaVendor || md.Slot() != d.Slot() {
					g.Blocker = fmt.Sprintf("IOMMU group %s also holds %s (%s); turn on ACS or move the card", d.IOMMUGroup, m, md.Class)
					continue
				}
				g.Companions = append(g.Companions, m)
			}
		}
		out = append(out, g)
	}
	return out, nil
}

// NVSwitches lists NVSwitch functions (HGX baseboards).
func (s Sys) NVSwitches() ([]PCIDevice, error) {
	devs, err := s.Devices()
	if err != nil {
		return nil, err
	}
	var out []PCIDevice
	for _, d := range devs {
		if d.IsNVSwitch() {
			out = append(out, d)
		}
	}
	return out, nil
}

// PCIIDs is a parsed pci.ids database (vendor 10de only, which is all we need).
type PCIIDs struct{ names map[string]string }

// LoadPCIIDs reads the first pci.ids found. A missing database is fine:
// names fall back to the device id.
func LoadPCIIDs(paths ...string) *PCIIDs {
	ids := &PCIIDs{names: map[string]string{}}
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<16), 1<<20)
		in := false
		for sc.Scan() {
			line := sc.Text()
			if line == "" || line[0] == '#' {
				continue
			}
			if line[0] != '\t' {
				in = strings.HasPrefix(line, "10de ")
				continue
			}
			if !in || strings.HasPrefix(line, "\t\t") {
				continue
			}
			l := strings.TrimPrefix(line, "\t")
			if len(l) > 6 && l[4] == ' ' {
				ids.names[strings.ToLower(l[:4])] = strings.TrimSpace(l[5:])
			}
		}
		return ids
	}
	return ids
}

// Name returns the pci.ids name for a device, or "NVIDIA <id>".
func (ids *PCIIDs) Name(vendor, device string) string {
	dev := strings.TrimPrefix(strings.ToLower(device), "0x")
	if ids != nil {
		if n, ok := ids.names[dev]; ok {
			return n
		}
	}
	return "NVIDIA " + dev
}

// DefaultPCIIDsPaths are where Debian and Ubuntu keep pci.ids.
var DefaultPCIIDsPaths = []string{"/usr/share/misc/pci.ids", "/usr/share/hwdata/pci.ids", "/usr/share/pci.ids"}

// Health is the host's view of a passed-through GPU: present, readable and
// on its expected link. Telemetry needs a driver, so it isn't available here.
func (s Sys) Health(addr string) string {
	base := s.p("sys/bus/pci/devices", addr)
	v := readTrim(filepath.Join(base, "vendor"))
	if v == "" || v == "0xffff" {
		return "failed"
	}
	// An uncorrectable error count above zero means the card is in trouble.
	if fatal := aerTotal(filepath.Join(base, "aer_dev_fatal")); fatal > 0 {
		return "failed"
	}
	if nonfatal := aerTotal(filepath.Join(base, "aer_dev_nonfatal")); nonfatal > 0 {
		return "degraded"
	}
	cur, max := readTrim(filepath.Join(base, "current_link_width")), readTrim(filepath.Join(base, "max_link_width"))
	if cur != "" && max != "" && cur != max {
		if c, err1 := strconv.Atoi(cur); err1 == nil {
			if m, err2 := strconv.Atoi(max); err2 == nil && c < m {
				return "degraded"
			}
		}
	}
	return "ok"
}

func aerTotal(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "TOTAL_ERR_FATAL" || len(f) == 2 && f[0] == "TOTAL_ERR_NONFATAL" {
			n, _ := strconv.Atoi(f[1])
			return n
		}
	}
	return 0
}
