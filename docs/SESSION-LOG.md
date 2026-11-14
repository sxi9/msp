# Session log — how this platform was built

A chronological record of the build, so a future session has the full context. Operational memory
(conventions, what's done, what's next) is in the repo-root `CLAUDE.md`; this file is the narrative.

## Origin

An IT-management company that supported clients on-site/offline wanted to become an MSP and asked for
**one platform** whose agent runs on **Android, Windows and Linux (Ubuntu) "all together"**, reporting
into one central server with a web dashboard. The GitHub repo `sxi9/msp` was **empty** — everything
here was built from scratch during one long session.

## Method

Every feature followed the same loop: write a contract spec in `docs/specs/`, run a multi-agent
`Workflow` (scaffold → implement → integrate → adversarial review → fix → verify), then commit.
All testing is in-container with fakes for anything external (fake apt, fake SMTP, httptest, local
file server) — no real mail, no real package installs, no internet fetches. Long builds were
checkpoint-committed via scheduled wakeups so container restarts couldn't lose work.

## Key decisions (see docs/DECISIONS.md for D1–D5)

- **One repo, one server binary, outbound-only agents.** Commands ride on the sync long-poll; no
  inbound ports at client sites.
- **Remote access in three tiers**: one-shot commands (phase 1), interactive remote shell (phase 2,
  RS1), remote desktop (phase 3-ish, via RustDesk).
- **Remote desktop = our own self-hosted RustDesk**, not a hand-written screen stack (D4). We run the
  relay (hbbs/hbbr) and point a RustDesk client at it; the branded compiled-in build is documented in
  `docs/RUSTDESK-BRANDING.md` as future work.
- **Remote lock only for managed devices** (D5): a built-in command kind, never free text, reachable
  only for devices enrolled under this server, role-gated and audited.
- **Go 1.24, not 1.26**: the newest pinned-dep releases declare `go 1.26`, which would force a
  toolchain switch and break the alpine Docker stage, so deps are pinned to their newest `go 1.24`
  releases. Documented in `go.mod` and the README.

## Build order (each verified before the next)

1. **Design panel** — three independent architecture proposals, judged and synthesized into
   `docs/PLAN.md` + `docs/WORKPACKAGES.md`. Winner: operator-first, one Go server, long-poll sync,
   Preact dashboard, SQLite.
2. **Phase 1 core** — server (multi-tenant orgs, roles, sessions, enrollment tokens, agent API with
   long-poll, command queue, alerts, audit, retention), Linux + Windows desktop agent, Android app,
   dashboard, ops (Docker/compose/Caddy/systemd/Windows service/CI), end-to-end script. Reviewed and
   fixed (7 findings incl. release APK signing, Account password change, phone top-bar overlap,
   boot-partition disk-alert false positives, clean shutdown). 30 labelled screenshots.
3. **Command preset library** — file-based `presets.json` with live reload, served to the dashboard.
4. **Remote desktop phase A (RustDesk)** — relay in compose, relay settings served to agents, RustDesk
   ID in device facts, `msp-agent rustdesk setup`, installer `--with-rustdesk`, Connect button with
   audit. Research pinned RustDesk 1.1.16 and the `rustdesk://connect/<id>` scheme from primary
   sources. Fixed 3 findings (relay starting unconditionally, Windows password-file ACL, SSRF-ish
   relay-arg hardening).
5. **Android installed-apps inventory** — the one phase-1 item that had been missed; added with a
   1000-entry/192 KiB cap and a searchable panel.
6. **Consolidated guide** (`docs/GUIDE.md`) and a published guide page; README screenshots section.

### Phase 2 (in order)

7. **RS1 — Remote shell.** Browser terminal (xterm.js) relayed over a WebSocket (`github.com/coder/
   websocket`) to a real PTY (Linux) / ConPTY (Windows). Single-use 60 s ticket, Origin=Host check,
   20-shell cap, idle/max timeouts, audited + transcribed (1 MiB cap), output never logged. A
   container restart killed the in-flight build; recovered by committing the surviving scaffold and
   resuming the workflow from cache. Verified by `TestRemoteShellEndToEnd`. Later fixed a flaky
   teardown race by flipping the command status **last**, after the session row and audit are written.
8. **RS2 — Per-technician org scoping.** Restrict an operator/viewer to chosen client orgs, enforced
   in SQL with the **404-not-403** rule at every endpoint, before any side effect. Admin editor on the
   Users page. The adversarial isolation audit found no leak; fixed 2 lows (stale comment; atomic
   scoped-user creation). `TestOrgScoping`.
9. **NS1 — Notifications.** Email (SMTP) + Slack/webhook channels, routing rules by org/severity/type,
   maintenance windows that suppress delivery, a retrying dispatcher with an idempotent delivery log,
   Send-test. Org-scoped like RS2; secrets write-only. Review fixed 3 real findings: **email header
   injection** via device hostname, **webhook SSRF** to internal addresses, and a channel-update kind
   bug. SMS and multi-step escalation deferred to NS2 (schema reserves room). `TestNotificationsDelivery`,
   `TestNotificationsOrgScoping`.
10. **PM1 — Patching.** apt (Linux) + Windows Update (Windows) scan + install behind an injectable
    executor; `patch` command kind; device Updates panel + fleet column. Tested with a **fake apt**
    stub on PATH — never touches real packages. Zero findings. `TestPatchEndToEnd`.
11. **SD1 — Software deployment + file transfer.** Server file library (upload, sha256, org-scoped),
    device-token agent download, `deploy` command kind (push+run a file, or install a package).
    sha256-verified before run, traversal-checked paths, argv not shell. Real-download e2e on Linux;
    package mode via the fake apt. Zero findings; 2 lows (delete-ordering, Windows-path doc).
    `TestDeployEndToEnd`.

## State at the time of writing

~44 commits on `main`. ~18k lines of Go (non-test) + ~15.5k test, ~4.8k TypeScript, 252 test
functions, 8 migrations, 7 `internal/e2e` tests all passing. Phase 1 complete; phase 2 items 7–11
done. Everything above is verified on Linux in this container; the Windows agent, Android app, a real
desktop lock, a real RustDesk session and the Docker image build are code-complete and cross-built but
**not yet run on real hardware** (see `docs/STATUS.md` for the manual tests).

## What's next

See `docs/ROADMAP.md`. Phase 2 remainder: desktop software inventory, watchlists + log forwarding,
agent self-update, NS2 (SMS + escalation), script library + automations, reports, two-factor auth,
Postgres, Android device-owner mode, monitoring beyond agents. Highest value before a real pilot:
two-factor auth and a real-hardware verification pass.
