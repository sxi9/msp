# Phase 1 checklist

Updated as each item is built and verified in this environment. A ticked item has a passing
test or a screenshot in `docs/screenshots/`. Unticked items are not done.

## Server
- [ ] Builds: `go build ./cmd/server`
- [ ] Unit tests pass: `go test ./...`
- [ ] Starts with embedded SQLite, creates schema on first run
- [ ] Admin user bootstrapped from env on first run
- [ ] Login / logout / session cookie
- [ ] Roles enforced: admin, technician, read-only
- [ ] Create client org
- [ ] Generate enrollment token (expiring, org-scoped)
- [ ] Agent enroll endpoint issues device token
- [ ] Heartbeat stores metrics and marks device online
- [ ] Offline sweeper marks device offline after 3 min
- [ ] Inventory upload stored and shown
- [ ] Command queue: create, deliver on heartbeat, result stored
- [ ] Built-in `lock` command type, allow-listed, technician+ role only
- [ ] Tenant isolation: org A cannot see or command org B devices (test)
- [ ] Alert rules evaluated on heartbeat; alert opens, acks, resolves, auto-resolves
- [ ] Audit log records every user action
- [ ] Metrics retention job
- [ ] docker-compose.yml and TLS reverse proxy example

## Desktop agent (Go)
- [ ] Builds for linux/amd64, linux/arm64, windows/amd64
- [ ] `enroll` subcommand stores config
- [ ] Heartbeat with CPU, memory, disk, uptime, IPs
- [ ] Inventory: OS, version, hardware, installed software
- [ ] Runs commands without shell, captures output, enforces timeout
- [ ] Lock: Windows LockWorkStation, Linux loginctl
- [ ] Reconnect with backoff
- [ ] systemd unit install
- [ ] Windows service install command
- [ ] End-to-end on Linux in this container: enroll → appears online → command runs → output visible

## Android agent (Kotlin)
- [ ] `gradle assembleDebug` produces APK in this container
- [ ] Enrollment screen (paste token + server URL)
- [ ] Foreground service heartbeat: battery, storage, RAM, network type, OS version
- [ ] Installed apps inventory
- [ ] Boot receiver restarts service
- [ ] Safe command set: ping, message, inventory, lock (device admin)
- [ ] Unit tests for protocol client pass on JVM

## Dashboard
- [ ] Login page
- [ ] Clients list and client page with enrollment token button
- [ ] Device list with online/offline, OS, last seen, filters
- [ ] Device page: metrics, inventory, software, command history, Run command, Lock
- [ ] Alerts page with acknowledge / resolve
- [ ] Audit log page
- [ ] Phone width: no horizontal scroll
- [ ] Screenshots of every page in docs/screenshots/

## Docs
- [ ] README: run, enroll, deploy
- [ ] CI workflow: go build/test, gradle assembleDebug
