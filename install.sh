#!/bin/sh
# Runcrate host installer.
#
#   curl -fsSL https://get.runcrate.ai/host | sudo sh                 (asks for the registration token)
#   curl -fsSL https://get.runcrate.ai/host | sudo sh -s -- --token-file /root/runcrate-token
#   curl -fsSL https://get.runcrate.ai/host | sudo sh -s -- --uninstall
#   curl -fsSL https://get.runcrate.ai/host | sh -s -- --check          (no token, no root: only reports)
#
# This script is signed like the agent: https://get.runcrate.ai/host.sig is
# Runcrate's ed25519 signature of it as runcrate-install <host.version>
# (hack/check-install.sh verifies it). The agent comes from
# https://get.runcrate.ai/releases/latest and is refused unless signed.
#
# Every step checks before it changes anything, so running it again is safe.
# Everything runs from main() on the last line, so a cut-off download does nothing.
set -eu

BASE_URL="${RUNCRATE_HOST_BASE_URL:-https://get.runcrate.ai/releases/latest}"
ASSET=runcrate-host-linux-amd64
BIN=/usr/local/bin/runcrate-host
CONF_DIR=/etc/runcrate-host
DEFAULT_DATA_DIR=/var/lib/runcrate-host
UNIT_DIR=/etc/systemd/system
MODPROBE_CONF=/etc/modprobe.d/runcrate-host-vfio.conf
MODULES_CONF=/etc/modules-load.d/runcrate-host.conf
GRUB_CONF=/etc/default/grub.d/90-runcrate-host.cfg
# The machine's own boot loader settings (read only), and what the running kernel reports.
GRUB_DEFAULT_FILE=/etc/default/grub
IOMMU_GROUPS=/sys/kernel/iommu_groups
KERNEL_CMDLINE=/proc/cmdline
CPUINFO=/proc/cpuinfo
# Read only, by the machine checks (--check and the install's own).
OS_RELEASE=/etc/os-release
PCI_DEVICES=/sys/bus/pci/devices
DMI_DIR=/sys/class/dmi/id
ACPI_TABLES=/sys/firmware/acpi/tables
MEMINFO=/proc/meminfo
KVM_DEV=/dev/kvm
# Where Debian and Ubuntu keep pci.ids; the agent reads the first one found too
# (internal/hw/pci.go DefaultPCIIDsPaths).
PCI_IDS="/usr/share/misc/pci.ids /usr/share/hwdata/pci.ids /usr/share/pci.ids"
# Free space the VM data directory needs (GB), as the README says.
MIN_DISK_GB=100
# RAM the agent keeps for the host (internal/vm/manager.go reserveRAMGB).
HOST_RAM_GB=8
UDEV_RULE=/etc/udev/rules.d/90-runcrate-host.rules
DISABLED_UNITS="$CONF_DIR/disabled-units"
# QEMU runs as this unprivileged user (group kvm); the agent expects it.
VM_USER=runcrate-vm
# Runcrate's release signing key (ed25519, SubjectPublicKeyInfo, base64).
# The agent carries the same key for its own updates and VM images.
RELEASE_PUBKEY=MCowBQYDK2VwAyEAYBrMXteHsZh26jJMcMZ3PI05QmTbNcd7gy9ALNlW2Yc=

# The registration token never goes on a command line (where every user on the
# machine can read it in ps): it comes from a root-only file or a hidden prompt.
TOKEN=""
TOKEN_FILE=""
HOST_NAME=""
SITE=""
GPUS=""
DATA_DIR=""
API=""
BINARY=""
YES=0
UNINSTALL=0
FORCE=0
CHECK=0
tmp=""
echo_off=0

say() { printf '%s\n' "$*"; }
step() { printf '\n==> %s\n' "$*"; }
die() { printf '\nError: %s\n' "$*" >&2; exit 1; }

cleanup() {
	if [ "$echo_off" -eq 1 ]; then stty echo < /dev/tty 2>/dev/null || true; fi
	if [ -n "$tmp" ]; then rm -rf "$tmp"; fi
}

usage() {
	cat <<'EOF'
Runcrate host installer

  --token-file F  read the registration token from file F (first line); F must be
                  owned by root and readable by root only (chmod 600)
  --name N        host name shown in your dashboard (default: hostname)
  --site S        site name, to group hosts in one location
  --gpus LIST     only these GPUs, as PCI addresses (0000:41:00.0,0000:42:00.0); default: all NVIDIA GPUs
  --data-dir D    where VM disks live (default /var/lib/runcrate-host); needs fast local disk
  --yes           don't ask: turn on IOMMU and reboot if needed
  --uninstall     stop every VM, wipe their disks, give the GPUs back, remove runcrate-host
  --check         only check whether this machine can be a Runcrate host: needs no token and
                  no root, installs and changes nothing (exit 0 when ready)
  --binary PATH   install this runcrate-host binary instead of downloading it (testing only:
                  it is not signature-checked)
  --api URL       host API (testing only)
  --force         go on past the OS, bare-metal and IOMMU checks (testing only)
EOF
}

# need_value OPTION VALUE: an option that takes a value got one. A missing
# value, or the next option in its place (--name --check), stops everything.
need_value() {
	case "${2-}" in ''|-*) die "$1 needs a value" ;; esac
}

parse_args() {
	while [ $# -gt 0 ]; do
		case "$1" in
			--token) die "--token is gone: the token would show in ps. Put it in a root-only file and pass --token-file, or leave it out to be asked for it" ;;
			--token-file) need_value "$@"; TOKEN_FILE=$2; shift 2 ;;
			--name) need_value "$@"; HOST_NAME=$2; shift 2 ;;
			--site) need_value "$@"; SITE=$2; shift 2 ;;
			--gpus) need_value "$@"; GPUS=$2; shift 2 ;;
			--data-dir) need_value "$@"; DATA_DIR=$2; shift 2 ;;
			--api) need_value "$@"; API=$2; shift 2 ;;
			--binary) need_value "$@"; BINARY=$2; shift 2 ;;
			--yes|-y) YES=1; shift ;;
			--uninstall) UNINSTALL=1; shift ;;
			--check) CHECK=1; shift ;;
			--force) FORCE=1; shift ;;
			-h|--help) usage; exit 0 ;;
			*) usage; die "unknown option: $1" ;;
		esac
	done
}

# has_tty: a terminal we can talk to, even when the script itself is piped in.
has_tty() { (: < /dev/tty) 2>/dev/null; }

