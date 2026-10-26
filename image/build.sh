#!/bin/sh
# Builds the Runcrate VM base image: Ubuntu 24.04 cloud image + NVIDIA open
# driver + CUDA runtime (cudart, cuBLAS) + Fabric Manager + rc-gpubench.
# Customer machine pieces come from cloud_init at launch, so this stays small.
#
#   sudo image/build.sh [version]         (Ubuntu/Debian x86_64 with KVM; no GPU needed)
#
# Writes out/runcrate-vm-<version>.qcow2 and its .sha256.
#   DRIVER_BRANCH=580   pin the driver branch (default: newest nvidia-open)
#   CUDA_VERSION=12-9   pin CUDA (default: newest)
set -eu

here=$(cd "$(dirname "$0")" && pwd)
VERSION="${1:-$(date -u +%Y%m%d)-1}"
DRIVER_BRANCH="${DRIVER_BRANCH:-}"
CUDA_VERSION="${CUDA_VERSION:-}"
BASE_URL=https://cloud-images.ubuntu.com/releases/noble/release
BASE=ubuntu-24.04-server-cloudimg-amd64.img
OUT="${OUT:-$here/../out}"
SIZE=16G

case "$VERSION" in *[!A-Za-z0-9._-]*|"") echo "bad version: $VERSION" >&2; exit 1 ;; esac
[ -e /dev/kvm ] || { echo "needs KVM (/dev/kvm)" >&2; exit 1; }
for c in qemu-system-x86_64 qemu-img genisoimage curl sha256sum; do
	command -v "$c" >/dev/null || { echo "missing $c (apt install qemu-system-x86 qemu-utils genisoimage curl)" >&2; exit 1; }
done

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$OUT"

echo "==> Ubuntu 24.04 cloud image"
curl -fsSL -o "$work/$BASE" "$BASE_URL/$BASE"
curl -fsSL -o "$work/SHA256SUMS" "$BASE_URL/SHA256SUMS"
(cd "$work" && grep " \*$BASE\$" SHA256SUMS | sha256sum -c -)
cp "$work/$BASE" "$work/build.qcow2"
qemu-img resize -q "$work/build.qcow2" "$SIZE"

