-- 0007_patch_commands: command kind 'patch' and its fields (PM1).
--
-- Two changes mirror the 0003 commands rebuild, with one addition: a child
-- table (shell_sessions) now references commands, so the rebuild must preserve
-- it.
--
-- (1) commands.kind must accept 'patch', and a patch command carries four extra
--     fields (action, mode, reboot, and the mode-ids package list). SQLite
--     cannot ALTER a CHECK in place, so commands is recreated with the enlarged
--     constraint and the new columns, and the rows, indexes and AUTOINCREMENT
--     sequence are preserved (the standard create-new / copy / drop-old / rename
--     rebuild).
--
-- (2) Unlike 0003, commands now HAS a child: shell_sessions references
--     commands(id) ON DELETE CASCADE. The migration runner wraps each file in
--     one foreign_keys=ON transaction, so a plain DROP TABLE commands would fire
--     that cascade and delete every shell_sessions row. To keep them, the
--     shell_sessions rows are copied into a plain backup table, shell_sessions is
--     dropped first (removing the FK before commands goes), commands is rebuilt,
--     shell_sessions is recreated with its original schema, and the rows are
--     copied back against the preserved command ids.

-- (2a) Back up the shell_sessions rows, then drop the child so dropping commands
--      cannot cascade into it. CREATE TABLE AS copies data only, no constraints.
CREATE TABLE shell_sessions_backup AS SELECT * FROM shell_sessions;
DROP TABLE shell_sessions;

-- (1) Rebuild commands with 'patch' in the kind CHECK and the patch columns.
CREATE TABLE commands_new (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    device_id      INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    org_id         INTEGER NOT NULL REFERENCES orgs(id),
    created_by     INTEGER NOT NULL REFERENCES users(id),
    kind           TEXT NOT NULL CHECK (kind IN ('exec', 'script', 'lock', 'ping', 'message', 'shell', 'patch')),
    argv_json      TEXT NULL,
    shell          TEXT NULL,
    body           TEXT NULL,
    body_sha256    TEXT NULL,
    text           TEXT NULL,
    action         TEXT NULL,
    mode           TEXT NULL,
    reboot         TEXT NULL,
    patch_ids_json TEXT NULL,
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
                          text, timeout_s, status, created_at, sent_at, started_at, finished_at,
                          exit_code, stdout, stderr, truncated, error)
    SELECT id, device_id, org_id, created_by, kind, argv_json, shell, body, body_sha256,
           text, timeout_s, status, created_at, sent_at, started_at, finished_at,
           exit_code, stdout, stderr, truncated, error
      FROM commands;

DROP TABLE commands;
ALTER TABLE commands_new RENAME TO commands;

CREATE INDEX commands_device_status ON commands(device_id, status);
CREATE INDEX commands_status_sent ON commands(status, sent_at);
CREATE INDEX commands_created ON commands(created_at);

-- (2b) Recreate shell_sessions with its original 0003 schema and restore the rows
--      (command ids were preserved by the rebuild above), then drop the backup.
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
