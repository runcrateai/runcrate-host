#!/bin/sh
# test-install.sh: checks install.sh (its IOMMU boot loader handling, which GPU models it
# counts as sold, its option parsing, its machine checks, what --check reports and that
# --check changes nothing) against a fake
# machine in a temp dir (no root, nothing on this machine is touched).
#   sh hack/test-install.sh
# The variables this sets are install.sh's, which shellcheck does not follow into.
# shellcheck disable=SC2034
set -eu
REPO=$(cd "$(dirname "$0")/.." && pwd)
T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

mkdir -p "$T/bin" "$T/grub.d" "$T/iommu_groups/0"
# update-grub is faked: it only records that it ran.
printf '#!/bin/sh\ntouch "%s/update-grub-ran"\n' "$T" > "$T/bin/update-grub"
chmod +x "$T/bin/update-grub"
ORIG_PATH=$PATH
PATH="$T/bin:$PATH"

. "$REPO/install.sh"
GRUB_CONF="$T/grub.d/90-runcrate-host.cfg"
GRUB_DEFAULT_FILE="$T/grub"
IOMMU_GROUPS="$T/iommu_groups"
KERNEL_CMDLINE="$T/cmdline"
CPUINFO="$T/cpuinfo"
echo "vendor_id : GenuineIntel" > "$CPUINFO"
: > "$GRUB_DEFAULT_FILE"
say() { :; }
step() { :; }
reset() { rm -f "$GRUB_CONF" "$T/update-grub-ran"; }

# 1. Installed, uninstalled (the file is gone), installed again without a reboot:
#    the kernel still has our parameters, so the file comes back for the next boot.
reset
echo "BOOT_IMAGE=/vmlinuz root=/dev/sda1 intel_iommu=on iommu=pt" > "$KERNEL_CMDLINE"
check_iommu
[ -f "$GRUB_CONF" ] || fail "the boot loader file was not written back"
grep -q 'intel_iommu=on iommu=pt' "$GRUB_CONF" || fail "the file does not carry the parameters"
[ -f "$T/update-grub-ran" ] || fail "update-grub did not run"

# 2. The file is there: nothing changes.
rm -f "$T/update-grub-ran"
check_iommu
[ ! -f "$T/update-grub-ran" ] || fail "update-grub ran with the file already in place"

# 3. IOMMU on by the machine's own settings (no parameters of ours on the kernel): nothing is written.
reset
echo "BOOT_IMAGE=/vmlinuz root=/dev/sda1" > "$KERNEL_CMDLINE"
check_iommu
[ ! -f "$GRUB_CONF" ] || fail "a file was written for a machine that turns IOMMU on itself"

# 4. The machine's own boot loader settings carry the parameters: left alone.
reset
echo "BOOT_IMAGE=/vmlinuz intel_iommu=on iommu=pt" > "$KERNEL_CMDLINE"
echo 'GRUB_CMDLINE_LINUX_DEFAULT="quiet intel_iommu=on iommu=pt"' > "$GRUB_DEFAULT_FILE"
check_iommu
[ ! -f "$GRUB_CONF" ] || fail "a file was written although /etc/default/grub has the parameters"

# 4b. The owner's settings carry the parameters on separate lines: still left alone.
reset
printf 'GRUB_CMDLINE_LINUX="intel_iommu=on"\nGRUB_CMDLINE_LINUX_DEFAULT="iommu=pt  quiet"\n' > "$GRUB_DEFAULT_FILE"
check_iommu
[ ! -f "$GRUB_CONF" ] || fail "a file was written although the owner's settings carry both parameters"

# 4c. The parameters are only in a commented-out line: that keeps nothing, so the file is written.
reset
echo '#GRUB_CMDLINE_LINUX_DEFAULT="quiet intel_iommu=on iommu=pt"' > "$GRUB_DEFAULT_FILE"
check_iommu
[ -f "$GRUB_CONF" ] || fail "a commented-out line was taken as keeping the parameters"

# 5. AMD picks its own parameters.
reset
: > "$GRUB_DEFAULT_FILE"
echo "vendor_id : AuthenticAMD" > "$CPUINFO"
echo "BOOT_IMAGE=/vmlinuz amd_iommu=on iommu=pt" > "$KERNEL_CMDLINE"
check_iommu
grep -q 'amd_iommu=on iommu=pt' "$GRUB_CONF" || fail "AMD parameters were not kept"