echo "==> Provisioning (driver, CUDA, Fabric Manager, rc-gpubench)"
mkdir -p "$work/seed"
cp "$here/gpubench.cu" "$work/seed/gpubench.cu"
b64=$(base64 -w0 "$here/gpubench.cu")
cat > "$work/seed/user-data" <<EOF
#cloud-config
write_files:
  - path: /root/gpubench.cu
    encoding: b64
    content: $b64
  - path: /root/provision.sh
    permissions: '0755'
    content: |
      #!/bin/bash
      set -euxo pipefail
      export DEBIAN_FRONTEND=noninteractive
      apt-get update
      apt-get install -y --no-install-recommends linux-headers-\$(uname -r) pciutils curl ca-certificates python3 build-essential dkms
      curl -fsSL -o /tmp/cuda-keyring.deb https://developer.download.nvidia.com/compute/cuda/repos/ubuntu2404/x86_64/cuda-keyring_1.1-1_all.deb
      dpkg -i /tmp/cuda-keyring.deb
      apt-get update
      if [ -n "$DRIVER_BRANCH" ]; then apt-get install -y nvidia-open-$DRIVER_BRANCH; else apt-get install -y nvidia-open; fi
      branch=\$(dpkg-query -W -f='\${Version}\n' nvidia-kernel-source-open 2>/dev/null | head -n1 | cut -d. -f1 || true)
      [ -n "\$branch" ] || branch=\$(ls /usr/src | sed -n 's/^nvidia-\([0-9]*\)\..*/\1/p' | head -n1)
      apt-get install -y nvidia-fabricmanager-\$branch || apt-get install -y nvidia-fabricmanager || true
      cv="$CUDA_VERSION"
      [ -n "\$cv" ] || cv=\$(apt-cache search --names-only '^cuda-nvcc-[0-9]+-[0-9]+\$' | awk '{print \$1}' | sed 's/cuda-nvcc-//' | sort -V | tail -n1)
      apt-get install -y cuda-cudart-\$cv libcublas-\$cv cuda-nvcc-\$cv cuda-cudart-dev-\$cv libcublas-dev-\$cv
      cudadir=/usr/local/cuda-\$(echo \$cv | tr - .)
      \$cudadir/bin/nvcc -O2 -o /usr/local/bin/rc-gpubench /root/gpubench.cu -lcublas
      apt-get purge -y cuda-nvcc-\$cv cuda-cudart-dev-\$cv libcublas-dev-\$cv
      apt-get autoremove -y
      ldd /usr/local/bin/rc-gpubench
      echo "\$cudadir" > /etc/runcrate-cuda
      printf 'PATH="%s/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"\n' "\$cudadir" > /etc/environment
      # Kernel modules built for every installed kernel.
      dkms status
      ls /lib/modules/*/updates/dkms/nvidia.ko* || ls /lib/modules/*/kernel/drivers/video/nvidia* || true
      systemctl enable nvidia-persistenced || true
      # Fabric Manager only matters on NVSwitch VMs; the benchmark starts it there.
      systemctl disable nvidia-fabricmanager || true
      # First-boot apt runs (the launch script) must not race unattended-upgrades.
      systemctl disable --now unattended-upgrades apt-daily.timer apt-daily-upgrade.timer || true
      apt-get purge -y unattended-upgrades || true
      printf 'datasource_list: [NoCloud, None]\n' > /etc/cloud/cloud.cfg.d/90-runcrate.cfg
      echo "runcrate-vm $VERSION" > /etc/runcrate-image
      apt-get clean
      rm -rf /var/lib/apt/lists/* /root/gpubench.cu /tmp/*
      rm -f /etc/ssh/ssh_host_*
      cloud-init clean --logs --seed --machine-id || { cloud-init clean --logs --seed; truncate -s0 /etc/machine-id; }
      fstrim -av || true
      echo RUNCRATE-IMAGE-BUILD-OK > /dev/ttyS0
runcmd:
  - [bash, -c, "/root/provision.sh > /dev/ttyS0 2>&1 || echo RUNCRATE-IMAGE-BUILD-FAILED > /dev/ttyS0; rm -f /root/provision.sh; sync; poweroff"]
EOF
printf 'instance-id: runcrate-image-build\nlocal-hostname: runcrate-image\n' > "$work/seed/meta-data"
genisoimage -quiet -output "$work/seed.iso" -volid cidata -joliet -rock "$work/seed/user-data" "$work/seed/meta-data"

timeout 90m qemu-system-x86_64 -machine q35,accel=kvm -cpu host -smp 8 -m 8G -nodefaults -display none \
	-drive file="$work/build.qcow2",if=virtio,format=qcow2,discard=unmap \
	-drive file="$work/seed.iso",if=virtio,format=raw,readonly=on \
	-netdev user,id=n0 -device virtio-net-pci,netdev=n0 \
	-serial file:"$work/serial.log" -no-reboot

if ! grep -q RUNCRATE-IMAGE-BUILD-OK "$work/serial.log"; then
	tail -n 80 "$work/serial.log" >&2
	echo "image build failed (serial log above)" >&2
	exit 1
fi

echo "==> Compressing"
img="$OUT/runcrate-vm-$VERSION.qcow2"
qemu-img convert -q -c -O qcow2 "$work/build.qcow2" "$img"
(cd "$OUT" && sha256sum "runcrate-vm-$VERSION.qcow2" > "runcrate-vm-$VERSION.qcow2.sha256")
cp "$work/serial.log" "$OUT/runcrate-vm-$VERSION.build.log"
ls -la "$img"
cat "$img.sha256"
