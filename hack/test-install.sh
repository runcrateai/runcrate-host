#!/bin/sh
# test-install.sh: checks install.sh (its IOMMU boot loader handling, which GPU models it
# counts as sold and what --check reports) against a fake
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
out=$(run_case)
has "WARN  GPU 0000:01:00.0: GP104 [GeForce GTX 1080], not a model Runcrate sells" "unsold card"
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

echo "ok: install.sh keeps the IOMMU boot loader file across an uninstall and reinstall, and --check reports"