# 6. gpu_model mirrors which models Runcrate sells (pci.ids and nvidia-smi names).
model_is() {
	got=$(gpu_model "$1" "$2") || got=-
	[ "$got" = "$3" ] || fail "gpu_model '$1' '$2' = $got, want $3"
}
model_is "GH100 [H100 SXM5 80GB]" "" H100
model_is "GH100 [H100 NVL]" "" H100_nvl
model_is "NVIDIA H100 80GB HBM3" 80 H100
model_is "AD102 [GeForce RTX 4090]" "" RTX4090
model_is "AD102 [GeForce RTX 4090 D]" "" -
model_is "GB202 [GeForce RTX 5090]" "" RTX5090
model_is "GB203 [GeForce RTX 5080]" "" -
model_is "GA102 [GeForce RTX 3090 Ti]" "" -
model_is "GA100 [A100 SXM4 80GB]" "" A100_80G
model_is "GA100 [A100 PCIe 40GB]" "" A100
model_is "NVIDIA A100-SXM4-80GB" 80 A100_80G
model_is "GA100 [A100]" 80 A100_80G
model_is "GA100 [A100]" "" -
model_is "GV100GL [Tesla V100 SXM2 32GB]" "" V100_32G
model_is "AD102GL [L40S]" "" L40S
model_is "AD104GL [L4]" "" L4
model_is "GA102GL [RTX A6000]" "" A6000
model_is "AD102GL [RTX 6000 Ada Generation]" "" RTX6000Ada
model_is "GB202GL [RTX PRO 6000 Blackwell Server Edition]" "" RTXPro6000
model_is "NVIDIA RTX PRO 6000 Blackwell Max-Q Workstation Edition" "" -
model_is "GB100 [B200]" "" B200
model_is "GK208B [GeForce GT 710]" "" -
model_is "NVIDIA 2330" "" -
# Intel Gaudi 3 is sold (family gaudi3) under every name the agent or hl-smi gives it; other Habana cards are not.
model_is "Gaudi3 [HL-325L]" "" Gaudi3
model_is "Intel Gaudi 3 OAM" "" Gaudi3
model_is "Intel Gaudi 3 PCIe" "" Gaudi3
model_is "HL-325L" 128 Gaudi3
model_is "HL-338" "" Gaudi3
model_is "Intel Gaudi 2" "" -
model_is "Intel Habana 1ffe" "" -
model_is "Gaudi" "" -

# 7. --check on a fake machine. Each case runs in a subshell that sources the
#    installer again (say is real there) and points every read at the fake tree.
printf '#!/bin/sh\n[ "$1" = -m ] && { echo x86_64; exit 0; }\nexec /usr/bin/uname "$@"\n' > "$T/bin/uname"
printf '#!/bin/sh\necho none\nexit 1\n' > "$T/bin/systemd-detect-virt"
printf '#!/bin/sh\nexit 0\n' > "$T/bin/curl"
printf '#!/bin/sh\nexit 9\n' > "$T/bin/nvidia-smi"
printf '#!/bin/sh\necho "[    0.1] DMAR: IOMMU enabled"\n' > "$T/bin/dmesg"
# shellcheck disable=SC2016
printf '#!/bin/sh\necho "Filesystem 1024-blocks Used Available Capacity Mounted on"\necho "/dev/x 4000000000 0 ${FAKE_FREE_KB:-2097152000} 0%% /"\n' > "$T/bin/df"
chmod +x "$T/bin/uname" "$T/bin/systemd-detect-virt" "$T/bin/curl" "$T/bin/nvidia-smi" "$T/bin/dmesg" "$T/bin/df"

