-- 0004_user_orgs: per-technician org scoping (RS2).
-- all_orgs defaults to 1 so every existing user keeps full visibility on upgrade;
-- a scoped operator/viewer has all_orgs=0 and exactly the user_orgs rows listed.
-- admin is always all-orgs (enforced in the store, never scoped here).

ALTER TABLE users ADD COLUMN all_orgs INTEGER NOT NULL DEFAULT 1;

CREATE TABLE user_orgs (
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    org_id  INTEGER NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    PRIMARY KEY (user_id, org_id)
);
CREATE INDEX user_orgs_org ON user_orgs(org_id);
