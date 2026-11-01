#!/bin/sh
# publish.sh VERSION: builds, signs and uploads a release, then checks it.
#
# Run it from a clean checkout of a commit that is on origin/main. The release
# key and the bucket's credentials come from the environment, filled from your
# secret store so they are never typed on a command line, e.g.
#
#   export RUNCRATE_HOST_SIGNING_KEY="$(<print the base64 ed25519 seed from your secret store>)"
#   export AWS_ACCESS_KEY_ID="$(...)" AWS_SECRET_ACCESS_KEY="$(...)"
#   export R2_ENDPOINT=https://<account>.r2.cloudflarestorage.com
#   hack/publish.sh 0.1.1
#
# Any secret left unset is asked for on the terminal, with echo off.
#
# It builds the agent with `make release` (CGO_ENABLED=0 linux/amd64,
# -X main.version), signs it as runcrate-host and install.sh as
# runcrate-install with the release key, uploads to the R2 bucket
# runcrate-host-images,
# which get.runcrate.ai serves through the Worker in hack/get-worker:
#
#   /host, /host.sig, /host.version                   install.sh, short cache
#   /releases/latest/runcrate-host-linux-amd64{,.sig,.version}   short cache
#   /releases/VERSION/runcrate-host-linux-amd64{,.sig,.version}  kept for good
#
# and then runs hack/check-install.sh against https://get.runcrate.ai.
# A published version is never replaced by a different build: re-running the
# same version from the same commit (the build is reproducible) only finishes
# an interrupted upload.
set -eu

PUBLIC=https://get.runcrate.ai
BUCKET=runcrate-host-images
R2=${R2_ENDPOINT:-}
ASSET=runcrate-host-linux-amd64

die() { printf 'Error: %s\n' "$*" >&2; exit 1; }

# ask_secret PROMPT: one line from the terminal, not echoed.
ask_secret() {
	(: < /dev/tty) 2>/dev/null || return 0
	printf '%s: ' "$1" > /dev/tty
	stty -echo < /dev/tty
	trap 'stty echo < /dev/tty' EXIT INT TERM
	IFS= read -r s < /dev/tty || s=""
	stty echo < /dev/tty
	trap - EXIT INT TERM
	printf '\n' > /dev/tty
	printf '%s' "$s"
}

[ $# = 1 ] || die "usage: hack/publish.sh VERSION (e.g. 0.1.1)"
# The secrets leave the environment here; each goes only to the one command that needs it.
key=${RUNCRATE_HOST_SIGNING_KEY:-}
access=${AWS_ACCESS_KEY_ID:-}
secret=${AWS_SECRET_ACCESS_KEY:-}
unset RUNCRATE_HOST_SIGNING_KEY AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY
[ -n "$key" ] || key=$(ask_secret "Release signing key (base64 ed25519 seed)")
[ -n "$key" ] || die "no release key: export RUNCRATE_HOST_SIGNING_KEY from your secret store"
case "$R2" in https://*) ;; *) die "set R2_ENDPOINT to the bucket's S3 endpoint (https://...)" ;; esac
[ -n "$access" ] || access=$(ask_secret "Bucket access key id")
[ -n "$secret" ] || secret=$(ask_secret "Bucket secret access key")
[ -n "$access" ] && [ -n "$secret" ] || die "no bucket credentials: export AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY from your secret store"
V=$1
printf '%s' "$V" | grep -Eq '^v?[0-9]+\.[0-9]+\.[0-9]+([-+][A-Za-z0-9.]+)?$' ||
	die "VERSION must look like 1.2.3 (optionally -suffix)"

cd "$(dirname "$0")/.."
[ -z "$(git status --porcelain)" ] || die "the tree is dirty; publish only a committed, clean checkout"
git fetch -q origin main
git merge-base --is-ancestor HEAD origin/main ||
	die "HEAD $(git rev-parse --short HEAD) is not on origin/main; push the reviewed commit first"
commit=$(git rev-parse HEAD)

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

# Never move latest backwards: V must be the published latest or newer.
code=$(curl -sS -o "$tmp/latest.version" -w '%{http_code}' "$PUBLIC/releases/latest/$ASSET.version") || code=000
case "$code" in
404) ;;
200)
	latest=$(head -n1 "$tmp/latest.version" | tr -d '[:space:]')
	[ "$latest" = "$V" ] || [ "$(printf '%s\n%s\n' "$latest" "$V" | sort -V | tail -n1)" = "$V" ] ||
		die "$V is older than the published $latest; publish a newer version" ;;
*) die "could not check $PUBLIC/releases/latest/ (HTTP $code)" ;;
esac

echo "==> Building runcrate-host $V from $commit"
make -s clean release VERSION="$V"
# -trimpath leaves -ldflags out of the build info, so the version itself is
# checked by install.sh (`runcrate-host version` must print it); here, check
# the binary was built from this clean commit.
go version -m "out/$ASSET" | grep -q "vcs.revision=$commit" &&
	go version -m "out/$ASSET" | grep -q 'vcs.modified=false' ||
	die "out/$ASSET was not built from a clean $commit"
cp install.sh out/host
printf '%s\n' "$V" > out/host.version

echo "==> Signing"
go build -o "$tmp/sign" ./hack/sign
RUNCRATE_HOST_SIGNING_KEY=$key "$tmp/sign" -kind runcrate-host -version "$V" "out/$ASSET"
RUNCRATE_HOST_SIGNING_KEY=$key "$tmp/sign" -kind runcrate-install -version "$V" out/host
key=""

# Versions are immutable: the same version may only carry the same signature.
code=$(curl -sS -o "$tmp/published.sig" -w '%{http_code}' "$PUBLIC/releases/$V/$ASSET.sig") || code=000
case "$code" in
404) ;;
200) cmp -s "$tmp/published.sig" "out/$ASSET.sig" ||
	die "$V is already published with a different build; publish a new version" ;;
*) die "could not check $PUBLIC/releases/$V/ (HTTP $code)" ;;
esac

echo "==> Uploading to R2 ($BUCKET)"
AWS_ACCESS_KEY_ID=$access
AWS_SECRET_ACCESS_KEY=$secret
access=""
secret=""
export AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_DEFAULT_REGION=auto AWS_EC2_METADATA_DISABLED=true \
	AWS_REQUEST_CHECKSUM_CALCULATION=WHEN_REQUIRED AWS_RESPONSE_CHECKSUM_VALIDATION=WHEN_REQUIRED

text='text/plain; charset=utf-8'
short='public, max-age=60'
long='public, max-age=31536000, immutable'
# put FILE KEY CONTENT-TYPE CACHE-CONTROL
put() {
	aws --endpoint-url "$R2" s3 cp --only-show-errors --content-type "$3" --cache-control "$4" \
		"$1" "s3://$BUCKET/$2"
	printf '    %s/%s\n' "$PUBLIC" "$2"
}
# The versioned copy first, then latest, then the installer that points at it.
# Each signature goes up after the file it signs.
for dir in "releases/$V" releases/latest; do
	cache=$long
	[ "$dir" = releases/latest ] && cache=$short
	put "out/$ASSET" "$dir/$ASSET" application/octet-stream "$cache"
	put "out/$ASSET.version" "$dir/$ASSET.version" "$text" "$cache"
	put "out/$ASSET.sig" "$dir/$ASSET.sig" "$text" "$cache"
done
put out/host.version host.version "$text" "$short"
put out/host host "$text" "$short"
put out/host.sig host.sig "$text" "$short"

echo "==> Checking $PUBLIC"
hack/check-install.sh "$PUBLIC" "$V"
echo "Published runcrate-host $V from $commit."