# ask QUESTION: yes with --yes, otherwise read y/N from the terminal.
ask() {
	[ "$YES" -eq 1 ] && return 0
	has_tty || return 1
	printf '%s [y/N] ' "$1" > /dev/tty
	read -r reply < /dev/tty || return 1
	case "$reply" in y|Y|yes|YES) return 0 ;; *) return 1 ;; esac
}

# read_token: from --token-file or a hidden prompt. Not needed when this
# machine is already registered.
read_token() {
	if [ -n "$TOKEN_FILE" ]; then
		[ -f "$TOKEN_FILE" ] && [ -r "$TOKEN_FILE" ] || die "can't read --token-file $TOKEN_FILE"
		# Other users must not be able to read (or swap) the token.
		perm=$(stat -L -c '%u %a' "$TOKEN_FILE")
		case "$perm" in
			"0 "[0-7]00 | "0 0") ;;
			*) die "--token-file $TOKEN_FILE must be owned by root and readable by root only: chown root $TOKEN_FILE && chmod 600 $TOKEN_FILE" ;;
		esac
		TOKEN=$(sed -n '1{s/[[:space:]]//g;p;}' "$TOKEN_FILE")
		[ -n "$TOKEN" ] || die "--token-file $TOKEN_FILE is empty"
		return 0
	fi
	[ -f "$CONF_DIR/credential" ] && return 0
	if has_tty; then
		printf 'Registration token (from your Runcrate partner dashboard): ' > /dev/tty
		if stty -echo < /dev/tty 2>/dev/null; then echo_off=1; fi
		IFS= read -r TOKEN < /dev/tty || TOKEN=""
		if [ "$echo_off" -eq 1 ]; then stty echo < /dev/tty 2>/dev/null || true; echo_off=0; fi
		printf '\n' > /dev/tty
		TOKEN=$(printf '%s' "$TOKEN" | tr -d '[:space:]')
	fi
	[ -n "$TOKEN" ] || { usage; die "a registration token is required: --token-file, or run it in a terminal to be asked"; }
}

# data_dir: the VM data directory from the agent's config, or the default.
data_dir() {
	d=""
	if [ -f "$CONF_DIR/config.json" ]; then
		d=$(sed -n 's/.*"data_dir": *"\([^"]*\)".*/\1/p' "$CONF_DIR/config.json" | head -n1)
	fi
	case "$d" in /?*) printf '%s\n' "$d" ;; *) printf '%s\n' "$DEFAULT_DATA_DIR" ;; esac
}

uninstall() {
	step "Stopping runcrate-host"
	systemctl disable --now runcrate-host.service >/dev/null 2>&1 || true
	systemctl disable --now runcrate-host-gpus.service >/dev/null 2>&1 || true
	if [ -x "$BIN" ]; then
		step "Stopping VMs, wiping their disks, giving the GPUs back"
		"$BIN" uninstall-runtime || say "Some cleanup failed; see above. Run again to retry."
	fi
	rm -f "$UNIT_DIR/runcrate-host.service" "$UNIT_DIR/runcrate-host-gpus.service" "$MODULES_CONF"
	systemctl daemon-reload || true
	if [ -f "$MODPROBE_CONF" ]; then
		rm -f "$MODPROBE_CONF"
		command -v update-initramfs >/dev/null 2>&1 && update-initramfs -u >/dev/null 2>&1 || true
	fi
	if [ -f "$GRUB_CONF" ]; then
		rm -f "$GRUB_CONF"
		command -v update-grub >/dev/null 2>&1 && update-grub >/dev/null 2>&1 || true
	fi
	if [ -f "$UDEV_RULE" ]; then
		rm -f "$UDEV_RULE"
		udevadm control --reload-rules >/dev/null 2>&1 || true
	fi
	if [ -f "$DISABLED_UNITS" ]; then
		while read -r u; do
			[ -n "$u" ] && systemctl enable --now "$u" >/dev/null 2>&1 || true
		done < "$DISABLED_UNITS"
	fi
	# Only a data directory the agent claimed (new or empty, with its marker),
	# and in it only what the agent creates; the directory goes too when that
	# leaves it empty.
	data=$(data_dir)
	if [ -f "$data/.runcrate-host-data" ] && [ ! -L "$data/.runcrate-host-data" ]; then
		rm -rf "$data/vms" "$data/images" "$data/holds"
		rm -f "$data/state.json" "$data/gpus-cache.json" "$data/.runcrate-diskbench" \
			"$data"/.state.json.tmp* "$data"/.gpus-cache.json.tmp* "$data/.runcrate-host-data"
		if ! rmdir "$data" 2>/dev/null; then
			say "Left $data in place: it holds files runcrate-host didn't create."
		fi
	elif [ -d "$data" ]; then
		say "Left $data alone: it isn't marked as runcrate-host's data directory."
	fi
	rm -rf "$CONF_DIR"
	rm -f "$BIN" "$BIN.prev" "$BIN.new"
	# The agent adds one QEMU user per VM slot (runcrate-vm-<n>, group runcrate-vm).
	for u in $(getent passwd | cut -d: -f1 | grep -E "^$VM_USER-[0-9]+\$" || true) "$VM_USER"; do
		if id "$u" >/dev/null 2>&1; then
			userdel "$u" >/dev/null 2>&1 || say "Could not remove the $u user (still in use?)."
		fi
	done
	if getent group "$VM_USER" >/dev/null 2>&1; then groupdel "$VM_USER" >/dev/null 2>&1 || true; fi
	step "Done"
	say "runcrate-host is removed and every GPU is back with the host. The GPUs reach your own"
	say "driver now (or after a reboot if nothing was loaded). Packages it installed (qemu, ovmf,"
	say "nftables, iperf3) are left in place. If the machine is still in your Runcrate dashboard,"
	say "remove it there too (Hosts, then its menu, Remove)."
}

# ---- Machine detection, shared by the install and --check. Read only. ----

# os_id: ID:VERSION_ID from os-release. Read in a subshell: os-release sets
# NAME, ID and others that must not leak into this script.
# shellcheck source=/dev/null
os_id() { (. "$OS_RELEASE" && printf '%s:%s' "${ID:-}" "${VERSION_ID:-}") 2>/dev/null || true; }
# shellcheck source=/dev/null
os_pretty() { (. "$OS_RELEASE" && printf '%s' "${PRETTY_NAME:-unknown}") 2>/dev/null || printf 'unknown'; }
os_supported() {
	case "$(os_id)" in ubuntu:22.04|ubuntu:24.04|debian:12) return 0 ;; esac
	return 1
}