M="$T/machine"
# dev ADDR VENDOR DEVICE CLASS DRIVER GROUP: one PCI function, as sysfs shows it.
dev() {
	b="$M/devices/$1"
	mkdir -p "$b"
	echo "$2" > "$b/vendor"; echo "$3" > "$b/device"; echo "$4" > "$b/class"
	[ -z "$5" ] || ln -s "../../drivers/$5" "$b/driver"
	if [ -n "$6" ]; then
		ln -s "../../groups/$6" "$b/iommu_group"
		mkdir -p "$M/groups/$6/devices"
		: > "$M/groups/$6/devices/$1"
	fi
}
new_machine() {
	rm -rf "$M"
	mkdir -p "$M/devices" "$M/groups" "$M/dmi" "$M/acpi"
	printf 'ID=ubuntu\nVERSION_ID="24.04"\nPRETTY_NAME="Ubuntu 24.04.1 LTS"\n' > "$M/os-release"
	printf 'vendor_id : GenuineIntel\nflags : fpu vmx sse\n' > "$M/cpuinfo"
	echo "MemTotal: 527958016 kB" > "$M/meminfo"
	echo "BOOT_IMAGE=/vmlinuz root=/dev/sda1 intel_iommu=on iommu=pt" > "$M/cmdline"
	echo "Supermicro" > "$M/dmi/sys_vendor"; echo "SYS-421GE" > "$M/dmi/product_name"
	: > "$M/acpi/DMAR"; : > "$M/kvm"
	printf '10de  NVIDIA Corporation\n\t1b80  GP104 [GeForce GTX 1080]\n\t2330  GH100 [H100 SXM5 80GB]\n\t2684  AD102 [GeForce RTX 4090]\n\t22ba  AD102 High Definition Audio Controller\n15b3  Mellanox Technologies\n\t1021  MT2910 Family [ConnectX-7]\n' > "$M/pci.ids"
}
# run_case: --check against the fake machine; prints its output and "exit=N".
run_case() {
	(
		. "$REPO/install.sh"
		OS_RELEASE="$M/os-release"; CPUINFO="$M/cpuinfo"; MEMINFO="$M/meminfo"; KERNEL_CMDLINE="$M/cmdline"
		PCI_DEVICES="$M/devices"; IOMMU_GROUPS="$M/groups"; DMI_DIR="$M/dmi"; ACPI_TABLES="$M/acpi"
		KVM_DEV="$M/kvm"; PCI_IDS="$M/pci.ids"; CONF_DIR="$M/etc"; DEFAULT_DATA_DIR="$M/data"
		if run_check; then echo "exit=0"; else echo "exit=1"; fi
	) 2>&1
}
has() { printf '%s\n' "$out" | grep -qF -- "$1" || { printf '%s\n' "$out" >&2; fail "$2: no line '$1'"; }; }
hasnt() { ! printf '%s\n' "$out" | grep -qF -- "$1" || { printf '%s\n' "$out" >&2; fail "$2: unexpected '$1'"; }; }

# 7a. An H100 with its audio function and a bridge in its group: ready.
new_machine
dev 0000:41:00.0 0x10de 0x2330 0x030200 nvidia 20
dev 0000:41:00.1 0x10de 0x22ba 0x040300 snd_hda_intel 20
dev 0000:40:01.0 0x8086 0x1234 0x060400 pcieport 20
out=$(run_case)
has "PASS  GPU 0000:41:00.0: GH100 [H100 SXM5 80GB], sold as H100" "ready machine"
has "IOMMU group 20: only this card (with 0000:41:00.1)" "ready machine"
has "PASS  RAM: 504 GB (61 GB stays with the host, the rest is for VMs)" "ready machine: 12% kept"
has "driver: nvidia; the install moves it to vfio-pci" "ready machine"
has "PASS  IOMMU: on (IOMMU groups: 1)" "ready machine"
has "kernel log: DMAR: IOMMU enabled" "ready machine"
has "Verdict: READY." "ready machine"
has "exit=0" "ready machine"
hasnt "FAIL" "ready machine"

# 7b. A second GPU sharing its group with a NIC: still ready, with a --gpus hint.
dev 0000:81:00.0 0x10de 0x2684 0x030000 vfio-pci 30
dev 0000:82:00.0 0x15b3 0x1021 0x020000 mlx5_core 30
out=$(run_case)
has "WARN  GPU 0000:81:00.0: AD102 [GeForce RTX 4090], sold as RTX4090" "shared group"
has "can't be passed through: IOMMU group 30 also holds 0000:82:00.0 (class 0x020000)" "shared group"
has "install with --gpus 0000:41:00.0 so the others stay with the host" "shared group"
has "exit=0" "shared group"

