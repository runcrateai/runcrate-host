# runcrate-host

Plug your GPU machines into Runcrate with one command. Runcrate sells their GPUs on demand, reserved or spot at the rates you set, and pays you out. You don't need a launch API, billing or a public IP.

```sh
curl -fsSL https://get.runcrate.ai/host | sudo sh
```

It asks for the registration token from your Runcrate partner dashboard (input hidden). One token can register every machine in a cluster. The token never goes on a command line, where every user of the machine could read it in `ps`. For unattended installs, put it in a file only root can read and pass that:

```sh
sudo sh -c 'umask 077; cat > /root/runcrate-token'    # paste the token, Enter, then Ctrl-D
curl -fsSL https://get.runcrate.ai/host | sudo sh -s -- --token-file /root/runcrate-token
```

The installer refuses a token file that is not owned by root or that anyone else can read.

## Requirements

- Bare metal, x86_64. Not a VM: GPUs are passed through to customer VMs, which needs the real hardware.
- Ubuntu 22.04, Ubuntu 24.04 or Debian 12.
- NVIDIA GPUs: datacenter cards (A100, H100, H200, B200, L40S, RTX 6000 and others) or GeForce RTX 30, 40 and 50 series.
- Virtualization (VT-x / AMD-V) and IOMMU (VT-d / AMD-Vi) turned on in the BIOS. If the kernel has the IOMMU off, the installer shows the exact GRUB line, adds it and reboots when you say yes (or with `--yes`). Then run the same command again.
- Outbound HTTPS. No inbound ports, NAT is fine. The one exception: while a benchmark runs on two or more hosts of the same site, iperf3 (and ib_write_bw with InfiniBand) listens on the hosts' private addresses for up to 5 minutes, open only to the site's other hosts.
- Fast local disk for VM disks, 100 GB or more free (`--data-dir` to choose where). The data directory must be new or empty: the agent marks it as its own (`.runcrate-host-data`) and never shares it with your files.
- No NVIDIA driver is needed on the host. If one is installed, the installer reads each GPU's name, memory and UUID from it first.

## What the installer does

Every step checks before it changes anything, so running it again is safe.

1. Checks the OS, CPU virtualization, the GPUs and the IOMMU.
2. Installs QEMU/KVM, OVMF, nftables, genisoimage and iperf3 (and perftest when InfiniBand is present).
3. Creates the unprivileged `runcrate-vm` user and group, with a udev rule giving group `kvm` access to `/dev/vhost-net`. Each VM's QEMU runs confined as its own user (`runcrate-vm-<slot>`, group `runcrate-vm`, in `kvm`), which the agent adds on first use, so one VM can't reach another's disk, sockets or process.
4. Downloads the agent, checks its ed25519 signature against Runcrate's release key with `openssl` (it refuses to install anything unsigned), and installs it at `/usr/local/bin/runcrate-host`.
5. Takes the GPUs off the host: stops `nvidia-persistenced` and `nvidia-fabricmanager` if they run, and moves each GPU (and the other functions of its card, such as HDMI audio) to `vfio-pci`. When Runcrate gets every GPU, a modprobe rule also claims them at boot before any host driver.
6. Registers the machine with your token. The host credential it gets back is stored in `/etc/runcrate-host/credential`, readable by root only.
7. Starts two systemd services: `runcrate-host-gpus` (binds the GPUs at boot) and `runcrate-host` (the agent).

Options: `--name` (shown in your dashboard), `--site` (groups hosts in one location), `--gpus 0000:41:00.0,0000:42:00.0` (only these GPUs; the rest stay yours), `--data-dir`, `--yes`.

## How it works

The agent calls Runcrate every 10 seconds over HTTPS with the host's state (VMs, GPU health, load) and gets back commands:

