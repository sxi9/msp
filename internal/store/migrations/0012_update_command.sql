-- 0012_update_command: command kind 'update' and its fields (AU1).
--
-- commands.kind must accept 'update', and an update command carries two extra
-- fields (the target version and the download url); the binary's hash reuses the
-- existing sha256 column (added in 0008 for deploy). SQLite cannot ALTER a CHECK
-- in place, so commands is recreated with the enlarged constraint and the two
-- new columns, preserving the rows, indexes and AUTOINCREMENT sequence.
--
-- As in 0007/0008, shell_sessions references commands(id) ON DELETE CASCADE, and
-- the migration runner wraps each file in one foreign_keys=ON transaction, so a
-- plain DROP TABLE commands would cascade-delete every shell_sessions row. The
-- rows are backed up and the child dropped before commands goes, then both are
-- restored.

-- (a) Back up the shell_sessions rows, then drop the child so dropping commands
--     cannot cascade into it. CREATE TABLE AS copies data only, no constraints.
CREATE TABLE shell_sessions_backup AS SELECT * FROM shell_sessions;
DROP TABLE shell_sessions;

-- (b) Rebuild commands with 'update' in the kind CHECK and the update columns
--     (keeping the 0007 patch columns and the 0008 deploy columns).
CREATE TABLE commands_new (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    device_id      INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    org_id         INTEGER NOT NULL REFERENCES orgs(id),
    created_by     INTEGER NOT NULL REFERENCES users(id),
    kind           TEXT NOT NULL CHECK (kind IN ('exec', 'script', 'lock', 'ping', 'message', 'shell', 'patch', 'deploy', 'update')),
    argv_json      TEXT NULL,
    shell          TEXT NULL,
    body           TEXT NULL,
    body_sha256    TEXT NULL,
    text           TEXT NULL,
    action         TEXT NULL,
    mode           TEXT NULL,
    reboot         TEXT NULL,
    patch_ids_json TEXT NULL,
    source         TEXT NULL,
    file_id        INTEGER NULL,
    sha256         TEXT NULL,
    dest           TEXT NULL,
    run            INTEGER NOT NULL DEFAULT 0,
    run_args_json  TEXT NULL,
    package        TEXT NULL,
    version        TEXT NULL,
    url            TEXT NULL,
    timeout_s      INTEGER NOT NULL,
    status         TEXT NOT NULL CHECK (status IN ('queued', 'sent', 'done', 'failed', 'timeout', 'lost', 'cancelled')),
    created_at     TEXT NOT NULL,
    sent_at        TEXT NULL,
    started_at     TEXT NULL,
    finished_at    TEXT NULL,
    exit_code      INTEGER NULL,
    stdout         TEXT NOT NULL DEFAULT '',
    stderr         TEXT NOT NULL DEFAULT '',
    truncated      INTEGER NOT NULL DEFAULT 0,
    error          TEXT NOT NULL DEFAULT ''
);

INSERT INTO commands_new (id, device_id, org_id, created_by, kind, argv_json, shell, body, body_sha256,
                          text, action, mode, reboot, patch_ids_json,
                          source, file_id, sha256, dest, run, run_args_json, package,
                          timeout_s, status, created_at, sent_at, started_at, finished_at,
                          exit_code, stdout, stderr, truncated, error)
    SELECT id, device_id, org_id, created_by, kind, argv_json, shell, body, body_sha256,
           text, action, mode, reboot, patch_ids_json,
           source, file_id, sha256, dest, run, run_args_json, package,
           timeout_s, status, created_at, sent_at, started_at, finished_at,
           exit_code, stdout, stderr, truncated, error
      FROM commands;

DROP TABLE commands;
ALTER TABLE commands_new RENAME TO commands;

CREATE INDEX commands_device_status ON commands(device_id, status);
CREATE INDEX commands_status_sent ON commands(status, sent_at);
CREATE INDEX commands_created ON commands(created_at);

-- (c) Recreate shell_sessions with its original 0003 schema and restore the rows
--     (command ids were preserved by the rebuild above), then drop the backup.
CREATE TABLE shell_sessions (
    command_id           INTEGER PRIMARY KEY REFERENCES commands(id) ON DELETE CASCADE,
    connected_at         TEXT NULL,
    closed_at            TEXT NULL,
    bytes_in             INTEGER NOT NULL DEFAULT 0,
    bytes_out            INTEGER NOT NULL DEFAULT 0,
    transcript           BLOB NOT NULL DEFAULT '',
    transcript_truncated INTEGER NOT NULL DEFAULT 0,
    close_reason         TEXT NOT NULL DEFAULT ''
);

INSERT INTO shell_sessions (command_id, connected_at, closed_at, bytes_in, bytes_out,
                            transcript, transcript_truncated, close_reason)
    SELECT command_id, connected_at, closed_at, bytes_in, bytes_out,
           transcript, transcript_truncated, close_reason
      FROM shell_sessions_backup;

DROP TABLE shell_sessions_backup;