# 7c. Only a card Runcrate doesn't sell: not ready.
new_machine
dev 0000:01:00.0 0x10de 0x1b80 0x030000 nouveau 5
echo "MemTotal: 33554432 kB" > "$M/meminfo"
out=$(run_case)
has "WARN  GPU 0000:01:00.0: GP104 [GeForce GTX 1080], not a model Runcrate sells" "unsold card"
has "PASS  RAM: 32 GB (8 GB stays with the host, the rest is for VMs)" "unsold card: 8 GB kept at least"
has "Verdict: NOT READY (no GPU Runcrate can rent out)" "unsold card"
has "exit=1" "unsold card"

# 7d. IOMMU off, the firmware has it: ready after the reboot the install offers.
new_machine
dev 0000:41:00.0 0x10de 0x2330 0x030200 "" ""
echo "BOOT_IMAGE=/vmlinuz root=/dev/sda1" > "$M/cmdline"
out=$(run_case)
has "WARN  IOMMU: off, the kernel command line lacks 'intel_iommu=on iommu=pt'" "iommu off"
has "Verdict: READY AFTER REBOOT" "iommu off"
has "exit=1" "iommu off"

# 7e. IOMMU off in the BIOS (no DMAR table): not ready.
rm -f "$M/acpi/DMAR"
out=$(run_case)
has "FAIL  IOMMU: off in the BIOS" "iommu off in bios"
has "Verdict: NOT READY (IOMMU off in the BIOS)" "iommu off in bios"

# 7f. A virtual machine, a data directory with other files and too little disk: every reason named.
new_machine
dev 0000:41:00.0 0x10de 0x2330 0x030200 vfio-pci 20
echo "QEMU" > "$M/dmi/sys_vendor"; echo "Standard PC (Q35 + ICH9, 2009)" > "$M/dmi/product_name"
mkdir -p "$M/data"; : > "$M/data/notes.txt"
out=$(FAKE_FREE_KB=52428800 run_case)
has "FAIL  Bare metal: no, this is a virtual machine (QEMU Standard PC (Q35 + ICH9, 2009))" "vm"
has "FAIL  Disk: 50 GB free at $M/data, 100 GB needed" "vm"
has "FAIL  Data directory $M/data holds other files" "vm"
has "Verdict: NOT READY (virtual machine; under 100 GB free for VM disks; data directory not empty)" "vm"

# 7g. pcie_acs_override fakes the groups: not ready.
new_machine
dev 0000:41:00.0 0x10de 0x2330 0x030200 vfio-pci 20
echo "BOOT_IMAGE=/vmlinuz intel_iommu=on pcie_acs_override=downstream" > "$M/cmdline"
out=$(run_case)
has "FAIL  IOMMU: on, but the kernel runs with pcie_acs_override" "acs override"
has "exit=1" "acs override"

# 7h. pci.ids present but without this device: the benchmark names it. No pci.ids at all: install pciutils.
new_machine
dev 0000:41:00.0 0x10de 0x2901 0x030200 "" 20
out=$(run_case)
has "WARN  GPU 0000:41:00.0: NVIDIA 2901, model not identified: this machine's pci.ids doesn't list 10de:2901" "unlisted device"
has "fix: nothing to do: the benchmark identifies it from inside the VM" "unlisted device"
hasnt "pciutils" "unlisted device"
rm -f "$M/pci.ids"
out=$(run_case)
has "model not identified: no pci.ids found and no NVIDIA driver" "no pci.ids"
has "install pciutils" "no pci.ids"

