// Package vm runs customer VMs: QEMU/KVM with VFIO GPU passthrough, one
// transient systemd unit per VM, a qcow2 overlay disk on our base image,
// a NoCloud seed, and a routed tap with egress-only NAT.
package vm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/runcrateai/runcrate-host/internal/api"
	"github.com/runcrateai/runcrate-host/internal/config"
	"github.com/runcrateai/runcrate-host/internal/hw"
	"github.com/runcrateai/runcrate-host/internal/metadata"
	"github.com/runcrateai/runcrate-host/internal/sysexec"
	"github.com/runcrateai/runcrate-host/internal/vfio"
)

// States reported to the control plane.
const (
	StateCreating = "creating"
	StateRunning  = "running"
	StateStopping = "stopping"
	StateStopped  = "stopped"
	StateFailed   = "failed"
)

// Host reserves: what the partner's OS keeps for itself.
const (
	reserveThreads = 2
	reserveRAMGB   = 8
	reserveDiskGB  = ReserveDiskGB
	// ReserveDiskGB is disk the agent never gives to VMs.
	ReserveDiskGB = 10
	// ImageAllowanceGB is room kept for base images when reporting disk.
	ImageAllowanceGB = 20
)

var idRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)

// ValidID reports whether a VM id is safe to use in paths and unit names.
func ValidID(id string) bool { return idRe.MatchString(id) }

// Reserved reports ids kept for the agent's own throwaway VMs.
func Reserved(id string) bool {
	return strings.HasPrefix(id, "scrub-") || strings.HasPrefix(id, "bench-")
}

// SetUUIDs records GPU UUIDs (learned from a benchmark).
func (m *Manager) SetUUIDs(byIndex map[int]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.GPUs {
		if u, ok := byIndex[m.GPUs[i].Index]; ok && m.GPUs[i].UUID == "" {
			m.GPUs[i].UUID = u
		}
	}
}

// Spec is what to run.
type Spec struct {
	ID           string `json:"id"`
	GPUIndexes   []int  `json:"gpu_indexes"`
	VCPUs        int    `json:"vcpus"`
	RAMGB        int    `json:"ram_gb"`
	DiskGB       int    `json:"disk_gb"`
	ImageVersion string `json:"image_version"`
	CloudInit    string `json:"-"`
	Ephemeral    bool   `json:"ephemeral"`
	// Owner is the scrub holding these GPUs (throwaway scrub VMs only): the
	// VM may take GPUs held by it, and gives them back to it when done.
	Owner string `json:"owner,omitempty"`
}

// Record is a VM's persisted state (vm.json).
type Record struct {
	Spec
	Slot        int       `json:"slot"`
	Tap         string    `json:"tap"`
	MAC         string    `json:"mac"`
	GuestIP     string    `json:"guest_ip"`
	PassThrough []string  `json:"pass_through"`
	State       string    `json:"state"`
	Error       string    `json:"error,omitempty"`
	Reclaim     time.Time `json:"reclaim,omitzero"`
	CreatedAt   time.Time `json:"created_at"`
	// Want is false once a destroy started; a restart then finishes it.
	Want bool `json:"want"`
	// Started is set once QEMU was launched with the GPUs, so a VM that
	// never ran needs no GPU scrub.
	Started bool `json:"started,omitempty"`
	// Base is the image the disk is layered on. QEMU gets it explicitly; the
	// overlay's own header is never trusted for it.
	Base string `json:"base"`
}

// Images provides base images by version ("" = current).
type Images interface {
	Ensure(ctx context.Context, version string) (path, version2 string, err error)
	// Local is Ensure that prefers the newest image already on disk.
	Local(ctx context.Context, version string) (path, version2 string, err error)
}

// Manager owns every VM on the host.
type Manager struct {
	DataDir  string
	Net      *Network
	Meta     *metadata.Server
	Run      sysexec.Runner
	Sys      hw.Sys
	VFIO     vfio.Sysfs
	Images   Images
	Firmware Firmware
	Log      *slog.Logger
	// GPUs is the inventory Runcrate may use (index order).
	GPUs []hw.GPU
	// NVSwitches go into a VM that takes every GPU of an NVSwitch node.
	NVSwitches []string
	// HostThreads and HostRAMGB size what VMs may take.
	HostThreads int
	HostRAMGB   int
	// Scrub runs a throwaway VM that overwrites GPU memory after each
	// customer VM.
	Scrub bool
	// BootWait is how long a new QEMU must stay up to count as started.
	BootWait time.Duration
	// NoPassthrough (devmode only) boots VMs without their GPUs.
	NoPassthrough bool
	// QemuUser runs QEMU unprivileged and confined ("" only in tests).
	QemuUser string
	// Ctx bounds background scrubs; cancel it at shutdown.
	Ctx context.Context

	mu      sync.Mutex
	vms     map[string]*Record
	busy    map[int]string // gpu index -> vm id, or "scrub:<vm id>"
	scrubOK map[int]bool   // false after a failed scrub: GPU held back
	wg      sync.WaitGroup
}

