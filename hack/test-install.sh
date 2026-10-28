#!/bin/sh
# test-install.sh: checks install.sh's IOMMU boot loader handling against a fake
# machine in a temp dir (no root, nothing on this machine is touched).
#   sh hack/test-install.sh
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

RUNCRATE_HOST_INSTALL_SOURCED=1
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

# 5. AMD picks its own parameters.
reset
: > "$GRUB_DEFAULT_FILE"
echo "vendor_id : AuthenticAMD" > "$CPUINFO"
echo "BOOT_IMAGE=/vmlinuz amd_iommu=on iommu=pt" > "$KERNEL_CMDLINE"
check_iommu
grep -q 'amd_iommu=on iommu=pt' "$GRUB_CONF" || fail "AMD parameters were not kept"

echo "ok: install.sh keeps the IOMMU boot loader file across an uninstall and reinstall"