# 7i. Gaudi: two OAM modules on habanalabs, each alone in its IOMMU group (a bridge shares one): ready.
new_machine
printf '1da3  Habana Labs Ltd.\n\t1060  Gaudi3 [HL-325L]\n' >> "$M/pci.ids"
dev 0000:19:00.0 0x1da3 0x1060 0x120000 habanalabs 1
dev 0000:18:01.0 0x8086 0x1234 0x060400 pcieport 1
dev 0000:3b:00.0 0x1da3 0x1060 0x120000 habanalabs 2
out=$(run_case)
has "PASS  GPU 0000:19:00.0: Gaudi3 [HL-325L], sold as Gaudi3" "gaudi ready"
has "PASS  GPU 0000:3b:00.0: Gaudi3 [HL-325L], sold as Gaudi3" "gaudi ready"
has "IOMMU group 1: only this accelerator" "gaudi ready"
has "driver: habanalabs; the install moves it to vfio-pci (no NVIDIA service is stopped for it)" "gaudi ready"
hasnt "nvidia-persistenced" "gaudi ready"
hasnt "WARN" "gaudi ready"
has "Verdict: READY." "gaudi ready"
has "exit=0" "gaudi ready"
# Without pci.ids the agent's own name is used; with hl-smi (header line and a longer bus id included) its name is.
rm -f "$M/pci.ids"
out=$(run_case)
has "PASS  GPU 0000:19:00.0: Intel Gaudi 3 OAM, sold as Gaudi3" "gaudi without pci.ids"
printf '#!/bin/sh\necho "bus_id, name, memory.total"\necho "00000000:19:00.0, HL-325L, 131072"\n' > "$T/bin/hl-smi"
chmod +x "$T/bin/hl-smi"
out=$(run_case)
has "PASS  GPU 0000:19:00.0: HL-325L, sold as Gaudi3" "gaudi with hl-smi"
has "PASS  GPU 0000:3b:00.0: Intel Gaudi 3 OAM, sold as Gaudi3" "gaudi without an hl-smi row"
rm -f "$T/bin/hl-smi"

# 7j. Gaudi: a module sharing its group with a NIC, one Runcrate doesn't sell, one with no driver.
new_machine
dev 0000:19:00.0 0x1da3 0x1060 0x120000 habanalabs 1
dev 0000:1a:00.0 0x15b3 0x1021 0x020000 mlx5_core 1
dev 0000:3b:00.0 0x1da3 0x1060 0x120000 habanalabs 2
dev 0000:5c:00.0 0x1da3 0x1ffe 0x120000 "" 3
out=$(run_case)
has "WARN  GPU 0000:19:00.0: Intel Gaudi 3 OAM, sold as Gaudi3" "gaudi shared group"
has "can't be passed through: IOMMU group 1 also holds 0000:1a:00.0 (class 0x020000)" "gaudi shared group"
has "PASS  GPU 0000:3b:00.0: Intel Gaudi 3 OAM, sold as Gaudi3" "gaudi shared group"
has "WARN  GPU 0000:5c:00.0: Intel Habana 1ffe, not a model Runcrate sells" "gaudi unsold"
has "driver: none; the install binds it to vfio-pci" "gaudi no driver"
has "install with --gpus 0000:3b:00.0 so the others stay with the host" "gaudi shared group"
has "exit=0" "gaudi shared group"

# 7k. NVIDIA and Gaudi on one host: every card reported, each by its own rules.
new_machine
dev 0000:41:00.0 0x10de 0x2330 0x030200 nvidia 20
dev 0000:19:00.0 0x1da3 0x1060 0x120000 habanalabs 1
out=$(run_case)
has "PASS  GPU 0000:41:00.0: GH100 [H100 SXM5 80GB], sold as H100" "mixed host"
has "PASS  GPU 0000:19:00.0: Intel Gaudi 3 OAM, sold as Gaudi3" "mixed host"
has "driver: nvidia; the install moves it to vfio-pci (stopping nvidia-persistenced" "mixed host"
has "Verdict: READY." "mixed host"