func (m *Manager) init() {
	if m.vms == nil {
		m.vms = map[string]*Record{}
		m.busy = map[int]string{}
		m.scrubOK = map[int]bool{}
	}
	if m.BootWait == 0 {
		m.BootWait = 5 * time.Second
	}
}

func (m *Manager) dir(id string) string { return filepath.Join(m.DataDir, "vms", id) }

func unit(id string) string { return "runcrate-vm-" + id + ".service" }

func hash(id string) []byte { h := sha256.Sum256([]byte(id)); return h[:] }

func tapName(id string) string { return "rc-" + hex.EncodeToString(hash(id))[:8] }

func macFor(id string) string {
	h := hash(id)
	return fmt.Sprintf("52:54:00:%02x:%02x:%02x", h[8], h[9], h[10])
}

func (m *Manager) save(r *Record) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(filepath.Join(m.dir(r.ID), "vm.json"), b, 0o600)
}

// Load reads persisted VMs. With resume, the host is brought back to them:
// VMs that should run are started again (after a host reboot), half-finished
// destroys, creates and throwaway VMs are cleaned up, held GPUs stay held,
// and pending scrubs and wipes finish. Without resume (uninstall) it only
// reads the records.
func (m *Manager) Load(ctx context.Context, resume bool) error {
	m.mu.Lock()
	m.init()
	m.mu.Unlock()
	for _, d := range []string{m.DataDir, filepath.Join(m.DataDir, "vms")} {
		// 0711: an unprivileged QEMU may pass through to its own VM dir.
		if err := os.MkdirAll(d, 0o711); err != nil {
			return err
		}
		_ = os.Chmod(d, 0o711)
	}
	for _, g := range m.heldFromDisk() {
		m.mu.Lock()
		m.scrubOK[g] = false
		m.mu.Unlock()
	}
	ents, err := os.ReadDir(filepath.Join(m.DataDir, "vms"))
	if err != nil {
		return err
	}
	type pending struct {
		id   string
		gpus []int
	}
	var scrubs []pending
	for _, e := range ents {
		if !resume && strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if id, ok := strings.CutPrefix(e.Name(), ".scrub-"); ok {
			var gpus []int
			if b, err := os.ReadFile(filepath.Join(m.DataDir, "vms", e.Name())); err == nil && json.Unmarshal(b, &gpus) == nil {
				// Hold the GPUs now, before any create_vm can take them.
				m.mu.Lock()
				for _, g := range gpus {
					m.busy[g] = "scrub:" + id
					m.scrubOK[g] = false
				}
				m.mu.Unlock()
				scrubs = append(scrubs, pending{id, gpus})
			}
			continue
		}
		if strings.HasPrefix(e.Name(), ".wipe-") {
			p := filepath.Join(m.DataDir, "vms", e.Name())
			m.wg.Add(1)
			go func() { defer m.wg.Done(); m.wipeDir(p) }()
			continue
		}
		b, err := os.ReadFile(filepath.Join(m.dir(e.Name()), "vm.json"))
		if err != nil {
			continue
		}
		var r Record
		if json.Unmarshal(b, &r) != nil || r.ID != e.Name() || !ValidID(r.ID) {
			continue
		}
		m.mu.Lock()
		m.vms[r.ID] = &r
		for _, g := range r.GPUIndexes {
			m.busy[g] = r.ID
		}
		m.mu.Unlock()
		if !resume {
			continue
		}
		// Half-made VMs (agent stopped mid-create) are removed; the control
		// plane re-sends create_vm until it has a result.
		if !r.Want || r.Ephemeral || r.State == StateCreating {
			if err := m.Destroy(ctx, r.ID); err != nil {
				m.Log.Error("finish destroy", "vm", r.ID, "err", err)
			}
			continue
		}
		if r.State == StateRunning {
			if err := m.start(ctx, &r); err != nil {
				m.setState(&r, StateFailed, err.Error())
			} else {
				m.setState(&r, StateRunning, "")
			}
		}
	}
	for _, p := range scrubs {
		m.startScrub(p.id, p.gpus)
	}
	return nil
}

func (m *Manager) holdPath(g int) string {
	return filepath.Join(m.DataDir, "holds", strconv.Itoa(g))
}

func (m *Manager) heldFromDisk() []int {
	ents, _ := os.ReadDir(filepath.Join(m.DataDir, "holds"))
	var out []int
	for _, e := range ents {
		if g, err := strconv.Atoi(e.Name()); err == nil {
			out = append(out, g)
		}
	}
	return out
}

