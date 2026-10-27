// Package agent is the host agent's loop: heartbeat every poll interval,
// run the commands that come back, and report each result exactly once.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/runcrateai/runcrate-host/internal/api"
	"github.com/runcrateai/runcrate-host/internal/release"
	"github.com/runcrateai/runcrate-host/internal/vm"
)

// API is the part of the host API the loop uses.
type API interface {
	Heartbeat(ctx context.Context, req api.HeartbeatRequest) (*api.HeartbeatResponse, error)
	Result(ctx context.Context, commandID int64, res api.CommandResult) error
	Benchmark(ctx context.Context, req api.BenchmarkRequest) (*api.BenchmarkResponse, error)
}

// VMs is the hypervisor (vm.Manager in production).
type VMs interface {
	Create(ctx context.Context, s vm.Spec) (string, error)
	Destroy(ctx context.Context, id string) error
	Reclaim(id string, deadline time.Time) error
	List(ctx context.Context) []api.VMStatus
	FreeGPUs(withHeld bool) []int
	HeldGPUs() map[int]bool
	ClearHold(gpus []int)
	Hold(gpus []int)
	RunEphemeral(ctx context.Context, id, owner string, gpus []int, script string, timeout time.Duration, during func(ctx context.Context, guestIP string)) ([]byte, string, error)
}

// Isolation is the host side of the benchmark's isolation checks.
type Isolation interface {
	// IsolationTargets are host addresses the benchmark VM must not reach.
	IsolationTargets() vm.IsolationTargets
	// LANToVM checks a LAN machine can't open a connection to the VM.
	LANToVM(ctx context.Context, guestIP string) api.IsolationCheck
}

// Images describes a local base image and how it was checked (nil: none).
type Images interface {
	Checked(version string) *api.BenchmarkImage
}

// Host reads the machine.
type Host interface {
	GPUs() []api.GPUStatus
	GPUAddress(index int) string
	// GPUUUID is the GPU's UUID when known (from the host driver or an
	// earlier benchmark), to match benchmark results to indexes.
	GPUUUID(index int) string
	// LearnUUIDs records GPU UUIDs seen by a benchmark, to match later
	// scrubs and benchmarks exactly.
	LearnUUIDs(byIndex map[int]string)
	Load() api.Load
	Topology() api.Topology
	HostID() string
	// Disk, Net and Peers are the host side of a benchmark.
	Disk() api.DiskBench
	Net(ctx context.Context) api.NetBench
	Peers(ctx context.Context, peers []api.Peer, topo api.Topology) []api.PeerResult
}

// Agent runs the loop.
type Agent struct {
	Version string
	API     API
	VMs     VMs
	Host    Host
	State   *State
	Log     *slog.Logger
	// Update installs a new agent binary; the agent then exits so systemd
	// starts the new one.
	Update func(ctx context.Context, u api.UpdateAgent) error
	// Restart is called after a successful update (cancel the run context).
	Restart func()
	// UnauthorizedGrace is how long 401s must last before the agent stops
	// (default 30 minutes).
	UnauthorizedGrace time.Duration
	// DevGPUs is set only in devnopassthrough builds with the switch on: the
	// benchmark VM boots without GPUs, so each GPU is reported as the host
	// sees it (name, UUID, VRAM) with its checks marked passed.
	DevGPUs func(index int) (name, uuid string, vramGB int)
	// Isolation and Images feed the benchmark's isolation checks and image
	// report; either may be nil (the report then leaves that part out).
	Isolation Isolation
	Images    Images

	interval time.Duration
	flushMu  sync.Mutex
	mu       sync.Mutex
	inflight map[int64]bool
	locks    map[string]*sync.Mutex
	wake     chan struct{}
	wg       sync.WaitGroup
}

// MinInterval and MaxInterval bound the control plane's poll_interval_s.
const (
	DefaultInterval = 10 * time.Second
	MinInterval     = 2 * time.Second
	MaxInterval     = 5 * time.Minute
)

func (a *Agent) unauthorizedGrace() time.Duration {
	if a.UnauthorizedGrace > 0 {
		return a.UnauthorizedGrace
	}
	return 30 * time.Minute
}

