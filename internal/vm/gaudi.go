package vm

import "github.com/runcrateai/runcrate-host/internal/hw"

// What a VM needs when its GPUs are Intel Gaudi, chosen by the vendor the
// registered GPU records carry (hw.GPU.Vendor). Nothing from Intel is bundled
// or downloaded by the host or the image: the Gaudi software stack is
// installed inside the guest by the customer's own launch script.

// GaudiKernelArgs are the guest kernel arguments Intel's virtualization guide
// gives for a Gaudi passthrough VM. The guest boots from its own disk, so they
// reach it as an SMBIOS OEM string the image applies on first boot (see
// QemuSpec.KernelArgs and image/build.sh).
const GaudiKernelArgs = "pci=nocrs,realloc,assign-busses"

// gaudiMMIO64PerDeviceMB is the 64-bit MMIO space set aside per Gaudi
// accelerator: 256 GiB, a generous guess at a Gaudi 3's BARs (it maps 128 GB
// of HBM). VERIFY ON THE BOX: lspci -vv -s <addr> | grep Region (GAUDI-NOTES.md).
const gaudiMMIO64PerDeviceMB = 256 << 10

// gaudiMMIO64MB is the 64-bit MMIO window for n Gaudi accelerators: a power of
// two, at least 32 GiB and gaudiMMIO64PerDeviceMB each.
func gaudiMMIO64MB(n int) int {
	mb := 32 << 10
	for mb < n*gaudiMMIO64PerDeviceMB {
		mb *= 2
	}
	return mb
}

// ForVendor sets what the GPU vendor needs on a spec whose PassThrough is
// already filled. NVIDIA (or no vendor) gets nothing: its arguments stay as
// they were. Gaudi gets the guest kernel arguments and a window big enough
// for its BARs, if MMIO64For (the BARs the host sees) wasn't already bigger.
func (s *QemuSpec) ForVendor(v hw.Vendor) {
	if v != hw.VendorHabana {
		return
	}
	s.KernelArgs = GaudiKernelArgs
	s.MMIO64MB = max(s.MMIO64MB, gaudiMMIO64MB(len(s.PassThrough)))
}

// ScrubScriptFor is the scrub script for a VM's GPU vendor.
func ScrubScriptFor(v hw.Vendor) string {
	if v == hw.VendorHabana {
		return GaudiScrubScript
	}
	return ScrubScript
}

// BenchScriptFor is the benchmark script for a vendor's GPUs. isolation says
// whether this VM runs the isolation checks: when one benchmark boots a VM per
// vendor, only the first does, as the checks describe the VM network, not the
// GPUs. BenchScript is the NVIDIA one with the checks.
func BenchScriptFor(v hw.Vendor, t IsolationTargets, isolation bool) string {
	if v == hw.VendorHabana {
		if isolation {
			return gaudiPrelude + isolationScript(t) + gaudiBenchGPUs
		}
		return gaudiPrelude + gaudiBenchGPUs
	}
	if isolation {
		return BenchScript(t)
	}
	return ephemeralPrelude + benchGPUs
}

// The Gaudi scripts run in the same throwaway VM as the NVIDIA ones and post
// the same JSON, but the image carries no Intel software, so they see the
// accelerators as the guest kernel does: the PCI addresses hl-smi -L lists when
// the guest has hl-smi, else the 1da3 PCI functions that are accelerators.