func (m *Manager) setState(r *Record, state, msg string) {
	m.mu.Lock()
	r.State, r.Error = state, msg
	m.mu.Unlock()
	if err := m.save(r); err != nil {
		m.Log.Error("save vm", "vm", r.ID, "err", err)
	}
}

// List reports customer VMs for the heartbeat, refreshing each one's state
// from systemd.
func (m *Manager) List(ctx context.Context) []api.VMStatus {
	m.mu.Lock()
	m.init()
	recs := make([]*Record, 0, len(m.vms))
	for _, r := range m.vms {
		if !r.Ephemeral {
			recs = append(recs, r)
		}
	}
	m.mu.Unlock()
	sort.Slice(recs, func(i, j int) bool { return recs[i].ID < recs[j].ID })
	out := make([]api.VMStatus, 0, len(recs))
	for _, r := range recs {
		m.mu.Lock()
		state := r.State
		m.mu.Unlock()
		if state == StateRunning {
			switch m.unitState(context.WithoutCancel(ctx), r.ID) {
			case "failed":
				m.setState(r, StateFailed, "qemu exited with an error")
				state = StateFailed
			case "inactive":
				// The guest powered itself off.
				m.setState(r, StateStopped, "")
				state = StateStopped
			default:
				// Up, or systemd didn't answer: keep the state, look again
				// next time.
			}
		}
		out = append(out, api.VMStatus{VMID: r.ID, State: state, GPUIndexes: append([]int{}, r.GPUIndexes...)})
	}
	return out
}

// HeldGPUs reports GPUs held back after a failed scrub.
func (m *Manager) HeldGPUs() map[int]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	out := map[int]bool{}
	for g, ok := range m.scrubOK {
		if !ok {
			out[g] = true
		}
	}
	return out
}

// FreeGPUs lists GPU indexes that can be passed through and are not in a
// VM or a scrub. withHeld includes GPUs held back after a failed scrub (the
// benchmark uses them to clear the hold).
func (m *Manager) FreeGPUs(withHeld bool) []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	var out []int
	for _, g := range m.GPUs {
		if _, busy := m.busy[g.Index]; busy || g.Blocker != "" {
			continue
		}
		if ok, seen := m.scrubOK[g.Index]; seen && !ok && !withHeld {
			continue
		}
		out = append(out, g.Index)
	}
	return out
}

