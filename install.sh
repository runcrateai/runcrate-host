#!/bin/sh
# Runcrate host installer.
#
#   curl -fsSL https://get.runcrate.ai/host | sudo sh                 (asks for the registration token)
#   curl -fsSL https://get.runcrate.ai/host | sudo sh -s -- --token-file /root/runcrate-token
#   curl -fsSL https://get.runcrate.ai/host | sudo sh -s -- --uninstall
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
  --binary PATH   install this runcrate-host binary instead of downloading it (testing only:
                  it is not signature-checked)
  --api URL       host API (testing only)
  --force         go on past the OS, bare-metal and IOMMU checks (testing only)
EOF
}

parse_args() {
	while [ $# -gt 0 ]; do
		case "$1" in
			--token) die "--token is gone: the token would show in ps. Put it in a root-only file and pass --token-file, or leave it out to be asked for it" ;;
			--token-file) TOKEN_FILE="${2:-}"; shift 2 ;;
			--name) HOST_NAME="${2:-}"; shift 2 ;;
			--site) SITE="${2:-}"; shift 2 ;;
			--gpus) GPUS="${2:-}"; shift 2 ;;
			--data-dir) DATA_DIR="${2:-}"; shift 2 ;;
			--api) API="${2:-}"; shift 2 ;;
			--binary) BINARY="${2:-}"; shift 2 ;;
			--yes|-y) YES=1; shift ;;
			--uninstall) UNINSTALL=1; shift ;;
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
			"0 "[0-7]00 | "0 "[0-7]) ;;
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
	say "nftables, iperf3) are left in place. Delist the host in your Runcrate dashboard too."
}

check_machine() {
	step "Checking the machine"
	[ "$(uname -m)" = "x86_64" ] || die "only x86_64 hosts are supported for now"
	# Read in a subshell: os-release sets NAME, ID and others that must not
	# leak into this script.
	os=$(. /etc/os-release && printf '%s:%s' "${ID:-}" "${VERSION_ID:-}")
	pretty=$(. /etc/os-release && printf '%s' "${PRETTY_NAME:-unknown}")
	case "$os" in
		ubuntu:22.04|ubuntu:24.04|debian:12) say "OS: $pretty" ;;
		*) [ "$FORCE" -eq 1 ] || die "supported: Ubuntu 22.04, Ubuntu 24.04, Debian 12 (found $pretty; --force to try anyway)" ;;
	esac
	if command -v systemd-detect-virt >/dev/null 2>&1 && systemd-detect-virt --vm --quiet; then
		[ "$FORCE" -eq 1 ] || die "this is a virtual machine; Runcrate hosts must be bare metal"
	fi
	grep -Eq 'vmx|svm' /proc/cpuinfo || die "CPU virtualization (VT-x / AMD-V) is off: turn it on in the BIOS"
	if ! command -v lspci >/dev/null 2>&1; then
		DEBIAN_FRONTEND=noninteractive apt-get install -y -qq pciutils >/dev/null
	fi
	lspci -nd 10de: | grep -Eq ' 03(00|02):' || die "no NVIDIA GPU found"
	say "GPUs:"
	lspci -d 10de: | grep -E 'VGA|3D' | sed 's/^/  /'
}

check_iommu() {
	step "Checking IOMMU"
	if [ -n "$(ls -A /sys/kernel/iommu_groups 2>/dev/null)" ]; then
		say "IOMMU is on."
		return 0
	fi
	if [ "$FORCE" -eq 1 ]; then
		say "IOMMU is off; continuing anyway (--force). No GPU can be passed through until it is on."
		return 0
	fi
	if grep -q GenuineIntel /proc/cpuinfo; then param="intel_iommu=on iommu=pt"; else param="amd_iommu=on iommu=pt"; fi
	if [ -f "$GRUB_CONF" ] && grep -q "$param" /proc/cmdline; then
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
	if command -v nvidia-smi >/dev/null 2>&1 && nvidia-smi -L >/dev/null 2>&1; then
		# (Registering reads names, memory and UUIDs from the host driver.)
		busy=$(nvidia-smi --query-compute-apps=pid,process_name --format=csv,noheader 2>/dev/null || true)
		[ -z "$busy" ] || die "these processes are using the GPUs; stop them and run again:
$busy"
	fi
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
	RUNCRATE_HOST_TOKEN="$TOKEN" "$BIN" "$@"
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

main() {
	parse_args "$@"
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

main "$@"
