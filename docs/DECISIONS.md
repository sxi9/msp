# Decisions

Recorded as they are made. Newest at the bottom.

## D1. One repository, one server binary, agents for Windows, Linux and Android
The platform is a monorepo. The server is a single Go binary with embedded SQLite and the
web dashboard embedded in it. Desktop agents (Windows, Linux/Ubuntu) are one Go binary
cross-compiled per OS. The Android agent is a Kotlin app with a foreground service that
speaks the same HTTP protocol.

## D2. Agents only connect outbound
Agents call the server over HTTPS. Commands are delivered in the heartbeat response.
No inbound ports at client sites, ever.

## D3. Remote access ships in three tiers
1. Phase 1: one-shot commands and scripts over the heartbeat channel. No shell interpreter;
   argv execution with a timeout and captured output.
2. Phase 2: interactive remote shell in the browser. Agent opens an outbound WebSocket to the
   server, the server relays it to the technician's tab. PTY on Linux, ConPTY + PowerShell on
   Windows. Every session is written to the audit log. SSH is not used as the transport.
3. Phase 3: remote desktop through RustDesk (see D4). We do not write our own screen capture
   and input stack.

## D4. Remote desktop: our own build of RustDesk
- We self-host the RustDesk relay and ID server (hbbs/hbbr) next to the MSP server, in the
  same docker-compose file.
- We build our own branded RustDesk client from the open-source tree with our relay address
  and public key compiled in, so it only ever talks to our relay. Clients cannot be pointed at
  the public RustDesk network.
- The desktop agent installs that client silently. The MSP server records each device's
  RustDesk ID. The dashboard device page shows a Connect button for technicians.
- Android: the same branded client, using MediaProjection for screen share with on-device
  consent, and the accessibility service for control.
- Connecting is logged in the audit log with who, which device, when, and session length.

## D5. Remote lock, only for managed devices
A technician can lock a device from the dashboard. Rules:
- The lock action is only available for devices that are enrolled (hold a valid device token)
  under the technician's own MSP tenant. There is no way to lock anything that is not enrolled.
- Lock is a built-in command type (`lock`), not a free-text script, so it is allow-listed on
  every platform, including Android where free-text commands are not accepted.
- Windows: LockWorkStation. Linux: `loginctl lock-sessions`. Android: DevicePolicyManager
  lockNow via device admin in phase 1, device-owner mode in phase 2 (adds wipe and kiosk).
- Requires the technician or admin role. Read-only users cannot lock. Every lock is audited.
- The agent verifies the command came with its own device token's session and rejects
  anything else, so a command queued for one device can never run on another.
