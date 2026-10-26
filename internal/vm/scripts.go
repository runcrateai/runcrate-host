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
for i in $(seq 90); do nvidia-smi -L >/dev/null 2>&1 && break; sleep 2; done
`

// ScrubScript puts back what a tenant may have changed and persists on the
// card (MIG mode, ECC off, clock locks), then overwrites every GPU's memory
// with zeros.
const ScrubScript = ephemeralPrelude + `
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
if ! /usr/local/bin/rc-gpubench --scrub > "$out/result.json" 2> "$out/err.txt"; then
  [ -s "$out/result.json" ] || printf '{"ok":false,"error":"rc-gpubench failed"}' > "$out/result.json"
fi
post "$out/result.json"
sync
poweroff
`

// BenchScript tests every GPU and gathers nvidia-smi and the NVLink topology.
const BenchScript = ephemeralPrelude + `
/usr/local/bin/rc-gpubench > "$out/gpus.json" 2> "$out/err.txt" || true
nvidia-smi -q > "$out/smi.txt" 2>&1 || true
nvidia-smi topo -m > "$out/topo.txt" 2>&1 || true
nvidia-smi nvlink -s > "$out/nvlink.txt" 2>&1 || true
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
print(json.dumps(g))
PY
post "$out/result.json"
sync
poweroff
`
