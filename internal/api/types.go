// Package api is the client for the Runcrate host API
// (https://www.runcrate.ai/api/partner-hosts/v1). The host only ever calls
// out; nothing here listens.
package api

import (
	"encoding/json"
	"time"
)

// DefaultBaseURL is the production host API.
const DefaultBaseURL = "https://www.runcrate.ai/api/partner-hosts/v1"

// Command kinds, exactly as the control plane sends them.
const (
	KindCreateVM      = "create_vm"
	KindDestroyVM     = "destroy_vm"
	KindReclaimNotice = "reclaim_notice"
	KindDrain         = "drain"
	KindUpdateAgent   = "update_agent"
	KindBenchmark     = "benchmark"
)

// RegisterGPU describes one GPU at registration.
type RegisterGPU struct {
	Index       int    `json:"index"`
	ModelRaw    string `json:"model_raw"`
	VRAMGB      int    `json:"vram_gb"`
	UUID        string `json:"uuid"`
	PCIeAddress string `json:"pcie_address"`
}

// CPUSpec is the host CPU summary.
type CPUSpec struct {
	Model   string `json:"model"`
	Cores   int    `json:"cores"`
	Threads int    `json:"threads"`
}

// GPUSummary is the GPU part of the host specs.
type GPUSummary struct {
	Count  int      `json:"count"`
	Models []string `json:"models"`
}

// Specs is the host hardware summary sent at registration.
type Specs struct {
	CPU      CPUSpec    `json:"cpu"`
	RAMGB    int        `json:"ram_gb"`
	DiskGB   int        `json:"disk_gb"`
	NetMbps  *int       `json:"net_mbps"`
	GPUs     GPUSummary `json:"gpus"`
	Topology Topology   `json:"topology"`
}

// RegisterHost is the host block of a register request.
type RegisterHost struct {
	Name         string        `json:"name"`
	Specs        Specs         `json:"specs"`
	GPUs         []RegisterGPU `json:"gpus"`
	AgentVersion string        `json:"agent_version"`
	PublicIPMode string        `json:"public_ip_mode"`
	Site         *string       `json:"site"`
}

// RegisterRequest is POST /register.
type RegisterRequest struct {
	RegistrationToken string       `json:"registration_token"`
	RequestID         string       `json:"request_id"`
	Host              RegisterHost `json:"host"`
}

// RegisterResponse is the answer to POST /register.
type RegisterResponse struct {
	HostID         string `json:"host_id"`
	HostCredential string `json:"host_credential"`
	PollIntervalS  int    `json:"poll_interval_s"`
}

// GPUStatus is one GPU in a heartbeat. Telemetry is nil while the GPU is
// bound to vfio-pci, because the host has no driver to read it with.
type GPUStatus struct {
	Index     int      `json:"index"`
	Health    string   `json:"health"`
	TempC     *float64 `json:"temp_c"`
	UtilPct   *float64 `json:"util_pct"`
	MemUsedMB *float64 `json:"mem_used_mb"`
}

// VMStatus is one VM in a heartbeat.
type VMStatus struct {
	VMID       string `json:"vm_id"`
	State      string `json:"state"`
	GPUIndexes []int  `json:"gpu_indexes"`
}

// Load is the host load block of a heartbeat.
type Load struct {
	Load1      float64 `json:"load1"`
	MemUsedGB  float64 `json:"mem_used_gb"`
	MemTotalGB float64 `json:"mem_total_gb"`
	DiskFreeGB float64 `json:"disk_free_gb"`
}

// HeartbeatRequest is POST /heartbeat.
type HeartbeatRequest struct {
	At           time.Time   `json:"at"`
	AgentVersion string      `json:"agent_version"`
	GPUs         []GPUStatus `json:"gpus"`
	VMs          []VMStatus  `json:"vms"`
	Load         Load        `json:"load"`
}

