package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/runcrateai/runcrate-host/internal/api"
	"github.com/runcrateai/runcrate-host/internal/vm"
)

// fakeCP is a fake control plane speaking the host API.
type fakeCP struct {
	t   *testing.T
	srv *httptest.Server
	mu  sync.Mutex
	// pending commands are returned on every heartbeat until a result lands.
	pending    []api.Command
	results    map[int64][]api.CommandResult
	heartbeats []api.HeartbeatRequest
	benchmarks []api.BenchmarkRequest
	failResult int // answer 503 to this many result posts first
	revoked    bool
	auth       []string
}

func newFakeCP(t *testing.T) *fakeCP {
	f := &fakeCP{t: t, results: map[int64][]api.CommandResult{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /heartbeat", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		if f.revoked {
			w.WriteHeader(401)
			return
		}
		var hb api.HeartbeatRequest
		if err := json.NewDecoder(r.Body).Decode(&hb); err != nil {
			t.Errorf("heartbeat body: %v", err)
		}
		f.heartbeats = append(f.heartbeats, hb)
		var cmds []api.Command
		for _, c := range f.pending {
			if len(f.results[c.ID]) == 0 {
				cmds = append(cmds, c)
			}
		}
		_ = json.NewEncoder(w).Encode(api.HeartbeatResponse{PollIntervalS: 10, Commands: cmds})
	})
	mux.HandleFunc("POST /commands/{id}/result", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.failResult > 0 {
			f.failResult--
			w.WriteHeader(503)
			return
		}
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		var res api.CommandResult
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &res); err != nil {
			t.Errorf("result body: %v", err)
		}
		f.results[id] = append(f.results[id], res)
		w.WriteHeader(204)
	})
	mux.HandleFunc("POST /benchmark", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var b api.BenchmarkRequest
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.benchmarks = append(f.benchmarks, b)
		_ = json.NewEncoder(w).Encode(api.BenchmarkResponse{Verified: true})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCP) queue(id int64, kind string, payload any) {
	b, _ := json.Marshal(payload)
	f.mu.Lock()
	f.pending = append(f.pending, api.Command{ID: id, Kind: kind, Payload: b})
	f.mu.Unlock()
}

func (f *fakeCP) resultsFor(id int64) []api.CommandResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]api.CommandResult{}, f.results[id]...)
}

// fakeVMs records what the agent asked the hypervisor.
type fakeVMs struct {
	mu         sync.Mutex
	created    []vm.Spec
	destroyed  []string
	reclaims   map[string]time.Time
	vms        map[string]api.VMStatus
	createErr  error
	createWait chan struct{}
	ephemeral  []string
	ephImage   []string // image version asked of each throwaway VM
	ephScript  []string
	ephOut     []byte
	ephErr     error
	cleared    []int
	held       map[int]bool
}

func newFakeVMs() *fakeVMs {
	return &fakeVMs{reclaims: map[string]time.Time{}, vms: map[string]api.VMStatus{}, held: map[int]bool{}}
}

func (v *fakeVMs) Create(ctx context.Context, s vm.Spec) (string, error) {
	if v.createWait != nil {
		<-v.createWait
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.created = append(v.created, s)
	if v.createErr != nil {
		return "", v.createErr
	}
	v.vms[s.ID] = api.VMStatus{VMID: s.ID, State: "running", GPUIndexes: s.GPUIndexes}
	return "running", nil
}

func (v *fakeVMs) Destroy(ctx context.Context, id string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.destroyed = append(v.destroyed, id)
	delete(v.vms, id)
	return nil
}

func (v *fakeVMs) Reclaim(id string, d time.Time) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, ok := v.vms[id]; !ok {
		return errors.New("vm " + id + " is not on this host")
	}
	v.reclaims[id] = d
	return nil
}

func (v *fakeVMs) List(ctx context.Context) []api.VMStatus {
	v.mu.Lock()
	defer v.mu.Unlock()
	var out []api.VMStatus
	for _, s := range v.vms {
		out = append(out, s)
	}
	return out
}

func (v *fakeVMs) FreeGPUs(bool) []int { return []int{0, 1} }

func (v *fakeVMs) HeldGPUs() map[int]bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.held
}

func (v *fakeVMs) ClearHold(g []int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.cleared = append(v.cleared, g...)
}

func (v *fakeVMs) Hold(g []int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, i := range g {
		v.held[i] = true
	}
}

