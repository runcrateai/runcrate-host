# runcrate-host

runcrate-host is the installer and agent that turn a bare-metal Linux server with NVIDIA GPUs into a Runcrate host: Runcrate rents its GPUs out on demand, reserved or spot, as KVM virtual machines with the GPUs passed through, at the rates the owner sets. The agent talks to Runcrate only over outbound HTTPS, so the server needs no public IP or open port.

## Check a machine first

```sh
curl -fsSL https://get.runcrate.ai/host | sh -s -- --check
```

No account, no token and no root needed. It installs nothing, changes nothing and writes no files. Each line is PASS, WARN, FAIL or SKIP with a one-line fix, and the last line is the verdict: `READY`, `READY AFTER REBOOT` (the IOMMU is off and the install will turn it on) or `NOT READY` with the reasons. The exit code is 0 only when ready. It checks:

- the OS (Ubuntu 22.04, Ubuntu 24.04 or Debian 12) and x86_64;
- bare metal, not a VM (`systemd-detect-virt`, the DMI tables, the CPU's hypervisor flag);
- CPU virtualization (VT-x / AMD-V) and `/dev/kvm`;
- the IOMMU: IOMMU groups present, the kernel command line, the firmware's DMAR / IVRS table, and the kernel log when it is readable (usually root only; without root that line is SKIP);
- each NVIDIA GPU: name, PCI address, whether it is a model Runcrate sells, its IOMMU group (only the card's own functions, such as HDMI audio, plus PCI bridges may share it), the driver it is on and what the install will do with it;
- free space for VM disks (100 GB or more, at `--data-dir` if you pass it), outbound HTTPS to `get.runcrate.ai` and `www.runcrate.ai`, and RAM.

Example, on a server with one H100 and no root:

```
PASS  CPU architecture: x86_64
PASS  OS: Ubuntu 24.04.1 LTS
PASS  Bare metal: no hypervisor found
PASS  RAM: 504 GB (8 GB stays with the host, the rest is for VMs)
PASS  CPU virtualization: on, /dev/kvm present
PASS  IOMMU: on (IOMMU groups: 96)
SKIP  IOMMU in the kernel log: dmesg is not readable here (most machines allow it to root only)
PASS  GPU 0000:41:00.0: GH100 [H100 SXM5 80GB], sold as H100
      IOMMU group 20: only this card (with 0000:41:00.1)
      driver: nvidia; the install moves it to vfio-pci (stopping nvidia-persistenced and nvidia-fabricmanager if they run)
PASS  Disk: 2000 GB free for VM disks at /var/lib/runcrate-host
PASS  Outbound HTTPS to get.runcrate.ai
PASS  Outbound HTTPS to www.runcrate.ai

Skipped: kernel log (dmesg).
Verdict: READY. Install: curl -fsSL https://get.runcrate.ai/host | sudo sh
```

## Install

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
- NVIDIA GPUs: datacenter cards (A100, H100, H200, B200, L40S, RTX 6000 Ada, RTX PRO 6000 but not the Max-Q, and others) or the GeForce RTX 3090, 4090 and 5090. Other GeForce cards aren't sold on Runcrate.
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
| `benchmark` | Boots a throwaway VM from the VM image Runcrate currently names (downloaded and its sha256 and signature checked first when it isn't on the host yet) that tests every free GPU (a memory test over nearly all VRAM, an FP16 matrix multiply checked against the CPU, `nvidia-smi`), and measures disk, internet and, for hosts on the same site, host-to-host bandwidth (iperf3, and ib_write_bw with InfiniBand). The same VM checks its isolation: it must reach the internet, DNS, the metadata server and every Runcrate tunnel gateway customers are reached through (one check per gateway, `gateway_<id>`, passing only when the gateway answers with an SSH banner), and must not reach this host, its gateway, the Docker bridge, private ranges, CGNAT or link-local addresses. Meanwhile the host checks from a throwaway network namespace that a machine on your LAN, routing through it, can't open a TCP connection to the VM. The results, and the checks of the image the VM booted, show on the host's page. |
| `update_agent` | Downloads the new agent over HTTPS and installs it only if it is signed with Runcrate's release key (see below), matches the sha256 it was sent and is newer than the running version; then restarts. The previous binary stays at `/usr/local/bin/runcrate-host.prev`. |

### Spot reclaim notice inside the VM

When you reclaim a spot GPU, the customer's VM sees it two ways, within 5 seconds:

- the file `/run/runcrate/reclaim-notice.json`;
- `http://169.254.169.254/runcrate/v1/reclaim-notice` (404 until there is a notice, then `{"reclaim": true, "vm_id", "deadline_at", "seconds_remaining"}`), and the AWS-shaped `http://169.254.169.254/latest/meta-data/spot/instance-action`.

## How isolation works

Customers get a VM, never a process on your machine. What keeps them in it:

- **GPUs by VFIO passthrough.** Each GPU, with the other functions of the same card (HDMI audio, USB-C), is bound to `vfio-pci` and handed to one VM. A GPU is used only when its IOMMU group holds nothing else but PCI bridges, so no other device shares its DMA isolation; a kernel booted with `pcie_acs_override` (which fakes IOMMU groups) is refused.
- **One unprivileged QEMU user per VM slot.** Each VM's QEMU runs as its own user (`runcrate-vm-<slot>`, group `runcrate-vm`, in `kvm`), so one VM can't reach another's disk, sockets or process.
- **An egress-only routed /30 per VM.** Each VM has its own tap device and a /30 from `10.254.0.0/16` (`vm_subnet` in the config), with no shared layer 2 between VMs. The firewall (nftables table `inet runcrate_host`) lets a VM reach the internet through NAT and nothing else: not the host, not other VMs, not private, link-local or carrier NAT ranges, and nothing in `blocked_cidrs`.
- **No inbound connections.** Nothing outside can open a connection into a VM. Customers reach their VM through a tunnel the VM itself opens, outbound, to a Runcrate gateway.
- **The firewall is re-applied every 5 seconds**, and whenever the agent is not running (stopped, crashed, updating or delisted) every VM's network is set down, so isolation never depends on rules nobody is watching. If Docker or ufw set the iptables FORWARD policy to DROP, the agent adds two narrow accept rules for its own `rc-*` interfaces; if IPv4 forwarding was off before the install, the agent drops any other forwarding so the host doesn't start routing for anything else.
- **Metadata on a privileged port.** A VM's `http://169.254.169.254` is redirected to the agent's metadata server on host port 980, below 1024 so only root can bind it, and the port is open only to the VMs' own interfaces. It answers each VM only about itself, and it is the one host service a VM can reach.
- **Wiped after each customer.** When a rental ends, the VM's disk is overwritten with zeros and deleted, and a throwaway VM overwrites the GPUs' memory before they can be rented again (the `scrub_gpus` setting in `config.json`, on by default). A GPU whose scrub fails is held back.
- **Signed agent and images, no downgrades.** The agent, the installer and the VM images are signed with Runcrate's ed25519 release key, held by the publisher, never on the control plane or the download server; the public key is built into the agent and into `install.sh`. The signature covers the kind of file, its version and its sha256, so a signed file can't be passed off as another. The installer checks the agent with `openssl` before installing it; the agent checks every VM image and every update, and refuses an update that is not newer than the running version. Once installed, a compromised control plane or download server can only point the agent at releases Runcrate signed.
- **The first install trusts get.runcrate.ai and TLS.** The installer you pipe into `sh` is not checked by itself, and it has no version floor: it installs whatever signed release is `latest`. To check the installer before running it:

  ```sh
  cd "$(mktemp -d)"
  curl -fsSL --remote-name-all https://get.runcrate.ai/host https://get.runcrate.ai/host.sig https://get.runcrate.ai/host.version
  printf -- '-----BEGIN PUBLIC KEY-----\n%s\n-----END PUBLIC KEY-----\n' \
    MCowBQYDK2VwAyEAYBrMXteHsZh26jJMcMZ3PI05QmTbNcd7gy9ALNlW2Yc= > runcrate-release.pem
  printf 'runcrate-install\n%s\n%s\n' "$(head -n1 host.version | tr -d '[:space:]')" \
    "$(sha256sum host | cut -d' ' -f1)" > host.msg
  base64 -d host.sig > host.sigbin
  openssl pkeyutl -verify -pubin -inkey runcrate-release.pem -rawin -in host.msg -sigfile host.sigbin
  sudo sh host        # only after "Signature Verified Successfully"
  ```

  The key is the same `RELEASE_PUBKEY` as in `install.sh` and `internal/release`. The signed message is `runcrate-install`, the version and the sha256, one per line. `-rawin` needs OpenSSL 3.

## Threat model

What the design protects:

- **The host from the customer.** Customer code runs only inside a KVM VM with the GPUs passed through by VFIO. The VM gets an egress-only routed /30, no inbound connections, and no route to the host, its networks, private ranges or anything in `blocked_cidrs`. Its QEMU runs as an unprivileged user of its own, under QEMU's seccomp sandbox (`-sandbox on`, no spawning or privilege changes).
- **Customers from each other.** There is no VM-to-VM traffic and no shared layer 2. After each rental the VM's disk is overwritten with zeros and deleted, and a throwaway VM overwrites the GPUs' memory before they are rented again. This assumes the host runs the unmodified agent with its default config: the GPU scrub is the `scrub_gpus` setting (on by default), which the host can turn off, and the agent does not report that setting to Runcrate.
- **Customers and hosts from a bad agent update.** Updates and VM images must be signed with Runcrate's release key, and the agent refuses an update that is not newer than the version it runs.

What it does not protect: **a host owner with root can read and change everything on the machine**: a customer VM's memory and disk, its first-boot script and any credentials in it, its unencrypted traffic, and the agent itself. That holds for any VM on hardware someone else runs. There is no confidential computing (encrypted, attested VMs) today.

What Runcrate does about that instead:

- Host companies accept the Host Partner Terms at signup. Those terms forbid opening, inspecting, copying or snapshotting a VM, its memory, disks or traffic, trying to identify a customer, and changing or getting around the host agent.
- Host companies, and the person who accepted the terms for them, are screened against sanctions lists (OFAC SDN and Consolidated, EU, UK) at signup and every day after. A new finding stops their listings and payouts until a person decides.
- Customers are shown options by GPU model, region and tier, never by host company. The dashboard shows hosts a pseudonym for each customer, never the customer's identity; a host with root can still learn it from the VM's contents. A VM's internet traffic leaves through the host's own connection, so its public IP is the host's.
- A storage volume's keys travel in the VM's first-boot script, which a host's operator can read. So a launch with a volume goes only to a Datacenter host (a site Runcrate has verified), never to a Community one.

## What the benchmark verifies

On Runcrate's command (a new host runs one before it is listed), the agent boots a throwaway VM from the current signed VM image, its sha256 and signature checked, and reports:

- **every free GPU:** a memory test over nearly all of its VRAM, an FP16 matrix multiply checked against the CPU, and `nvidia-smi`;
- **disk, internet and, for hosts on the same site, host-to-host bandwidth** (iperf3, and ib_write_bw with InfiniBand);
- **isolation, from inside the VM:** it must reach the internet, DNS, the metadata server and up to 8 Runcrate tunnel gateways customers come in through (each must answer with an SSH banner). It must not reach this host, its gateway or the Docker bridge, and it probes one address in each private range (10.0.0.1, 172.16.0.1, 192.168.0.1, 100.64.0.1, 169.254.1.1), which must not answer. The firewall blocks the whole ranges; the benchmark only samples them;
- **isolation, from outside:** from a throwaway network namespace routed through the host like a machine on your LAN, the host checks that it can't open a TCP connection to the VM.

A GPU is sold only after it passes on the VM image customers get. The results show on the host's page in your dashboard.

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
make test                            # go test -race ./... and hack/test-install.sh (install.sh and --check on fake machines)
make release VERSION=0.1.0           # out/runcrate-host-linux-amd64, .sha256 and .version
sudo image/build.sh 20261026-1       # the VM image, on any x86_64 Linux box with KVM
export RUNCRATE_HOST_SIGNING_KEY="$(<print the base64 ed25519 seed from your secret store>)"
go run ./hack/sign -kind runcrate-vm -version 20261026-1 out/runcrate-vm-20261026-1.qcow2   # .sig, uploaded next to the image
```

The VM image is Ubuntu 24.04 with the NVIDIA open driver, the CUDA runtime (cudart, cuBLAS), Fabric Manager and `rc-gpubench`. Everything a customer machine needs beyond that comes from its setup script at first boot.

`install.sh` carries the list of GPU models Runcrate sells, for `--check`; Runcrate's web app decides at registration with its own copy. `make check-models APP=<web app checkout>` runs both on every NVIDIA name in pci.ids and fails when they disagree.

### Publishing a release

```sh
hack/publish.sh 0.1.1                # from a clean checkout of a commit on origin/main
hack/check-install.sh                # any time: what partners download is signed and consistent
```

`publish.sh` takes the release key and the bucket's credentials from the environment (`RUNCRATE_HOST_SIGNING_KEY`, `R2_ENDPOINT`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`). Export them from your secret store, as in `export RUNCRATE_HOST_SIGNING_KEY="$(...)"`, never typed on a command line; any secret left unset is asked for on the terminal with echo off.

Releases are served from `get.runcrate.ai`, the Worker in `hack/get-worker` (`cd hack/get-worker && wrangler deploy`): it serves only these release files, read-only, from the R2 bucket `runcrate-host-images`. `publish.sh` builds the agent, signs it (`runcrate-host`) and `install.sh` (`runcrate-install`) with the release key, and uploads:

| URL | What |
| --- | --- |
| `https://get.runcrate.ai/host`, `host.sig`, `host.version` | the installer, its signature and version (60 s cache) |
| `https://get.runcrate.ai/releases/latest/runcrate-host-linux-amd64`, `.sig`, `.version` | the agent the installer downloads (60 s cache) |
| `https://get.runcrate.ai/releases/<version>/...` | the same, kept for good |

Then it runs `hack/check-install.sh https://get.runcrate.ai <version>`, which verifies both signatures with `openssl` against the key in this checkout's `install.sh` and checks that the installer, the latest agent and the versioned copy agree. A version is never replaced by a different build, and `latest` never moves back to an older version (to roll back, publish the old code as a newer version); a failed upload is finished by running the same command again.

## License

The source is published so that anyone running the Runcrate host agent on their machines can read exactly what it does; see [LICENSE](LICENSE). The official releases at `get.runcrate.ai` are signed with Runcrate's ed25519 release key: the installer checks the agent's signature before installing it, and the agent checks the signature of every update and VM image it takes. The Host Partner Terms that host companies accept forbid changing or getting around the host agent.
