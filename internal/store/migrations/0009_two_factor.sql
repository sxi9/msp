-- 0009_two_factor: TOTP second factor for staff logins (TF1).
--
-- Three new columns on users keep every existing account at 2FA-off (the
-- defaults), so nothing changes for a user who never enrols. totp_secret is the
-- base32 TOTP secret: it is write-only over the API (never returned, never
-- logged) and is cleared when 2FA is turned off. recovery_codes holds the
-- one-time recovery codes (10 per user), stored only as a sha256 hash and
-- single-use (used_at set when consumed). All timestamps are TEXT RFC3339 UTC.

ALTER TABLE users ADD COLUMN totp_secret TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN totp_enabled INTEGER NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN totp_confirmed_at TEXT NULL;

CREATE TABLE recovery_codes (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_hash  TEXT NOT NULL,
    used_at    TEXT NULL,
    created_at TEXT NOT NULL
);
CREATE INDEX recovery_codes_user ON recovery_codes(user_id);
