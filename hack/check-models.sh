#!/bin/sh
# check-models.sh: install.sh's list of the GPU models Runcrate sells (gpu_model)
# must agree with the one Runcrate decides with at registration (gpuTypeFromName
# in the web app's src/lib/providers/gpu-names.ts, which is not in this repo).
# It runs both on every NVIDIA device name in pci.ids, on nvidia-smi style names
# with their memory, and on a name for every model key the web app lists, and
# fails on any difference. Needs node 22.18 or newer (it runs the .ts as is).
#   sh hack/check-models.sh <path to gpu-names.ts> [pci.ids]
#   make check-models APP=<web app checkout>
set -eu
REPO=$(cd "$(dirname "$0")/.." && pwd)
TS=${1:?usage: sh hack/check-models.sh <path to gpu-names.ts> [pci.ids]}
IDS=${2:-}
[ -r "$TS" ] || { echo "can't read $TS" >&2; exit 2; }
T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT

if [ -z "$IDS" ]; then
	for f in /usr/share/misc/pci.ids /usr/share/hwdata/pci.ids /usr/share/pci.ids; do
		if [ -r "$f" ]; then IDS=$f; break; fi
	done
fi
if [ -z "$IDS" ]; then
	curl -fsSL https://pci-ids.ucw.cz/v2.2/pci.ids -o "$T/pci.ids"
	IDS="$T/pci.ids"
fi

# NAME<tab>MEMORY_GB, one per line. pci.ids names come without memory, as the
# agent reports them when the host has no NVIDIA driver.
awk '/^#/ || /^$/ {next}
	/^[^\t]/ {nv = ($0 ~ /^10de /); next}
	nv && /^\t[^\t]/ {s = substr($0, 7); sub(/^[ \t]+/, "", s); sub(/[ \t]+$/, "", s); print s "\t"}' "$IDS" > "$T/names"
cat >> "$T/names" <<'EOF'
NVIDIA H100 80GB HBM3	80
NVIDIA H100 PCIe	80
NVIDIA H100 NVL	94
NVIDIA H200	141
NVIDIA B200	180
NVIDIA B300 SXM6 AC	288
NVIDIA A100-SXM4-80GB	80
NVIDIA A100-SXM4-40GB	40
NVIDIA A100 80GB PCIe	80
NVIDIA A100-PCIE-40GB	40
NVIDIA A100	80
NVIDIA A100	40
NVIDIA A100	20
NVIDIA A100	
Tesla V100-SXM2-32GB	32
Tesla V100-SXM2-16GB	16
Tesla V100	32
Tesla V100	16
Tesla V100	8
NVIDIA L40S	46
NVIDIA L40	46
NVIDIA L4	23
NVIDIA A40	45
NVIDIA A10	23
NVIDIA RTX A6000	48
NVIDIA RTX A5000	24
NVIDIA RTX A4000	16
NVIDIA RTX 6000 Ada Generation	48
NVIDIA RTX PRO 6000 Blackwell Server Edition	96
NVIDIA RTX PRO 6000 Blackwell Max-Q Workstation Edition	96
NVIDIA GeForce RTX 5090	32
NVIDIA GeForce RTX 5080	16
NVIDIA GeForce RTX 5070 Ti	16
NVIDIA GeForce RTX 4090	24
NVIDIA GeForce RTX 4090 D	24
NVIDIA GeForce RTX 4080 SUPER	16
NVIDIA GeForce RTX 3090	24
NVIDIA GeForce RTX 3090 Ti	24
NVIDIA H100 MIG 1g.10gb	10
EOF
# One name per model key in the web app's lists, so a key added there shows up
# even when no name above carries it.
awk '/^const (MODEL_KEYS|RTX_KEYS)/ {on = 1; sub(/^[^{]*[{]/, "")}
	on {line = $0; end = index(line, "}"); if (end) {line = substr(line, 1, end - 1); on = 0}
		while (match(line, /[a-z0-9]+'"'"'?:/)) {k = substr(line, RSTART, RLENGTH - 1); gsub(/'"'"'/, "", k); print "NVIDIA " toupper(k) "\t"; line = substr(line, RSTART + RLENGTH)}}' "$TS" >> "$T/names"
# Hosts are NVIDIA: AMD models stay out of the comparison.
grep -v '^NVIDIA MI[0-9]' "$T/names" > "$T/names.nv"

# The web app's answer for each name.
# shellcheck disable=SC2016
node --input-type=module -e '
import { readFileSync } from "node:fs";
import { pathToFileURL } from "node:url";
const { gpuTypeFromName } = await import(pathToFileURL(process.argv[1]).href);
const lines = readFileSync(0, "utf8").split("\n").filter(Boolean);
for (const line of lines) {
  const [name, mem = ""] = line.split("\t");
  const code = gpuTypeFromName(name, mem ? Number(mem) : undefined) ?? "-";
  process.stdout.write(`${name}\t${mem}\t${code}\n`);
}' "$TS" < "$T/names.nv" > "$T/web"

# install.sh's answer for each name.
# shellcheck source=install.sh
. "$REPO/install.sh"
while IFS='	' read -r name mem; do
	code=$(gpu_model "$name" "$mem") || code=-
	printf '%s\t%s\t%s\n' "$name" "$mem" "$code"
done < "$T/names.nv" > "$T/installer"

if ! diff "$T/web" "$T/installer" > "$T/diff"; then
	echo "install.sh's gpu_model disagrees with gpuTypeFromName (< web app, > install.sh):" >&2
	cat "$T/diff" >&2
	exit 1
fi
echo "ok: install.sh and the web app agree on $(wc -l < "$T/web" | tr -d ' ') GPU names ($(grep -cv '	-$' "$T/web") sold)"
