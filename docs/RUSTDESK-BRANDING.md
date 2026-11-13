# RustDesk phase B: the branded client (not built)

Decision D4 wants a RustDesk client that **only** talks to our relay. Phase A (feature RD1,
built) uses the official client and configures it for our relay with
`msp-agent rustdesk setup`; a user with local admin rights can still open the client's
Settings > Network and point it elsewhere. Phase B removes that by building the client
ourselves with the relay address and public key compiled in. This page records how that is
done, what it needs and what is unverified, so it can be picked up later. Nothing on this page
has been executed in this repository.

Facts below were checked against the RustDesk documentation and source on 2026-11-13
(sources at the end). Items marked PARTIAL could not be read in full through this build
container's proxy and must be re-checked against the named source before building.

## 1. What a branded build is

The client reads two compile-time constants from `libs/hbb_common/src/config.rs`
(`config::RENDEZVOUS_SERVER` and `config::RS_PUB_KEY`, used in `src/common.rs`). When they
are set at build time the client uses that ID/relay server and that key and the user cannot
change them (the official page calls this "Hardcoding custom settings"). There is **no
runtime flag** that permanently hardcodes them: the only way is to build the client.

Build-time inputs:

| variable | value | source |
|---|---|---|
| `RENDEZVOUS_SERVER` | our `MSP_RUSTDESK_HOST` (hbbs host or IP; `host:port` when not 21116) | official docs (PARTIAL: page body not fetchable, name confirmed by docs search and source) |
| `RS_PUB_KEY` | the content of `deploy/rustdesk-data/id_ed25519.pub`, one base64 line | same |
| `API_SERVER` | optional; unused by us (Pro API) | same |

The official page warns that setting **either** `RENDEZVOUS_SERVER` or `RS_PUB_KEY` without
the other produces an executable that does not work: always set both.

## 2. How to build

Two documented ways; both produce the normal RustDesk installers (`.deb`, `.exe`, `.msi`,
`.dmg`, `.apk`, ...) with the constants baked in.

**GitHub Actions (recommended).** Fork `rustdesk/rustdesk`, add the repository secrets
`RENDEZVOUS_SERVER` and `RS_PUB_KEY` (Settings > Secrets and variables > Actions), then run
the build workflow (`.github/workflows` in the fork; the official "Build all" page under
`https://rustdesk.com/docs/en/dev/build/all` describes which workflow and inputs). The
workflow reads the secrets into the build environment and uploads the installers as
artifacts/releases of the fork. Expect a multi-hour, multi-platform job and a fork that must
be kept in sync with upstream for security fixes.

**Local build.** Export the two variables before running the build script:

```sh
export RENDEZVOUS_SERVER=rmm.example.com
export RS_PUB_KEY="$(cat deploy/rustdesk-data/id_ed25519.pub)"
python3 build.py --flutter          # per the official per-platform build pages
```

The toolchain (Rust, Flutter, vcpkg, platform SDKs) is large; follow the official per-OS
build pages (Linux, Windows, macOS, Android) rather than this summary.

## 3. What it needs from us

- A running relay (hbbs/hbbr) with a **stable** public key: rotating the key invalidates
  every branded client in the field (they must be rebuilt and redeployed). Back up
  `deploy/rustdesk-data/id_ed25519` as carefully as the MSP database.
- A stable public hostname for `RENDEZVOUS_SERVER`: an IP works but moving the relay later
  means rebuilding.
- Code signing for the Windows build (SmartScreen) and an Android signing key for the
  `.apk`; macOS needs notarisation. Unsigned branded builds are the main support cost of
  phase B.
- Publishing: the resulting installers replace the official ones at `/dl/`
  (`rustdesk-<version>-x86_64.deb`, `.exe`; `scripts/build-agents.sh --sums-only` to re-sum).
  The agent installers' `--with-rustdesk` and `msp-agent rustdesk setup` keep working: with
  the relay compiled in, the `RustDesk2.toml` keys written by `setup` are simply ignored and
  only `--password` matters.
- A rebuild and redeploy for every RustDesk release we want to follow.

## 4. Android

The branded Android build (`.apk`) uses the same two constants. Screen capture on Android
goes through MediaProjection, which always shows a system consent dialog to the person
holding the phone (no silent capture is possible on a non-rooted phone), and input control
needs the RustDesk accessibility service enabled in the phone's settings. The MSP Android
agent neither installs nor configures the RustDesk app in phase A or B; it is a separate
sideload (`/dl/`), and the phone's RustDesk id would have to be reported by a future agent
version (the `facts.rustdesk_id` field is already in the protocol).

## 5. Why phase A ships first

Phase A gives the same relay, the same audit trail and the same Connect button with zero
build infrastructure. The gap it leaves is "a local admin can re-point the client", which is
acceptable for managed endpoints where the MSP is the admin, and is mitigated by the facts
showing the id (a re-pointed client stops answering on our relay and the technician sees a
failed connection). Phase B closes the gap at the cost of a Rust/Flutter build farm, code
signing and a rebuild per release.

## Sources

- https://rustdesk.com/docs/en/self-host/hardcode-settings (body returned 404 through this
  container's proxy; variable names and the "both required" warning corroborated by the docs
  search result and the source)
- https://rustdesk.com/docs/en/dev/build/all
- https://raw.githubusercontent.com/rustdesk/rustdesk/master/src/common.rs
  (`config::RENDEZVOUS_SERVER`, `config::RS_PUB_KEY`)
- https://rustdesk.com/docs/en/self-host/rustdesk-server-oss/docker/ and
  https://rustdesk.com/docs/en/self-host/rustdesk-server-oss/install/ (server image, ports,
  key file)
- https://rustdesk.com/docs/en/self-host/client-configuration/ and
  https://rustdesk.com/docs/en/self-host/client-deployment/ (client flags used by phase A)
- https://github.com/rustdesk/rustdesk/releases/latest (client 1.5.3 assets)
- https://hub.docker.com/v2/repositories/rustdesk/rustdesk-server/tags (server 1.1.16)