| Command | What the agent does |
|---|---|
| `create_vm` | Boots a KVM virtual machine from the Runcrate image with the given GPUs passed through, vCPUs, RAM and disk. The customer's setup runs inside it at first boot and opens its own outbound tunnel to the Runcrate gateway, which is how the customer reaches it. |
| `destroy_vm` | Stops the VM, overwrites its disk with zeros and deletes it, then boots a throwaway VM that overwrites the GPUs' memory before they can be rented again. |
| `reclaim_notice` | Spot only: tells the VM it will be reclaimed (see below). |
| `drain` | Takes no new rentals; running ones finish. |
| `benchmark` | Boots a throwaway VM that tests every free GPU (a memory test over nearly all VRAM, an FP16 matrix multiply checked against the CPU, `nvidia-smi`), and measures disk, internet and, for hosts on the same site, host-to-host bandwidth (iperf3, and ib_write_bw with InfiniBand). The same VM checks its isolation: it must reach the internet, DNS and the metadata server, and must not reach this host, its gateway, the Docker bridge, private ranges, CGNAT or link-local addresses. Meanwhile the host checks from a throwaway network namespace that a machine on your LAN, routing through it, can't open a TCP connection to the VM. The results, and the VM image's checks, show on the host's page. |
| `update_agent` | Downloads a new agent over HTTPS, checks its sha256, swaps it in and restarts. The previous binary stays at `/usr/local/bin/runcrate-host.prev`. |

Customer VMs are isolated from your network and from each other:

- each VM has its own tap device and a /30 from `10.254.0.0/16` (`vm_subnet` in the config), with no shared layer 2 between VMs;
- the firewall (nftables table `inet runcrate_host`) lets a VM reach the internet through NAT and nothing else: not the host, not other VMs, not private, link-local or carrier NAT ranges, and nothing from outside can open a connection into a VM. It is re-applied every 5 seconds, and whenever the agent is not running (stopped, crashed, updating or delisted) every VM's network is set down, so isolation never depends on a firewall nobody is watching;
- the only host service a VM can reach is the metadata server at `http://169.254.169.254`, which answers each VM only about itself;
- if Docker or ufw set the iptables FORWARD policy to DROP, the agent adds two narrow accept rules for its own `rc-*` interfaces; if IPv4 forwarding was off before the install, the agent drops any other forwarding so the host doesn't start routing for anything else.

### Spot reclaim notice inside the VM

When you reclaim a spot GPU, the customer's VM sees it two ways, within 5 seconds:

- the file `/run/runcrate/reclaim-notice.json`;
- `http://169.254.169.254/runcrate/v1/reclaim-notice` (404 until there is a notice, then `{"reclaim": true, "vm_id", "deadline_at", "seconds_remaining"}`), and the AWS-shaped `http://169.254.169.254/latest/meta-data/spot/instance-action`.

## What the agent can and can't see

The agent can see:

- the hardware: CPU, RAM, disk space, PCI devices, NICs and their speeds, private IPv4 addresses (reported for site topology);
- the VMs it runs: their size, state and GPUs;
- the customer's setup script for each VM, which it hands to the VM inside the VM's boot seed (root only, deleted with the VM).

The agent doesn't look inside VMs. It doesn't read customer disks, memory, network traffic or GPU contents. It never opens a port on your network and only talks to Runcrate over HTTPS.

What you, as root on the machine, could see: a host administrator can always read the memory and disk of VMs running on hardware they control. Runcrate protects customers from you through your agreement with Runcrate and by wiping disks and GPU memory after every rental, and protects you from customers by keeping their code inside VMs with no route into your network.

Reported to Runcrate: GPU health, VM states, load, the benchmark results, and the topology described above. GPU temperature and utilization are not reported, because the GPUs are inside VMs where the host can't read them.

## Files

| Path | What |
|---|---|
| `/usr/local/bin/runcrate-host` | the agent |
| `/etc/runcrate-host/config.json` | config (root only) |
| `/etc/runcrate-host/credential` | host credential (root only, 0600) |
| `/var/lib/runcrate-host/` | VM disks, the base image, agent state (root only) |
| `/etc/systemd/system/runcrate-host.service`, `runcrate-host-gpus.service` | services |
| `/etc/modprobe.d/runcrate-host-vfio.conf`, `/etc/modules-load.d/runcrate-host.conf` | GPU binding at boot |
| `/etc/udev/rules.d/90-runcrate-host.rules` | `/dev/vhost-net` for group `kvm` |
| users `runcrate-vm`, `runcrate-vm-<slot>` | run QEMU, unprivileged, one user per VM slot |
| `/etc/default/grub.d/90-runcrate-host.cfg` | only if the installer turned on the IOMMU |