# 8. check_machine, the install's own checks, on the same fake machines.
# check_case: check_machine against the fake machine; its output and "exit=N".
check_case() {
	(
		. "$REPO/install.sh"
		OS_RELEASE="$M/os-release"; CPUINFO="$M/cpuinfo"; PCI_DEVICES="$M/devices"; DMI_DIR="$M/dmi"; PCI_IDS="$M/pci.ids"
		FORCE=${FAKE_FORCE:-0}
		check_machine
	) 2>&1 && echo "exit=0" || echo "exit=$?"
}
new_machine
dev 0000:41:00.0 0x10de 0x2330 0x030200 nvidia 20
out=$(check_case)
has "OS: Ubuntu 24.04.1 LTS" "check_machine ready"
has "  0000:41:00.0 GH100 [H100 SXM5 80GB]" "check_machine ready"
has "exit=0" "check_machine ready"
echo "QEMU" > "$M/dmi/sys_vendor"
out=$(check_case)
has "Error: this is a virtual machine (QEMU SYS-421GE); Runcrate hosts must be bare metal" "check_machine vm"
has "exit=1" "check_machine vm"
out=$(FAKE_FORCE=1 check_case)
has "exit=0" "check_machine vm with --force"
new_machine
out=$(check_case)
has "Error: no NVIDIA GPU found" "check_machine no gpu"
has "exit=1" "check_machine no gpu"
printf 'vendor_id : GenuineIntel\nflags : fpu sse\n' > "$M/cpuinfo"
dev 0000:41:00.0 0x10de 0x2330 0x030200 nvidia 20
out=$(check_case)
has "Error: CPU virtualization (VT-x / AMD-V) is off" "check_machine no vmx"
new_machine
dev 0000:19:00.0 0x1da3 0x1060 0x120000 habanalabs 1
out=$(check_case)
has "  0000:19:00.0 Intel Gaudi 3 OAM" "check_machine gaudi"
has "exit=0" "check_machine gaudi"

# 8b. The boot-time vfio-pci file: an NVIDIA host's stays what it was; a Gaudi adds its ids and the habanalabs softdep.
printf '#!/bin/sh\ncase "$2" in\n10de:) [ -r "${LSPCI_NV:-/nonexistent}" ] && cat "$LSPCI_NV" ;;\n1da3:) [ -r "${LSPCI_HL:-/nonexistent}" ] && cat "$LSPCI_HL" ;;\nesac\nexit 0\n' > "$T/bin/lspci"
chmod +x "$T/bin/lspci"
printf '41:00.0 0302: 10de:2330 (rev a1)\n41:00.1 0403: 10de:22ba (rev a1)\n40:01.0 0604: 10de:1234\n' > "$T/lspci-nv"
printf '19:00.0 1200: 1da3:1060 (rev 01)\n3b:00.0 1200: 1da3:1060 (rev 01)\n' > "$T/lspci-hl"
# conf_of NVIDIA_LSPCI_FILE HABANA_LSPCI_FILE: vfio_modprobe_conf on the fake machine.
conf_of() { (. "$REPO/install.sh"; PCI_DEVICES="$M/devices"; LSPCI_NV=$1; LSPCI_HL=$2; export LSPCI_NV LSPCI_HL; vfio_modprobe_conf); }
nv_conf='# Written by the Runcrate host installer. Removed by --uninstall.
options vfio-pci ids=10de:22ba,10de:2330
softdep nvidia pre: vfio-pci
softdep nouveau pre: vfio-pci
softdep snd_hda_intel pre: vfio-pci
softdep xhci_pci pre: vfio-pci'
new_machine
dev 0000:41:00.0 0x10de 0x2330 0x030200 nvidia 20
got=$(conf_of "$T/lspci-nv" "")
[ "$got" = "$nv_conf" ] || { printf '%s\n' "$got" >&2; fail "an NVIDIA host's modprobe file changed"; }
dev 0000:19:00.0 0x1da3 0x1060 0x120000 habanalabs 1
got=$(conf_of "$T/lspci-nv" "$T/lspci-hl")
printf '%s\n' "$got" | grep -qx 'options vfio-pci ids=10de:22ba,10de:2330,1da3:1060' || { printf '%s\n' "$got" >&2; fail "mixed host: ids"; }
[ "$(printf '%s\n' "$got" | tail -n 1)" = "softdep habanalabs pre: vfio-pci" ] || fail "mixed host: no habanalabs softdep"
printf '%s\n' "$got" | grep -qx 'softdep nvidia pre: vfio-pci' || fail "mixed host: no nvidia softdep"
got=$(conf_of "" "$T/lspci-hl")
printf '%s\n' "$got" | grep -qx 'options vfio-pci ids=1da3:1060' || { printf '%s\n' "$got" >&2; fail "gaudi host: ids"; }
printf '%s\n' "$got" | grep -qx 'softdep habanalabs pre: vfio-pci' || fail "gaudi host: no habanalabs softdep"
rm -f "$T/bin/lspci"