func (v *fakeVMs) RunEphemeral(ctx context.Context, id, owner, image string, gpus []int, script string, timeout time.Duration, during func(context.Context, string)) ([]byte, string, error) {
	v.mu.Lock()
	v.ephemeral = append(v.ephemeral, id)
	v.ephImage = append(v.ephImage, image)
	v.ephScript = append(v.ephScript, script)
	out, err := v.ephOut, v.ephErr
	v.mu.Unlock()
	if during != nil {
		during(ctx, "10.254.0.2")
	}
	if image == "" {
		image = "20261026-1"
	}
	return out, image, err
}

type fakeHost struct{}

func (fakeHost) GPUs() []api.GPUStatus {
	return []api.GPUStatus{{Index: 0, Health: "ok"}, {Index: 1, Health: "ok"}}
}
func (fakeHost) GPUAddress(i int) string   { return "0000:4" + strconv.Itoa(i) + ":00.0" }
func (fakeHost) GPUUUID(i int) string      { return "" }
func (fakeHost) LearnUUIDs(map[int]string) {}
func (fakeHost) Load() api.Load            { return api.Load{Load1: 0.5, MemTotalGB: 512} }
func (fakeHost) Topology() api.Topology {
	return api.Topology{GPUsPerNode: 2, Interconnect: "unknown", PartialVMOK: true}
}
func (fakeHost) HostID() string                                                   { return "host-a" }
func (fakeHost) Disk() api.DiskBench                                              { return api.DiskBench{WriteMbps: 8000, ReadMbps: 9000} }
func (fakeHost) Net(context.Context) api.NetBench                                 { return api.NetBench{DownMbps: 900, UpMbps: 800} }
func (fakeHost) Peers(context.Context, []api.Peer, api.Topology) []api.PeerResult { return nil }

