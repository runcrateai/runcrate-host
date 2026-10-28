package vm

// The throwaway VM scripts. They run as user-data in our image, post one
// JSON object to the metadata server and power the VM off.

const ephemeralPrelude = `#!/bin/bash
set -u
export CUDA_DEVICE_ORDER=PCI_BUS_ID
out=/run/rc-ephemeral
mkdir -p "$out"
post() { curl -fsS --retry 5 --retry-delay 2 --max-time 30 -X POST -H 'Content-Type: application/json' --data-binary @"$1" http://169.254.169.254/runcrate/v1/ephemeral-result; }
# NVSwitch nodes need Fabric Manager before CUDA can start.
if lspci -nd 10de: 2>/dev/null | grep -q ' 0680:'; then systemctl start nvidia-fabricmanager.service || true; sleep 10; fi
# Progress goes to the serial console (runcrate-host console <vm>), so a
# stuck run shows where it stopped.
say() { echo "rc-ephemeral: $*" > /dev/ttyS0 2>/dev/null || true; }
# fail MESSAGE: post {"ok":false,"error":MESSAGE} and power off.
fail() { python3 -c 'import json,sys; print(json.dumps({"ok": False, "error": sys.argv[1][:500], "gpus": []}))' "$1" > "$out/result.json"; say "failed: $1"; post "$out/result.json"; sync; poweroff; exit 0; }
say start
for i in $(seq 90); do nvidia-smi -L >/dev/null 2>&1 && break; sleep 2; done
# A driver that can't take the card (too old, too new for it) is an image
# problem, not a GPU one: name it, with the kernel's reason.
drv_err=""
if ! timeout 60 nvidia-smi -L >/dev/null 2>&1; then
  why=$(dmesg 2>/dev/null | grep -E 'NVRM: ' | grep -v -E 'NVRM: (Please|in this|specific|README)' | sed 's/^\[[^]]*\] *//' | tr -s ' ' | head -n 4 | tr '\n' ' ')
  drv_err="the NVIDIA driver in the VM did not start: ${why:-nvidia-smi found no GPU}"
fi
say "driver: ${drv_err:-ok}"
`

// ScrubScript puts back what a tenant may have changed and persists on the
// card (MIG mode, ECC off, clock locks), then overwrites every GPU's memory
// with zeros.
const ScrubScript = ephemeralPrelude + `
[ -z "$drv_err" ] || fail "$drv_err"
modes() { nvidia-smi --query-gpu=index,ecc.mode.current,ecc.mode.pending,mig.mode.current,mig.mode.pending --format=csv,noheader 2>/dev/null; }
n=$(nvidia-smi -L 2>/dev/null | wc -l)
for i in $(seq 0 $((n - 1))); do
  nvidia-smi -i "$i" -mig 0 >/dev/null 2>&1 || true
  nvidia-smi -i "$i" -e 1 >/dev/null 2>&1 || true
  nvidia-smi -i "$i" -rgc >/dev/null 2>&1 || true
  nvidia-smi -i "$i" -rmc >/dev/null 2>&1 || true
  nvidia-smi -i "$i" -rac >/dev/null 2>&1 || true
done
# Pending ECC or MIG changes take effect on a GPU reset.
modes | while IFS=', ' read -r i ec ep mc mp; do
  if [ "$ec" != "$ep" ] || [ "$mc" != "$mp" ]; then nvidia-smi -r -i "$i" >/dev/null 2>&1 || true; fi
done
bad=$(modes | awk -F', ' '($2 != "Enabled" && $2 != "[N/A]") || ($4 != "Disabled" && $4 != "[N/A]") {print "gpu " $1 ": ecc " $2 ", mig " $4}')
if [ -n "$bad" ]; then
  printf '{"ok":false,"error":"GPU modes could not be reset: %s"}' "$(echo $bad)" > "$out/result.json"
  post "$out/result.json"; sync; poweroff; exit 0
fi
say scrub
if ! timeout 1200 /usr/local/bin/rc-gpubench --scrub > "$out/result.json" 2> "$out/err.txt"; then
  [ -s "$out/result.json" ] || fail "rc-gpubench --scrub failed or timed out: $(head -c 300 "$out/err.txt")"
fi
say posting
post "$out/result.json"
sync
poweroff
`

// BenchScript runs the isolation checks (see isolationScript), tests every
// GPU and gathers nvidia-smi and the NVLink topology.
func BenchScript(t IsolationTargets) string {
	return ephemeralPrelude + isolationScript(t) + benchGPUs
}

const benchGPUs = `
say "isolation checks done"
if [ -n "$drv_err" ]; then
  python3 -c 'import json,sys; print(json.dumps({"ok": False, "error": sys.argv[1][:500], "gpus": []}))' "$drv_err" > "$out/gpus.json"
else
  say gpubench
  timeout 1200 /usr/local/bin/rc-gpubench > "$out/gpus.json" 2> "$out/err.txt" || true
  timeout 120 nvidia-smi -q > "$out/smi.txt" 2>&1 || true
  timeout 120 nvidia-smi topo -m > "$out/topo.txt" 2>&1 || true
  timeout 120 nvidia-smi nvlink -s > "$out/nvlink.txt" 2>&1 || true
fi
python3 - "$out" > "$out/result.json" <<'PY'
import json, sys, subprocess, os
d = sys.argv[1]
def rd(n, cap=200000):
    try:
        return open(os.path.join(d, n), errors="replace").read()[:cap]
    except OSError:
        return ""
try:
    g = json.loads(rd("gpus.json") or "{}")
except ValueError:
    g = {"ok": False, "error": rd("err.txt", 2000), "gpus": []}
per = []
for gpu in g.get("gpus", []):
    try:
        q = subprocess.run(["nvidia-smi", "-q", "-i", str(gpu.get("ordinal", 0))], capture_output=True, text=True, timeout=60).stdout
    except Exception as e:
        q = str(e)
    gpu["nvidia_smi"] = q[:60000]
    per.append(gpu)
g["gpus"] = per
g["topo"] = rd("topo.txt", 20000)
g["nvlink"] = rd("nvlink.txt", 50000)
g["stderr"] = rd("err.txt", 4000)
iso = []
for line in rd("iso.txt", 20000).splitlines():
    f = line.split("\t", 2)
    if len(f) == 3:
        iso.append({"name": f[0][:64], "ok": f[1] == "true", "detail": f[2][:200]})
g["isolation"] = iso[:32]
print(json.dumps(g))
PY
[ -s "$out/result.json" ] || fail "the benchmark result could not be assembled"
say posting
post "$out/result.json" || say "post failed"
sync
poweroff
`
