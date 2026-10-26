# runcrate-host

Plug your GPU machines into Runcrate with one command. Runcrate sells their GPUs on demand, reserved or spot at the rates you set, and pays you out. You don't need a launch API, billing or a public IP.

```sh
curl -fsSL https://get.runcrate.ai/host | sudo sh -s -- --token <registration token>
```

Get the token from your Runcrate partner dashboard. One token can register every machine in a cluster.

## Requirements

- Bare metal, x86_64. Not a VM: GPUs are passed through to customer VMs, which needs the real hardware.
- Ubuntu 22.04, Ubuntu 24.04 or Debian 12.
- NVIDIA GPUs: datacenter cards (A100, H100, H200, B200, L40S, RTX 6000 and others) or GeForce RTX 30, 40 and 50 series.
- Virtualization (VT-x / AMD-V) and IOMMU (VT-d / AMD-Vi) turned on in the BIOS. If the kernel has the IOMMU off, the installer shows the exact GRUB line, adds it and reboots when you say yes (or with `--yes`). Then run the same command again.
- Outbound HTTPS. Nothing listens on your network: no inbound ports, NAT is fine.
- Fast local disk for VM disks, 100 GB or more free (`--data-dir` to choose where).
- No NVIDIA driver is needed on the host. If one is installed, the installer reads each GPU's name, memory and UUID from it first.

## What the installer does

Every step checks before it changes anything, so running it again is safe.

1. Checks the OS, CPU virtualization, the GPUs and the IOMMU.
2. Installs QEMU/KVM, OVMF, nftables, genisoimage and iperf3 (and perftest when InfiniBand is present).
3. Installs the agent at `/usr/local/bin/runcrate-host`.
4. Takes the GPUs off the host: stops `nvidia-persistenced` and `nvidia-fabricmanager` if they run, and moves each GPU (and the other functions of its card, such as HDMI audio) to `vfio-pci`. When Runcrate gets every GPU, a modprobe rule also claims them at boot before any host driver.
5. Registers the machine with your token. The host credential it gets back is stored in `/etc/runcrate-host/credential`, readable by root only.
6. Starts two systemd services: `runcrate-host-gpus` (binds the GPUs at boot) and `runcrate-host` (the agent).

Options: `--name` (shown in your dashboard), `--site` (groups hosts in one location), `--gpus 0000:41:00.0,0000:42:00.0` (only these GPUs; the rest stay yours), `--data-dir`, `--yes`.

## How it works

The agent calls Runcrate every 10 seconds over HTTPS with the host's state (VMs, GPU health, load) and gets back commands:

| Command | What the agent does |
|---|---|
| `create_vm` | Boots a KVM virtual machine from the Runcrate image with the given GPUs passed through, vCPUs, RAM and disk. The customer's setup runs inside it at first boot and opens its own outbound tunnel to the Runcrate gateway, which is how the customer reaches it. |
| `destroy_vm` | Stops the VM, overwrites its disk with zeros and deletes it, then boots a throwaway VM that overwrites the GPUs' memory before they can be rented again. |
| `reclaim_notice` | Spot only: tells the VM it will be reclaimed (see below). |
| `drain` | Takes no new rentals; running ones finish. |
| `benchmark` | Boots a throwaway VM that tests every free GPU (a memory test over nearly all VRAM, an FP16 matrix multiply checked against the CPU, `nvidia-smi`), and measures disk, internet and, for hosts on the same site, host-to-host bandwidth (iperf3, and ib_write_bw with InfiniBand). |
| `update_agent` | Downloads a new agent over HTTPS, checks its sha256, swaps it in and restarts. The previous binary stays at `/usr/local/bin/runcrate-host.prev`. |

Customer VMs are isolated from your network and from each other:

- each VM has its own tap device and a /30 from `10.254.0.0/16` (`vm_subnet` in the config), with no shared layer 2 between VMs;
- the firewall (nftables table `inet runcrate_host`) lets a VM reach the internet through NAT and nothing else: not the host, not other VMs, not private, link-local or carrier NAT ranges, and nothing from outside can open a connection into a VM;
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
| `/etc/default/grub.d/90-runcrate-host.cfg` | only if the installer turned on the IOMMU |

Useful commands: `runcrate-host status`, `journalctl -u runcrate-host -f`, `runcrate-host console <vm id>` (a VM's serial console).

## Uninstall

```sh
curl -fsSL https://get.runcrate.ai/host | sudo sh -s -- --uninstall
```

This stops every VM, overwrites and deletes their disks, removes the VM network and firewall table, gives every GPU back to the host (your NVIDIA driver picks them up again, or after a reboot), turns `nvidia-persistenced` back on if the installer stopped it, removes the boot rules and the IOMMU GRUB line it added, and deletes the agent, its config and its data. Packages it installed (qemu, ovmf, nftables, iperf3) are left in place. Delist the host in your dashboard as well.

## Development

```sh
go test ./...
make release VERSION=0.1.0           # out/runcrate-host-linux-amd64 and .sha256
sudo image/build.sh 20261026-1       # the VM image, on any x86_64 Linux box with KVM
```

The VM image is Ubuntu 24.04 with the NVIDIA open driver, the CUDA runtime (cudart, cuBLAS), Fabric Manager and `rc-gpubench`. Everything a customer machine needs beyond that comes from its setup script at first boot.