func newAgent(t *testing.T, cp *fakeCP, vms *fakeVMs, statePath string) *Agent {
	t.Helper()
	c, err := api.New(cp.srv.URL, "cred-123", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	st, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	return &Agent{Version: "1.0.0", API: c, VMs: vms, Host: fakeHost{}, State: st,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// tickUntil heartbeats until cond holds (commands run in the background).
func tickUntil(t *testing.T, a *Agent, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		if err := a.Tick(context.Background()); err != nil && !strings.Contains(err.Error(), "503") {
			t.Fatal(err)
		}
		a.Wait()
	}
}

func TestCreateVMRunsOnceAndReportsDone(t *testing.T) {
	cp := newFakeCP(t)
	vms := newFakeVMs()
	a := newAgent(t, cp, vms, filepath.Join(t.TempDir(), "state.json"))
	cp.queue(7, api.KindCreateVM, map[string]any{
		"vm_id": "vm-1", "gpu_indexes": []int{1}, "vcpus": 8, "ram_gb": 64, "disk_gb": 200,
		"image_version": "v1", "cloud_init": "#!/bin/bash\necho hi\n", "tunnel": map[string]any{"mode": "self_register"},
	})
	// The control plane keeps sending the command while it runs.
	vms.createWait = make(chan struct{})
	for i := 0; i < 3; i++ {
		if err := a.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	close(vms.createWait)
	tickUntil(t, a, func() bool { return len(cp.resultsFor(7)) > 0 })
	for i := 0; i < 3; i++ {
		_ = a.Tick(context.Background())
		a.Wait()
	}
	if len(vms.created) != 1 {
		t.Fatalf("create ran %d times, want 1", len(vms.created))
	}
	s := vms.created[0]
	if s.ID != "vm-1" || s.VCPUs != 8 || s.RAMGB != 64 || s.DiskGB != 200 || s.ImageVersion != "v1" ||
		len(s.GPUIndexes) != 1 || s.GPUIndexes[0] != 1 || !strings.Contains(s.CloudInit, "echo hi") {
		t.Fatalf("spec = %+v", s)
	}
	res := cp.resultsFor(7)
	if len(res) != 1 || res[0].Status != "done" {
		t.Fatalf("results = %+v", res)
	}
	if res[0].RequestID != api.RequestID("command:7") {
		t.Fatalf("request_id %q not stable", res[0].RequestID)
	}
	// The next heartbeat carries the VM.
	last := cp.heartbeats[len(cp.heartbeats)-1]
	if len(last.VMs) != 1 || last.VMs[0].VMID != "vm-1" || last.VMs[0].State != "running" {
		t.Fatalf("heartbeat vms = %+v", last.VMs)
	}
	if cp.auth[0] != "Bearer cred-123" {
		t.Fatalf("auth header = %q", cp.auth[0])
	}
}

func TestFailedCreateReportsFailure(t *testing.T) {
	cp := newFakeCP(t)
	vms := newFakeVMs()
	vms.createErr = errors.New("gpu 1 is in use by vm other")
	a := newAgent(t, cp, vms, filepath.Join(t.TempDir(), "state.json"))
	cp.queue(8, api.KindCreateVM, map[string]any{"vm_id": "vm-2", "gpu_indexes": []int{1}, "vcpus": 4, "ram_gb": 16})
	tickUntil(t, a, func() bool { return len(cp.resultsFor(8)) > 0 })
	r := cp.resultsFor(8)[0]
	if r.Status != "failed" || !strings.Contains(r.Result.(map[string]any)["error"].(string), "in use") {
		t.Fatalf("result = %+v", r)
	}
}

func TestRefusedPayloads(t *testing.T) {
	cp := newFakeCP(t)
	vms := newFakeVMs()
	a := newAgent(t, cp, vms, filepath.Join(t.TempDir(), "state.json"))
	cp.queue(1, api.KindCreateVM, map[string]any{"vm_id": "../../etc", "vcpus": 1, "ram_gb": 1})
	cp.queue(2, "format_disk", map[string]any{})
	cp.queue(3, api.KindCreateVM, map[string]any{"vm_id": "vm-3", "vcpus": 1, "ram_gb": 1, "tunnel": map[string]any{"mode": "inbound"}})
	cp.queue(4, api.KindReclaimNotice, map[string]any{"vm_id": "nope", "deadline_at": time.Now().Add(time.Minute)})
	tickUntil(t, a, func() bool {
		return len(cp.resultsFor(1)) > 0 && len(cp.resultsFor(2)) > 0 && len(cp.resultsFor(3)) > 0 && len(cp.resultsFor(4)) > 0
	})
	for _, id := range []int64{1, 2, 3, 4} {
		if r := cp.resultsFor(id)[0]; r.Status != "failed" {
			t.Fatalf("command %d: status %s, want failed", id, r.Status)
		}
	}
	if len(vms.created) != 0 {
		t.Fatalf("refused commands reached the hypervisor: %+v", vms.created)
	}
}

func TestDestroyReclaimDrain(t *testing.T) {
	cp := newFakeCP(t)
	vms := newFakeVMs()
	vms.vms["vm-9"] = api.VMStatus{VMID: "vm-9", State: "running", GPUIndexes: []int{0}}
	a := newAgent(t, cp, vms, filepath.Join(t.TempDir(), "state.json"))
	deadline := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	cp.queue(10, api.KindReclaimNotice, map[string]any{"vm_id": "vm-9", "deadline_at": deadline})
	tickUntil(t, a, func() bool { return len(cp.resultsFor(10)) > 0 })
	if !vms.reclaims["vm-9"].Equal(deadline) {
		t.Fatalf("reclaim = %v", vms.reclaims)
	}
	cp.queue(11, api.KindDestroyVM, map[string]any{"vm_id": "vm-9", "reason": "customer"})
	cp.queue(12, api.KindDestroyVM, map[string]any{"vm_id": "never-existed", "reason": "cleanup"})
	cp.queue(13, api.KindDrain, map[string]any{})
	tickUntil(t, a, func() bool {
		return len(cp.resultsFor(11)) > 0 && len(cp.resultsFor(12)) > 0 && len(cp.resultsFor(13)) > 0
	})
	for _, id := range []int64{10, 11, 12, 13} {
		if r := cp.resultsFor(id)[0]; r.Status != "done" {
			t.Fatalf("command %d = %+v", id, r)
		}
	}
	_ = a.Tick(context.Background())
	last := cp.heartbeats[len(cp.heartbeats)-1]
	if len(last.VMs) != 0 {
		t.Fatalf("heartbeat after drain/destroy = %+v", last)
	}
}

func TestResultRetriedAfterControlPlaneError(t *testing.T) {
	cp := newFakeCP(t)
	cp.failResult = 2
	vms := newFakeVMs()
	path := filepath.Join(t.TempDir(), "state.json")
	a := newAgent(t, cp, vms, path)
	cp.queue(20, api.KindDrain, map[string]any{})
	if err := a.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	// Both posts so far failed: the result waits in the outbox on disk, and
	// a restarted agent sends it without running the command again.
	if len(cp.resultsFor(20)) != 0 {
		t.Fatal("result should not have landed yet")
	}
	a2 := newAgent(t, cp, vms, path)
	tickUntil(t, a2, func() bool { return len(cp.resultsFor(20)) > 0 })
	if got := len(cp.resultsFor(20)); got != 1 {
		t.Fatalf("result posted %d times", got)
	}
}

func TestUpdateReportsAfterRestart(t *testing.T) {
	cp := newFakeCP(t)
	vms := newFakeVMs()
	path := filepath.Join(t.TempDir(), "state.json")
	a := newAgent(t, cp, vms, path)
	var got api.UpdateAgent
	restarted := false
	a.Update = func(ctx context.Context, u api.UpdateAgent) error { got = u; return nil }
	a.Restart = func() { restarted = true }
	cp.queue(30, api.KindUpdateAgent, map[string]any{"version": "1.1.0", "url": "https://x/runcrate-host", "sha256": strings.Repeat("a", 64)})
	_ = a.Tick(context.Background())
	a.Wait()
	if got.Version != "1.1.0" || !restarted {
		t.Fatalf("update not applied: %+v restarted=%v", got, restarted)
	}
	if len(cp.resultsFor(30)) != 0 {
		t.Fatal("old binary must not report the update")
	}
	// The new binary starts and reports.
	b := newAgent(t, cp, vms, path)
	b.Version = "1.1.0"
	b.Update = func(context.Context, api.UpdateAgent) error { t.Fatal("update ran twice"); return nil }
	b.init()
	b.reportUpdate()
	tickUntil(t, b, func() bool { return len(cp.resultsFor(30)) > 0 })
	r := cp.resultsFor(30)[0]
	if r.Status != "done" || r.Result.(map[string]any)["version"] != "1.1.0" {
		t.Fatalf("update result = %+v", r)
	}
}

func TestUnauthorizedStopsTheLoop(t *testing.T) {
	cp := newFakeCP(t)
	cp.revoked = true
	a := newAgent(t, cp, newFakeVMs(), filepath.Join(t.TempDir(), "state.json"))
	a.UnauthorizedGrace = time.Nanosecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Run(ctx); !errors.Is(err, api.ErrUnauthorized) {
		t.Fatalf("Run = %v, want ErrUnauthorized", err)
	}
}

func TestBenchmarkMapsGPUsAndPosts(t *testing.T) {
	cp := newFakeCP(t)
	vms := newFakeVMs()
	vms.ephOut = []byte(`{"ok":false,"gpus":[
		{"ordinal":0,"name":"NVIDIA H100 80GB HBM3","uuid":"GPU-a","vram_gb":79.6,"mem_ok":true,"mem_tested_gb":79,"matmul_ok":true,"matmul_tflops_fp16":610.5,"nvidia_smi":"q0"},
		{"ordinal":1,"name":"NVIDIA H100 80GB HBM3","uuid":"GPU-b","vram_gb":79.6,"mem_ok":false,"mem_tested_gb":79,"matmul_ok":false,"error":"12 words read back wrong"}],
		"topo":"\tGPU0\tGPU1\nGPU0\t X \tNV18\nGPU1\tNV18\t X \n"}`)
	a := newAgent(t, cp, vms, filepath.Join(t.TempDir(), "state.json"))
	cp.queue(40, api.KindBenchmark, map[string]any{})
	tickUntil(t, a, func() bool { return len(cp.resultsFor(40)) > 0 })
	if len(cp.benchmarks) != 1 {
		t.Fatalf("benchmarks posted: %d", len(cp.benchmarks))
	}
	b := cp.benchmarks[0]
	if len(b.GPUs) != 2 || b.GPUs[0].Index != 0 || b.GPUs[0].UUID != "GPU-a" || b.GPUs[0].VRAMGB != 80 ||
		!b.GPUs[0].MatmulOK || b.GPUs[1].MemOK || b.GPUs[1].Error == "" || b.GPUs[1].PCIeAddress != "0000:41:00.0" {
		t.Fatalf("gpus = %+v", b.GPUs)
	}
	if b.Topology.Interconnect != "nvlink" || !strings.Contains(b.Topology.NVLinkMatrix, "NV18") {
		t.Fatalf("topology = %+v", b.Topology)
	}
	if b.Disk.WriteMbps == 0 || b.Net.DownMbps == 0 {
		t.Fatalf("host parts missing: %+v %+v", b.Disk, b.Net)
	}
	if len(vms.cleared) != 1 || vms.cleared[0] != 0 {
		t.Fatalf("only the passing GPU may be cleared: %v", vms.cleared)
	}
	if !vms.held[1] || vms.held[0] {
		t.Fatalf("the failing GPU must be held: %v", vms.held)
	}
	if r := cp.resultsFor(40)[0]; r.Status != "done" || r.Result.(map[string]any)["verified"] != true {
		t.Fatalf("result = %+v", r)
	}
}

func TestHeldGPUReportsDegraded(t *testing.T) {
	cp := newFakeCP(t)
	vms := newFakeVMs()
	vms.held[1] = true
	a := newAgent(t, cp, vms, filepath.Join(t.TempDir(), "state.json"))
	if err := a.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	g := cp.heartbeats[0].GPUs
	if g[0].Health != "ok" || g[1].Health != "degraded" || g[1].TempC != nil {
		t.Fatalf("gpus = %+v", g)
	}
}

func TestBenchmarkWithAMissingGPUTrustsNoPosition(t *testing.T) {
	cp := newFakeCP(t)
	vms := newFakeVMs()
	// Two GPUs given, one seen: without UUIDs nobody knows which one failed.
	vms.ephOut = []byte(`{"ok":true,"gpus":[{"ordinal":0,"name":"H100","uuid":"GPU-b","vram_gb":80,"mem_ok":true,"matmul_ok":true}]}`)
	a := newAgent(t, cp, vms, filepath.Join(t.TempDir(), "state.json"))
	cp.queue(41, api.KindBenchmark, map[string]any{})
	tickUntil(t, a, func() bool { return len(cp.resultsFor(41)) > 0 })
	b := cp.benchmarks[0]
	for _, g := range b.GPUs {
		if g.MemOK || g.MatmulOK || g.UUID != "" {
			t.Fatalf("a result was pinned to a GPU by position: %+v", g)
		}
	}
	if len(vms.cleared) != 0 || !vms.held[0] || !vms.held[1] {
		t.Fatalf("cleared %v held %v", vms.cleared, vms.held)
	}
}

type fakeImages struct{ asked []string }

func (f *fakeImages) Checked(v string) *api.BenchmarkImage {
	f.asked = append(f.asked, v)
	return &api.BenchmarkImage{Version: v, SHA256OK: true, SignatureOK: true}
}

func TestBenchmarkBootsThePayloadImageAndChecksItsGateways(t *testing.T) {
	cp := newFakeCP(t)
	vms := newFakeVMs()
	vms.ephOut = []byte(`{"ok":true,"gpus":[],"isolation":[{"name":"gateway_iad","ok":true,"detail":"203.0.113.7:443 answers SSH: works"}]}`)
	a := newAgent(t, cp, vms, filepath.Join(t.TempDir(), "state.json"))
	imgs := &fakeImages{}
	a.Images = imgs // a.Isolation stays nil: the gateways are checked anyway
	cp.queue(42, api.KindBenchmark, map[string]any{
		"image_version": "20261101-1",
		"gateways": []map[string]any{
			{"id": "iad", "ipv4": "203.0.113.7", "port": 443},
			{"id": "bad id; reboot", "ipv4": "203.0.113.8", "port": 443},
			{"id": "fra", "ipv4": "203.0.113.9 && reboot", "port": 443},
		},
	})
	tickUntil(t, a, func() bool { return len(cp.resultsFor(42)) > 0 })
	if len(vms.ephImage) != 1 || vms.ephImage[0] != "20261101-1" {
		t.Fatalf("benchmark vm image = %v, want 20261101-1", vms.ephImage)
	}
	script := vms.ephScript[0]
	if !strings.Contains(script, "must gateway_iad \"203.0.113.7:443 answers SSH\" ssh_banner 203.0.113.7 443\n") {
		t.Fatalf("script lacks the iad gateway check:\n%s", script)
	}
	for _, bad := range []string{"reboot", "203.0.113.8", "203.0.113.9", "gateway_fra"} {
		if strings.Contains(script, bad) {
			t.Fatalf("script carries %q from an invalid gateway", bad)
		}
	}
	b := cp.benchmarks[0]
	if b.Image == nil || b.Image.Version != "20261101-1" || len(imgs.asked) != 1 {
		t.Fatalf("image = %+v, asked %v", b.Image, imgs.asked)
	}
	if b.Isolation == nil || len(b.Isolation.Checks) != 1 || b.Isolation.Checks[0].Name != "gateway_iad" || !b.Isolation.Checks[0].OK {
		t.Fatalf("isolation = %+v", b.Isolation)
	}
}
