#!/bin/sh
# MSP agent installer for Linux (systemd). Served by the server at /dl/install.sh.
#
# Usage (as root):
#   curl -fsSLo /tmp/msp-install.sh https://rmm.example.com/dl/install.sh
#   sudo sh /tmp/msp-install.sh --server https://rmm.example.com --token et_... [--arch amd64|arm64] [--insecure-http] [--with-rustdesk]
#
# It downloads msp-agent-linux-<arch> and SHA256SUMS from <server>/dl/, verifies
# the checksum, installs the binary to /usr/local/bin/msp-agent and runs
# `msp-agent install`, which enrolls the device, writes
# /etc/msp-agent/agent.json and enables the msp-agent systemd unit.
#
# --with-rustdesk additionally downloads the official RustDesk client package
# (rustdesk-<version>-<x86_64|aarch64>.deb, published by the MSP at <server>/dl/
# and listed in SHA256SUMS), installs it and runs `msp-agent rustdesk setup`,
# which points it at the MSP's own relay only and sets a random permanent password
# (printed once, kept in /etc/msp-agent/rustdesk-password, mode 0600; never sent to
# the server).
set -eu

SERVER=""
TOKEN=""
ARCH=""
INSECURE=""
WITH_RUSTDESK=""
BIN=/usr/local/bin/msp-agent
# Official client release the MSP publishes at /dl/ (docs/OPS.md, Remote desktop).
RUSTDESK_VERSION=${RUSTDESK_VERSION:-1.5.3}

usage() {
	cat <<'EOF'
usage: install.sh --server URL --token et_... [--arch amd64|arm64] [--insecure-http] [--with-rustdesk]

  --server URL      base URL of the MSP server (https://rmm.example.com)
  --token et_...    enrollment token from the dashboard (Orgs > New enrollment token)
  --arch A          override the detected architecture (amd64 or arm64)
  --insecure-http   allow a plain http:// server URL that is not loopback (lab use only)
  --with-rustdesk   also install the RustDesk client from <server>/dl/ and point it at
                    the MSP's relay (msp-agent rustdesk setup); needs the MSP to have
                    published rustdesk-<version>-<arch>.deb there
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
	--with-rustdesk)
		WITH_RUSTDESK=1
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

# --- optional: RustDesk client for remote desktop (RD1) -----------------------
if [ -n "$WITH_RUSTDESK" ]; then
	case "$ARCH" in
	amd64) RD_ARCH=x86_64 ;;
	arm64) RD_ARCH=aarch64 ;;
	esac
	RD_FILE="rustdesk-$RUSTDESK_VERSION-$RD_ARCH.deb"
	echo "downloading $SERVER/dl/$RD_FILE"
	if ! curl -fsSL -o "$TMP/$RD_FILE" "$SERVER/dl/$RD_FILE"; then
		cat >&2 <<MSG
install.sh: $RD_FILE is not published at $SERVER/dl/; the agent is installed, RustDesk is not.
  To publish it (on the MSP server, docs/OPS.md "Remote desktop"):
    download $RD_FILE from https://github.com/rustdesk/rustdesk/releases/download/$RUSTDESK_VERSION/$RD_FILE
    copy it into MSP_AGENT_DIR and run: scripts/build-agents.sh --sums-only
  then re-run this installer with --with-rustdesk, or install RustDesk by hand and run: msp-agent rustdesk setup
MSG
		exit 1
	fi
	echo "verifying checksum of $RD_FILE"
	grep " $RD_FILE\$" "$TMP/SHA256SUMS" >"$TMP/expected-rd" ||
		fail "$RD_FILE is not listed in SHA256SUMS; on the server run scripts/build-agents.sh --sums-only after copying it into MSP_AGENT_DIR"
	if ! (cd "$TMP" && sha256sum -c - <"$TMP/expected-rd" >/dev/null); then
		fail "checksum mismatch for $RD_FILE; refusing to install"
	fi
	echo "installing $RD_FILE"
	if command -v apt-get >/dev/null 2>&1; then
		DEBIAN_FRONTEND=noninteractive apt-get install -y "$TMP/$RD_FILE" >/dev/null ||
			fail "apt-get install $RD_FILE failed"
	else
		dpkg -i "$TMP/$RD_FILE" || fail "dpkg -i $RD_FILE failed (missing dependencies?)"
	fi
	echo "configuring RustDesk for the MSP relay"
	"$BIN" rustdesk setup --config /etc/msp-agent/agent.json
	# The agent sent its facts before RustDesk existed; a restart sends the id now.
	systemctl restart msp-agent >/dev/null 2>&1 || true
fi
