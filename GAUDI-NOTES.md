# Intel Gaudi 3: what to verify on the host's own box

The Gaudi support in `internal/hw`, `internal/vfio` and `cmd/runcrate-host` was written from Intel's documentation (docs.habana.ai, Virtualization) and tested only against fake sysfs trees. Nothing here has run on a Gaudi machine. Every item below is an assumption the code makes. Run the command, compare with the expected value, and fix the code or the table named in the last column when they differ.

Target: ModelForgelabs, Dell XE9680, 8x Gaudi 3 OAM, bare metal. `ADDR` is a module's PCI address (`0000:19:00.0` style); run each per-module command for all eight.

## What is documented (taken from Intel's docs, not verified here)

- Gaudi supports PCI passthrough only: no SR-IOV, no MIG, smallest unit is one accelerator.
- PCI vendor `1da3`; Gaudi 3 OAM is device `1060`, Gaudi 3 PCIe is `1063`.
- Host kernel command line: `intel_iommu=on iommu=pt vfio-pci.ids=1da3:1060`. The host needs no Habana driver for passthrough.
- Guest kernel command line adds `pci=nocrs,realloc,assign-busses`.
- 128 GB of HBM2e per accelerator.

## Checklist

### 0. Isolation between tenants (do these first; nothing is rented out until they pass)

The host reports Gaudi GPUs with no benchmark pass (visibility only) and its scrub does not overwrite their memory, so these four checks decide whether a Gaudi can ever be rented. Until they are done: `runcrate-host status` and `install.sh --check` print a WARN for a module with no reset method, a failed scrub keeps the module held back (`Create` answers `held back after a failed memory scrub`), and the control plane must treat gaudi3 + `visibility_only` as "pending manual".

