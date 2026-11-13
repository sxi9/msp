-- 0003_shell_sessions: remote shell (RS1, decision D3).
--
-- Two changes:
--
-- (1) commands.kind must accept 'shell'. SQLite cannot ALTER a CHECK in place,
--     so the commands table is recreated with the enlarged constraint and the
--     rows, indexes and AUTOINCREMENT sequence are preserved. This is the
--     standard "create new, copy, drop old, rename" rebuild. It is safe with
--     foreign_keys ON because nothing references commands yet: shell_sessions,
--     the first and only child, is created afterwards.
--
-- (2) shell_sessions holds the per-session relay accounting and the recorded
--     agent->browser transcript (cap 1 MiB; see internal/store/shell.go).

CREATE TABLE commands_new (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    device_id   INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    org_id      INTEGER NOT NULL REFERENCES orgs(id),
    created_by  INTEGER NOT NULL REFERENCES users(id),
    kind        TEXT NOT NULL CHECK (kind IN ('exec', 'script', 'lock', 'ping', 'message', 'shell')),
    argv_json   TEXT NULL,
    shell       TEXT NULL,
    body        TEXT NULL,
    body_sha256 TEXT NULL,
    text        TEXT NULL,
    timeout_s   INTEGER NOT NULL,
    status      TEXT NOT NULL CHECK (status IN ('queued', 'sent', 'done', 'failed', 'timeout', 'lost', 'cancelled')),
    created_at  TEXT NOT NULL,
    sent_at     TEXT NULL,
    started_at  TEXT NULL,
    finished_at TEXT NULL,
    exit_code   INTEGER NULL,
    stdout      TEXT NOT NULL DEFAULT '',
    stderr      TEXT NOT NULL DEFAULT '',
    truncated   INTEGER NOT NULL DEFAULT 0,
    error       TEXT NOT NULL DEFAULT ''
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
