#!/bin/sh
# MSP agent installer for Linux (systemd). Served by the server at /dl/install.sh.
#
# Usage (as root):
#   curl -fsSLo /tmp/msp-install.sh https://rmm.example.com/dl/install.sh
#   sudo sh /tmp/msp-install.sh --server https://rmm.example.com --token et_... [--arch amd64|arm64] [--insecure-http]
#
# It downloads msp-agent-linux-<arch> and SHA256SUMS from <server>/dl/, verifies
# the checksum, installs the binary to /usr/local/bin/msp-agent and runs
# `msp-agent install`, which enrolls the device, writes
# /etc/msp-agent/agent.json and enables the msp-agent systemd unit.
set -eu

SERVER=""
TOKEN=""
ARCH=""
INSECURE=""
BIN=/usr/local/bin/msp-agent

usage() {
	cat <<'EOF'
usage: install.sh --server URL --token et_... [--arch amd64|arm64] [--insecure-http]

  --server URL      base URL of the MSP server (https://rmm.example.com)
  --token et_...    enrollment token from the dashboard (Orgs > New enrollment token)
  --arch A          override the detected architecture (amd64 or arm64)
  --insecure-http   allow a plain http:// server URL that is not loopback (lab use only)
EOF
}

fail() {
	echo "install.sh: $*" >&2
	exit 1
}

while [ $# -gt 0 ]; do
	case "$1" in
	--server)
		[ $# -ge 2 ] || fail "--server needs a value"
		SERVER="$2"
		shift 2
		;;
	--server=*)
		SERVER="${1#--server=}"
		shift
		;;
	--token)
		[ $# -ge 2 ] || fail "--token needs a value"
		TOKEN="$2"
		shift 2
		;;
	--token=*)
		TOKEN="${1#--token=}"
		shift
		;;
	--arch)
		[ $# -ge 2 ] || fail "--arch needs a value"
		ARCH="$2"
		shift 2
		;;
	--arch=*)
		ARCH="${1#--arch=}"
		shift
		;;
	--insecure-http)
		INSECURE=1
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		usage >&2
		fail "unknown argument: $1"
		;;
	esac
done

[ -n "$SERVER" ] || { usage >&2; fail "--server is required"; }
[ -n "$TOKEN" ] || { usage >&2; fail "--token is required"; }

case "$SERVER" in
https://* | http://*) ;;
*) fail "--server must start with https:// (or http:// for loopback / --insecure-http)" ;;
esac
# Strip a trailing slash so the /dl/ paths are built cleanly.
SERVER="${SERVER%/}"

[ "$(id -u)" -eq 0 ] || fail "run as root (sudo): the agent installs a systemd unit and writes /etc/msp-agent"

if [ -z "$ARCH" ]; then
	case "$(uname -m)" in
	x86_64 | amd64) ARCH=amd64 ;;
	aarch64 | arm64) ARCH=arm64 ;;
	*) fail "unsupported architecture $(uname -m); pass --arch amd64|arm64 if you know better" ;;
	esac
fi
case "$ARCH" in
amd64 | arm64) ;;
*) fail "--arch must be amd64 or arm64" ;;
esac

command -v curl >/dev/null 2>&1 || fail "curl is required"
command -v sha256sum >/dev/null 2>&1 || fail "sha256sum is required (coreutils)"
command -v systemctl >/dev/null 2>&1 || fail "systemctl not found: this installer supports systemd hosts only"

FILE="msp-agent-linux-$ARCH"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "downloading $SERVER/dl/$FILE"
if ! curl -fsSL -o "$TMP/$FILE" "$SERVER/dl/$FILE" || ! curl -fsSL -o "$TMP/SHA256SUMS" "$SERVER/dl/SHA256SUMS"; then
	fail "download failed: agent binaries are not published: run scripts/build-agents.sh with MSP_AGENT_DIR set (or check the server URL)"
fi

echo "verifying checksum"
grep " $FILE\$" "$TMP/SHA256SUMS" >"$TMP/expected" || fail "$FILE is not listed in SHA256SUMS"
if ! (cd "$TMP" && sha256sum -c - <"$TMP/expected" >/dev/null); then
	fail "checksum mismatch for $FILE; refusing to install"
fi

echo "installing $BIN"
# Stop a running service first so the binary can be replaced on upgrades;
# `msp-agent install` restarts it afterwards.
systemctl stop msp-agent >/dev/null 2>&1 || true
install -m 0755 "$TMP/$FILE" "$BIN"

set -- install --server "$SERVER" --token "$TOKEN"
if [ -n "$INSECURE" ]; then
	set -- "$@" --insecure-http
fi
echo "enrolling with $SERVER"
"$BIN" "$@"

echo "enrolled; device appears on the Fleet page within 30 s"
echo "logs: journalctl -u msp-agent -f    status: $BIN status"
