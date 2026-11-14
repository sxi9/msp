# AUT1 — Script library, bulk actions, scheduled and triggered automations (phase 2)

Turn one-off commands into reusable, scalable operations: a saved **script library**, **bulk run** of a
script across many devices as a tracked **job**, **scheduled** automations (cron/interval), and
**triggered** automations (run a script when an alert fires). All reuse the existing command queue,
RS2 org-scoping and audit. Fully testable in-container with the server fake clock and the real Linux
agent; no external anything.

## Model (migration `0014_automations.sql`)

- `scripts(id, org_id INTEGER NULL REFERENCES orgs(id) ON DELETE CASCADE, name, description,
  platform TEXT CHECK(platform IN ('linux','windows','any')), kind TEXT CHECK(kind IN ('exec','script')),
  argv_json TEXT NULL, shell TEXT NULL, body TEXT NULL, timeout_s INTEGER NOT NULL DEFAULT 300,
  created_at, created_by)`. `org_id NULL` = a global script usable for any client (RS2 visibility). This
  is the DB-backed, editable library (distinct from the static `presets.json` console presets).
- `jobs(id, org_id INTEGER NULL, name, origin TEXT CHECK(origin IN ('manual','schedule','trigger')),
  script_id INTEGER NULL REFERENCES scripts(id) ON DELETE SET NULL, created_by INTEGER NULL,
  automation_id INTEGER NULL, created_at, total INTEGER NOT NULL DEFAULT 0)`. A job groups the per-device
  commands created in one fan-out; progress is derived by counting its commands by status.
- `commands` gains `job_id INTEGER NULL REFERENCES jobs(id) ON DELETE SET NULL` (plain `ALTER TABLE ADD
  COLUMN`, no CHECK change). COMMAND JSON gains `job_id` (omitempty).
- `automations(id, org_id INTEGER NULL, name, enabled INTEGER NOT NULL DEFAULT 1,
  trigger TEXT CHECK(trigger IN ('schedule','alert')), script_id INTEGER NOT NULL REFERENCES scripts(id)
  ON DELETE CASCADE, target_json TEXT NOT NULL, schedule_json TEXT NULL, alert_json TEXT NULL,
  last_run_at TEXT NULL, next_run_at TEXT NULL, created_at, created_by)`.
  `target_json` = a device filter `{org_id?, status?, platform?, hostname_q?}` resolved to devices at
  run time. `schedule_json` (trigger=schedule) = `{every_minutes}` or `{cron}` (support a simple
  `every_minutes` first; a cron string is optional). `alert_json` (trigger=alert) =
  `{alert_type?, min_severity?}`; the target is the alerting device.
- Store: `scripts` CRUD; `CreateJob` + `JobProgress` (counts by status); `CreateCommandsForDevices`
  (fan-out a script to a device set under one job, reusing `CreateCommand`); `automations` CRUD;
  `DueAutomations(now)` (schedule) and the alert-trigger lookup; `SetAutomationRan(id, now, next)`.

## Bulk run (manual jobs)

- `POST /api/v1/jobs` (admin|operator, CSRF) `{script_id | inline command, target:{filter} | device_ids}`
  → resolves the caller's **visible** devices matching the target (RS2: only devices in the caller's
  org set; a global script may run across visible orgs), creates a `job` and one command per device
  (skipping android for exec/script with a per-device "unsupported" note, skipping offline devices as
  queued anyway), returns `{job_id, total}`. Audit `job.created` (origin manual, script, count).
- `GET /api/v1/jobs?org_id=` and `GET /api/v1/jobs/{id}` (RS2) → the job plus per-device command status
  (progress: queued/sent/done/failed counts and the device rows). The dashboard polls this for progress.
- A command queued by a job is the normal command the agent already runs; no agent change.

## Scheduled automations

- The server scheduler (a job in the existing jobs loop, fake-clock-injectable, using `now()`) each tick
  finds `DueAutomations(now)` (enabled, trigger=schedule, `next_run_at <= now`), and for each resolves
  the target to visible devices **of the automation's org scope**, creates a job (origin schedule), and
  sets `last_run_at`/`next_run_at` (now + every_minutes, or the next cron time). Missed ticks do not
  stack (compute next from now). Audit `automation.ran`.

## Triggered automations

- On an alert **opening** (the same evaluator hook notifications use: `s.open` where `OpenAlert`
  returned true), find enabled `trigger=alert` automations matching the alert's org, `alert_type` and
  `min_severity`; for each, create a job (origin trigger) with a single command on the **alerting
  device** running the automation's script. Respect: desktop-only command kinds (skip android), not
  revoked, and a short **per-automation-per-device cooldown** (e.g. don't re-run the same automation on
  the same device within N minutes) to avoid a flapping alert hammering a device. Audit
  `automation.triggered`.

## Dashboard

- **Scripts** page (admin|operator): CRUD the library (name, platform, kind exec/script, argv/shell/body,
  timeout, description, client or global). Org-scoped.
- **Run on many**: from the Scripts page or the Fleet, a "Run on devices" action → pick a script + a
  target (org/status/platform filter or checked devices) → creates a job; a job progress view
  (n done / n total, per-device status, links to each device/command).
- **Automations** page (admin|operator): CRUD; choose trigger (schedule: every N minutes; or alert:
  type + min severity), a script, and a target filter; enable/disable; show last run / next run; a link
  to the jobs it produced.
- Pure helpers (target-filter summary, schedule/next-run preview, job progress math, platform/kind
  validation) in `web/src/lib/automations.ts` with a `node --test`. `?mock=1` gets a couple of scripts,
  a finished job with mixed statuses, a scheduled and a triggered automation. Phone width intact.

## Security / scope

Everything is RS2 org-scoped (scripts, jobs, automations, and the devices a job targets). Commands keep
argv-not-shell. A global (org_id NULL) script/automation requires all-orgs to create. Viewer is
read-only on all of it. Bulk and automation runs are audited, and each resulting command is audited as
usual. A target filter can never reach a device outside the caller's / automation's visible org set —
test this.

## Docs

`docs/PROTOCOL.md` (scripts, jobs, automations endpoints + the `job_id` on commands), `docs/GUIDE.md`
(a "Scripts and automations" section: save a script, run on many, schedule it, trigger on an alert),
`docs/OPS.md` (the scheduler tick, cooldowns, that automations reuse the command queue),
`docs/SECURITY.md` (org-scoped targets, argv, cooldown, audit), `docs/ARCHITECTURE.md`,
`docs/FEATURES.md`, `docs/STATUS.md` (AUT1 row WORKING), `docs/ROADMAP.md` (tick script library +
automations + bulk actions), `docs/CHECKLIST.md`.

## Proof (what the build must show here)

The six proof commands pass. A new `internal/e2e` test with a fake clock + the real Linux agent:
- create a library script; bulk-run it on a 2-device target → a job with total 2, the real Linux agent
  runs its command to `done`, job progress reflects it; `job.created` audited;
- a scheduled automation (every N minutes) fires when the clock advances, creating a job for the target;
  missed ticks don't stack; `automation.ran` audited;
- a triggered automation runs its script on a device when a matching alert opens, and the cooldown
  prevents an immediate re-run; `automation.triggered` audited;
- RS2: an operator scoped to org A cannot create a job/automation targeting org B's devices, and sees
  only A's scripts/jobs/automations (no cross-org rows); a bulk target resolves only to visible devices;
- android devices are skipped for exec/script fan-out with a clear per-device note.

Verify stage screenshots the Scripts library, a job progress view, and the Automations page.