# virt_kind: the hypervisor's name when this is a virtual machine, empty on bare
# metal. systemd-detect-virt first, then the DMI tables and the CPU's hypervisor
# flag, which also catch a VM without systemd.
virt_kind() {
	if command -v systemd-detect-virt >/dev/null 2>&1; then
		v=$(systemd-detect-virt --vm 2>/dev/null || true)
		case "$v" in ''|none) ;; *) printf '%s\n' "$v"; return 0 ;; esac
	fi
	v=$(cat "$DMI_DIR/sys_vendor" "$DMI_DIR/product_name" 2>/dev/null | tr -s '\n' ' ' | sed 's/ *$//')
	case "$v" in
		*QEMU*|*KVM*|*VMware*|*VirtualBox*|*innotek*|*Xen*|*Bochs*|*BHYVE*|*Parallels*|*"Virtual Machine"*|*Virtualization*)
			printf '%s\n' "$v"; return 0 ;;
	esac
	if grep -qw hypervisor "$CPUINFO" 2>/dev/null; then echo "the CPU reports a hypervisor"; fi
}

# container_kind: the container runtime when this runs in one, else empty.
container_kind() {
	if command -v systemd-detect-virt >/dev/null 2>&1; then
		v=$(systemd-detect-virt --container 2>/dev/null || true)
		case "$v" in ''|none) ;; *) printf '%s\n' "$v"; return 0 ;; esac
	fi
	if [ -f /.dockerenv ]; then echo docker; elif [ -f /run/.containerenv ]; then echo podman; fi
}

# cpu_virt_on: VT-x or AMD-V is on (the kernel hides the flag when the BIOS turns it off).
cpu_virt_on() { grep -Eq 'vmx|svm' "$CPUINFO" 2>/dev/null; }

# iommu_on: the kernel has IOMMU groups.
iommu_on() { [ -n "$(ls -A "$IOMMU_GROUPS" 2>/dev/null)" ]; }

# acs_override: the kernel fakes IOMMU groups, which the agent refuses
# (internal/hw/pci.go acsOverride).
acs_override() { grep -q pcie_acs_override "$KERNEL_CMDLINE" 2>/dev/null; }

# nvidia_gpus: the PCI address of every NVIDIA display or 3D controller, sorted
# (the agent's rule: internal/hw/pci.go IsGPU).
nvidia_gpus() {
	for d in "$PCI_DEVICES"/*; do
		[ -r "$d/vendor" ] && [ "$(cat "$d/vendor")" = 0x10de ] || continue
		case "$(cat "$d/class" 2>/dev/null)" in 0x0300*|0x0302*) printf '%s\n' "${d##*/}" ;; esac
	done
}

# pci_driver ADDR: the driver bound to a PCI function, or "none".
pci_driver() {
	l=$(readlink "$PCI_DEVICES/$1/driver" 2>/dev/null) || l=none
	printf '%s\n' "${l##*/}"
}

# pci_ids_file: the pci.ids the agent reads (the first one found), or nothing.
pci_ids_file() {
	for f in $PCI_IDS; do
		if [ -r "$f" ]; then printf '%s\n' "$f"; return 0; fi
	done
}

# pci_device_id ADDR: the PCI device id, lower case, without 0x.
pci_device_id() { sed 's/^0x//' "$PCI_DEVICES/$1/device" 2>/dev/null | tr '[:upper:]' '[:lower:]'; }

# pci_ids_name ADDR: the GPU's name from pci.ids, as the agent reads it
# (internal/hw/pci.go LoadPCIIDs), or "NVIDIA <device id>".
pci_ids_name() {
	dev=$(pci_device_id "$1")
	n=""
	f=$(pci_ids_file)
	if [ -n "$f" ]; then
		n=$(awk -v id="$dev" '/^#/ || /^$/ {next}
			/^[^\t]/ {nv = ($0 ~ /^10de /); next}
			nv && /^\t[^\t]/ && tolower(substr($0, 2, 4)) == id && substr($0, 6, 1) == " " {s = substr($0, 7); sub(/^[ \t]+/, "", s); sub(/[ \t]+$/, "", s); print s; exit}' "$f")
	fi
	printf '%s\n' "${n:-NVIDIA $dev}"
}

# smi_gpus: "bus id, name, memory MiB" per GPU from the host's NVIDIA driver,
# empty without one. The installer registers these names when it can.
smi_gpus() {
	command -v nvidia-smi >/dev/null 2>&1 || return 0
	nvidia-smi --query-gpu=pci.bus_id,name,memory.total --format=csv,noheader,nounits 2>/dev/null || true
}

# gpu_processes: the processes using the GPUs through the host's NVIDIA driver.
gpu_processes() {
	command -v nvidia-smi >/dev/null 2>&1 && nvidia-smi -L >/dev/null 2>&1 || return 0
	nvidia-smi --query-compute-apps=pid,process_name --format=csv,noheader 2>/dev/null || true
}

# The GPU models Runcrate sells. Mirrors gpuTypeFromName in Runcrate's web app
# (src/lib/providers/gpu-names.ts), which decides at registration whether a
# GPU is sold; NVIDIA models only, since hosts are NVIDIA. When that list
# changes, change this one: `make check-models APP=<web app checkout>` runs
# both on every NVIDIA name in pci.ids and fails when they disagree.
SOLD_MODELS="b200:B200 b300:B300 h200:H200 l40s:L40S l40:L40 l4:L4 a40:A40 a10:A10 a6000:A6000 a5000:A5000 a4000:A4000"
# GeForce cards sold, by exact model number (a 4090 D or 5070 Ti is not).
SOLD_RTX="5090:RTX5090 4090:RTX4090 3090:RTX3090"

# has_word WORD " LIST ": WORD is one of the space-separated words.
has_word() { case "$2" in *" $1 "*) return 0 ;; esac; return 1; }

