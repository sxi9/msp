-- 0010_totp_last_step: make an accepted TOTP code one-time (TF1 hardening).
--
-- RFC 6238 Section 5.2 requires the verifier to reject a second use of a TOTP
-- code that has already validated. totp_last_step records the highest 30 s time
-- step the user has successfully authenticated with at the login second factor;
-- a later login must present a code from a strictly greater step. A still-valid
-- code captured within its +/-1 skew window therefore cannot be replayed to mint
-- a second session. Default 0 (no step consumed yet); it is reset to 0 whenever
-- 2FA is turned off or admin-reset, so a re-enrolled user starts clean.

ALTER TABLE users ADD COLUMN totp_last_step INTEGER NOT NULL DEFAULT 0;