const gaudiPrelude = `#!/bin/bash
set -u
out=/run/rc-ephemeral
mkdir -p "$out"
post() { curl -fsS --retry 5 --retry-delay 2 --max-time 30 -X POST -H 'Content-Type: application/json' --data-binary @"$1" http://169.254.169.254/runcrate/v1/ephemeral-result; }
# Progress goes to the serial console (runcrate-host console <vm>), so a
# stuck run shows where it stopped.
say() { echo "rc-ephemeral: $*" > /dev/ttyS0 2>/dev/null || true; }
# fail MESSAGE: post {"ok":false,"error":MESSAGE} and power off.
fail() { python3 -c 'import json,sys; print(json.dumps({"ok": False, "error": sys.argv[1][:500], "gpus": []}))' "$1" > "$out/result.json"; say "failed: $1"; post "$out/result.json"; sync; poweroff; exit 0; }
say start
# gaudi_pci: the PCI address of every Gaudi accelerator (vendor 1da3, class 12xx or 03xx), in address order.
gaudi_pci() {
  local d
  for d in /sys/bus/pci/devices/*; do
    [ "$(cat "$d/vendor" 2>/dev/null)" = 0x1da3 ] || continue
    case "$(cat "$d/class" 2>/dev/null)" in 0x12*|0x03*) basename "$d" ;; esac
  done
}
# gaudi_count: how many accelerators the VM sees (the format of hl-smi -L is not
# verified: any PCI address it prints counts once).
gaudi_count() {
  local n=0
  if command -v hl-smi >/dev/null 2>&1; then
    n=$(timeout 60 hl-smi -L 2>/dev/null | grep -o -E '[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]' | sort -u | wc -l)
  fi
  [ "$n" -gt 0 ] || n=$(gaudi_pci | wc -l)
  echo $((n + 0))
}
n=$(gaudi_count)
say "gaudi accelerators: $n"
`

// GaudiScrubScript checks every accelerator reached the VM. It does NOT
// overwrite their memory: that takes Intel's software, which the image doesn't
// carry; "wiped": false says so, and the host (Manager.checkScrub) reads that
// as a failed scrub: the modules stay held back.
const GaudiScrubScript = gaudiPrelude + `
[ "$n" -gt 0 ] || fail "no Gaudi accelerator is visible in the VM"
say scrub
python3 - "$n" > "$out/result.json" <<'PY'
import json, sys
n = int(sys.argv[1])
print(json.dumps({"ok": True, "wiped": False, "gpus": [{"uuid": "", "ok": True} for _ in range(n)]}))
PY
say posting
post "$out/result.json"
sync
poweroff
`

// gaudiBenchGPUs reports what the VM can tell without Intel's software: how
// many accelerators it sees (visible_count), their names from the PCI ids. No
// memory or compute is tested, so mem_ok and matmul_ok are false and
// visibility_only is true; the agent sets those itself for Gaudi too.
const gaudiBenchGPUs = `
say "isolation checks done"
gaudi_pci > "$out/pci.txt"
python3 - "$out" "$n" > "$out/result.json" <<'PY'
import json, os, sys
d, n = sys.argv[1], int(sys.argv[2])
# Mirrors the Gaudi 3 rows of gpuVendors in internal/hw/pci.go.
NAMES = {"0x1060": "Intel Gaudi 3 OAM", "0x1063": "Intel Gaudi 3 PCIe"}
def rd(name, cap=200000):
    try:
        return open(os.path.join(d, name), errors="replace").read()[:cap]
    except OSError:
        return ""
pci = rd("pci.txt").split()
gpus = []
for i in range(n):
    name = "Intel Gaudi"
    if i < len(pci):
        try:
            dev = open("/sys/bus/pci/devices/%s/device" % pci[i]).read().strip().lower()
        except OSError:
            dev = ""
        name = NAMES.get(dev, "Intel Habana " + dev[2:])
    gpus.append({"ordinal": i, "name": name, "uuid": "", "vram_gb": 0, "mem_ok": False, "mem_tested_gb": 0,
                 "matmul_ok": False, "matmul_tflops_fp16": 0, "visibility_only": True, "visible_count": n,
                 "nvidia_smi": "intel gaudi: seen by the VM only (visibility_only). Memory and compute were NOT tested (the image carries no Intel software).",
                 "error": ""})
iso = []
for line in rd("iso.txt", 20000).splitlines():
    f = line.split("\t", 2)
    if len(f) == 3:
        iso.append({"name": f[0][:64], "ok": f[1] == "true", "detail": f[2][:200]})
g = {"ok": n > 0, "error": "" if n > 0 else "no Gaudi accelerator is visible in the VM", "gpus": gpus,
     "topo": "", "nvlink": "", "stderr": "", "isolation": iso[:32]}
print(json.dumps(g))
PY
[ -s "$out/result.json" ] || fail "the benchmark result could not be assembled"
say posting
post "$out/result.json" || say "post failed"
sync
poweroff
`