func (a *Agent) init() {
	if a.inflight == nil {
		a.inflight = map[int64]bool{}
		a.locks = map[string]*sync.Mutex{}
		a.wake = make(chan struct{}, 1)
	}
	if a.interval == 0 {
		a.interval = DefaultInterval
	}
}

// Run loops until ctx ends or the credential is rejected
// (api.ErrUnauthorized).
func (a *Agent) Run(ctx context.Context) error {
	a.init()
	a.reportUpdate()
	backoff := time.Second
	var rejectedSince time.Time
	for {
		err := a.Tick(ctx)
		switch {
		case errors.Is(err, api.ErrUnauthorized):
			// A revoked credential stays rejected; a control-plane blip
			// doesn't. Give up only after UnauthorizedGrace of rejections.
			if rejectedSince.IsZero() {
				rejectedSince = time.Now()
			}
			if time.Since(rejectedSince) >= a.unauthorizedGrace() {
				a.wg.Wait()
				return err
			}
			a.Log.Warn("credential rejected; retrying", "since", rejectedSince)
			backoff = min(time.Minute, a.unauthorizedGrace())
		case err != nil:
			rejectedSince = time.Time{}
			a.Log.Warn("heartbeat failed", "err", err)
			// Back off on errors, never slower than the max interval.
			backoff = min(backoff*2, MaxInterval)
		default:
			rejectedSince = time.Time{}
			backoff = time.Second
		}
		wait := a.interval
		if err != nil {
			wait = backoff
			if !errors.Is(err, api.ErrUnauthorized) {
				wait = min(a.interval, backoff)
			}
		}
		select {
		case <-ctx.Done():
			a.wg.Wait()
			return nil
		case <-a.wake:
		case <-time.After(wait):
		}
	}
}

// Wait waits for running commands (tests).
func (a *Agent) Wait() { a.wg.Wait() }

// Tick sends one heartbeat, flushes unsent results and starts new commands.
func (a *Agent) Tick(ctx context.Context) error {
	a.init()
	load := a.Host.Load()
	gpus := a.Host.GPUs()
	held := a.VMs.HeldGPUs()
	for i := range gpus {
		if held[gpus[i].Index] && gpus[i].Health == "ok" {
			gpus[i].Health = "degraded"
		}
	}
	resp, err := a.API.Heartbeat(ctx, api.HeartbeatRequest{
		At:           time.Now().UTC(),
		AgentVersion: a.Version,
		GPUs:         gpus,
		VMs:          a.VMs.List(ctx),
		Load:         load,
	})
	if err != nil {
		return err
	}
	if s := resp.PollIntervalS; s > 0 {
		a.interval = min(max(time.Duration(s)*time.Second, MinInterval), MaxInterval)
	}
	if err := a.flush(ctx); errors.Is(err, api.ErrUnauthorized) {
		return err
	}
	for _, c := range resp.Commands {
		a.dispatch(ctx, c)
	}
	return nil
}

// dispatch starts a command unless it's running, finished, or waiting in
// the outbox.
func (a *Agent) dispatch(ctx context.Context, c api.Command) {
	var known bool
	a.State.view(func(s *State) {
		_, known = s.Done[c.ID]
		for _, o := range s.Outbox {
			known = known || o.CommandID == c.ID
		}
		if s.PendingUpdate != nil && s.PendingUpdate.CommandID == c.ID {
			known = true
		}
	})
	a.mu.Lock()
	if known || a.inflight[c.ID] {
		a.mu.Unlock()
		return
	}
	a.inflight[c.ID] = true
	a.mu.Unlock()
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer func() {
			a.mu.Lock()
			delete(a.inflight, c.ID)
			a.mu.Unlock()
		}()
		a.Log.Info("command", "id", c.ID, "kind", c.Kind)
		result, err := a.handle(ctx, c)
		if errors.Is(err, errRestarting) {
			return // the new binary reports it
		}
		if ctx.Err() != nil && err != nil {
			return // shutting down: the command comes back after the restart
		}
		res := api.CommandResult{RequestID: api.RequestID("command:" + strconv.FormatInt(c.ID, 10)), Status: "done", Result: result}
		if err != nil {
			a.Log.Error("command failed", "id", c.ID, "kind", c.Kind, "err", err)
			res.Status = "failed"
			res.Result = map[string]any{"error": err.Error()}
		}
		if err := a.State.update(func(s *State) { s.Outbox = append(s.Outbox, Outgoing{CommandID: c.ID, Result: res}) }); err != nil {
			a.Log.Error("save result", "id", c.ID, "err", err)
		}
		_ = a.flush(context.WithoutCancel(ctx))
		select {
		case a.wake <- struct{}{}: // report the new VM state now
		default:
		}
	}()
}