func (m *Manager) unitState(ctx context.Context, id string) string {
	out, err := m.Run.Run(ctx, "systemctl", "show", "-p", "ActiveState", "--value", unit(id))
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

func (m *Manager) gpu(idx int) (hw.GPU, bool) {
	for _, g := range m.GPUs {
		if g.Index == idx {
			return g, true
		}
	}
	return hw.GPU{}, false
}

// ErrCapacity means the host can't fit the VM.
var ErrCapacity = errors.New("not enough capacity on this host")

// claim validates a spec and reserves its GPUs and a network slot. Waits for
// GPUs that are being scrubbed.
func (m *Manager) claim(ctx context.Context, s Spec) (*Record, bool, error) {
	if !ValidID(s.ID) {
		return nil, false, fmt.Errorf("bad vm_id %q", s.ID)
	}
	if s.VCPUs < 1 || s.RAMGB < 1 || s.DiskGB < 0 {
		return nil, false, fmt.Errorf("vcpus and ram_gb must be positive")
	}
	seen := map[int]bool{}
	for _, g := range s.GPUIndexes {
		gpu, ok := m.gpu(g)
		if !ok {
			return nil, false, fmt.Errorf("gpu index %d is not on this host", g)
		}
		if gpu.Blocker != "" && !m.NoPassthrough {
			return nil, false, fmt.Errorf("gpu %d can't be passed through: %s", g, gpu.Blocker)
		}
		if seen[g] {
			return nil, false, fmt.Errorf("gpu index %d listed twice", g)
		}
		seen[g] = true
	}
	if len(m.NVSwitches) > 0 && len(s.GPUIndexes) > 0 && len(s.GPUIndexes) != len(m.GPUs) {
		return nil, false, fmt.Errorf("this is an NVSwitch node: a VM must take all %d GPUs", len(m.GPUs))
	}
	deadline := time.Now().Add(15 * time.Minute)
	for {
		m.mu.Lock()
		if r, ok := m.vms[s.ID]; ok {
			m.mu.Unlock()
			return r, true, nil
		}
		blocked := ""
		for _, g := range s.GPUIndexes {
			if who, ok := m.busy[g]; ok && (s.Owner == "" || who != s.Owner) {
				if !strings.HasPrefix(who, "scrub:") && !strings.HasPrefix(who, "destroying:") {
					m.mu.Unlock()
					return nil, false, fmt.Errorf("gpu %d is in use by vm %s", g, who)
				}
				blocked = who
			} else if ok, seen := m.scrubOK[g]; seen && !ok && !s.Ephemeral {
				m.mu.Unlock()
				return nil, false, fmt.Errorf("gpu %d is held back after a failed memory scrub", g)
			}
		}
		if blocked == "" {
			break
		}
		m.mu.Unlock()
		if time.Now().After(deadline) {
			return nil, false, fmt.Errorf("gpus still being scrubbed after 15 minutes")
		}
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	defer m.mu.Unlock()
	// Capacity.
	usedCPU, usedRAM := 0, 0
	slots := map[int]bool{}
	taps := map[string]bool{}
	for _, r := range m.vms {
		usedCPU += r.VCPUs
		usedRAM += r.RAMGB
		slots[r.Slot] = true
		taps[r.Tap] = true
	}
	if m.HostThreads > 0 && usedCPU+s.VCPUs > m.HostThreads-reserveThreads {
		return nil, false, fmt.Errorf("%w: %d vcpus asked, %d free", ErrCapacity, s.VCPUs, m.HostThreads-reserveThreads-usedCPU)
	}
	if m.HostRAMGB > 0 && usedRAM+s.RAMGB > m.HostRAMGB-reserveRAMGB {
		return nil, false, fmt.Errorf("%w: %d GB RAM asked, %d free", ErrCapacity, s.RAMGB, m.HostRAMGB-reserveRAMGB-usedRAM)
	}
	// VM disks are thin: count what every VM may still grow into, not only
	// what the filesystem has free now.
	if free, _ := hw.DiskFree(m.DataDir); free > 0 {
		var committed, written uint64
		for _, r := range m.vms {
			committed += uint64(r.DiskGB) << 30
			written += allocated(filepath.Join(m.dir(r.ID), "disk.qcow2"))
		}
		capacity := free + written
		need := committed + uint64(s.DiskGB+reserveDiskGB)<<30
		if need > capacity {
			left := int64(capacity>>30) - int64(committed>>30) - reserveDiskGB
			return nil, false, fmt.Errorf("%w: %d GB disk asked, %d GB left", ErrCapacity, s.DiskGB, max(left, 0))
		}
	}
	slot := -1
	for i := 0; i < m.Net.MaxSlots(); i++ {
		if !slots[i] {
			slot = i
			break
		}
	}
	if slot < 0 {
		return nil, false, fmt.Errorf("%w: no network slots left", ErrCapacity)
	}
	tap := tapName(s.ID)
	if taps[tap] {
		return nil, false, fmt.Errorf("tap name collision for %s", s.ID)
	}
	_, guest, err := m.Net.Slot(slot)
	if err != nil {
		return nil, false, err
	}
	r := &Record{Spec: s, Slot: slot, Tap: tap, MAC: macFor(s.ID), GuestIP: guest.String(), State: StateCreating, CreatedAt: time.Now().UTC(), Want: true}
	for _, g := range s.GPUIndexes {
		gpu, _ := m.gpu(g)
		if !m.NoPassthrough {
			r.PassThrough = append(r.PassThrough, gpu.Address)
		}
	}
	if len(m.NVSwitches) > 0 && len(s.GPUIndexes) == len(m.GPUs) && !m.NoPassthrough {
		r.PassThrough = append(r.PassThrough, m.NVSwitches...)
	}
	m.vms[s.ID] = r
	for _, g := range s.GPUIndexes {
		if s.Owner != "" {
			m.busy[g] = s.Owner // a scrub keeps holding them as itself
		} else {
			m.busy[g] = s.ID
		}
	}
	return r, false, nil
}

// Create boots a VM. Idempotent: an existing VM with the same id is left as
// it is and its state returned.
func (m *Manager) Create(ctx context.Context, s Spec) (string, error) {
	m.mu.Lock()
	m.init()
	m.mu.Unlock()
	r, existed, err := m.claim(ctx, s)
	if err != nil {
		return "", err
	}
	if existed {
		m.mu.Lock()
		st := r.State
		m.mu.Unlock()
		if st == StateFailed {
			return st, fmt.Errorf("vm %s failed earlier: %s", r.ID, r.Error)
		}
		return st, nil
	}
	if err := m.prepare(ctx, r); err != nil {
		m.Log.Error("create vm", "vm", r.ID, "err", err)
		_ = m.Destroy(context.WithoutCancel(ctx), r.ID)
		return StateFailed, err
	}
	if err := m.start(ctx, r); err != nil {
		m.setState(r, StateFailed, err.Error())
		_ = m.Destroy(context.WithoutCancel(ctx), r.ID)
		return StateFailed, err
	}
	m.setState(r, StateRunning, "")
	return StateRunning, nil
}

// prepare makes the disk, firmware vars and seed. The record is saved first
// so a crash part-way is cleaned up on restart.
func (m *Manager) prepare(ctx context.Context, r *Record) error {
	dir := m.dir(r.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := m.save(r); err != nil {
		return err
	}
	ensure := m.Images.Ensure
	if r.Ephemeral {
		// Throwaway VMs use whatever image is already here; only a host with
		// none asks the API.
		ensure = m.Images.Local
	}
	base, version, err := ensure(ctx, r.ImageVersion)
	if err != nil {
		return fmt.Errorf("image: %w", err)
	}
	r.ImageVersion, r.Base = version, base
	disk := filepath.Join(dir, "disk.qcow2")
	args := []string{"create", "-q", "-f", "qcow2", "-F", "qcow2", "-b", base, disk}
	if r.DiskGB > 0 {
		args = append(args, fmt.Sprintf("%dG", r.DiskGB))
	}
	if _, err := m.Run.Run(ctx, "qemu-img", args...); err != nil {
		// A smaller disk than the image is refused here; say so plainly.
		return fmt.Errorf("disk: %w", err)
	}
	_ = os.Chmod(disk, 0o600)
	if err := copyFile(m.Firmware.Vars, filepath.Join(dir, "OVMF_VARS.fd"), 0o600); err != nil {
		return fmt.Errorf("firmware vars: %w", err)
	}
	ud, err := UserData(r.CloudInit)
	if err != nil {
		return err
	}
	host, guest, err := m.Net.Slot(r.Slot)
	if err != nil {
		return err
	}
	hostname := "runcrate-" + strings.ToLower(r.ID)
	if len(hostname) > 30 {
		hostname = hostname[:30]
	}
	if _, err := WriteSeed(ctx, m.Run, dir, ud, MetaData(r.ID, strings.TrimRight(hostname, "-")), NetworkConfig(r.MAC, guest.String(), host.String())); err != nil {
		return fmt.Errorf("seed: %w", err)
	}
	return m.save(r)
}

// start brings up networking, GPUs and the QEMU unit for a prepared VM.
func (m *Manager) start(ctx context.Context, r *Record) error {
	if st := m.unitState(ctx, r.ID); st == "active" || st == "activating" {
		// Already running (agent restart): only the metadata entry is new.
		m.Meta.Set(r.GuestIP, &metadata.Entry{VMID: r.ID, Ephemeral: r.Ephemeral, Reclaim: r.Reclaim})
		return nil
	}
	if err := m.Net.AddTap(ctx, r.Tap, r.Slot, m.QemuUser); err != nil {
		return fmt.Errorf("network: %w", err)
	}
	m.Meta.Set(r.GuestIP, &metadata.Entry{VMID: r.ID, Ephemeral: r.Ephemeral, Reclaim: r.Reclaim})
	for _, g := range r.GPUIndexes {
		if m.NoPassthrough {
			break
		}
		gpu, _ := m.gpu(g)
		for _, a := range append([]string{gpu.Address}, gpu.Companions...) {
			if err := m.VFIO.Bind(a); err != nil {
				return fmt.Errorf("gpu %d: %w", g, err)
			}
		}
	}
	for _, a := range m.NVSwitches {
		if contains(r.PassThrough, a) {
			if err := m.VFIO.Bind(a); err != nil {
				return fmt.Errorf("nvswitch: %w", err)
			}
		}
	}
	groups, err := m.groupDevices(r.PassThrough)
	if err != nil {
		return err
	}
	if err := m.giveToQemu(r, groups); err != nil {
		return err
	}
	_, _ = m.Run.Run(ctx, "systemctl", "reset-failed", unit(r.ID))
	dir := m.dir(r.ID)
	spec := QemuSpec{
		Name: r.ID, Dir: dir, VCPUs: r.VCPUs, RAMGB: r.RAMGB,
		Disk: filepath.Join(dir, "disk.qcow2"), Base: r.Base, Seed: filepath.Join(dir, "seed.iso"),
		Tap: r.Tap, MAC: r.MAC, Firmware: m.Firmware, PassThrough: r.PassThrough,
		MMIO64MB: MMIO64For(m.Sys.Root, r.PassThrough),
	}
	if filepath.Dir(spec.Base) != filepath.Join(m.DataDir, "images") {
		return fmt.Errorf("vm %s has no valid base image (%q)", r.ID, spec.Base)
	}
	args := []string{
		"--unit=" + unit(r.ID), "--slice=runcrate.slice", "--description=Runcrate VM " + r.ID,
		"--property=KillMode=mixed", "--property=TimeoutStopSec=30", "--property=LimitMEMLOCK=infinity",
		"--property=Restart=no", "--property=LimitCORE=0",
	}
	args = append(args, m.confinement(dir, groups)...)
	args = append(args, "--", "qemu-system-x86_64")
	m.mu.Lock()
	r.Started = true
	m.mu.Unlock()
	if err := m.save(r); err != nil {
		return err
	}
	if _, err := m.Run.Run(ctx, "systemd-run", append(args, spec.Args()...)...); err != nil {
		return err
	}
	// QEMU fails fast on bad devices or memory: give it a moment.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(m.BootWait):
	}
	if st := m.unitState(ctx, r.ID); st != "active" {
		out, _ := m.Run.Run(ctx, "journalctl", "-u", unit(r.ID), "-n", "20", "--no-pager", "-o", "cat")
		return fmt.Errorf("qemu did not stay up (%s): %s", st, lastLines(string(out), 6))
	}
	return nil
}

// groupDevices maps PCI functions to their /dev/vfio/<group> nodes.
func (m *Manager) groupDevices(addrs []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, a := range addrs {
		d, err := m.Sys.Device(a)
		if err != nil {
			return nil, err
		}
		if d.IOMMUGroup == "" {
			return nil, fmt.Errorf("%s has no IOMMU group", a)
		}
		p := "/dev/vfio/" + d.IOMMUGroup
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out, nil
}

// giveToQemu hands the VM's files and VFIO groups to the unprivileged QEMU
// user.
func (m *Manager) giveToQemu(r *Record, groups []string) error {
	if m.QemuUser == "" {
		return nil
	}
	u, err := user.Lookup(m.QemuUser)
	if err != nil {
		return fmt.Errorf("qemu user: %w", err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	dir := m.dir(r.ID)
	if err := os.Chown(dir, uid, gid); err != nil {
		return err
	}
	for _, f := range []string{"disk.qcow2", "OVMF_VARS.fd", "seed.iso"} {
		if err := os.Chown(filepath.Join(dir, f), uid, gid); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	for _, g := range groups {
		if err := os.Chown(g, uid, gid); err != nil {
			return fmt.Errorf("vfio group %s: %w", g, err)
		}
	}
	return nil
}

// takeBack returns VFIO groups to root once a VM is gone.
func (m *Manager) takeBack(groups []string) {
	for _, g := range groups {
		_ = os.Chown(g, 0, 0)
	}
}

// confinement is the systemd sandbox for an unprivileged QEMU: no
// capabilities, a read-only system, only its own directory writable, and
// only the devices a VM needs.
func (m *Manager) confinement(dir string, groups []string) []string {
	if m.QemuUser == "" {
		return nil
	}
	p := []string{
		"--uid=" + m.QemuUser, "--gid=" + m.QemuUser,
		"--property=SupplementaryGroups=kvm",
		"--property=NoNewPrivileges=yes",
		"--property=CapabilityBoundingSet=",
		"--property=AmbientCapabilities=",
		"--property=ProtectSystem=strict",
		"--property=ReadWritePaths=" + dir,
		"--property=ProtectHome=yes",
		"--property=PrivateTmp=yes",
		"--property=ProtectKernelTunables=yes",
		"--property=ProtectKernelModules=yes",
		"--property=ProtectKernelLogs=yes",
		"--property=ProtectControlGroups=yes",
		"--property=ProtectClock=yes",
		"--property=ProtectHostname=yes",
		"--property=LockPersonality=yes",
		"--property=RestrictRealtime=yes",
		"--property=RestrictNamespaces=yes",
		"--property=RestrictSUIDSGID=yes",
		"--property=SystemCallArchitectures=native",
		"--property=RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK",
		"--property=DevicePolicy=closed",
		"--property=DeviceAllow=/dev/kvm rw",
		"--property=DeviceAllow=/dev/net/tun rw",
		"--property=DeviceAllow=/dev/vhost-net rw",
		"--property=DeviceAllow=/dev/vfio/vfio rw",
		"--property=DeviceAllow=/dev/urandom r",
	}
	for _, g := range groups {
		p = append(p, "--property=DeviceAllow="+g+" rw")
	}
	return p
}

// ImagesInUse lists image versions under VM disks.
func (m *Manager) ImagesInUse() map[string]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]bool{}
	for _, r := range m.vms {
		out[r.ImageVersion] = true
	}
	return out
}

// Reclaim records a reclaim deadline and shows it to the VM.
func (m *Manager) Reclaim(id string, deadline time.Time) error {
	m.mu.Lock()
	m.init()
	r, ok := m.vms[id]
	m.mu.Unlock()
	if !ok || r.Ephemeral {
		return fmt.Errorf("vm %s is not on this host", id)
	}
	m.mu.Lock()
	r.Reclaim = deadline.UTC()
	m.mu.Unlock()
	m.Meta.SetReclaim(id, deadline)
	return m.save(r)
}

// Destroy stops a VM, removes its network and wipes its disk. Unknown ids
// succeed (already gone). For customer VMs that ran, the GPUs are then
// scrubbed in the background before they can be used again; until then a
// create_vm on them waits.
func (m *Manager) Destroy(ctx context.Context, id string) error {
	if !ValidID(id) {
		return fmt.Errorf("bad vm_id %q", id)
	}
	// Teardown must finish even if the caller is shutting down.
	ctx = context.WithoutCancel(ctx)
	m.mu.Lock()
	m.init()
	r, ok := m.vms[id]
	m.mu.Unlock()
	if !ok {
		return nil
	}
	gpus := append([]int{}, r.GPUIndexes...)
	scrub := m.Scrub && !r.Ephemeral && r.Started && len(gpus) > 0
	if scrub {
		// Remember the scrub before anything else, so a crash or restart at
		// any point still does it.
		b, _ := json.Marshal(gpus)
		if err := config.WriteFileAtomic(m.scrubMarker(id), b, 0o600); err != nil {
			return fmt.Errorf("scrub marker: %w", err)
		}
	}
	m.mu.Lock()
	r.Want = false
	if r.Owner == "" {
		for _, g := range gpus {
			m.busy[g] = "destroying:" + id
		}
	}
	m.mu.Unlock()
	m.setState(r, StateStopping, "")
	_, _ = m.Run.Run(ctx, "systemctl", "stop", unit(id))
	st := m.unitState(ctx, id)
	for i := 0; i < 60 && st == "deactivating"; i++ {
		time.Sleep(time.Second)
		st = m.unitState(ctx, id)
	}
	if st == "active" || st == "deactivating" || st == "activating" {
		_, _ = m.Run.Run(ctx, "systemctl", "kill", "-s", "KILL", unit(id))
		time.Sleep(2 * time.Second)
		st = m.unitState(ctx, id)
	}
	if st != "inactive" && st != "failed" {
		// QEMU may still hold the disk and GPUs: keep the record (Want is
		// false), so a retry or the next start finishes the destroy.
		return fmt.Errorf("vm %s did not stop (systemd says %q)", id, st)
	}
	_, _ = m.Run.Run(ctx, "systemctl", "reset-failed", unit(id))
	m.Meta.Remove(r.GuestIP)
	m.Net.DelTap(ctx, r.Tap)
	if groups, err := m.groupDevices(r.PassThrough); err == nil {
		m.takeBack(groups)
	}
	// Move the directory aside, then overwrite the disk before deleting it.
	wipe := filepath.Join(m.DataDir, "vms", ".wipe-"+id)
	if err := os.Rename(m.dir(id), wipe); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("move disk aside: %w", err)
	}
	m.wipeDir(wipe)
	m.mu.Lock()
	delete(m.vms, id)
	for _, g := range gpus {
		switch {
		case scrub:
			m.busy[g] = "scrub:" + id
		case r.Owner != "":
			m.busy[g] = r.Owner
		default:
			delete(m.busy, g)
		}
	}
	m.mu.Unlock()
	if scrub {
		m.startScrub(id, gpus)
	}
	return nil
}

func (m *Manager) scrubMarker(id string) string {
	return filepath.Join(m.DataDir, "vms", ".scrub-"+id)
}

// startScrub runs a scrub under the manager's own context (Ctx), which the
// agent cancels at shutdown: a cancelled scrub keeps its marker and resumes
// after the restart.
func (m *Manager) startScrub(id string, gpus []int) {
	ctx := m.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.scrub(ctx, id, gpus)
	}()
}

// scrub overwrites the memory of GPUs a customer just used. Until it
// passes, the GPUs stay out of use, across restarts. A scrub that couldn't
// run (no image, no capacity, a timeout) says nothing about the GPUs and is
// retried with backoff; one that ran and failed holds them until a passing
// benchmark.
func (m *Manager) scrub(ctx context.Context, after string, gpus []int) {
	owner := "scrub:" + after
	m.Hold(gpus)
	m.mu.Lock()
	for _, g := range gpus {
		m.busy[g] = owner
	}
	m.mu.Unlock()
	var err error
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			wait := min(time.Duration(1<<min(attempt, 5))*time.Minute, 30*time.Minute)
			select {
			case <-ctx.Done():
				return // marker kept: the next start resumes it
			case <-time.After(wait):
			}
		}
		id := fmt.Sprintf("scrub-%s-%d", hex.EncodeToString(hash(after))[:10], time.Now().UnixNano()%1e9)
		rctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
		var out []byte
		out, err = m.RunEphemeral(rctx, id, owner, gpus, ScrubScript, 15*time.Minute)
		cancel()
		if ctx.Err() != nil {
			return // shutting down: marker kept
		}
		if err != nil {
			m.Log.Warn("gpu scrub could not run; retrying", "after", after, "gpus", gpus, "err", err)
			continue
		}
		err = m.checkScrub(out, gpus)
		break
	}
	if err == nil {
		m.ClearHold(gpus)
	}
	m.mu.Lock()
	for _, g := range gpus {
		if m.busy[g] == owner {
			delete(m.busy, g)
		}
	}
	m.mu.Unlock()
	_ = os.Remove(m.scrubMarker(after))
	if err != nil {
		m.Log.Error("gpu scrub failed; gpus held back until a benchmark passes", "after", after, "gpus", gpus, "err", err)
	}
}

