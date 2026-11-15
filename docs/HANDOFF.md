# Handoff — 2026-11-15

The autonomous phase-2 build was stopped here at the user's request. This file is the precise
stop-point so anyone (human or agent) can pick up cleanly. Read `CLAUDE.md` first for the frozen
architecture and conventions; `docs/ROADMAP.md` is the live checklist and `docs/STATUS.md` the
per-component verified/not-verified labels.

## Repo state at handoff

- Branch `main`, green and pushed. The six proof commands all pass at HEAD:
  `CGO_ENABLED=0 go build ./...`, `go vet ./...`, `CGO_ENABLED=0 go test ./... -count=1`,
  `GOOS=windows GOARCH=amd64 go build ./...`, `make web`, `scripts/e2e.sh` ("E2E OK").
- Highest migration: `0015_watchlists_logs`. 88 labelled screenshots in `docs/screenshots/`.

## Done, verified in-container on Linux (committed)

Phase 1 (full core), then phase 2: remote shell (RS1), per-technician org scoping (RS2),
notifications email+webhook (NS1), patching (PM1), software deployment + file transfer (SD1),
desktop software inventory (SI1), agent self-update (AU1), SMS + multi-step escalation (NS2),
script library + bulk jobs + scheduled/triggered automations (AUT1), reports + CSV exports (RP1),
two-factor auth (TF1). Each shipped through the spec → workflow (scaffold/implement/integrate/
review/fix/verify) method, with the adversarial review's confirmed findings fixed (e.g. the TOTP
replay guard, the self-update reachability rollback, the escalation-after-ack and SMS SSRF guards,
the sync body-budget guard). See `docs/ROADMAP.md` for the per-feature detail and proofs.

## IN PROGRESS — WL1 watchlists + log forwarding (NOT finished, NOT functional end-to-end)

Spec: `docs/specs/WL1-watchlists-logs.md`. The build was stopped during its implement stage.

**Built and unit-green (committed):** migration `0015_watchlists_logs` (watchlist_items, device
service columns, device_logs, alerts recreated FK-safely to add the `service_down` type);
`internal/store/watchlist.go` (+ tests); protocol `Facts.Services` / `SyncRequest.Logs` /
`SyncResponse.Watch`; the agent collectors (`internal/agent/services*.go`, `logs*.go`); the server
watchlist/logs endpoints and the `service_down` evaluator (`internal/server/dashboard_watchlist.go`,
`watchlist_eval.go`, + tests); the web Scripts/Device/Watchlists UI additions. All packages build and
their unit tests pass.

**NOT done (why WL1 does not work yet):** the **integrate** stage never ran. Specifically still
needed — follow the spec's "Server" and "Proof" sections:
1. Wire the sync/enroll handler to fill `SyncResponse.Watch` (`ComputeWatchForDevice`), call
   `RecordServices` from `facts.services` and `AppendLogs` from `req.Logs`.
2. Add `service_down` to `alertTypes` and run the watchlist evaluator each evaluator tick so it feeds
   notifications + triggered automations.
3. Wire the agent Run loop to report services and forward logs using the `Watch` list, advancing the
   log cursor only after the sync acks.
4. Prune `device_logs` by `MSP_LOG_RETENTION_DAYS` + the per-device cap in the hourly pruner; confirm
   route registration.
5. Write `internal/e2e/watchlist_test.go` (real Linux agent + fake `systemctl`/`journalctl`), run the
   adversarial review (cross-tenant log scoping, log-volume bounds), fix, and capture screenshots
   46/47.

**To resume WL1:** re-run its workflow — `Workflow({scriptPath:
"<session>/workflows/scripts/msp-phase2-watchlists-wf_3663dc8f-21d.js"})`. Its scaffold/implement
agents read the already-present files and complete; integrate/review/verify then run. (A fresh run is
cleaner than resume — the prior run's cache is nearly empty after a container restart.) Or finish the
five integration points above by hand against the spec.

## NOT started

- **MON1 — monitoring beyond agents** (website/TCP checks run as server-side scheduled probes; SNMP +
  network discovery). No spec written yet. Plan: a `monitors` table + results, a scheduler tick like
  AUT1, a `monitor_down` alert feeding notifications/automations, a dashboard Monitors page; probe
  targets tested with httptest/local listeners (never real external hosts). SNMP/discovery likely need
  a dependency or a probe agent — scope to HTTP+TCP first and mark SNMP/discovery deferred.

## Deferred (explicit, with reasons)

- **Postgres option** — a large refactor touching every `internal/store` query; high risk, low
  in-container payoff. Left as a second store implementation behind the existing interface.
- **Android device-owner mode** (wipe, app push, kiosk), QR enrollment, location — needs a real phone
  to verify; not buildable-and-provable in this container.
- **SNMP / network discovery** (part of MON1) — needs a probe agent and/or a new dependency.
- Mandatory-2FA-by-policy; device-token rotation / API keys / webhooks; security-posture reporting
  (AV/EDR, firewall, disk encryption); client self-service portal. All still phase-2 roadmap items.

## Still needs real hardware (unchanged from STATUS.md)

The Windows agent on a PC, the Android app on a phone, a real desktop screen lock, a real RustDesk
session between two machines, and the Docker image build — all code-complete and cross-built, verified
only on Linux here. These are the top items to confirm before a real pilot, along with turning on 2FA.