// flush posts every outstanding result; accepted ones move to Done.
func (a *Agent) flush(ctx context.Context) error {
	a.flushMu.Lock()
	defer a.flushMu.Unlock()
	var out []Outgoing
	a.State.view(func(s *State) { out = append(out, s.Outbox...) })
	var firstErr error
	for _, o := range out {
		err := a.API.Result(ctx, o.CommandID, o.Result)
		var he *api.HTTPError
		if errors.As(err, &he) && (he.Status == 400 || he.Status == 413) && o.Result.Status == "done" {
			// The control plane refused the body: settle the command as failed
			// with a short reason instead of leaving it pending.
			short := api.CommandResult{RequestID: o.Result.RequestID, Status: "failed",
				Result: map[string]any{"error": fmt.Sprintf("result refused by the control plane (HTTP %d)", he.Status)}}
			err = a.API.Result(ctx, o.CommandID, short)
		}
		switch {
		case err == nil, errors.As(err, &he) && !he.Retryable() && he.Status != 408:
			// Accepted, or refused for good (unknown command): stop retrying.
			if err != nil {
				a.Log.Error("result refused", "id", o.CommandID, "err", err)
			}
			_ = a.State.update(func(s *State) {
				s.Done[o.CommandID] = time.Now()
				for i := range s.Outbox {
					if s.Outbox[i].CommandID == o.CommandID {
						s.Outbox = append(s.Outbox[:i], s.Outbox[i+1:]...)
						break
					}
				}
			})
		default:
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// reportUpdate turns a pending update into a result once the new binary runs.
func (a *Agent) reportUpdate() {
	var p *PendingUpdate
	a.State.view(func(s *State) { p = s.PendingUpdate })
	if p == nil {
		return
	}
	res := api.CommandResult{RequestID: api.RequestID("command:" + strconv.FormatInt(p.CommandID, 10)), Status: "done", Result: map[string]any{"version": a.Version}}
	if a.Version != p.Version {
		res.Status = "failed"
		res.Result = map[string]any{"error": fmt.Sprintf("still running %s after updating to %s", a.Version, p.Version)}
	}
	_ = a.State.update(func(s *State) {
		s.PendingUpdate = nil
		s.Outbox = append(s.Outbox, Outgoing{CommandID: p.CommandID, Result: res})
	})
}

func (a *Agent) lock(key string) func() {
	a.mu.Lock()
	l, ok := a.locks[key]
	if !ok {
		l = &sync.Mutex{}
		a.locks[key] = l
	}
	a.mu.Unlock()
	l.Lock()
	return l.Unlock
}

var errRestarting = errors.New("restarting into the new agent")

func decode[T any](raw json.RawMessage) (T, error) {
	var v T
	if len(raw) == 0 || string(raw) == "null" {
		return v, nil
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, fmt.Errorf("bad payload: %w", err)
	}
	return v, nil
}

func (a *Agent) handle(ctx context.Context, c api.Command) (any, error) {
	switch c.Kind {
	case api.KindCreateVM:
		p, err := decode[api.CreateVM](c.Payload)
		if err != nil {
			return nil, err
		}
		if !vm.ValidID(p.VMID) || vm.Reserved(p.VMID) {
			return nil, fmt.Errorf("bad vm_id %q", p.VMID)
		}
		if p.Tunnel.Mode != "" && p.Tunnel.Mode != "self_register" {
			return nil, fmt.Errorf("tunnel mode %q is not supported", p.Tunnel.Mode)
		}
		defer a.lock("vm:" + p.VMID)()
		state, err := a.VMs.Create(ctx, vm.Spec{
			ID: p.VMID, GPUIndexes: p.GPUIndexes, VCPUs: p.VCPUs, RAMGB: p.RAMGB, DiskGB: p.DiskGB,
			ImageVersion: p.ImageVersion, CloudInit: p.CloudInit,
		})
		if err != nil {
			return nil, err
		}
		return map[string]any{"vm_id": p.VMID, "state": state}, nil

	case api.KindDestroyVM:
		p, err := decode[api.DestroyVM](c.Payload)
		if err != nil {
			return nil, err
		}
		if !vm.ValidID(p.VMID) {
			return nil, fmt.Errorf("bad vm_id %q", p.VMID)
		}
		defer a.lock("vm:" + p.VMID)()
		if err := a.VMs.Destroy(ctx, p.VMID); err != nil {
			return nil, err
		}
		return map[string]any{"vm_id": p.VMID, "wiped": true}, nil

	case api.KindReclaimNotice:
		p, err := decode[api.ReclaimNotice](c.Payload)
		if err != nil {
			return nil, err
		}
		if p.DeadlineAt.IsZero() {
			return nil, errors.New("deadline_at is required")
		}
		defer a.lock("vm:" + p.VMID)()
		if err := a.VMs.Reclaim(p.VMID, p.DeadlineAt); err != nil {
			return nil, err
		}
		return map[string]any{"vm_id": p.VMID, "deadline_at": p.DeadlineAt.UTC().Format(time.RFC3339)}, nil

	case api.KindDrain:
		// The control plane stops placing rentals; running VMs are untouched.
		return map[string]any{}, nil

	case api.KindUpdateAgent:
		p, err := decode[api.UpdateAgent](c.Payload)
		if err != nil {
			return nil, err
		}
		if p.Version == "" || p.URL == "" || p.SHA256 == "" {
			return nil, errors.New("version, url and sha256 are required")
		}
		if p.Version == a.Version {
			return map[string]any{"version": a.Version}, nil
		}
		if newer, err := release.Newer(p.Version, a.Version); err != nil || !newer {
			return nil, fmt.Errorf("refusing to replace %s with %s: not a newer release", a.Version, p.Version)
		}
		defer a.lock("host")()
		if err := a.Update(ctx, p); err != nil {
			return nil, err
		}
		if err := a.State.update(func(s *State) { s.PendingUpdate = &PendingUpdate{CommandID: c.ID, Version: p.Version} }); err != nil {
			return nil, err
		}
		a.Log.Info("updated agent; restarting", "version", p.Version)
		if a.Restart != nil {
			a.Restart()
		}
		return nil, errRestarting

	case api.KindBenchmark:
		p, err := decode[api.Benchmark](c.Payload)
		if err != nil {
			return nil, err
		}
		defer a.lock("host")()
		return a.benchmark(ctx, c.ID, p)
	}
	return nil, fmt.Errorf("unknown command kind %q", c.Kind)
}

// vmGPU is one GPU result from the benchmark VM.
type vmGPU struct {
	Ordinal          int     `json:"ordinal"`
	Name             string  `json:"name"`
	UUID             string  `json:"uuid"`
	VRAMGB           float64 `json:"vram_gb"`
	MemOK            bool    `json:"mem_ok"`
	MemTestedGB      float64 `json:"mem_tested_gb"`
	MatmulOK         bool    `json:"matmul_ok"`
	MatmulTFLOPSFP16 float64 `json:"matmul_tflops_fp16"`
	NvidiaSMI        string  `json:"nvidia_smi"`
	Error            string  `json:"error"`
}

type vmBench struct {
	OK        bool                 `json:"ok"`
	Error     string               `json:"error"`
	GPUs      []vmGPU              `json:"gpus"`
	Topo      string               `json:"topo"`
	NVLink    string               `json:"nvlink"`
	Stderr    string               `json:"stderr"`
	Isolation []api.IsolationCheck `json:"isolation"`
}

// isolationChecks merges the VM's checks with the host's LAN probe, bounded
// to what the control plane stores (32 checks, names 64, details 200).
func isolationChecks(fromVM []api.IsolationCheck, lan *api.IsolationCheck) []api.IsolationCheck {
	all := fromVM
	if lan != nil {
		all = append(all[:len(all):len(all)], *lan)
	}
	var out []api.IsolationCheck
	for _, c := range all {
		if c.Name == "" || len(out) == 32 {
			continue
		}
		out = append(out, api.IsolationCheck{Name: clip(c.Name, 64), OK: c.OK, Detail: clip(c.Detail, 200)})
	}
	return out
}

// benchmark runs the GPU checks in a throwaway VM while the host measures
// disk, internet and same-site peers, then reports it all.
func (a *Agent) benchmark(ctx context.Context, cmdID int64, p api.Benchmark) (any, error) {
	topo := a.Host.Topology()
	req := api.BenchmarkRequest{RequestID: api.RequestID("benchmark:" + strconv.FormatInt(cmdID, 10))}
	var wg sync.WaitGroup
	var disk api.DiskBench
	var netb api.NetBench
	wg.Add(1)
	go func(topo api.Topology) {
		defer wg.Done()
		disk = a.Host.Disk()
		netb = a.Host.Net(ctx)
		netb.Peers = a.Host.Peers(ctx, p.Peers, topo)
	}(topo)
	free := a.VMs.FreeGPUs(true)
	sort.Ints(free)
	var gpuErr string
	var passed, failed []int
	if len(free) > 0 {
		var targets vm.IsolationTargets
		var during func(context.Context, string)
		var lan *api.IsolationCheck
		if a.Isolation != nil {
			targets = a.Isolation.IsolationTargets()
			during = func(ctx context.Context, guestIP string) {
				c := a.Isolation.LANToVM(ctx, guestIP)
				lan = &c
			}
		}
		out, image, err := a.VMs.RunEphemeral(ctx, "bench-"+strconv.FormatInt(cmdID, 10), "", free, vm.BenchScript(targets), 30*time.Minute, during)
		if a.Images != nil && image != "" {
			req.Image = a.Images.Checked(image)
		}
		var vb vmBench
		switch {
		case err != nil:
			gpuErr = err.Error()
		case json.Unmarshal(out, &vb) != nil:
			gpuErr = "benchmark vm returned unreadable output"
		}
		if vb.Error != "" {
			gpuErr = vb.Error
		}
		if checks := isolationChecks(vb.Isolation, lan); len(checks) > 0 {
			req.Isolation = &api.Isolation{Checks: checks}
		}
		if a.DevGPUs != nil && err == nil {
			// The VM booted and ran the script; there are no GPUs in it.
			gpuErr, vb.GPUs = "", nil
			for _, idx := range free {
				name, uuid, vram := a.DevGPUs(idx)
				vb.GPUs = append(vb.GPUs, vmGPU{Ordinal: len(vb.GPUs), Name: name, UUID: uuid, VRAMGB: float64(vram),
					MemOK: true, MatmulOK: true, NvidiaSMI: "devnopassthrough: not measured"})
			}
		}
		for idx, v := range a.matchGPUs(free, vb.GPUs) {
			g := api.BenchmarkGPU{Index: idx, PCIeAddress: a.Host.GPUAddress(idx), Error: gpuErr}
			if v != nil {
				g = api.BenchmarkGPU{
					Index: idx, Name: v.Name, UUID: v.UUID, VRAMGB: int(v.VRAMGB + 0.5), PCIeAddress: a.Host.GPUAddress(idx),
					MemOK: v.MemOK, MemTestedGB: v.MemTestedGB, MatmulTFLOPSFP16: v.MatmulTFLOPSFP16, MatmulOK: v.MatmulOK,
					NvidiaSMI: clip(v.NvidiaSMI, smiCap(len(free))), Error: clip(v.Error, 500),
				}
			} else if gpuErr == "" {
				g.Error = "gpu not visible inside the benchmark vm"
			}
			switch {
			case g.MemOK && g.MatmulOK:
				passed = append(passed, idx)
			case gpuErr == "":
				// The VM ran and this GPU failed its checks (not a boot or
				// image problem, which says nothing about the GPU).
				failed = append(failed, idx)
			}
			req.GPUs = append(req.GPUs, g)
		}
		sort.Slice(req.GPUs, func(i, j int) bool { return req.GPUs[i].Index < req.GPUs[j].Index })
		learned := map[int]string{}
		for _, g := range req.GPUs {
			if g.UUID != "" && a.Host.GPUUUID(g.Index) == "" {
				learned[g.Index] = g.UUID
			}
		}
		if len(learned) > 0 {
			a.Host.LearnUUIDs(learned)
		}
		if vb.Topo != "" {
			topo.NVLinkMatrix = clip(vb.Topo, maxTopo)
			if topo.Interconnect != "nvswitch" {
				topo.Interconnect = interconnectFrom(vb.Topo)
			}
		}
	}
	wg.Wait()
	req.Disk, req.Net = disk, netb
	req.Topology = topo
	req.Disk.Error, req.Net.Error = clip(req.Disk.Error, 500), clip(req.Net.Error, 500)
	// GPUs that fail stay out of rentals until a later benchmark passes.
	a.VMs.Hold(failed)
	a.VMs.ClearHold(passed)
	var resp *api.BenchmarkResponse
	var err error
	for attempt := 1; ; attempt++ {
		resp, err = a.API.Benchmark(ctx, req)
		var he *api.HTTPError
		if err == nil || errors.Is(err, api.ErrUnauthorized) || (errors.As(err, &he) && !he.Retryable()) || attempt == 5 || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Duration(attempt*attempt) * 5 * time.Second):
		}
	}
	if err != nil {
		return nil, fmt.Errorf("post benchmark: %w", err)
	}
	return map[string]any{
		"verified": resp.Verified,
		"reasons":  resp.Reasons,
		"gpus":     len(req.GPUs),
		"gpus_ok":  len(passed),
		"disk":     req.Disk,
		"net":      req.Net,
		"error":    gpuErr,
	}, nil
}

// Size caps keep a benchmark well under the control plane's 64 KiB body
// limit: the nvidia-smi text shares 32 KiB across the GPUs.
const (
	maxSMIPerGPU = 4000
	maxTopo      = 6000
)

func smiCap(gpus int) int {
	if gpus <= 0 {
		return maxSMIPerGPU
	}
	return min(maxSMIPerGPU, 32<<10/gpus)
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// matchGPUs pairs each benchmarked index with its result: by UUID when the
// host knows it, otherwise by order (the VM sees the GPUs in index order),
// but only when the VM saw exactly the GPUs it was given. Unmatched indexes
// map to nil.
func (a *Agent) matchGPUs(free []int, res []vmGPU) map[int]*vmGPU {
	out := make(map[int]*vmGPU, len(free))
	if len(res) != len(free) {
		known := 0
		for _, idx := range free {
			if a.Host.GPUUUID(idx) != "" {
				known++
			}
		}
		if known != len(free) {
			// Order says nothing when a GPU is missing: trust none.
			for _, idx := range free {
				out[idx] = nil
			}
			for _, idx := range free {
				if u := a.Host.GPUUUID(idx); u != "" {
					for i := range res {
						if strings.EqualFold(res[i].UUID, u) {
							out[idx] = &res[i]
						}
					}
				}
			}
			return out
		}
	}
	used := make([]bool, len(res))
	for _, idx := range free {
		out[idx] = nil
		if u := a.Host.GPUUUID(idx); u != "" {
			for i := range res {
				if !used[i] && strings.EqualFold(res[i].UUID, u) {
					out[idx], used[i] = &res[i], true
					break
				}
			}
		}
	}
	next := 0
	for _, idx := range free {
		if out[idx] != nil {
			continue
		}
		for next < len(res) && used[next] {
			next++
		}
		if next < len(res) {
			out[idx], used[next] = &res[next], true
			next++
		}
	}
	return out
}

// interconnectFrom reads `nvidia-smi topo -m`: any NV# link means NVLink.
func interconnectFrom(topo string) string {
	for _, f := range strings.Fields(topo) {
		if len(f) > 2 && strings.HasPrefix(f, "NV") {
			if _, err := strconv.Atoi(f[2:]); err == nil {
				return "nvlink"
			}
		}
	}
	return "pcie"
}