# gpu_model NAME [MEMORY_GB]: Runcrate's model for a GPU name (pci.ids or
# nvidia-smi), or nothing (status 1) when Runcrate doesn't sell it. MEMORY_GB
# tells A100 and V100 sizes apart when the name doesn't.
gpu_model() {
	gw=" "
	for t in $(printf '%s' "$1" | tr '[:upper:]' '[:lower:]' | tr -cs 'a-z0-9.' ' '); do
		case "$t" in nvidia|geforce|tesla|amd|instinct) ;; *) gw="$gw$t " ;; esac
	done
	[ "$gw" != " " ] || return 1
	# A MIG slice is not a whole GPU.
	has_word mig "$gw" && return 1
	if has_word rtx "$gw" && has_word pro "$gw" && has_word 6000 "$gw"; then
		if has_word max "$gw" || has_word maxq "$gw"; then return 1; fi
		echo RTXPro6000; return 0
	fi
	if has_word 6000 "$gw" && has_word ada "$gw"; then echo RTX6000Ada; return 0; fi
	for t in $gw; do
		for kv in $SOLD_MODELS; do
			if [ "$t" = "${kv%%:*}" ]; then echo "${kv#*:}"; return 0; fi
		done
	done
	rtx=""
	for t in $gw; do
		for kv in $SOLD_RTX; do
			if [ -z "$rtx" ] && [ "$t" = "${kv%%:*}" ]; then rtx=${kv#*:}; fi
		done
	done
	if [ -n "$rtx" ] && ! has_word ti "$gw" && ! has_word super "$gw" && ! has_word d "$gw"; then
		echo "$rtx"; return 0
	fi
	if has_word h100 "$gw"; then
		if has_word nvl "$gw"; then echo H100_nvl; else echo H100; fi
		return 0
	fi
	gb=""
	for t in $gw; do
		case "$t" in *gb) n=${t%gb}; case "$n" in ''|*[!0-9]*) ;; *) gb=$n; break ;; esac ;; esac
	done
	if [ -z "$gb" ]; then case "${2:-}" in ''|*[!0-9]*|0) ;; *) gb=$2 ;; esac; fi
	if has_word a100 "$gw"; then
		[ -n "$gb" ] || return 1
		if [ "$gb" -ge 70 ]; then echo A100_80G; return 0; fi
		if [ "$gb" -ge 35 ]; then echo A100; return 0; fi
		return 1
	fi
	if has_word v100 "$gw"; then
		[ -n "$gb" ] || return 1
		if [ "$gb" -ge 28 ]; then echo V100_32G; return 0; fi
		if [ "$gb" -ge 14 ]; then echo V100; return 0; fi
		return 1
	fi
	return 1
}

# gpu_group ADDR: the GPU's IOMMU group, empty when the IOMMU is off.
gpu_group() {
	g=$(readlink "$PCI_DEVICES/$1/iommu_group" 2>/dev/null) || g=""
	printf '%s\n' "${g##*/}"
}

# group_members GROUP: the PCI functions in an IOMMU group (status 1 when it can't be read).
group_members() {
	[ -d "$IOMMU_GROUPS/$1/devices" ] || return 1
	for p in "$IOMMU_GROUPS/$1/devices"/*; do
		if [ -e "$p" ] || [ -L "$p" ]; then printf '%s\n' "${p##*/}"; fi
	done
}

# gpu_companions ADDR: the other functions of the same card in its IOMMU group
# (HDMI audio, USB-C), which go to vfio-pci with it.
gpu_companions() {
	g=$(gpu_group "$1")
	[ -n "$g" ] || return 0
	for m in $(group_members "$g"); do
		if [ "$m" != "$1" ] && [ "${m%.*}" = "${1%.*}" ] && [ "$(cat "$PCI_DEVICES/$m/vendor" 2>/dev/null)" = 0x10de ]; then
			printf '%s\n' "$m"
		fi
	done
}

# gpu_blocker ADDR: why the GPU can't be passed through, empty when it can. The
# agent's rule (internal/hw/pci.go GPUs): besides bridges, its IOMMU group may
# hold only other functions of the same NVIDIA card.
gpu_blocker() {
	if acs_override; then
		echo "the kernel runs with pcie_acs_override, so IOMMU groups don't prove isolation"; return 0
	fi
	g=$(gpu_group "$1")
	if [ -z "$g" ]; then echo "IOMMU is off"; return 0; fi
	members=$(group_members "$g") || { echo "can't read IOMMU group $g"; return 0; }
	for m in $members; do
		[ "$m" != "$1" ] && [ -r "$PCI_DEVICES/$m/vendor" ] || continue
		class=$(cat "$PCI_DEVICES/$m/class" 2>/dev/null)
		case "$class" in 0x0604*) continue ;; esac
		if [ "$(cat "$PCI_DEVICES/$m/vendor")" != 0x10de ] || [ "${m%.*}" != "${1%.*}" ]; then
			echo "IOMMU group $g also holds $m (class $class)"; return 0
		fi
	done
}

# free_gb DIR: free GB on the filesystem DIR is on (or would be created on).
free_gb() {
	d=$1
	while [ ! -d "$d" ]; do d=$(dirname "$d"); done
	df -Pk "$d" 2>/dev/null | awk 'NR == 2 {print int($4 / 1048576)}'
}

# mem_gb: the machine's RAM in GB, rounded.
mem_gb() { awk '/^MemTotal:/ {print int($2 / 1048576 + 0.5)}' "$MEMINFO" 2>/dev/null; }

# https_ok URL: an HTTPS request to URL gets any answer.
https_ok() {
	if command -v curl >/dev/null 2>&1; then
		curl -sS -L -o /dev/null --max-time 10 "$1" 2>/dev/null
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O /dev/null -T 10 "$1" 2>/dev/null
	else
		return 2
	fi
}

check_machine() {
	step "Checking the machine"
	[ "$(uname -m)" = "x86_64" ] || die "only x86_64 hosts are supported for now"
	if os_supported; then
		say "OS: $(os_pretty)"
	elif [ "$FORCE" -ne 1 ]; then
		die "supported: Ubuntu 22.04, Ubuntu 24.04, Debian 12 (found $(os_pretty); --force to try anyway)"
	fi
	vm=$(virt_kind)
	if [ -n "$vm" ] && [ "$FORCE" -ne 1 ]; then
		die "this is a virtual machine ($vm); Runcrate hosts must be bare metal"
	fi
	cpu_virt_on || die "CPU virtualization (VT-x / AMD-V) is off: turn it on in the BIOS"
	gpus=$(nvidia_gpus)
	[ -n "$gpus" ] || die "no NVIDIA GPU found"
	say "GPUs:"
	for a in $gpus; do say "  $a $(pci_ids_name "$a")"; done
}

# iommu_param: the kernel parameters this installer adds to turn IOMMU on.
iommu_param() {
	if grep -q GenuineIntel "$CPUINFO"; then echo "intel_iommu=on iommu=pt"; else echo "amd_iommu=on iommu=pt"; fi
}

