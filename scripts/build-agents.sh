#!/usr/bin/env bash
# scripts/build-agents.sh — cross-compile matrix (= `make agents`).
#
# Builds the desktop agent for linux/amd64, linux/arm64 and windows/amd64 plus
# the server for linux/amd64 into dist/, proves the android/arm64 agent still
# compiles (the !linux&&!windows stubs), type-checks the windows build tag
# with go vet, copies the install scripts (and the signed RELEASE APK when it
# exists) next to the binaries and writes dist/SHA256SUMS. With MSP_AGENT_DIR
# set the finished dist/ is copied there so msp-server can serve it at /dl/.
#
# The published APK is only ever the signed release build
# (android/app/build/outputs/apk/release/app-release.apk from `make android-release`
# with MSP_ANDROID_KEYSTORE set): it refuses cleartext, is not debuggable and
# carries the MSP's own signature. The debug APK (cleartext allowed, debuggable,
# signed with the public Android debug key) is never copied into dist/.
#
# CGO is never needed: every binary is static. VERSION defaults to the VERSION
# file; the Makefile exports its own value.
set -euo pipefail

R=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$R"

VERSION=${VERSION:-$(cat VERSION)}
LDFLAGS="-s -w -X main.version=$VERSION"
export CGO_ENABLED=0

mkdir -p dist

# build GOOS GOARCH OUTPUT PACKAGE
build() {
	echo "build $3 ($1/$2, version $VERSION)"
	GOOS=$1 GOARCH=$2 go build -trimpath -ldflags "$LDFLAGS" -o "dist/$3" "$4"
}

build linux   amd64 msp-agent-linux-amd64       ./cmd/msp-agent
build linux   arm64 msp-agent-linux-arm64       ./cmd/msp-agent
build windows amd64 msp-agent-windows-amd64.exe ./cmd/msp-agent
build linux   amd64 msp-server-linux-amd64      ./cmd/msp-server

echo "compile-only proof: android/arm64 agent"
GOOS=android GOARCH=arm64 go build -o /dev/null ./cmd/msp-agent

echo "vet: windows build tag"
GOOS=windows go vet ./internal/agent/ ./cmd/msp-agent/

cp deploy/install.sh deploy/install.ps1 dist/

# A stale APK from an earlier run must never survive into SHA256SUMS.
rm -f dist/msp-agent.apk

APK=android/app/build/outputs/apk/release/app-release.apk
UNSIGNED=android/app/build/outputs/apk/release/app-release-unsigned.apk
if [ -f "$APK" ]; then
	# Belt and braces: refuse anything debuggable or debug-signed even if a
	# signed file of that name was produced some other way.
	if [ -n "${ANDROID_HOME:-}" ] && [ -d "$ANDROID_HOME/build-tools" ]; then
		BT=$(ls -d "$ANDROID_HOME"/build-tools/* 2>/dev/null | sort -V | tail -1)
		if [ -x "$BT/aapt2" ] && "$BT/aapt2" dump badging "$APK" | grep -q "application-debuggable"; then
			echo "refusing to publish $APK: it is debuggable" >&2
			exit 1
		fi
		if [ -x "$BT/apksigner" ] && "$BT/apksigner" verify --print-certs "$APK" | grep -q "CN=Android Debug"; then
			echo "refusing to publish $APK: it is signed with the public Android debug key" >&2
			exit 1
		fi
	fi
	cp "$APK" dist/msp-agent.apk
	echo "copied $APK -> dist/msp-agent.apk"
elif [ -f "$UNSIGNED" ]; then
	echo "no signed release APK: $UNSIGNED exists but is unsigned (set MSP_ANDROID_KEYSTORE and run make android-release); dist/msp-agent.apk not written"
else
	echo "no release APK at $APK (run make android-release with MSP_ANDROID_KEYSTORE set); dist/msp-agent.apk not written"
fi

# SHA256SUMS must not list a stale copy of itself.
(cd dist && rm -f SHA256SUMS && sha256sum -- * > SHA256SUMS)

if [ -n "${MSP_AGENT_DIR:-}" ]; then
	mkdir -p "$MSP_AGENT_DIR"
	cp -r dist/. "$MSP_AGENT_DIR"/
	echo "copied dist/ -> $MSP_AGENT_DIR"
fi

echo "dist/:"
ls -l dist
