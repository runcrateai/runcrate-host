#!/bin/sh
# check-install.sh [BASE] [VERSION]: checks what partners download.
#
# Fetches BASE/host (the installer), host.sig and host.version, and the agent
# triple under BASE/releases/latest/, and verifies both ed25519 signatures with
# openssl against the release key in this checkout's install.sh (the same check
# install.sh's verify_release does). The installer and the agent must carry the
# same version (VERSION too, when given), and /releases/<version>/ must hold the
# same signed agent. BASE defaults to https://get.runcrate.ai. Exits nonzero on
# any failure.
set -eu

BASE="${1:-https://get.runcrate.ai}"
BASE="${BASE%/}"
WANT="${2:-}"
ASSET=runcrate-host-linux-amd64
REPO=$(cd "$(dirname "$0")/.." && pwd)

fail=0
ok() { printf 'ok    %s\n' "$*"; }
bad() { printf 'FAIL  %s\n' "$*"; fail=1; }
die() { printf 'FAIL  %s\n' "$*" >&2; exit 1; }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

sha256() { { sha256sum "$1" 2>/dev/null || shasum -a 256 "$1"; } | cut -d' ' -f1; }

# fetch PATH FILE: GET BASE/PATH into FILE, and print its Content-Type.
fetch() {
	curl -fsSL --proto-redir '=https' --retry 2 -o "$tmp/$2" -w '%{content_type}' "$BASE/$1" ||
		die "could not download $BASE/$1"
}

# version FILE: the first line, checked like install.sh does.
version() {
	v=$(head -n1 "$tmp/$1" | tr -d '[:space:]')
	case "$v" in ""|*[!A-Za-z0-9._+-]*) die "bad version in $1" ;; esac
	printf '%s' "$v"
}

# verify KIND FILE VERSION: FILE.sig is Runcrate's signature of
# "KIND\nVERSION\nSHA256\n".
verify() {
	printf '%s\n%s\n%s\n' "$1" "$3" "$(sha256 "$tmp/$2")" > "$tmp/msg"
	base64 -d < "$tmp/$2.sig" > "$tmp/sigbin" 2>/dev/null || return 1
	openssl pkeyutl -verify -pubin -inkey "$tmp/release.pem" -rawin \
		-in "$tmp/msg" -sigfile "$tmp/sigbin" >/dev/null 2>&1
}

key=$(sed -n 's/^RELEASE_PUBKEY=//p' "$REPO/install.sh")
[ -n "$key" ] || die "no RELEASE_PUBKEY in $REPO/install.sh"
printf -- '-----BEGIN PUBLIC KEY-----\n%s\n-----END PUBLIC KEY-----\n' "$key" > "$tmp/release.pem"
printf 'checking %s\n' "$BASE"

# The installer.
ctype=$(fetch host host)
fetch host.sig host.sig >/dev/null
fetch host.version host.version >/dev/null
iv=$(version host.version)
case "$ctype" in
text/plain*|text/x-shellscript*) ok "/host Content-Type $ctype" ;;
*) bad "/host Content-Type is '$ctype', want text/plain or text/x-shellscript" ;;
esac
if verify runcrate-install host "$iv"; then
	ok "/host is signed: runcrate-install $iv, sha256 $(sha256 "$tmp/host")"
else
	bad "/host.sig is not Runcrate's signature of runcrate-install $iv"
fi
served=$(sed -n 's/^RELEASE_PUBKEY=//p' "$tmp/host")
[ "$served" = "$key" ] && ok "/host carries the same release key" ||
	bad "/host carries release key '$served', this checkout has '$key'"
grep -qF "BASE_URL=\"\${RUNCRATE_HOST_BASE_URL:-$BASE/releases/latest}\"" "$tmp/host" &&
	ok "/host downloads the agent from $BASE/releases/latest" ||
	bad "/host's BASE_URL default is not $BASE/releases/latest"

# The agent.
for f in "$ASSET" "$ASSET.sig" "$ASSET.version"; do
	fetch "releases/latest/$f" "$f" >/dev/null
done
av=$(version "$ASSET.version")
if verify runcrate-host "$ASSET" "$av"; then
	ok "/releases/latest/$ASSET is signed: runcrate-host $av, sha256 $(sha256 "$tmp/$ASSET")"
else
	bad "/releases/latest/$ASSET.sig is not Runcrate's signature of runcrate-host $av"
fi
[ "$(head -c 4 "$tmp/$ASSET" | od -An -tx1 | tr -d ' \n')" = 7f454c46 ] &&
	ok "/releases/latest/$ASSET is an ELF binary" || bad "/releases/latest/$ASSET is not an ELF binary"
[ "$av" = "$iv" ] && ok "installer and agent are both $av" ||
	bad "installer is $iv but the latest agent is $av"
if [ -n "$WANT" ]; then
	[ "$av" = "$WANT" ] && ok "published version is $WANT" || bad "published version is $av, want $WANT"
fi

# The versioned copy. Ed25519 signatures are deterministic, so the same
# signature means the same signed message: same version, same sha256.
fetch "releases/$av/$ASSET.sig" v.sig >/dev/null
fetch "releases/$av/$ASSET.version" v.version >/dev/null
if cmp -s "$tmp/v.sig" "$tmp/$ASSET.sig" && [ "$(version v.version)" = "$av" ]; then
	ok "/releases/$av/ holds the same signed agent"
else
	bad "/releases/$av/ does not match /releases/latest/"
fi

[ "$fail" = 0 ] && printf 'all checks passed\n' || printf 'some checks failed\n'
exit "$fail"