| # | Question | Command | Expect | If not |
| --- | --- | --- | --- | --- |
| 1 | **Is the module reset between tenants?** (The FIRST thing to settle: with no reset method nothing clears HBM or device state for the next customer.) | per module: `cat /sys/bus/pci/devices/ADDR/reset_method`; `ls -l /sys/bus/pci/devices/ADDR/reset*`; `lspci -vv -s ADDR \| grep -i FLReset`; then `runcrate-host status` and `sh install.sh --check` (they WARN on an empty or missing `reset_method`). Then the data test: VM A (Habana runtime from the customer launch script) writes a known pattern to HBM and stops; VM B on the same module reads it back | `reset_method` lists a method (`flr`, `bus`, ...), VM B reads zeros or garbage, never A's pattern | no method, or the pattern survives: keep Gaudi off customer launches; try the `1da3:1060` row of `busResetIDs` (`internal/vfio/vfio.go`) only with a run behind it; or decide on a Gaudi-specific scrub image |
| 2 | **Can a guest write firmware or SPI flash state that survives a reset?** | in a VM with one module: with the Habana tools from the customer's own stack, list the firmware/SPI/flash interfaces (`hl-fw-loader`, `hl-smi -L`, `hl-smi --help`, and `ls /sys/class/accel/*/`), try a harmless write or a read-back of a changed setting, stop the VM, start a new one on the same module and read it again | a guest can't write persistent device state, or whatever it writes is cleared by the reset | a guest-writable persistent store means a tenant can leave something for the next one: needs a firmware lock or no rental |
| 3 | **Do the Gaudi scale-out ports reach the site network from a customer VM?** (The virtio NIC isolation and the benchmark's LAN probe do not cover them: Gaudi 3 has its own 200 GbE ports on the module.) | host: `ethtool -i` and `ip -br link` for the Habana network ports (`hl-smi -L` / `lspci -nn -d 1da3:` for NIC functions), `ls /sys/class/net/` before and after the modules are bound; in a VM: `ip -br link`, then try to reach a site address through any Habana port | the ports are not visible in a VM, or are down, or are unplugged; a VM can't reach the LAN through them | if a VM can bring a port up, they must be cabled to nothing, disabled in firmware/BIOS, or the install must refuse the machine |
| 4 | **OAM link fabric:** do the modules' ports come up inside a VM, and what does a neighbour module see? (The 8 modules are linked on the baseboard, which is why a VM takes all of them: `Manager.claim` and `PartialVMOK: false`.) | VM with all 8: from the Habana tools `hl-smi -L`, a topology/link query, and a collective between 2 modules; host side while it runs: `dmesg \| tail`; no other VM exists | the links come up inside the VM and only the VM's own modules answer | if a module can ever be given to another tenant, a neighbour could be reached over the link fabric: keep the whole-baseboard rule |

### 1. PCI identity

| Assumption | Command | Expect | If not, change |
| --- | --- | --- | --- |
| Eight modules, vendor 1da3, device 1060 | `lspci -nn -d 1da3:` | 8 lines ending `[1da3:1060]` | `gpuVendors` in `internal/hw/pci.go` |
| No other 1da3 function that is not a module (a NIC, a management function) | `lspci -nn -d 1da3: \| wc -l`, and `ls /sys/bus/pci/devices/<ADDR-without-function>.*` | exactly the 8 module functions | the companion rule in `GPUs()` binds every other function of the same vendor and slot; a stray function becomes a companion |
| Sysfs values the code reads | `for f in vendor device class; do echo $f $(cat /sys/bus/pci/devices/ADDR/$f); done` | `vendor 0x1da3`, `device 0x1060` | `PCIDevice`, `gpuVendors` |
| Class code is a processing accelerator | `lspci -nnk -d 1da3:` (the bracket after the class name), `cat /sys/bus/pci/devices/ADDR/class` | `[1200]` / `0x120000` | `Classes` of the Habana row. The code accepts any `0x12xx` or `0x03xx`; a different class (for example `0x0b40`) makes the module invisible, so check this first |
| pci.ids knows the card | `grep -A4 '^1da3' /usr/share/misc/pci.ids` | a name under `1060` (and `1063`), or nothing | none; with no entry the agent names the card `Intel Gaudi 3 OAM` from `deviceInfo`. Note the name pci.ids gives: it wins over ours and goes to Runcrate as `ModelRaw` |
| Gaudi 3 PCIe id | `lspci -nn -d 1da3:1063` (only if a PCIe card is ever added) | the id exists | `Devices` of the Habana row |

### 2. Driver and unbind

| Assumption | Command | Expect | If not, change |
| --- | --- | --- | --- |
| Host driver is named `habanalabs` | `readlink /sys/bus/pci/devices/ADDR/driver`, `lsmod \| grep -i habana` | `.../drivers/habanalabs` | only comments and tests name it; `Bind` unbinds whatever driver is there, so a different name changes nothing in the code |
| Unbind from the host driver works while it is loaded and idle | as root, one module: `echo vfio-pci > /sys/bus/pci/devices/ADDR/driver_override; echo ADDR > /sys/bus/pci/devices/ADDR/driver/unbind; echo ADDR > /sys/bus/pci/drivers_probe; readlink /sys/bus/pci/devices/ADDR/driver` | returns promptly, ends on `vfio-pci`; `dmesg \| tail -50` shows no hang or oops | `Bind` in `internal/vfio/vfio.go` (a Habana-specific pre-step, such as stopping a service, would go there) |
| What holds a module and blocks the unbind | `fuser -v /dev/accel/* /dev/hl* 2>&1`, `systemctl list-units '*habana*'` | nothing running | the installer must stop it before `bind-gpus`, as it stops `nvidia-persistenced` |
| The module comes back to habanalabs on release | after the test above: `echo > /sys/bus/pci/devices/ADDR/driver_override; echo ADDR > /sys/bus/pci/devices/ADDR/driver/unbind; echo ADDR > /sys/bus/pci/drivers_probe; readlink /sys/bus/pci/devices/ADDR/driver`, then `hl-smi -L` | `habanalabs`; `hl-smi -L` lists it again | `Release` |
| `runcrate-host bind-gpus` and `release-gpus` do the same for all eight | `runcrate-host bind-gpus; lspci -nnk -d 1da3: \| grep 'in use'` then `runcrate-host release-gpus` | 8x `vfio-pci`, then 8x `habanalabs` | `bindGPUs`, `releaseGPUs` in `cmd/runcrate-host/main.go` |

### 3. IOMMU groups

| Assumption | Command | Expect | If not, change |
| --- | --- | --- | --- |
| IOMMU on | `cat /proc/cmdline` | contains `intel_iommu=on iommu=pt`, no `pcie_acs_override` | the installer's GRUB step (piece 2) |
| Each module is alone in its group (bridges excepted) | `for d in $(lspci -D -d 1da3: \| cut -d' ' -f1); do echo "$d group $(basename $(readlink /sys/bus/pci/devices/$d/iommu_group)): $(ls /sys/bus/pci/devices/$d/iommu_group/devices \| tr '\n' ' ')"; done` | one module per group, no NIC or other device | `Blocker` rules in `GPUs()`; if the XE9680 puts two modules in one group nothing can be passed through singly |
| Group members of a module | `ls /sys/bus/pci/devices/ADDR/iommu_group/devices` | only the module (and its bridge, if shared) | as above |

### 4. Reset method

The code treats Gaudi's reset as unknown: the kernel's own `reset_method` list is left alone. `busResetIDs` in `internal/vfio/vfio.go` has no `1da3` entry.

| Assumption | Command | Expect | If not, change |
| --- | --- | --- | --- |
| Which resets the kernel offers | `cat /sys/bus/pci/devices/ADDR/reset_method` | for example `flr bus` | add a `1da3:1060` row to `busResetIDs` only if FLR fails the test below |
| Function Level Reset is advertised | `lspci -vv -s ADDR \| grep -i -E 'FLReset\|DevCap'` | `FLReset+` | as above |
| A module survives being opened and reset by QEMU | bind one module, boot a VM with it, stop the VM, then `lspci -vv -s ADDR \| grep -E 'Region\|Control'` and check the BARs are still assigned and memory decode is on; `hl-smi -L` after `release-gpus` | BARs intact, `hl-smi` sees the module again with no error | the H100 PCIe needed a bus reset after exactly this failure; add `1da3:1060` only with a run like that behind it |

### 5. hl-smi (optional: the agent works without it)

The agent calls `hl-smi --query-aip=bus_id,name,memory.total --format=csv,noheader,nounits` (the form from Intel's hl-smi documentation, from memory, not verified). If the call fails or prints no line that starts with a PCI address, the cache is left untouched and the card keeps its pci.ids/table name and the labelled 128 GB.

| Assumption | Command | Expect | If not, change |
| --- | --- | --- | --- |
| hl-smi exists on the host and works before the modules are bound | `command -v hl-smi; hl-smi -L; hl-smi` | lists 8 devices | `habanaSMI` in `cmd/runcrate-host/host.go` |
| The query option and field names | `hl-smi --help \| grep -i -E 'query\|format'`, then the call above | 8 lines `<bus id>, <name>, <memory>` | the argument list in `habanaSMI.snapshot` |
| Bus id format | the first column of the call above | `0000:19:00.0`, or a 8-digit domain (`00000000:19:00.0`, handled by `normalizeBusID`) | `normalizeBusID` |
| Memory units | the third column | MiB (a number like `131072`, or less if some is reserved) | `memoryGB`, which reads a bare number as MiB and `GiB`/`GB` suffixes |
| Name string | the second column | for example `HL-325L` | none: it goes to Runcrate as `ModelRaw`; check Runcrate's model list accepts it |
| UUID / serial field | `hl-smi --help`, `hl-smi -L` | a serial or uuid per module | add it to the query and to `Cached.UUID`; today Gaudi has no UUID, the benchmark learns one (`LearnUUIDs`) |
| 128 GB fallback matches reality | the memory column vs the 128 in `gpuVendors` | within a few GB | `VRAMGB` in `Devices` |

### 6. Health and topology

| Assumption | Command | Expect | If not, change |
| --- | --- | --- | --- |
| Link is full width so `Health` is not `degraded` | `cat /sys/bus/pci/devices/ADDR/{current_link_width,max_link_width,current_link_speed,max_link_speed}` | equal pairs (x16, Gen5) | `Health` in `internal/hw/pci.go` |
| AER files exist (or are absent, which counts as ok) | `ls /sys/bus/pci/devices/ADDR/aer_dev_*` | any | none |
| No NVSwitch-like function; scale-up is direct OAM links, not PCI | `lspci -nn \| grep -i ' 0680:'` | none | `Topology` reports `Interconnect: unknown`, `NVSwitchCount: 0`, `PartialVMOK: false` (the modules share a baseboard: the node is rented whole), empty `NVLinkMatrix` for Gaudi; check Runcrate's API accepts these values for an 8-accelerator host |

### 7. Installer (`install.sh`, `sh install.sh --check`)

| Assumption | Command | Expect | If not, change |
| --- | --- | --- | --- |
| `--check` sees the 8 modules, names and sells them | `sh install.sh --check` (as root for the kernel log) | 8 lines `PASS  GPU <addr>: <name>, sold as Gaudi3`, each with `IOMMU group N: only this accelerator`, `driver: habanalabs; ...`, then `Verdict: READY.` | `gaudi_gpus`, `check_gaudi`, `pci_ids_name`, `gpu_model` in `install.sh` |
| The name `gpu_model` sees maps to Gaudi3 | compare the name in the `--check` line with `hl-smi` and `grep -A3 '^1da3' /usr/share/misc/pci.ids`; if the line says `not a model Runcrate sells`, the name is outside `gpu_model`'s Gaudi rule | `Gaudi3`, `Intel Gaudi 3 OAM` or an `HL-325L`/`HL-328`/`HL-338` name | the Gaudi rule in `gpu_model` (the HL model numbers are not verified) |
| The model code `Gaudi3` is what Runcrate's web app wants | `make check-models APP=<web app checkout>` prints `note: the web app's gpuTypeFromName doesn't sell ...` until the web app lists it | no note | `gpuTypeFromName` in the web app (not in this repo) must return `Gaudi3` for these names, or install.sh's code changes to match |
| Open device nodes block the install | `ls -l /dev/accel/ /dev/hl* 2>&1`, then with a Habana process running `fuser /dev/accel/* /dev/hl*` | the nodes that exist, and the pids with the process running | `GAUDI_DEVS` in `install.sh` (nodes are a guess); `fuser` (psmisc) must be installed or the check is silently empty |
| The boot-time file claims the modules before habanalabs | after the install: `cat /etc/modprobe.d/runcrate-host-vfio.conf`; reboot; `lspci -nnk -d 1da3: \| grep 'in use'` and `lsinitramfs /boot/initrd.img-$(uname -r) \| grep -E 'vfio\|runcrate'` | `options vfio-pci ids=1da3:1060`, `softdep habanalabs pre: vfio-pci`; 8x `vfio-pci` right after boot, before the agent runs | add `vfio-pci.ids=1da3:1060 vfio-pci.ids=1da3:1063` to GRUB_CMDLINE_LINUX_DEFAULT as Intel's guide does (the install does not write it: it uses the modprobe file, as for NVIDIA); `update-initramfs -u` must have run |
| Taking Gaudi leaves NVIDIA services alone, and a mixed host takes both; a busy Gaudi aborts before any NVIDIA service is stopped | mixed host: `systemctl is-active nvidia-persistenced` after `install.sh --gpus <only the Gaudi addresses>`; with a process holding `/dev/accel/*`, the install must stop with nothing changed (`systemctl is-enabled nvidia-persistenced` unchanged) | still active; abort leaves the machine as it was | `takes_vendor`, `take_gpus` |
| IOMMU lines are unchanged | `cat /etc/default/grub.d/90-runcrate-host.cfg` (if written) | `intel_iommu=on iommu=pt` only | none |

### 8. VM side (`internal/vm`)

| Assumption | Command | Expect | If not, change |
| --- | --- | --- | --- |
| BAR sizes fit the 64-bit window (the guess is 256 GiB per accelerator, `gaudiMMIO64PerDeviceMB` in `internal/vm/gaudi.go`; the host's own BAR total, doubled, is used when bigger) | host: `lspci -vv -s ADDR \| grep -E 'Region [0-9]+.*size=\|Resizable'`; start one VM with 8 modules and read the command line: `ps -o args= -C qemu-system-x86_64 \| tr ' ' '\n' \| grep -E 'pci-hole64\|PciMmio64'` | each module's regions sum well under 256 GiB; 8 modules give `pci-hole64-size=2097152M` | `gaudiMMIO64PerDeviceMB` |
| The CPU can address a 2 TiB hole | `grep -m1 'address sizes' /proc/cpuinfo` on the host | at least 46 bits | with fewer bits QEMU or OVMF refuses the window: lower the per-device figure |
| OVMF lays the BARs out and the VM boots with 8 modules | boot a VM with all 8; `runcrate-host console <vm>` shows OVMF and the kernel; in the guest `lspci -nn -d 1da3:` | 8 devices, no `BAR ... failed to assign` in `dmesg \| grep -i bar` | the window, or the guest kernel arguments below |
| The guest gets `pci=nocrs,realloc,assign-busses` through the SMBIOS OEM string | host: `ps -o args= -C qemu-system-x86_64 \| grep -o 'smbios[^ ]*'` shows `type=11,value=runcrate.kernel_args=pci=nocrs,,realloc,,assign-busses`; guest: `dmidecode -t 11`, then `cat /proc/cmdline` after the first boot | the OEM string in the guest; the three arguments on the command line | `runcrate-guest-cmdline` and its unit in `image/build.sh` |
| The first-boot reboot comes before the customer's launch script | guest: `journalctl -b -1 -u runcrate-guest-cmdline`, `cloud-init status --long`, `ls /var/lib/runcrate-guest-cmdline.done` | one reboot, the launch script runs after it exactly once, the marker exists | the unit's `Before=`; if cloud-init starts first, the arguments land after the launch script ran |
| A VM without the string is untouched | boot an NVIDIA VM from the same image | no reboot, no `/etc/default/grub.d/91-runcrate-guest.cfg` | the script exits early without the string |
| The image in use has the service | `runcrate-host status` for the image version; in a VM `systemctl status runcrate-guest-cmdline` | present | images built before this change do not have it: build and sign a new image (`image/build.sh`) before a Gaudi VM can get the arguments |
| The customer's own Intel stack still works | launch script installs the Gaudi stack from Intel's repo; then in the guest `hl-smi -L` | all passed-through modules listed; the stack comes from Intel's repo, nothing from the host or the image | nothing is added for it here |
| What `hl-smi -L` prints (the scripts count PCI addresses in it and fall back to the 1da3 PCI functions) | guest: `hl-smi -L`, `ls /sys/bus/pci/devices/*/vendor \| xargs grep -l 0x1da3` | one PCI address per module | `gaudi_count` in `internal/vm/gaudi.go` |
| Throwaway VMs (benchmark, scrub) see the modules without Intel software | `runcrate-host console bench-<id>-intel-habana` during a benchmark | `rc-ephemeral: gaudi accelerators: 8` | `gaudiPrelude` |
| A mixed host boots one benchmark VM per vendor | host with NVIDIA and Gaudi: `systemctl list-units 'runcrate-vm-bench-*'` during a benchmark | `bench-<id>` (NVIDIA, with the isolation checks) then `bench-<id>-intel-habana` | `benchmark` in `internal/agent/agent.go` |

### 9. Decisions the box cannot settle (read before renting a Gaudi out)

- **Scrub.** After each customer VM the NVIDIA host overwrites GPU memory with a CUDA program. For Gaudi no Intel software is bundled, so the Gaudi scrub only checks that every accelerator reached the VM and posts `"wiped": false`. `Manager.checkScrub` treats that (and a Gaudi result that does not say `"wiped": true`) as a FAILED scrub: the modules stay held back and `Create` answers `held back after a failed memory scrub`; `destroy_vm` reports `"wiped": false` for a Gaudi VM. So a Gaudi module can be rented once and is then held. Only a passing benchmark clears a hold, and a Gaudi benchmark never passes, so after the item-1 test (or once a Gaudi scrub image exists) the operator clears it by deleting `<data dir>/holds/<gpu index>` and restarting the agent. NVIDIA results that leave `wiped` out are accepted as ever.
- **Benchmark.** For Gaudi the benchmark VM only counts the accelerators. The agent reports each Gaudi GPU with `mem_ok` false, `matmul_ok` false, `mem_tested_gb` and TFLOPS 0, `visibility_only: true` and `visible_count` (the benchmark VM's count), whatever the VM's script says; the Gaudi GPUs are not passed, not failed, not in `gpus_ok` (the result carries `gpus_visibility_only`), and no hold is set or cleared. The control plane (a separate app change) must treat gaudi3 + `visibility_only` as "pending manual", not as a failure. NVIDIA payloads carry neither field.
- **Whole baseboard.** Gaudi OAM modules are linked to each other on the baseboard. The host reports `partial_vm_ok: false` for any host with a Gaudi module (also a mixed host: the topology is per host), and `Manager.claim` refuses a VM holding some but not all of the host's Gaudi modules. On a mixed host the NVIDIA GPUs keep their own rule (the NVSwitch rule counts only the NVIDIA GPUs) and a VM holds one vendor.
- **The boot-time claim is broader than the accelerators.** The modprobe file's `options vfio-pci ids=` lists every non-bridge 1da3 function id on the machine (broader than `gaudi_gpus` in `install.sh`, which is the class 0x12xx/0x03xx accelerators): a Habana NIC or management function with its own id is claimed by vfio-pci at boot too. `release-gpus` (uninstall) likewise releases every 1da3 function bound to vfio-pci. Check on the box: `lspci -nn -d 1da3:` shows only the 8 module functions, or decide which other ids are meant to be claimed.
- **hl-smi calls have deadlines.** The agent gives hl-smi 30 seconds, `install.sh` 60 seconds when `timeout` exists (a hung Habana driver must not hang start or the install). If a healthy `hl-smi` is slower than that on the box, the names and memory fall back to pci.ids and the 128 GB label. The memory figure is rejected (0) when not a finite positive number or above 1 TiB.
- **Runcrate's side.** Whether the control plane accepts the model name, 128 GB, VRAM 0 from the benchmark VM, `visibility_only`, `partial_vm_ok: false` and an `unknown` interconnect for an 8-accelerator host, and that its model code for Gaudi (`GAUDI3` in the web app, `Gaudi3` in `install.sh`) is compared case-insensitively by `hack/check-models.sh`.
