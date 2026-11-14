-- 0008_files_and_deploy: the server file library and command kind 'deploy' (SD1).
--
-- Two changes, mirroring the 0007 commands rebuild:
--
-- (1) A `files` table holds the metadata for the server-side artifact library
--     (the bytes live on disk under MSP_FILES_DIR/<id>). org_id NULL is a global
--     artifact usable for any client; a non-null org_id scopes it to one client
--     (RS2 visibility applies). It cascades when its org is deleted.
--
-- (2) commands.kind must accept 'deploy', and a deploy command carries seven
--     extra fields (source, file_id, sha256, dest, run, the run_args argv and the
--     package name). SQLite cannot ALTER a CHECK in place, so commands is
--     recreated with the enlarged constraint and the new columns, preserving the
--     rows, indexes and AUTOINCREMENT sequence. As in 0007, shell_sessions
--     references commands(id) ON DELETE CASCADE, so its rows are backed up and the
--     child dropped before commands goes (the migration runner wraps each file in
--     one foreign_keys=ON transaction, so a plain DROP TABLE commands would
--     otherwise cascade-delete every shell_sessions row), then both are restored.

-- (1) The file library metadata table.
CREATE TABLE files (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    org_id       INTEGER NULL REFERENCES orgs(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    size         INTEGER NOT NULL,
    sha256       TEXT NOT NULL,
    content_type TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    created_by   INTEGER NULL REFERENCES users(id)
);
CREATE INDEX files_org ON files(org_id);

-- (2a) Back up the shell_sessions rows, then drop the child so dropping commands
--      cannot cascade into it. CREATE TABLE AS copies data only, no constraints.
CREATE TABLE shell_sessions_backup AS SELECT * FROM shell_sessions;
DROP TABLE shell_sessions;

-- (2b) Rebuild commands with 'deploy' in the kind CHECK and the deploy columns
--      (keeping the 0007 patch columns).
CREATE TABLE commands_new (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    device_id      INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    org_id         INTEGER NOT NULL REFERENCES orgs(id),
    created_by     INTEGER NOT NULL REFERENCES users(id),
    kind           TEXT NOT NULL CHECK (kind IN ('exec', 'script', 'lock', 'ping', 'message', 'shell', 'patch', 'deploy')),
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
                          timeout_s, status, created_at, sent_at, started_at, finished_at,
                          exit_code, stdout, stderr, truncated, error)
    SELECT id, device_id, org_id, created_by, kind, argv_json, shell, body, body_sha256,
           text, action, mode, reboot, patch_ids_json,
           timeout_s, status, created_at, sent_at, started_at, finished_at,
           exit_code, stdout, stderr, truncated, error
      FROM commands;

DROP TABLE commands;
ALTER TABLE commands_new RENAME TO commands;

CREATE INDEX commands_device_status ON commands(device_id, status);
CREATE INDEX commands_status_sent ON commands(status, sent_at);
CREATE INDEX commands_created ON commands(created_at);

-- (2c) Recreate shell_sessions with its original 0003 schema and restore the rows
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