// Command is one queued command carried by a heartbeat answer.
type Command struct {
	ID      int64           `json:"id"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

// HeartbeatResponse is the answer to POST /heartbeat.
type HeartbeatResponse struct {
	PollIntervalS int       `json:"poll_interval_s"`
	Commands      []Command `json:"commands"`
}

// CommandResult is POST /commands/{id}/result.
type CommandResult struct {
	RequestID string `json:"request_id"`
	Status    string `json:"status"` // done | failed
	Result    any    `json:"result"`
}

// BenchmarkGPU is one GPU's result from the throwaway benchmark VM.
type BenchmarkGPU struct {
	Index            int     `json:"index"`
	Name             string  `json:"name"`
	UUID             string  `json:"uuid"`
	VRAMGB           int     `json:"vram_gb"`
	PCIeAddress      string  `json:"pcie_address"`
	MemOK            bool    `json:"mem_ok"`
	MemTestedGB      float64 `json:"mem_tested_gb"`
	MatmulTFLOPSFP16 float64 `json:"matmul_tflops_fp16"`
	MatmulOK         bool    `json:"matmul_ok"`
	NvidiaSMI        string  `json:"nvidia_smi"`
	Error            string  `json:"error,omitempty"`
}

// DiskBench is the host disk result.
type DiskBench struct {
	WriteMbps float64 `json:"write_mbps"`
	ReadMbps  float64 `json:"read_mbps"`
	Error     string  `json:"error,omitempty"`
}

// NetBench is the host network result.
type NetBench struct {
	DownMbps float64      `json:"down_mbps"`
	UpMbps   float64      `json:"up_mbps"`
	Peers    []PeerResult `json:"peers"`
	Error    string       `json:"error,omitempty"`
}

// BenchmarkRequest is POST /benchmark.
type BenchmarkRequest struct {
	RequestID string         `json:"request_id"`
	GPUs      []BenchmarkGPU `json:"gpus"`
	Disk      DiskBench      `json:"disk"`
	Net       NetBench       `json:"net"`
	Topology  Topology       `json:"topology"`
	// Image is the VM image the benchmark VM booted, with its checks.
	Image *BenchmarkImage `json:"image,omitempty"`
	// Isolation is what the benchmark VM could and could not reach.
	Isolation *Isolation `json:"isolation,omitempty"`
}

// BenchmarkImage is a base image on the host and how it was checked.
type BenchmarkImage struct {
	Version     string `json:"version"`
	SHA256OK    bool   `json:"sha256_ok"`
	SignatureOK bool   `json:"signature_ok"`
	ReadyAt     string `json:"ready_at"` // RFC 3339
}

// Isolation is the benchmark's network isolation checks (at most 32).
type Isolation struct {
	Checks []IsolationCheck `json:"checks"`
}

// IsolationCheck is one check: a stable snake_case name (at most 64
// characters), whether it passed, and what was seen (at most 200).
type IsolationCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// BenchmarkResponse is the answer to POST /benchmark.
type BenchmarkResponse struct {
	Verified bool     `json:"verified"`
	Reasons  []string `json:"reasons"`
}

// Image is the answer to GET /image.
type Image struct {
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
	Version string `json:"version"`
}

// Payloads of the commands.

// CreateVM is the create_vm payload.
type CreateVM struct {
	VMID         string `json:"vm_id"`
	GPUIndexes   []int  `json:"gpu_indexes"`
	VCPUs        int    `json:"vcpus"`
	RAMGB        int    `json:"ram_gb"`
	DiskGB       int    `json:"disk_gb"`
	ImageVersion string `json:"image_version"`
	CloudInit    string `json:"cloud_init"`
	Tunnel       struct {
		Mode string `json:"mode"`
	} `json:"tunnel"`
}

// DestroyVM is the destroy_vm payload.
type DestroyVM struct {
	VMID   string `json:"vm_id"`
	Reason string `json:"reason"`
}

// ReclaimNotice is the reclaim_notice payload.
type ReclaimNotice struct {
	VMID       string    `json:"vm_id"`
	DeadlineAt time.Time `json:"deadline_at"`
}

// UpdateAgent is the update_agent payload.
type UpdateAgent struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
}

// NIC is one physical network interface in the topology.
type NIC struct {
	Name       string  `json:"name"`
	Type       string  `json:"type"` // infiniband | roce | ethernet
	SpeedGbps  float64 `json:"speed_gbps"`
	RDMADevice string  `json:"rdma_device"`
	LinkUp     bool    `json:"link_up"`
}

// Topology is specs.topology: what the capacity desk needs to know about how
// the GPUs connect inside the node and how the node connects to its site.
type Topology struct {
	GPUsPerNode   int    `json:"gpus_per_node"`
	Interconnect  string `json:"interconnect"` // nvswitch | nvlink | pcie | unknown
	NVSwitchCount int    `json:"nvswitch_count"`
	NVLinkMatrix  string `json:"nvlink_matrix"`
	// PartialVMOK is false on NVSwitch nodes: the switches and Fabric Manager
	// go into the VM with every GPU, so the node is only rented whole.
	PartialVMOK  bool     `json:"partial_vm_ok"`
	NICs         []NIC    `json:"nics"`
	PrivateAddrs []string `json:"private_addrs"`
}

// PeerResult is one same-site host-to-host measurement.
type PeerResult struct {
	HostID        string   `json:"host_id"`
	Addr          string   `json:"addr"`
	Reachable     bool     `json:"reachable"`
	IperfGbps     float64  `json:"iperf_gbps"`
	IBWriteBWGbps *float64 `json:"ib_write_bw_gbps"`
	Error         string   `json:"error,omitempty"`
}

// Peer is another online host on the same site, from the benchmark payload.
type Peer struct {
	HostID       string   `json:"host_id"`
	PrivateAddrs []string `json:"private_addrs"`
}

// Benchmark is the benchmark payload. ImageVersion is the VM image the
// benchmark VM must boot ("" = whatever is on the host); Gateways are the
// Runcrate tunnel gateways it must reach.
type Benchmark struct {
	Peers        []Peer    `json:"peers"`
	ImageVersion string    `json:"image_version,omitempty"`
	Gateways     []Gateway `json:"gateways,omitempty"`
}

// Gateway is a Runcrate tunnel gateway customers are reached through: an id
// matching ^[a-z0-9-]{1,32}$, its IPv4 address and the port its SSH answers on.
type Gateway struct {
	ID   string `json:"id"`
	IPv4 string `json:"ipv4"`
	Port int    `json:"port"`
}
