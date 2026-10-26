#!/bin/sh
# Runcrate host installer.
#
#   curl -fsSL https://get.runcrate.ai/host | sudo sh -s -- --token <registration token>
#   curl -fsSL https://get.runcrate.ai/host | sudo sh -s -- --uninstall
#
# Every step checks before it changes anything, so running it again is safe.
set -eu

BASE_URL="${RUNCRATE_HOST_BASE_URL:-https://github.com/runcrateai/runcrate-host/releases/latest/download}"
BIN=/usr/local/bin/runcrate-host
CONF_DIR=/etc/runcrate-host
UNIT_DIR=/etc/systemd/system
MODPROBE_CONF=/etc/modprobe.d/runcrate-host-vfio.conf
MODULES_CONF=/etc/modules-load.d/runcrate-host.conf
GRUB_CONF=/etc/default/grub.d/90-runcrate-host.cfg
DISABLED_UNITS="$CONF_DIR/disabled-units"

TOKEN="${RUNCRATE_HOST_TOKEN:-}"
NAME=""
SITE=""
GPUS=""
DATA_DIR=""
API=""
BINARY=""
YES=0
UNINSTALL=0
FORCE=0

say() { printf '%s\n' "$*"; }
step() { printf '\n==> %s\n' "$*"; }
die() { printf '\nError: %s\n' "$*" >&2; exit 1; }

usage() {
	cat <<'EOF'
Runcrate host installer

  --token T       registration token from your Runcrate partner dashboard
  --name N        host name shown in your dashboard (default: hostname)
  --site S        site name, to group hosts in one location
  --gpus LIST     only these GPUs, as PCI addresses (0000:41:00.0,0000:42:00.0); default: all NVIDIA GPUs
  --data-dir D    where VM disks live (default /var/lib/runcrate-host); needs fast local disk
  --yes           don't ask: turn on IOMMU and reboot if needed
  --uninstall     stop every VM, wipe their disks, give the GPUs back, remove runcrate-host
  --binary PATH   install this runcrate-host binary instead of downloading it
  --api URL       host API (testing only)
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
		--token) TOKEN="${2:-}"; shift 2 ;;
		--name) NAME="${2:-}"; shift 2 ;;
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

[ "$(id -u)" -eq 0 ] || die "run as root (sudo sh -s -- ...)"

# ask QUESTION: yes with --yes, otherwise read y/N from the terminal.
ask() {
	[ "$YES" -eq 1 ] && return 0
	[ -r /dev/tty ] || return 1
	printf '%s [y/N] ' "$1" > /dev/tty
	read -r reply < /dev/tty || return 1
	case "$reply" in y|Y|yes|YES) return 0 ;; *) return 1 ;; esac
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
	if [ -f "$DISABLED_UNITS" ]; then
		while read -r u; do
			[ -n "$u" ] && systemctl enable --now "$u" >/dev/null 2>&1 || true
		done < "$DISABLED_UNITS"
	fi
	data="/var/lib/runcrate-host"
	if [ -f "$CONF_DIR/config.json" ]; then
		d=$(sed -n 's/.*"data_dir": *"\([^"]*\)".*/\1/p' "$CONF_DIR/config.json" | head -n1)
		[ -n "$d" ] && data="$d"
	fi
	case "$data" in /|/etc|/usr|/var|/home|/root) ;; *) rm -rf "$data" ;; esac
	rm -rf "$CONF_DIR"
	rm -f "$BIN" "$BIN.prev"
	step "Done"
	say "runcrate-host is removed and every GPU is back with the host. The GPUs reach your own"
	say "driver now (or after a reboot if nothing was loaded). Packages it installed (qemu, ovmf,"
	say "nftables, iperf3) are left in place. Delist the host in your Runcrate dashboard too."
	exit 0
}

[ "$UNINSTALL" -eq 1 ] && uninstall

[ -n "$TOKEN" ] || [ -f "$CONF_DIR/credential" ] || { usage; die "--token is required"; }

step "Checking the machine"
[ "$(uname -m)" = "x86_64" ] || die "only x86_64 hosts are supported for now"
. /etc/os-release
case "${ID:-}:${VERSION_ID:-}" in
	ubuntu:22.04|ubuntu:24.04|debian:12) say "OS: $PRETTY_NAME" ;;
	*) [ "$FORCE" -eq 1 ] || die "supported: Ubuntu 22.04, Ubuntu 24.04, Debian 12 (found ${PRETTY_NAME:-unknown}; --force to try anyway)" ;;
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

step "Checking IOMMU"
if [ -n "$(ls -A /sys/kernel/iommu_groups 2>/dev/null)" ]; then
	say "IOMMU is on."
else
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
fi

step "Installing KVM and QEMU"
need=""
for p in qemu-system-x86 qemu-utils ovmf genisoimage nftables pciutils iperf3 curl ca-certificates; do
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

step "Installing the agent"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
if [ -n "$BINARY" ]; then
	cp "$BINARY" "$tmp/runcrate-host"
else
	curl -fsSL --proto '=https' -o "$tmp/runcrate-host" "$BASE_URL/runcrate-host-linux-amd64"
	curl -fsSL --proto '=https' -o "$tmp/runcrate-host.sha256" "$BASE_URL/runcrate-host-linux-amd64.sha256"
	want=$(cut -d' ' -f1 < "$tmp/runcrate-host.sha256")
	got=$(sha256sum "$tmp/runcrate-host" | cut -d' ' -f1)
	[ "$want" = "$got" ] || die "checksum mismatch on the downloaded agent"
fi
install -m 0755 "$tmp/runcrate-host" "$BIN"
mkdir -p "$CONF_DIR" && chmod 700 "$CONF_DIR"
say "runcrate-host $("$BIN" version)"

step "Taking the GPUs off the host"
if command -v nvidia-smi >/dev/null 2>&1 && nvidia-smi -L >/dev/null 2>&1; then
	# Names, memory and UUIDs while the host driver still has the GPUs.
	"$BIN" snapshot-gpus || true
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

step "Registering with Runcrate"
set -- register
[ -n "$NAME" ] && set -- "$@" --name "$NAME"
[ -n "$SITE" ] && set -- "$@" --site "$SITE"
[ -n "$GPUS" ] && set -- "$@" --gpus "$GPUS"
[ -n "$DATA_DIR" ] && set -- "$@" --data-dir "$DATA_DIR"
[ -n "$API" ] && set -- "$@" --api "$API"
RUNCRATE_HOST_TOKEN="$TOKEN" "$BIN" "$@"

"$BIN" bind-gpus || die "could not move the GPUs to vfio-pci (see above). Nothing else is changed; fix it and run again."

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

step "Done"
"$BIN" status
say ""
say "This machine is now a Runcrate host. It shows up in your dashboard, runs a short"
say "benchmark, and then sells the GPUs at the rates you set. Logs: journalctl -u runcrate-host -f"
say "To remove it: curl -fsSL https://get.runcrate.ai/host | sudo sh -s -- --uninstall"