# keep_iommu_param: IOMMU is on because the kernel booted with our parameters,
# but their boot loader file is gone (an earlier --uninstall removed it, then
# the machine was installed again without a reboot). Write it back, or the next
# reboot boots without IOMMU and no GPU can be passed through. A machine whose
# own boot loader settings carry the parameters is left alone.
keep_iommu_param() {
	param=$(iommu_param)
	[ -f "$GRUB_CONF" ] && return 0
	grep -q -- "$param" "$KERNEL_CMDLINE" 2>/dev/null || return 0
	# The owner's own settings keep it when every parameter is on a line that is not a comment.
	kept=1
	for p in $param; do
		grep -hsv '^[[:space:]]*#' "$GRUB_DEFAULT_FILE" "$(dirname "$GRUB_CONF")"/*.cfg | grep -q -- "$p" || kept=0
	done
	[ "$kept" -eq 1 ] && return 0
	command -v update-grub >/dev/null 2>&1 || { say "The kernel has '$param' but no boot loader file keeps it: add it to your boot loader, or IOMMU is off after the next reboot."; return 0; }
	mkdir -p "$(dirname "$GRUB_CONF")"
	printf '# Written by the Runcrate host installer. Removed by --uninstall.\nGRUB_CMDLINE_LINUX_DEFAULT="$GRUB_CMDLINE_LINUX_DEFAULT %s"\n' "$param" > "$GRUB_CONF"
	if update-grub >/dev/null; then
		say "Kept '$param' in $GRUB_CONF so IOMMU stays on after a reboot."
	else
		say "update-grub failed (see above): run it yourself, or IOMMU is off after the next reboot."
	fi
}

check_iommu() {
	step "Checking IOMMU"
	if [ -n "$(ls -A "$IOMMU_GROUPS" 2>/dev/null)" ]; then
		say "IOMMU is on."
		keep_iommu_param
		return 0
	fi
	if [ "$FORCE" -eq 1 ]; then
		say "IOMMU is off; continuing anyway (--force). No GPU can be passed through until it is on."
		return 0
	fi
	param=$(iommu_param)
	if [ -f "$GRUB_CONF" ] && grep -q -- "$param" "$KERNEL_CMDLINE"; then
		die "the kernel has '$param' but no IOMMU came up: turn on VT-d (Intel) or AMD-Vi/IOMMU in the BIOS, reboot, and run this again"
	fi
	say "IOMMU is off. GPU passthrough needs it. This adds to the kernel command line:"
	say ""
	say "  GRUB_CMDLINE_LINUX_DEFAULT=\"\$GRUB_CMDLINE_LINUX_DEFAULT $param\""
	say ""
	say "in $GRUB_CONF, runs update-grub and reboots. Run the same install command again after the reboot."
	command -v update-grub >/dev/null 2>&1 || die "update-grub not found: add '$param' to your boot loader's kernel command line, reboot, and run this again"
	if ask "Add it and reboot now?"; then
		mkdir -p /etc/default/grub.d
		printf '# Written by the Runcrate host installer. Removed by --uninstall.\nGRUB_CMDLINE_LINUX_DEFAULT="$GRUB_CMDLINE_LINUX_DEFAULT %s"\n' "$param" > "$GRUB_CONF"
		update-grub
		say "Rebooting in 10 seconds. Run the install command again when the machine is back."
		sleep 10
		reboot
		exit 0
	fi
	die "IOMMU is required. Add the line above yourself, reboot, and run this again."
}

install_packages() {
	step "Installing KVM and QEMU"
	need=""
	for p in qemu-system-x86 qemu-utils ovmf genisoimage nftables pciutils iperf3 curl ca-certificates openssl; do
		dpkg-query -W -f='${Status}' "$p" 2>/dev/null | grep -q "install ok installed" || need="$need $p"
	done
	if [ -d /sys/class/infiniband ] && [ -n "$(ls -A /sys/class/infiniband 2>/dev/null)" ]; then
		dpkg-query -W -f='${Status}' perftest 2>/dev/null | grep -q "install ok installed" || need="$need perftest"
	fi
	if [ -n "$need" ]; then
		echo "iperf3 iperf3/start_daemon boolean false" | debconf-set-selections
		DEBIAN_FRONTEND=noninteractive apt-get update -qq
		# shellcheck disable=SC2086
		DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends $need >/dev/null
		say "Installed:$need"
	else
		say "Already installed."
	fi
	cat > "$MODULES_CONF" <<'EOF'
# Written by the Runcrate host installer.
kvm
vfio
vfio_pci
vfio_iommu_type1
vhost_net
tun
EOF
	for m in kvm vfio vfio_pci vfio_iommu_type1 vhost_net tun; do modprobe "$m" 2>/dev/null || true; done
	[ -e /dev/kvm ] || die "/dev/kvm is missing: turn on VT-x / AMD-V in the BIOS"
	[ -e /dev/vfio/vfio ] || die "vfio did not load"
}

# setup_vm_user: the unprivileged user QEMU runs as, and the device access it
# needs through group kvm (/dev/kvm is root:kvm 0660 on every supported OS;
# /dev/vhost-net is root-only by default).
setup_vm_user() {
	step "Setting up the VM user"
	getent group kvm >/dev/null 2>&1 || groupadd --system kvm
	if ! id "$VM_USER" >/dev/null 2>&1; then
		useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin --groups kvm "$VM_USER"
		say "Created user $VM_USER."
	fi
	id -nG "$VM_USER" | tr ' ' '\n' | grep -qx kvm || usermod -a -G kvm "$VM_USER"
	rule='# Written by the Runcrate host installer. Removed by --uninstall.
# QEMU runs as runcrate-vm (group kvm) and needs vhost-net for VM networking.
KERNEL=="vhost-net", GROUP="kvm", MODE="0660", OPTIONS+="static_node=vhost-net"'
	if [ "$(cat "$UDEV_RULE" 2>/dev/null)" != "$rule" ]; then
		printf '%s\n' "$rule" > "$UDEV_RULE"
		udevadm control --reload-rules >/dev/null 2>&1 || true
	fi
	[ -e /dev/vhost-net ] || die "/dev/vhost-net is missing (modprobe vhost_net failed)"
	chgrp kvm /dev/vhost-net && chmod 0660 /dev/vhost-net
	[ "$(stat -c %G /dev/kvm)" = kvm ] || die "/dev/kvm is not in group kvm; $VM_USER can't use it"
}

# verify_release FILE VERSION: FILE.sig must be Runcrate's ed25519 signature
# of "runcrate-host\nVERSION\nSHA256\n".
verify_release() {
	printf -- '-----BEGIN PUBLIC KEY-----\n%s\n-----END PUBLIC KEY-----\n' "$RELEASE_PUBKEY" > "$tmp/release.pem"
	printf 'runcrate-host\n%s\n%s\n' "$2" "$(sha256sum "$1" | cut -d' ' -f1)" > "$tmp/release.msg"
	base64 -d < "$1.sig" > "$tmp/release.sigbin" 2>/dev/null || return 1
	openssl pkeyutl -verify -pubin -inkey "$tmp/release.pem" -rawin \
		-in "$tmp/release.msg" -sigfile "$tmp/release.sigbin" >/dev/null 2>&1
}

install_agent() {
	step "Installing the agent"
	v=""
	if [ -n "$BINARY" ]; then
		cp "$BINARY" "$tmp/runcrate-host"
		say "Using $BINARY (not signature-checked: --binary is for testing)."
	else
		for f in "$ASSET" "$ASSET.sig" "$ASSET.version"; do
			curl -fsSL --proto '=https' -o "$tmp/$f" "$BASE_URL/$f" || die "could not download $f"
		done
		v=$(head -n1 "$tmp/$ASSET.version" | tr -d '[:space:]')
		case "$v" in ""|*[!A-Za-z0-9._+-]*) die "bad release version in $ASSET.version" ;; esac
		mv "$tmp/$ASSET" "$tmp/runcrate-host"
		mv "$tmp/$ASSET.sig" "$tmp/runcrate-host.sig"
		verify_release "$tmp/runcrate-host" "$v" || die "the downloaded agent is not signed by Runcrate; nothing was installed"
		say "Signature OK (runcrate-host $v)."
	fi
	# Run it from its final directory (/tmp may be noexec), then swap it in.
	install -m 0755 "$tmp/runcrate-host" "$BIN.new"
	if [ -n "${v:-}" ] && [ "$("$BIN.new" version 2>/dev/null)" != "$v" ]; then
		rm -f "$BIN.new"
		die "the downloaded agent does not report version $v; nothing was installed"
	fi
	mv -f "$BIN.new" "$BIN"
	mkdir -p "$CONF_DIR" && chmod 700 "$CONF_DIR"
	say "runcrate-host $("$BIN" version)"
}

take_gpus() {
	step "Taking the GPUs off the host"
	# (Registering reads names, memory and UUIDs from the host driver.)
	busy=$(gpu_processes)
	[ -z "$busy" ] || die "these processes are using the GPUs; stop them and run again:
$busy"
	for u in nvidia-persistenced.service nvidia-fabricmanager.service; do
		if systemctl is-enabled --quiet "$u" 2>/dev/null || systemctl is-active --quiet "$u" 2>/dev/null; then
			systemctl disable --now "$u" >/dev/null 2>&1 || true
			grep -qx "$u" "$DISABLED_UNITS" 2>/dev/null || echo "$u" >> "$DISABLED_UNITS"
			say "Stopped $u (turned back on by --uninstall)."
		fi
	done
	if [ -z "$GPUS" ] && ! grep -q '"gpus"' "$CONF_DIR/config.json" 2>/dev/null; then
		# Every NVIDIA function (not bridges) goes to vfio-pci at boot, before any
		# host driver can take it.
		ids=$(lspci -nd 10de: | awk '$2 !~ /^0604/ {print $3}' | sort -u | paste -sd, -)
		conf=$(printf '# Written by the Runcrate host installer. Removed by --uninstall.\noptions vfio-pci ids=%s\nsoftdep nvidia pre: vfio-pci\nsoftdep nouveau pre: vfio-pci\nsoftdep snd_hda_intel pre: vfio-pci\nsoftdep xhci_pci pre: vfio-pci\n' "$ids")
		if [ "$(cat "$MODPROBE_CONF" 2>/dev/null)" != "$conf" ]; then
			printf '%s\n' "$conf" > "$MODPROBE_CONF"
			command -v update-initramfs >/dev/null 2>&1 && update-initramfs -u >/dev/null 2>&1 || true
		fi
	fi
}

register() {
	step "Registering with Runcrate"
	set -- register
	[ -n "$HOST_NAME" ] && set -- "$@" --name "$HOST_NAME"
	[ -n "$SITE" ] && set -- "$@" --site "$SITE"
	[ -n "$GPUS" ] && set -- "$@" --gpus "$GPUS"
	[ -n "$DATA_DIR" ] && set -- "$@" --data-dir "$DATA_DIR"
	[ -n "$API" ] && set -- "$@" --api "$API"
	# A token file goes to the agent by name: it opens it, then checks the
	# open file's owner and mode, which a swap after our check can't fool.
	if [ -n "$TOKEN_FILE" ]; then
		"$BIN" "$@" --token-file "$TOKEN_FILE"
	else
		RUNCRATE_HOST_TOKEN="$TOKEN" "$BIN" "$@"
	fi
	"$BIN" bind-gpus || die "could not move the GPUs to vfio-pci (see above). Nothing else is changed; fix it and run again."
}

start_service() {
	step "Starting the service"
	cat > "$UNIT_DIR/runcrate-host-gpus.service" <<EOF
[Unit]
Description=Runcrate host: move GPUs to vfio-pci
After=systemd-modules-load.service
Before=runcrate-host.service nvidia-persistenced.service display-manager.service
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=$BIN bind-gpus
[Install]
WantedBy=multi-user.target
EOF
	cat > "$UNIT_DIR/runcrate-host.service" <<EOF
[Unit]
Description=Runcrate host agent
After=network-online.target runcrate-host-gpus.service
Wants=network-online.target runcrate-host-gpus.service
StartLimitIntervalSec=0
[Service]
ExecStart=$BIN run
# Whenever the agent stops (crash, update, revoked credential), the VMs lose
# their network until it is back and enforcing the firewall again.
ExecStopPost=$BIN cut-network
Restart=always
RestartSec=5
# 3 = credential revoked or host delisted: don't loop.
RestartPreventExitStatus=3
KillMode=process
LimitMEMLOCK=infinity
LimitNOFILE=65536
[Install]
WantedBy=multi-user.target
EOF
	systemctl daemon-reload
	systemctl enable runcrate-host-gpus.service runcrate-host.service >/dev/null 2>&1
	systemctl restart runcrate-host.service
	sleep 3
	systemctl is-active --quiet runcrate-host.service || { journalctl -u runcrate-host -n 30 --no-pager; die "the agent did not start"; }
}

# ---- --check: can this machine be a Runcrate host? ----
# One line per item: PASS, WARN, FAIL or SKIP, with a one-line fix under it.
# Reads only: no token, no root (root adds the kernel log), nothing written.
check_fails=""
check_skipped=""
check_reboot=0

# report STATUS TEXT [FIX [REASON]]: one result. A FAIL puts REASON (default
# TEXT) into the NOT READY verdict.
report() {
	printf '%-5s %s\n' "$1" "$2"
	if [ -n "${3:-}" ]; then printf '      fix: %s\n' "$3"; fi
	case "$1" in
		FAIL) check_fails="${check_fails:+$check_fails; }${4:-$2}" ;;
		SKIP) check_skipped="${check_skipped:+$check_skipped; }${4:-$2}" ;;
	esac
}
detail() { printf '      %s\n' "$*"; }

# acpi_iommu_table: the firmware describes an IOMMU (Intel DMAR, AMD IVRS),
# which it does only when VT-d / AMD-Vi is on in the BIOS.
acpi_iommu_table() { [ -e "$ACPI_TABLES/DMAR" ] || [ -e "$ACPI_TABLES/IVRS" ]; }

# count_entries DIR: how many entries DIR holds.
count_entries() {
	c=0
	for p in "$1"/* "$1"/.[!.]* "$1"/..?*; do
		if [ -e "$p" ] || [ -L "$p" ]; then c=$((c + 1)); fi
	done
	echo "$c"
}

# holds_other_files DIR: DIR has entries besides a fresh filesystem's
# lost+found, so the agent would refuse it as its data directory.
holds_other_files() {
	for p in "$1"/* "$1"/.[!.]* "$1"/..?*; do
		if { [ -e "$p" ] || [ -L "$p" ]; } && [ "${p##*/}" != lost+found ]; then return 0; fi
	done
	return 1
}