VM traffic follows the agent's own firewall, not yours: the agent lets VMs reach the internet through your iptables or nftables drop policies, and still blocks your networks, private and link-local ranges and anything in `blocked_cidrs` (config.json). Put any extra ranges VMs must never reach there.

Useful commands: `runcrate-host status`, `journalctl -u runcrate-host -f`, `runcrate-host console <vm id>` (a VM's serial console).

The agent only uses a data directory carrying its marker, `.runcrate-host-data`: it takes a new or empty directory (a fresh filesystem's `lost+found` is fine) and refuses one holding other files. A host installed before the marker existed won't start the agent until you run the installer again (or `sudo runcrate-host register`), which adds the marker when the directory holds only the agent's own files and its state. If it still refuses only because the directory has no agent state (no `gpus-cache.json` or `state.json`: a host without the NVIDIA driver that never ran a VM), uninstall, delete the data directory (it holds only the agent's files) and install again. If the data directory is a separate disk, the agent also refuses to start while it isn't mounted.

## Uninstall

```sh
curl -fsSL https://get.runcrate.ai/host | sudo sh -s -- --uninstall
```

This stops every VM, overwrites and deletes their disks, removes the VM network and firewall table, gives every GPU back to the host (your NVIDIA driver picks them up again, or after a reboot), turns `nvidia-persistenced` back on if the installer stopped it, removes the boot and udev rules and the IOMMU GRUB line it added, the `runcrate-vm` users and group, the agent and its config. In the data directory (only one carrying the agent's marker) it deletes only what the agent created (`vms/`, `images/`, `holds/`, `state.json`, `gpus-cache.json`), and the directory itself only if that leaves it empty. Packages it installed (qemu, ovmf, nftables, iperf3) are left in place. Delist the host in your dashboard as well.

## Development

```sh
go test ./...
make release VERSION=0.1.0           # out/runcrate-host-linux-amd64, .sha256 and .version
sudo image/build.sh 20261026-1       # the VM image, on any x86_64 Linux box with KVM
RUNCRATE_HOST_SIGNING_KEY=$(security find-generic-password -s runcrate-host-release-signing -a runcrate-host -w) \
  go run ./hack/sign -kind runcrate-vm -version 20261026-1 out/runcrate-vm-20261026-1.qcow2   # .sig, uploaded next to the image
```

The VM image is Ubuntu 24.04 with the NVIDIA open driver, the CUDA runtime (cudart, cuBLAS), Fabric Manager and `rc-gpubench`. Everything a customer machine needs beyond that comes from its setup script at first boot.

### Publishing a release

```sh
hack/publish.sh 0.1.1                # from a clean checkout of a commit on origin/main, on the owner's Mac
hack/check-install.sh                # any time: what partners download is signed and consistent
```

The repo is private, so GitHub release downloads don't work for partners. `get.runcrate.ai` is the Worker in `hack/get-worker` (`cd hack/get-worker && wrangler deploy`): it serves only these release files, read-only, from the R2 bucket `runcrate-host-images`. `publish.sh` builds the agent, signs it (`runcrate-host`) and `install.sh` (`runcrate-install`) with the release key, and uploads:

| URL | What |
| --- | --- |
| `https://get.runcrate.ai/host`, `host.sig`, `host.version` | the installer, its signature and version (60 s cache) |
| `https://get.runcrate.ai/releases/latest/runcrate-host-linux-amd64`, `.sig`, `.version` | the agent the installer downloads (60 s cache) |
| `https://get.runcrate.ai/releases/<version>/...` | the same, kept for good |

Then it runs `hack/check-install.sh https://get.runcrate.ai <version>`, which verifies both signatures with `openssl` against the key in this checkout's `install.sh` and checks that the installer, the latest agent and the versioned copy agree. A version is never replaced by a different build, and `latest` never moves back to an older version (to roll back, publish the old code as a newer version); a failed upload is finished by running the same command again.