// checkScrub requires one passing entry per scrubbed GPU, matched by UUID
// when the host knows it: a GPU missing from the VM was not wiped.
func (m *Manager) checkScrub(out []byte, gpus []int) error {
	var res struct {
		OK   bool `json:"ok"`
		GPUs []struct {
			UUID string `json:"uuid"`
			OK   bool   `json:"ok"`
		} `json:"gpus"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return fmt.Errorf("scrub output: %w", err)
	}
	if !res.OK {
		return fmt.Errorf("scrub reported failure: %s", truncate(res.Error, 300))
	}
	if len(res.GPUs) != len(gpus) {
		return fmt.Errorf("scrub saw %d GPUs, expected %d", len(res.GPUs), len(gpus))
	}
	seen := map[string]bool{}
	for _, g := range res.GPUs {
		if !g.OK {
			return fmt.Errorf("scrub failed on %s", g.UUID)
		}
		seen[strings.ToLower(g.UUID)] = true
	}
	for _, idx := range gpus {
		if gpu, ok := m.gpu(idx); ok && gpu.UUID != "" && !seen[strings.ToLower(gpu.UUID)] {
			return fmt.Errorf("gpu %d (%s) was not in the scrub", idx, gpu.UUID)
		}
	}
	return nil
}

// Hold keeps GPUs out of rentals (after a failed benchmark) until ClearHold.
func (m *Manager) Hold(gpus []int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	_ = os.MkdirAll(filepath.Join(m.DataDir, "holds"), 0o700)
	for _, g := range gpus {
		m.scrubOK[g] = false
		if err := os.WriteFile(m.holdPath(g), nil, 0o600); err != nil {
			m.Log.Error("persist gpu hold", "gpu", g, "err", err)
		}
	}
}

// ClearHold marks GPUs good again (after a passing benchmark).
func (m *Manager) ClearHold(gpus []int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	for _, g := range gpus {
		m.scrubOK[g] = true
		_ = os.Remove(m.holdPath(g))
	}
}

// RunEphemeral boots a throwaway VM on gpus that runs script, waits for it
// to post its JSON result to the metadata server, and destroys it.
func (m *Manager) RunEphemeral(ctx context.Context, id, owner string, gpus []int, script string, timeout time.Duration) ([]byte, error) {
	if !ValidID(id) {
		return nil, fmt.Errorf("bad id %q", id)
	}
	m.mu.Lock()
	m.init()
	_, exists := m.vms[id]
	m.mu.Unlock()
	if exists {
		return nil, fmt.Errorf("throwaway vm %s already exists", id)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	vcpus, ram := 4, 16
	if m.HostRAMGB > 0 && m.HostRAMGB-reserveRAMGB < ram {
		ram = max(4, m.HostRAMGB-reserveRAMGB)
	}
	if _, err := m.Create(ctx, Spec{ID: id, GPUIndexes: gpus, VCPUs: vcpus, RAMGB: ram, CloudInit: script, Ephemeral: true, Owner: owner}); err != nil {
		return nil, err
	}
	defer func() { _ = m.Destroy(context.WithoutCancel(ctx), id) }()
	m.mu.Lock()
	ip := m.vms[id].GuestIP
	m.mu.Unlock()
	out, err := m.Meta.Result(ctx, ip)
	if err != nil {
		return nil, fmt.Errorf("throwaway vm gave no result: %w", err)
	}
	return out, nil
}

// Wait waits for background wipes and scrubs (shutdown, tests).
func (m *Manager) Wait() { m.wg.Wait() }

// Busy reports whether any VM or scrub is running.
func (m *Manager) Busy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.vms) > 0 || len(m.busy) > 0
}

// wipeDir overwrites every regular file with zeros, syncs, and deletes the
// directory, so a customer's disk doesn't linger on the partner's drive.
func (m *Manager) wipeDir(dir string) {
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		if err := zeroFile(p); err != nil {
			m.Log.Error("wipe", "file", p, "err", err)
		}
		return nil
	})
	if err := os.RemoveAll(dir); err != nil {
		m.Log.Error("remove vm dir", "dir", dir, "err", err)
	}
}

func zeroFile(p string) error {
	f, err := os.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	buf := make([]byte, 4<<20)
	for left := fi.Size(); left > 0; {
		n := int64(len(buf))
		if left < n {
			n = left
		}
		if _, err := f.Write(buf[:n]); err != nil {
			return err
		}
		left -= n
	}
	return f.Sync()
}

// allocated is the space a file really uses on disk.
func allocated(p string) uint64 {
	var st syscall.Stat_t
	if err := syscall.Stat(p, &st); err != nil {
		return 0
	}
	return uint64(st.Blocks) * 512
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