check_gpus() {
	gpus=$(nvidia_gpus)
	if [ -z "$gpus" ]; then
		report FAIL "NVIDIA GPUs: none found" "Runcrate rents out NVIDIA GPUs; this machine has none on its PCI bus" "no NVIDIA GPU"
		return 0
	fi
	smi=$(smi_gpus)
	usable=""
	unusable=0
	for a in $gpus; do
		name=$(pci_ids_name "$a")
		vram=""
		row=$(printf '%s\n' "$smi" | awk -F', *' -v a="$a" 'length($1) >= 12 && tolower(substr($1, length($1) - 11)) == a {print; exit}')
		if [ -n "$row" ]; then
			name=$(printf '%s\n' "$row" | awk -F', *' '{print $2}')
			vram=$(printf '%s\n' "$row" | awk -F', *' '{print int(($3 + 512) / 1024)}')
		fi
		model=$(gpu_model "$name" "$vram") || model=""
		blocker=$(gpu_blocker "$a")
		driver=$(pci_driver "$a")
		group=$(gpu_group "$a")
		with=$(gpu_companions "$a" | paste -sd, -)
		ok=1
		if [ -n "$model" ]; then
			sold="sold as $model"
		else
			case "$name" in
				"NVIDIA "[0-9a-f][0-9a-f][0-9a-f][0-9a-f])
					if [ -n "$(pci_ids_file)" ]; then
						sold="model not identified: this machine's pci.ids doesn't list 10de:$(pci_device_id "$a")"
						unknown_fix="nothing to do: the benchmark identifies it from inside the VM"
					else
						sold="model not identified: no pci.ids found and no NVIDIA driver"
						unknown_fix="install pciutils (it brings pci.ids) and run the check again; the install does the same before registering"
					fi ;;
				*) sold="not a model Runcrate sells"; ok=0 ;;
			esac
		fi
		case "$blocker" in
			'') groupline="IOMMU group $group: only this card${with:+ (with $with)}" ;;
			"IOMMU is off") groupline="IOMMU group: none yet, the IOMMU is off (see IOMMU)" ;;
			*) groupline="can't be passed through: $blocker"; ok=0 ;;
		esac
		if [ "$ok" -eq 1 ] && [ -n "$model" ]; then
			report PASS "GPU $a: $name, $sold"
		elif [ "$ok" -eq 1 ]; then
			report WARN "GPU $a: $name, $sold" "$unknown_fix"
		elif [ -n "$blocker" ] && [ "$blocker" != "IOMMU is off" ]; then
			case "$blocker" in
				*pcie_acs_override*) fix="remove pcie_acs_override from the kernel command line" ;;
				*) fix="turn on ACS for this slot in the BIOS or move the card to another slot, or leave it out with --gpus" ;;
			esac
			report WARN "GPU $a: $name, $sold" "$fix"
		else
			report WARN "GPU $a: $name, $sold" "leave it out with --gpus (below); it stays with the host"
		fi
		detail "$groupline"
		case "$driver" in
			vfio-pci) detail "driver: vfio-pci, already set aside for passthrough" ;;
			nvidia) detail "driver: nvidia; the install moves it to vfio-pci (stopping nvidia-persistenced and nvidia-fabricmanager if they run)" ;;
			none) detail "driver: none; the install binds it to vfio-pci" ;;
			*) detail "driver: $driver; the install moves it to vfio-pci" ;;
		esac
		if [ "$ok" -eq 1 ]; then usable="${usable:+$usable,}$a"; else unusable=$((unusable + 1)); fi
	done
	if [ -z "$usable" ]; then
		report FAIL "GPUs: none of them can be rented out" "see each GPU above" "no GPU Runcrate can rent out"
	elif [ "$unusable" -gt 0 ]; then
		report WARN "GPUs: $unusable can't be rented out" "install with --gpus $usable so the others stay with the host"
	fi
	busy=$(gpu_processes)
	if [ -n "$busy" ]; then
		report WARN "Processes are using the GPUs: $(printf '%s\n' "$busy" | paste -sd ';' -)" "stop them before installing; the install refuses to start while they run"
	fi
}

