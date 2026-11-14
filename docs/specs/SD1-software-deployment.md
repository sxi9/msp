# SD1 — Software deployment and file transfer (phase 2)

Push a file to a device and optionally run it, or install a named package from the OS package
manager. A server-side file library holds the artifacts; agents download them over their existing
authenticated channel and verify the hash before use. Fully testable in the container on Linux (a
real agent downloads an uploaded file and runs it; package mode uses the fake apt from PM1). The
Windows **file-deploy** path (drive-letter/UNC dest validation, temp+rename+chmod+optional run) is
written and cross-built (`GOOS=windows go build ./...`), marked NOT VERIFIED until a real Windows
host. The Windows **package-install** path is deferred in this phase: a package deploy on Windows
fails cleanly (NOT VERIFIED stub), matching `docs/PROTOCOL.md`; a real installer (e.g. msiexec behind
the same injectable executor seam, fixture-tested like PM1's `patch_windows.go`) is future work.
**No test fetches anything from the public internet.**

## File library (server)

- Config: `MSP_FILES_DIR` (flag `--files-dir`), default `<dir of MSP_DB>/files`; created on start. Files
  live on disk at `<MSP_FILES_DIR>/<id>`; metadata in a `files` table.
- Migration `000N_files.sql`: `files(id INTEGER PK, org_id INTEGER NULL REFERENCES orgs(id) ON DELETE
  CASCADE, name TEXT NOT NULL, size INTEGER NOT NULL, sha256 TEXT NOT NULL, content_type TEXT NOT NULL
  DEFAULT '', created_at TEXT NOT NULL, created_by INTEGER NULL REFERENCES users(id))`. `org_id NULL` =
  a global artifact usable for any client; non-null = scoped to one client (RS2 visibility applies).
- Size cap: `MSP_MAX_FILE_MB` (default 200), reject larger uploads with 413.
- Endpoints (staff, session auth, CSRF on writes, RS2 org-scoped):
  - `POST /api/v1/files` multipart (`file` part + `name`, optional `org_id`): streams to a temp file,
    computes sha256 and size, moves into place, inserts the row. Admin, or operator for an org they can
    see; a global (`org_id` omitted/null) file requires all-orgs. Audit `file.uploaded` (name, size,
    sha256). `GET /api/v1/files` (list, org-scoped; never streams bytes). `GET /api/v1/files/{id}`
    (metadata). `DELETE /api/v1/files/{id}` (removes the row and the disk file; audit `file.deleted`).
- Agent download: `GET /api/agent/v1/files/{id}` with `Authorization: Bearer dt_` → streams the bytes
  with `Content-Length` and an `X-MSP-SHA256` header. Allowed only when the file's `org_id` is the
  device's org or NULL (global); otherwise `404 not_found`. Device-token auth, never a session.

## Command kind `deploy`

Desktop only (`KindsFor` adds `deploy` for linux/windows; android → 400 unsupported_on_platform;
revoked → unsupported_on_device). Agent-facing JSON:

```
{"id":N,"kind":"deploy","source":"file"|"package",
 "file_id":M,"sha256":"<64 hex>","dest":"/opt/acme/installer.sh","run":true,"run_args":["--silent"],
 "package":"htop","timeout_s":3600}
```

- `source:"file"`: `file_id`, `sha256` (the library file's hash, re-checked by the agent after
  download), `dest` (absolute, cleaned, no `..`, must not traverse a symlink; parent created 0755 if
  missing), `run` (default false), `run_args` (argv, each ≤4096 bytes, only when run). The agent
  downloads `/api/agent/v1/files/{file_id}`, verifies size and sha256 **before** writing, writes to a
  temp file in the dest directory then renames into `dest` (0755 if run else 0644), and if `run`
  executes `dest` with `run_args` via argv (no shell), capturing output. A hash mismatch fails the
  command without writing dest.
- `source:"package"`: `package` (one name, charset `[A-Za-z0-9._+-]`, argv not shell), installed via the
  PM1 injectable executor: `DEBIAN_FRONTEND=noninteractive apt-get -y install <pkg>` (Linux). The
  Windows package-install path is deferred (a NOT VERIFIED clean-fail stub) in this phase; captured
  output in the result.
- `protocol`: add `KindDeploy`, the deploy fields on `Command`, `KindsFor`/`KindSupportedOn`/`ValidKind`
  and `Command.Validate` (source required; file needs file_id+sha256+dest; package needs a valid
  package name; run_args only with run; reject a `dest` with `..` or a non-absolute path). Fixture
  `command_deploy.json`, extend types_test.
- `store.CreateCommand` gains a `KindDeploy` case persisting source/file_id/sha256/dest/run/run_args/
  package (reuse/extend the patch columns pattern; add the columns in the migration). `ClaimQueuedCommands`
  and the command scan carry them. Reuses the command queue, result, history and lost-command sweeper.

Dashboard open: `POST /api/v1/devices/{id}/deploy` (admin|operator, CSRF, RS2 org-visible device,
desktop, not revoked, online else 409 device_offline; a `source:file` deploy requires `file_id` to be a
file visible to the caller for that device's org or global, else 404). Audit `deploy.requested`
(source, file_id/package, dest, run).

## Agent

`internal/agent/deploy.go`: dispatch `KindDeploy` from the runner. File source: a download helper on the
existing `Client` (streaming GET to a temp file with the device token + the same TLS/CA config; verify
size and sha256), safe path handling for `dest` (reject traversal, resolve the parent, temp+rename),
optional argv exec with timeout and output capture. Package source: call the PM1 patch executor's
install primitive (or a shared `installPackage`) — keep one code path for apt. Unit tests: path-safety
(reject `..`, non-absolute, symlinked parent), a hash-mismatch abort, and a run path driven by a fake
file server + a harmless script; never run a real installer.

## Dashboard

- A **Software** page (admin/operator; nav entry): upload a file (name, optional client, the file
  picker), a table of library files (name, size, sha256 short, client or "All clients", uploaded by,
  date) with Delete. Org-scoped. Secrets n/a. Upload shows progress/really just a disabled state.
- Device page **Deploy** action: a dialog — "Deploy a file" (choose from the library filtered to the
  device's org + global, a dest path, a Run toggle + args) or "Install a package" (a package-name
  field). Queues the deploy command; the result (download + install log) shows in the command history.
- Pure helpers (file-size format, package-name validation, dest-path validation preview, library filter
  for a device's org) in `web/src/lib/deploy.ts` with a `node --test`. `?mock=1` gets a couple of
  library files (one global, one client-scoped) and the device Deploy dialog working. Phone width
  intact (tables scroll in their own box; upload control usable at 400 px).

## Docs

`docs/PROTOCOL.md` (files endpoints + agent download + deploy kind), `docs/GUIDE.md` (a "Software
deployment" section: upload a file or installer, deploy and run it or install a package, that it is
hash-verified and audited), `docs/OPS.md` (`MSP_FILES_DIR`, `MSP_MAX_FILE_MB`, backups now include the
files dir, the agent needs outbound to the server only), `docs/SECURITY.md` (hash verification before
run, dest path validation, org-scoped file access by agents, size caps, package names are argv),
`docs/ARCHITECTURE.md`, `docs/FEATURES.md`, `docs/STATUS.md`, `docs/ROADMAP.md` (tick software
deployment + file transfer; Windows installer NOT VERIFIED), `docs/CHECKLIST.md`.

## Proof (what the build must show here)

`CGO_ENABLED=0 go build ./...`, `go vet ./...`, `CGO_ENABLED=0 go test ./... -count=1`,
`GOOS=windows GOARCH=amd64 go build ./...`, `make web`, `scripts/e2e.sh`.

A new `internal/e2e` test:
- upload a small file (a shell script that writes a marker) via `POST /api/v1/files`; it appears in
  `GET /api/v1/files` with the right size and sha256;
- `POST /api/v1/devices/{id}/deploy {source:file, file_id, sha256, dest:<tmp>, run:true, run_args:[...]}`
  queues a command; the **real Linux agent** downloads it over `/api/agent/v1/files/{id}`, verifies the
  hash, writes dest and runs it; the command completes `done` and the marker is in the output / on disk;
  `deploy.requested` is audited;
- a deploy whose `sha256` does not match the stored file fails without writing dest;
- a `dest` containing `..` is rejected (400 at the API or a failed command — pick and test one);
- `source:package` with the fake apt installs and completes done;
- RS2: an operator scoped to another org gets 404 on the deploy endpoint and cannot download a file in
  another org via the agent endpoint (device-token test); android → 400 unsupported_on_platform.

Verify stage screenshots the Software library page and the device Deploy dialog.