# 8c. The install takes a vendor's GPUs when it takes all of them or --gpus names one; a Gaudi's open device nodes are found.
# tv GPUS ADDRESSES: takes_vendor with --gpus GPUS.
tv() { if (. "$REPO/install.sh"; GPUS=$1; takes_vendor "$2"); then echo yes; else echo no; fi; }
[ "$(tv "" "0000:19:00.0")" = yes ] || fail "takes_vendor: all GPUs by default"
[ "$(tv "" "")" = no ] || fail "takes_vendor: no GPUs of that vendor"
[ "$(tv "0000:41:00.0" "0000:19:00.0")" = no ] || fail "takes_vendor: --gpus names only the other vendor"
[ "$(tv "0000:41:00.0,0000:19:00.0" "0000:3b:00.0 0000:19:00.0")" = yes ] || fail "takes_vendor: --gpus names one of them"
mkdir -p "$T/accel"; : > "$T/accel/accel0"
printf '#!/bin/sh\necho " 1"\n' > "$T/bin/fuser"
chmod +x "$T/bin/fuser"
got=$(. "$REPO/install.sh"; GAUDI_DEVS="$T/accel/*"; gaudi_processes)
[ "$got" = "1 $(ps -o comm= -p 1)" ] || fail "gaudi_processes = '$got'"
got=$(. "$REPO/install.sh"; GAUDI_DEVS="$T/none/*"; gaudi_processes)
[ -z "$got" ] || fail "gaudi_processes without device nodes = '$got'"
rm -f "$T/bin/fuser"

# 9. Options that take a value refuse a missing one, or the next option in its place.
for args in "--name --check" "--gpus" "--data-dir -y" "--token-file --check"; do
	# shellcheck disable=SC2086
	if out=$( (. "$REPO/install.sh"; parse_args $args; echo parsed) 2>&1); then fail "parse_args $args went on: $out"; fi
	case "$out" in *"needs a value"*) ;; *) fail "parse_args $args: $out" ;; esac
done

# 10. sh install.sh --check, as a process: every command that changes a machine is a stub
#     that records the call and fails, and nothing is written in its working directory.
G="$T/guard"
mkdir -p "$G" "$T/cwd" "$T/home"
for c in apt apt-get dpkg debconf-set-selections modprobe rmmod insmod systemctl update-grub update-initramfs \
	udevadm useradd userdel usermod groupadd groupdel reboot shutdown install mkdir rm cp mv ln chmod chown \
	touch tee dd stty mount umount sysctl nft iptables ip6tables ip tc sudo su; do
	printf '#!/bin/sh\necho "%s $*" >> "%s/mutations"\nexit 97\n' "$c" "$T" > "$G/$c"
	chmod +x "$G/$c"
done
cp "$T/bin/curl" "$T/bin/nvidia-smi" "$T/bin/dmesg" "$G/"
for who in user root; do
	if [ "$who" = root ]; then printf '#!/bin/sh\n[ "$1" = -u ] && { echo 0; exit 0; }\nexec /usr/bin/id "$@"\n' > "$G/id"; chmod 755 "$G/id"; fi
	# The real install.sh, run the way curl | sh runs it, with only the guard in front of PATH.
	out=$(cd "$T/cwd" && HOME="$T/home" PATH="$G:$ORIG_PATH" sh "$REPO/install.sh" --check 2>&1) && rc=0 || rc=$?
	[ ! -s "$T/mutations" ] || { cat "$T/mutations" >&2; fail "--check as $who ran a mutating command"; }
	case "$rc" in 0|1) ;; *) printf '%s\n' "$out" >&2; fail "--check as $who exited $rc" ;; esac
	has "Verdict: " "--check as $who"
	[ -z "$(ls -A "$T/cwd")$(ls -A "$T/home")" ] || fail "--check as $who wrote files"
done
rm -f "$G/id"
out=$(cd "$T/cwd" && PATH="$G:$ORIG_PATH" sh "$REPO/install.sh" --name --check 2>&1) && fail "--name --check went on: $out"
has "--name needs a value" "--name --check"
[ ! -s "$T/mutations" ] || fail "--name --check ran a mutating command"
echo "ok: install.sh keeps the IOMMU boot loader file, its checks and --check report the fake machines, and --check changes nothing"
