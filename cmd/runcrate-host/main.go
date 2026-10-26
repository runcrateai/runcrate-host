// Command runcrate-host turns a bare-metal GPU machine into a Runcrate host.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/runcrateai/runcrate-host/internal/agent"
	"github.com/runcrateai/runcrate-host/internal/api"
	"github.com/runcrateai/runcrate-host/internal/bench"
	"github.com/runcrateai/runcrate-host/internal/config"
	"github.com/runcrateai/runcrate-host/internal/devmode"
	"github.com/runcrateai/runcrate-host/internal/hw"
	"github.com/runcrateai/runcrate-host/internal/metadata"
	"github.com/runcrateai/runcrate-host/internal/release"
	"github.com/runcrateai/runcrate-host/internal/sysexec"
	"github.com/runcrateai/runcrate-host/internal/update"
	"github.com/runcrateai/runcrate-host/internal/vfio"
	"github.com/runcrateai/runcrate-host/internal/vm"
)

// version is set at build time (-ldflags "-X main.version=...").
var version = "dev"

// qemuUser runs QEMU (created by the installer, in group kvm).
const qemuUser = "runcrate-vm"

// exitUnauthorized tells systemd not to restart (RestartPreventExitStatus).
const exitUnauthorized = 3

const usage = `runcrate-host: plug this machine's GPUs into Runcrate.

Usage:
  runcrate-host register --token <registration token> [--name N] [--site S] [--gpus 0000:41:00.0,...] [--data-dir D] [--api URL]
  runcrate-host run                  the agent (systemd runs this)
  runcrate-host status               config, GPUs and VMs on this host
  runcrate-host snapshot-gpus        record GPU names, VRAM and UUIDs while the host driver still has them
  runcrate-host bind-gpus            move the configured GPUs to vfio-pci
  runcrate-host release-gpus         give every GPU back to the host
  runcrate-host uninstall-runtime    stop all VMs, wipe their disks, remove the VM network, release the GPUs
  runcrate-host console <vm id>      the VM's serial console (last 1 MB)
  runcrate-host version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	dir := os.Getenv("RUNCRATE_HOST_CONFIG_DIR")
	if dir == "" {
		dir = config.DefaultDir
	}
	var err error
	switch os.Args[1] {
	case "version", "--version", "-v":
		fmt.Println(version)
		return
	case "register":
		err = cmdRegister(ctx, dir, os.Args[2:])
	case "run":
		err = cmdRun(ctx, dir, log)
	case "status":
		err = cmdStatus(ctx, dir)
	case "snapshot-gpus":
		err = withConfig(dir, func(cfg *config.Config) error { _, err := snapshotGPUs(ctx, cfg); return err })
	case "bind-gpus":
		err = withConfig(dir, func(cfg *config.Config) error { return bindGPUs(cfg) })
	case "release-gpus":
		err = withConfig(dir, func(cfg *config.Config) error { return releaseGPUs(cfg) })
	case "uninstall-runtime":
		err = cmdUninstallRuntime(ctx, dir, log)
	case "console":
		if len(os.Args) != 3 {
			err = errors.New("usage: runcrate-host console <vm id>")
			break
		}
		err = withConfig(dir, func(cfg *config.Config) error { return console(cfg, os.Args[2]) })
	case "help", "-h", "--help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if errors.Is(err, api.ErrUnauthorized) {
		log.Error("stopping", "err", err)
		os.Exit(exitUnauthorized)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "runcrate-host:", err)
		os.Exit(1)
	}
}

func withConfig(dir string, fn func(*config.Config) error) error {
	cfg, err := config.Load(dir)
	if err != nil {
		return err
	}
	return fn(cfg)
}

// inventory lists the GPUs Runcrate uses. After registration their indexes
// are the registered ones (cfg.GPUIndex), so a missing card never shifts the
// others; before it, every passable GPU is numbered in address order.
func inventory(cfg *config.Config) ([]hw.GPU, []string, gpuCache, error) {
	cache := loadGPUCache(cfg)
	all, err := hw.Host.GPUs(cfg.GPUs, cache.GPUs, hw.LoadPCIIDs(hw.DefaultPCIIDsPaths...))
	if err != nil {
		return nil, nil, cache, err
	}
	var gpus []hw.GPU
	for _, g := range all {
		if len(cfg.GPUIndex) > 0 {
			idx, ok := cfg.GPUIndex[g.Address]
			if !ok {
				continue
			}
			g.Index = idx
		} else {
			if g.Blocker != "" && !devmode.NoPassthrough() {
				continue
			}
			g.Index = len(gpus)
		}
		gpus = append(gpus, g)
	}
	var sw []string
	if devs, err := hw.Host.NVSwitches(); err == nil {
		for _, d := range devs {
			sw = append(sw, d.Address)
		}
	}
	return gpus, sw, cache, nil
}

// bindGPUs moves every usable configured GPU (and its companions and any
// NVSwitches) to vfio-pci.
func bindGPUs(cfg *config.Config) error {
	if devmode.NoPassthrough() {
		return nil
	}
	gpus, sw, _, err := inventory(cfg)
	if err != nil {
		return err
	}
	if !hw.Host.IOMMUEnabled() {
		return errors.New("IOMMU is off: turn on VT-d/AMD-Vi in the BIOS and add intel_iommu=on iommu=pt (or amd_iommu=on iommu=pt) to the kernel command line")
	}
	var errs []error
	sys := vfio.Sysfs{Root: "/"}
	for _, g := range gpus {
		if g.Blocker != "" {
			errs = append(errs, fmt.Errorf("gpu %s: %s", g.Address, g.Blocker))
			continue
		}
		for _, a := range append([]string{g.Address}, g.Companions...) {
			if err := sys.Bind(a); err != nil {
				errs = append(errs, err)
			}
		}
	}
	for _, a := range sw {
		if err := sys.Bind(a); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// releaseGPUs gives every NVIDIA function on vfio-pci back to the host.
func releaseGPUs(cfg *config.Config) error {
	devs, err := hw.Host.Devices()
	if err != nil {
		return err
	}
	var errs []error
	sys := vfio.Sysfs{Root: "/"}
	for _, d := range devs {
		if d.Vendor == hw.NvidiaVendor && d.Driver == vfio.Driver {
			if err := sys.Release(d.Address); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func cmdRegister(ctx context.Context, dir string, args []string) error {
	fs := flag.NewFlagSet("register", flag.ContinueOnError)
	token := fs.String("token", os.Getenv("RUNCRATE_HOST_TOKEN"), "registration token (or RUNCRATE_HOST_TOKEN)")
	name := fs.String("name", "", "host name shown to you in Runcrate (default: hostname)")
	site := fs.String("site", "", "site name, to group hosts in one location")
	gpus := fs.String("gpus", "", "comma-separated PCI addresses to give Runcrate (default: every NVIDIA GPU)")
	dataDir := fs.String("data-dir", "", "where VM disks and images live (default /var/lib/runcrate-host)")
	apiURL := fs.String("api", "", "host API base URL (default production)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	_, statErr := os.Stat(filepath.Join(dir, "config.json"))
	cfg, err := config.Load(dir)
	if err != nil {
		return err
	}
	if os.IsNotExist(statErr) {
		// First install: remember whether the host routed before we did.
		fwd, _ := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
		cfg.ForwardWasOff = strings.TrimSpace(string(fwd)) == "0"
	}
	if cred, _ := cfg.Credential(); cred != "" && cfg.HostID != "" {
		fmt.Printf("Already registered as host %s. To register again, remove %s first.\n", cfg.HostID, cfg.CredentialPath())
		return nil
	}
	if *token == "" {
		return errors.New("--token is required")
	}
	if *name != "" {
		cfg.Name = *name
	}
	if *site != "" {
		cfg.Site = *site
	}
	if *gpus != "" {
		cfg.GPUs = nil
		for _, g := range strings.Split(*gpus, ",") {
			cfg.GPUs = append(cfg.GPUs, strings.ToLower(strings.TrimSpace(g)))
		}
	}
	if *dataDir != "" {
		cfg.DataDir = *dataDir
	}
	if *apiURL != "" {
		cfg.APIURL = *apiURL
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := devmode.Check(cfg.APIURL); err != nil {
		return err
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	if _, err := snapshotGPUs(ctx, cfg); err != nil {
		return err
	}
	list, sw, cache, err := inventory(cfg)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		var why []string
		all, _ := hw.Host.GPUs(cfg.GPUs, cache.GPUs, hw.LoadPCIIDs(hw.DefaultPCIIDsPaths...))
		for _, g := range all {
			why = append(why, g.Address+": "+g.Blocker)
		}
		if len(why) == 0 {
			return errors.New("no NVIDIA GPUs found")
		}
		return fmt.Errorf("no GPU can be passed through:\n  %s", strings.Join(why, "\n  "))
	}
	cfg.GPUIndex = map[string]int{}
	for _, g := range list {
		cfg.GPUIndex[g.Address] = g.Index
	}
	_, subnet, _ := net.ParseCIDR(cfg.VMSubnet)
	h := &host{cfg: cfg, sys: hw.Host, gpus: list, nvswitches: sw, cacheTopo: cache.Topo, subnet: subnet}
	model, cores, threads := hw.Host.CPU()
	total, _ := hw.Host.Mem()
	// What VM disks can use: free space less the host reserve and the image.
	free, _ := hw.DiskFree(filepath.Dir(cfg.DataDir))
	if f, _ := hw.DiskFree(cfg.DataDir); f > 0 {
		free = f
	}
	usableDisk := max(int64(free>>30)-vm.ReserveDiskGB-vm.ImageAllowanceGB, 0)
	req := api.RegisterRequest{
		RegistrationToken: *token,
		RequestID:         registerRequestID(cfg),
		Host: api.RegisterHost{
			Name: cfg.Name,
			Specs: api.Specs{
				CPU:      api.CPUSpec{Model: model, Cores: cores, Threads: threads},
				RAMGB:    int(total >> 30),
				DiskGB:   int(usableDisk),
				Topology: h.Topology(),
			},
			AgentVersion: version,
			PublicIPMode: "none",
		},
	}
	if cfg.Site != "" {
		req.Host.Site = &cfg.Site
	}
	models := map[string]bool{}
	for _, g := range list {
		req.Host.GPUs = append(req.Host.GPUs, api.RegisterGPU{Index: g.Index, ModelRaw: g.ModelRaw, VRAMGB: g.VRAMGB, UUID: g.UUID, PCIeAddress: g.Address})
		models[g.ModelRaw] = true
	}
	req.Host.Specs.GPUs.Count = len(list)
	for m := range models {
		req.Host.Specs.GPUs.Models = append(req.Host.Specs.GPUs.Models, m)
	}
	sort.Strings(req.Host.Specs.GPUs.Models)
	client, err := api.New(cfg.APIURL, "", version)
	if err != nil {
		return err
	}
	var resp *api.RegisterResponse
	for attempt := 1; ; attempt++ {
		resp, err = client.Register(ctx, req)
		var he *api.HTTPError
		// 409: the same request is still being processed; ask again.
		if err == nil || (errors.As(err, &he) && !he.Retryable() && he.Status != 409) || attempt == 5 {
			break
		}
		time.Sleep(time.Duration(attempt) * 3 * time.Second)
	}
	if err != nil {
		return fmt.Errorf("register: %w", err)
	}
	if err := cfg.SaveCredential(resp.HostCredential); err != nil {
		return err
	}
	cfg.HostID = resp.HostID
	// Remember which GPUs were registered with which index.
	if err := cfg.Save(); err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(cfg.Dir(), "register-request-id"))
	fmt.Printf("Registered host %s (%s) with %d GPU(s).\n", resp.HostID, cfg.Name, len(list))
	for _, g := range list {
		note := ""
		if g.Blocker != "" {
			note = "  (can't be used: " + g.Blocker + ")"
		}
		fmt.Printf("  GPU %d  %s  %s%s\n", g.Index, g.Address, g.ModelRaw, note)
	}
	return nil
}

// registerRequestID is stable until registration succeeds, so a retry after
// a lost answer registers the same host instead of a second one.
func registerRequestID(cfg *config.Config) string {
	p := filepath.Join(cfg.Dir(), "register-request-id")
	if b, err := os.ReadFile(p); err == nil && len(strings.TrimSpace(string(b))) == 36 {
		return strings.TrimSpace(string(b))
	}
	id := api.RequestID(fmt.Sprintf("register:%d:%d", time.Now().UnixNano(), os.Getpid()))
	_ = config.WriteFileAtomic(p, []byte(id+"\n"), 0o600)
	return id
}

type runtime struct {
	cfg    *config.Config
	net    *vm.Network
	meta   *metadata.Server
	mgr    *vm.Manager
	host   *host
	client *api.Client
}

func buildRuntime(cfg *config.Config, log *slog.Logger, cred string) (*runtime, error) {
	_, subnet, err := net.ParseCIDR(cfg.VMSubnet)
	if err != nil {
		return nil, err
	}
	client, err := api.New(cfg.APIURL, cred, version)
	if err != nil {
		return nil, err
	}
	gpus, sw, cache, err := inventory(cfg)
	if err != nil {
		return nil, err
	}
	fw, err := vm.FindFirmware()
	if err != nil {
		return nil, err
	}
	if err := devmode.Check(cfg.APIURL); err != nil {
		return nil, err
	}
	_, _, threads := hw.Host.CPU()
	total, _ := hw.Host.Mem()
	run := sysexec.Exec{}
	n := &vm.Network{Subnet: subnet, ForwardWasOff: cfg.ForwardWasOff, Blocked: cfg.BlockedCIDRs, Run: run, ProcRoot: "/"}
	meta := metadata.New(subnet)
	mgr := &vm.Manager{
		DataDir: cfg.DataDir, Net: n, Meta: meta, Run: run, Sys: hw.Host, VFIO: vfio.Sysfs{Root: "/"},
		Images: &vm.ImageStore{Dir: filepath.Join(cfg.DataDir, "images"), API: client, Run: run,
			Verify: func(ctx context.Context, img *api.Image) error {
				return release.Fetch(ctx, nil, release.KindImage, img.Version, img.SHA256, img.URL)
			}},
		QemuUser: qemuUser,
		Firmware: fw, Log: log, GPUs: gpus, NVSwitches: sw,
		HostThreads: threads, HostRAMGB: int(total >> 30), Scrub: cfg.ScrubGPUs && !devmode.NoPassthrough(),
		NoPassthrough: devmode.NoPassthrough(),
	}
	h := &host{cfg: cfg, sys: hw.Host, gpus: gpus, nvswitches: sw, cacheTopo: cache.Topo, subnet: subnet, speed: bench.DefaultSpeedTest, mgr: mgr}
	return &runtime{cfg: cfg, net: n, meta: meta, mgr: mgr, host: h, client: client}, nil
}

func cmdRun(ctx context.Context, dir string, log *slog.Logger) error {
	cfg, err := config.Load(dir)
	if err != nil {
		return err
	}
	cred, err := cfg.Credential()
	if err != nil {
		return err
	}
	if cred == "" || cfg.HostID == "" {
		return errors.New("not registered: run `runcrate-host register --token ...` first")
	}
	rt, err := buildRuntime(cfg, log, cred)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o711); err != nil {
		return err
	}
	if err := ensureQemuUser(ctx); err != nil {
		return err
	}
	if err := rt.net.Setup(ctx); err != nil {
		return err
	}
	if err := bindGPUs(cfg); err != nil {
		// Report and keep going: unusable GPUs show as failed.
		log.Error("bind gpus", "err", err)
	}
	// Two lifetimes: ctx ends the agent loop; the manager's scrubs get their
	// own context, cancelled last, and never block the shutdown (a cancelled
	// scrub resumes after the restart).
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	mgrCtx, mgrCancel := context.WithCancel(context.WithoutCancel(ctx))
	defer mgrCancel()
	rt.mgr.Ctx = mgrCtx
	if store, ok := rt.mgr.Images.(*vm.ImageStore); ok {
		store.Used = rt.mgr.ImagesInUse
	}
	metaErr := make(chan error, 1)
	go func() {
		metaErr <- rt.meta.Serve(mgrCtx, fmt.Sprintf("0.0.0.0:%d", vm.MetadataPort))
	}()
	// Keep the firewall in place: a partner restarting nftables flushes it.
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-mgrCtx.Done():
				return
			case <-t.C:
				if err := rt.net.Reconcile(mgrCtx); err != nil {
					log.Error("firewall", "err", err)
				}
			}
		}
	}()
	if err := rt.mgr.Load(ctx, true); err != nil {
		return err
	}
	// Fetch the current image ahead of the first rental.
	go func() {
		if _, v, err := rt.mgr.Images.Ensure(ctx, ""); err != nil {
			log.Warn("prefetch image", "err", err)
		} else {
			log.Info("image ready", "version", v)
		}
	}()
	state, err := agent.LoadState(filepath.Join(cfg.DataDir, "state.json"))
	if err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	a := &agent.Agent{
		Version: version, API: rt.client, VMs: rt.mgr, Host: rt.host, State: state, Log: log,
		Update: func(ctx context.Context, u api.UpdateAgent) error {
			if err := release.Fetch(ctx, nil, release.KindAgent, u.Version, u.SHA256, u.URL); err != nil {
				return err
			}
			return update.Apply(ctx, nil, u.URL, u.SHA256, self)
		},
		Restart: cancel,
	}
	if devmode.NoPassthrough() {
		a.DevGPUs = rt.host.GPUIdentity
	}
	log.Info("runcrate-host started", "version", version, "host_id", cfg.HostID, "gpus", len(rt.host.gpus), "api", cfg.APIURL)
	runErr := make(chan error, 1)
	go func() { runErr <- a.Run(ctx) }()
	select {
	case err := <-metaErr:
		cancel()
		<-runErr
		if err != nil {
			return fmt.Errorf("metadata server: %w", err)
		}
		return nil
	case err := <-runErr:
		cancel()
		mgrCancel()
		return err
	}
}

// ensureQemuUser creates the unprivileged user QEMU runs as (the installer
// does too; this covers updated agents).
func ensureQemuUser(ctx context.Context) error {
	if _, err := user.Lookup(qemuUser); err == nil {
		return nil
	}
	_, err := sysexec.Exec{}.Run(ctx, "useradd", "--system", "--no-create-home", "--home-dir", "/nonexistent",
		"--shell", "/usr/sbin/nologin", "--groups", "kvm", qemuUser)
	return err
}

func cmdStatus(ctx context.Context, dir string) error {
	cfg, err := config.Load(dir)
	if err != nil {
		return err
	}
	gpus, sw, _, err := inventory(cfg)
	if err != nil {
		return err
	}
	cred, _ := cfg.Credential()
	fmt.Printf("host id:     %s\nname:        %s\napi:         %s\nregistered:  %v\ndata dir:    %s\nIOMMU:       %v\n",
		orDash(cfg.HostID), cfg.Name, cfg.APIURL, cred != "", cfg.DataDir, hw.Host.IOMMUEnabled())
	fmt.Println("GPUs:")
	for _, g := range gpus {
		d, _ := hw.Host.Device(g.Address)
		line := fmt.Sprintf("  %d  %s  %-40s driver=%s health=%s", g.Index, g.Address, g.ModelRaw, orDash(d.Driver), hw.Host.Health(g.Address))
		if g.Blocker != "" {
			line += "  BLOCKED: " + g.Blocker
		}
		fmt.Println(line)
	}
	if len(sw) > 0 {
		fmt.Printf("NVSwitches: %d (VMs take the whole node)\n", len(sw))
	}
	ents, _ := os.ReadDir(filepath.Join(cfg.DataDir, "vms"))
	fmt.Println("VMs:")
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), ".rec-") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(cfg.DataDir, "vms", e.Name()))
		if err != nil {
			continue
		}
		var r vm.Record
		if json.Unmarshal(b, &r) == nil {
			fmt.Printf("  %s  %s  gpus=%v vcpus=%d ram=%dG disk=%dG ip=%s\n", r.ID, r.State, r.GPUIndexes, r.VCPUs, r.RAMGB, r.DiskGB, r.GuestIP)
		}
	}
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// cmdUninstallRuntime stops and wipes every VM, removes the VM network and
// gives the GPUs back. The installer then removes the files and services.
func cmdUninstallRuntime(ctx context.Context, dir string, log *slog.Logger) error {
	cfg, err := config.Load(dir)
	if err != nil {
		return err
	}
	_, subnet, _ := net.ParseCIDR(cfg.VMSubnet)
	run := sysexec.Exec{}
	n := &vm.Network{Subnet: subnet, ForwardWasOff: cfg.ForwardWasOff, Blocked: cfg.BlockedCIDRs, Run: run, ProcRoot: "/"}
	mgr := &vm.Manager{DataDir: cfg.DataDir, Net: n, Meta: metadata.New(subnet), Run: run, Sys: hw.Host, VFIO: vfio.Sysfs{Root: "/"}, Log: log}
	if err := mgr.Load(ctx, false); err != nil {
		log.Warn("load vms", "err", err)
	}
	for _, id := range mgr.IDs() {
		if err := mgr.Destroy(ctx, id); err != nil {
			log.Error("destroy", "vm", id, "err", err)
		}
	}
	mgr.Wait()
	n.Teardown(ctx)
	return releaseGPUs(cfg)
}

func console(cfg *config.Config, id string) error {
	if !vm.ValidID(id) {
		return fmt.Errorf("bad vm id %q", id)
	}
	out, err := vm.SerialLog(filepath.Join(cfg.DataDir, "vms", id, "qmp.sock"))
	if err != nil {
		return err
	}
	// The guest writes this: never hand its escape sequences to a terminal.
	fmt.Print(strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || (r >= 0x20 && r != 0x7f && (r < 0x80 || r > 0x9f)) {
			return r
		}
		return '?'
	}, out))
	return nil
}