run_check() {
	say "Runcrate host check: reads this machine, installs and changes nothing."
	if [ "$(id -u)" -ne 0 ]; then say "Not root: checks that need root are marked SKIP (sudo sh -s -- --check runs them)."; fi
	say ""

	arch=$(uname -m)
	if [ "$arch" = x86_64 ]; then report PASS "CPU architecture: x86_64"
	else report FAIL "CPU architecture: $arch" "Runcrate hosts are x86_64 only for now" "not x86_64"; fi

	if os_supported; then report PASS "OS: $(os_pretty)"
	else report FAIL "OS: $(os_pretty), not supported" "install Ubuntu 22.04, Ubuntu 24.04 or Debian 12" "unsupported OS"; fi

	ctr=$(container_kind)
	if [ -n "$ctr" ]; then
		report WARN "Running in a container ($ctr): these results describe the container, not the machine" "run the check on the machine itself"
	fi
	vm=$(virt_kind)
	if [ -n "$vm" ]; then
		report FAIL "Bare metal: no, this is a virtual machine ($vm)" "run Runcrate on the physical server: its GPUs are passed through to customer VMs" "virtual machine"
	elif [ -n "$ctr" ]; then
		report WARN "Bare metal: can't tell from inside a container" "run the check on the machine itself"
	else
		report PASS "Bare metal: no hypervisor found"
	fi

	mem=$(mem_gb)
	if [ -z "$mem" ]; then report SKIP "RAM: can't read $MEMINFO" "" "RAM"
	elif [ "$mem" -gt "$HOST_RAM_GB" ]; then report PASS "RAM: $mem GB ($HOST_RAM_GB GB stays with the host, the rest is for VMs)"
	else report FAIL "RAM: $mem GB" "the agent keeps $HOST_RAM_GB GB for the host, so VMs need more than that: add RAM" "$HOST_RAM_GB GB RAM or less"; fi

	if ! cpu_virt_on; then
		report FAIL "CPU virtualization (VT-x / AMD-V): off" "turn on VT-x (Intel) or SVM (AMD) in the BIOS" "CPU virtualization off"
	elif [ -e "$KVM_DEV" ]; then
		report PASS "CPU virtualization: on, $KVM_DEV present"
	else
		report WARN "CPU virtualization: on, but $KVM_DEV is missing" "the install loads the kvm module; if $KVM_DEV still doesn't appear, check VT-x / AMD-V in the BIOS"
	fi

	param=$(iommu_param)
	if iommu_on; then
		if acs_override; then
			report FAIL "IOMMU: on, but the kernel runs with pcie_acs_override" "remove pcie_acs_override from the kernel command line: it fakes IOMMU groups" "pcie_acs_override"
		else
			report PASS "IOMMU: on (IOMMU groups: $(count_entries "$IOMMU_GROUPS"))"
		fi
	elif grep -q -- "$param" "$KERNEL_CMDLINE" 2>/dev/null; then
		report FAIL "IOMMU: off, although the kernel has '$param'" "turn on VT-d (Intel) or AMD-Vi / IOMMU (AMD) in the BIOS" "IOMMU off in the BIOS"
	elif [ -d "$ACPI_TABLES" ] && ! acpi_iommu_table; then
		report FAIL "IOMMU: off in the BIOS (the firmware has no DMAR or IVRS table)" "turn on VT-d (Intel) or AMD-Vi / IOMMU (AMD) in the BIOS" "IOMMU off in the BIOS"
	else
		report WARN "IOMMU: off, the kernel command line lacks '$param'" "the install adds '$param' to GRUB and reboots (it asks first); then run the install again"
		check_reboot=1
	fi
	if klog=$(dmesg 2>/dev/null) && [ -n "$klog" ]; then
		kl=$(printf '%s\n' "$klog" | grep -Ei 'DMAR: IOMMU enabled|AMD-Vi: (Found|Interrupt remapping)|iommu: Default domain' | tail -n 1 | sed 's/^\[[^]]*\] *//')
		detail "kernel log: ${kl:-no IOMMU line}"
	else
		report SKIP "IOMMU in the kernel log: dmesg is not readable here (most machines allow it to root only)" "" "kernel log (dmesg)"
	fi

	check_gpus

	dir=${DATA_DIR:-$(data_dir)}
	free=$(free_gb "$dir")
	if [ -z "$free" ]; then
		report SKIP "Disk: can't read the free space at $dir" "" "free disk"
	elif [ "$free" -ge "$MIN_DISK_GB" ]; then
		report PASS "Disk: $free GB free for VM disks at $dir"
	else
		report FAIL "Disk: $free GB free at $dir, $MIN_DISK_GB GB needed" "free space there, or choose a fast local disk with --data-dir" "under $MIN_DISK_GB GB free for VM disks"
	fi
	if [ -d "$dir" ]; then
		if [ ! -r "$dir" ] || [ ! -x "$dir" ]; then
			report SKIP "Data directory $dir: can't list it without root" "" "data directory contents"
		elif [ ! -f "$dir/.runcrate-host-data" ] && holds_other_files "$dir"; then
			report FAIL "Data directory $dir holds other files" "choose a new or empty directory with --data-dir: the agent never shares one with other files" "data directory not empty"
		fi
	fi

	for u in https://get.runcrate.ai/host.version https://www.runcrate.ai/; do
		h=${u#https://}
		h=${h%%/*}
		if https_ok "$u"; then
			report PASS "Outbound HTTPS to $h"
		elif [ $? -eq 2 ]; then
			report SKIP "Outbound HTTPS to $h: neither curl nor wget is installed" "" "HTTPS to $h"
		else
			report FAIL "Outbound HTTPS to $h: no answer within 10 s" "allow outbound HTTPS (TCP 443) to $h; no inbound port is needed" "no HTTPS to $h"
		fi
	done

	say ""
	[ -z "$check_skipped" ] || say "Skipped: $check_skipped."
	if [ -n "$check_fails" ]; then
		say "Verdict: NOT READY ($check_fails)"
		return 1
	fi
	if [ "$check_reboot" -eq 1 ]; then
		say "Verdict: READY AFTER REBOOT (the IOMMU is off: the install turns it on and reboots, then run the install again)"
		return 1
	fi
	say "Verdict: READY. Install: curl -fsSL https://get.runcrate.ai/host | sudo sh"
}

main() {
	parse_args "$@"
	if [ "$CHECK" -eq 1 ]; then
		[ "$UNINSTALL" -eq 0 ] || die "--check and --uninstall don't go together"
		# In an if, a failed probe can't stop the report half way.
		if run_check; then exit 0; fi
		exit 1
	fi
	[ "$(id -u)" -eq 0 ] || die "run as root (sudo sh -s -- ...)"
	trap cleanup EXIT
	trap 'exit 130' INT TERM
	if [ "$UNINSTALL" -eq 1 ]; then
		uninstall
		return 0
	fi
	read_token
	check_machine
	check_iommu
	install_packages
	setup_vm_user
	tmp=$(mktemp -d)
	install_agent
	take_gpus
	register
	start_service
	step "Done"
	"$BIN" status
	say ""
	say "This machine is now a Runcrate host. It shows up in your dashboard, runs a short"
	say "benchmark, and then sells the GPUs at the rates you set. Logs: journalctl -u runcrate-host -f"
	say "To remove it: curl -fsSL https://get.runcrate.ai/host | sudo sh -s -- --uninstall"
}

# hack/test-install.sh and hack/check-models.sh source this file to test its functions; $0 is
# then the test's own path, which no inherited environment variable can fake.
case "$0" in
*/test-install.sh | test-install.sh | */check-models.sh | check-models.sh) ;;
*) main "$@" ;;
esac
