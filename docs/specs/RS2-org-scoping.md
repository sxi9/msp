# RS2 — Per-technician org scoping (phase 2)

Today every staff user sees and acts on every client org. This adds the ability to restrict an
operator or viewer to a chosen set of client orgs. The whole value is **isolation**: a scoped user
must not read or touch anything outside their orgs, by any endpoint. This spec is the contract.

## Model

- New table `user_orgs(user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE, org_id
  INTEGER NOT NULL REFERENCES orgs(id) ON DELETE CASCADE, PRIMARY KEY(user_id, org_id))`.
- `users` gains `all_orgs INTEGER NOT NULL DEFAULT 1`. `1` = this user sees every org (today's
  behaviour); `0` = this user sees only the orgs listed in `user_orgs`.
- **admin** is always all-orgs: the model forbids scoping an admin (`all_orgs` forced to 1, any
  `user_orgs` rows ignored). Only **operator** and **viewer** can be scoped.
- Migration `000N_user_orgs.sql`: create the table, add the column with DEFAULT 1 so every existing
  user keeps full visibility on upgrade. Nothing is locked out by the upgrade. Do not edit
  `0001_init.sql`.

### Visible-org rule (one definition, used everywhere)

A user's visible org set is: **all orgs** if `role == admin` or `all_orgs == 1`; otherwise exactly
the `user_orgs` rows. Store helper `VisibleOrgIDs(ctx, userID) (ids []int64, all bool)` (`all=true`
means unrestricted, `ids` nil) and `UserCanSeeOrg(ctx, userID, orgID) (bool, error)`. The server
resolves the caller's set once per request from the session user.

## Enforcement — every org-scoped read and write

A scoped user hitting anything outside their set gets the **same response an unrelated resource
would give**, never a distinguishable "exists but forbidden":

- `GET /api/v1/orgs` → only visible orgs. `GET /api/v1/orgs/{id}` → `404 not_found` if not visible.
  `POST /api/v1/orgs` stays admin-only. Token create/revoke under an org → `404` if the org is not
  visible (checked before the action).
- `GET /api/v1/summary` → counts computed over visible orgs only.
- `GET /api/v1/devices` (list) → only devices whose org is visible; an explicit `?org_id=` that is
  not visible → `404 not_found` (today it is already 404 for a non-existent org; keep that shape).
- `GET /api/v1/devices/{id}` and **every device-scoped action** — commands (`POST …/commands`,
  `GET …/commands`, `GET /commands/{id}`), shell (`POST …/shell`, both WS legs, `GET …/shell/{id}/
  transcript`), lock/ping/message, remote-desktop (`POST …/remote-desktop`), heartbeats, revoke,
  delete — must resolve the device, then return `404 not_found` if its org is not visible, **before**
  any side effect, role check result, or 409. A scoped user must not be able to tell a device in
  another org exists.
- `GET /api/v1/alerts` and `POST /api/v1/alerts/{id}/ack` → alerts filtered to visible orgs; ack of
  an alert in an invisible org → `404`.
- `GET /api/v1/audit` → entries filtered to visible orgs. Audit rows with `org_id IS NULL` (system
  actions not tied to an org) are visible only to all-orgs users.
- The agent API (`/api/agent/v1/*`) is device-token auth and is **unchanged**: scoping is a
  staff-visibility feature only.

Push the filter into the store queries that return lists (devices, alerts, audit, orgs, summary):
add an optional `VisibleOrgs []int64` (nil = all) to the relevant params/filters and a SQL
`org_id IN (…)` clause. Do not filter large lists in Go after the fact. For single-resource loads
(`GetDevice`, a command, an alert, a token), load then check the org against the caller.

## Admin management

- `GET /api/v1/users` (admin) → each user row gains `all_orgs` (bool) and `org_ids` (array, empty
  when all_orgs). 
- `POST /api/v1/users` (admin) unchanged for admin role; for operator/viewer it may include
  `"all_orgs":false,"org_ids":[1,2]` (validated: every id exists; empty list with all_orgs false is
  allowed and means the user sees nothing yet).
- `PUT /api/v1/users/{id}/orgs` (admin) `{"all_orgs":bool,"org_ids":[...]}` → replaces the user's org
  assignment. `400` if the target is an admin and all_orgs is false (admins cannot be scoped). Audit
  `user.orgs_changed` with the new set. `404` if the user does not exist.
- A scoped user who is later promoted to admin becomes all-orgs; demoting an admin to operator keeps
  all_orgs=1 until an admin scopes them (document this).

## Dashboard

- Users page (admin only): each operator/viewer row shows "All clients" or the list of assigned
  clients, with an "Edit access" control → a dialog with an "All clients" toggle and, when off, a
  checklist of every org. Saving calls `PUT …/orgs`. Admin rows show "All clients (admin)" and no
  editor.
- The create-user form, when role is operator or viewer, offers the same All-clients toggle +
  checklist.
- A scoped user's own experience: the org filter dropdowns, the Orgs page, the Fleet, Alerts and
  Audit pages already receive only their visible orgs from the API, so they simply show less; make
  sure nothing in the UI assumes it can see an org it cannot (e.g. a device row linking to an org
  page that 404s). Hide the "Add client" button for non-admins (org creation is admin-only).
- Keep `?mock=1` working: add a scoped operator and show the editor.

## Docs

`docs/PROTOCOL.md` (the user endpoints, the `all_orgs`/`org_ids` fields, the visible-org rule and
that device-scoped endpoints 404 across orgs), `docs/SECURITY.md` (tenant isolation now has two
layers: org ownership of data, and per-user visibility; the 404-not-403 rule), `docs/GUIDE.md`
(a "Client access per technician" subsection under Users), `docs/ARCHITECTURE.md`, `docs/FEATURES.md`,
`docs/STATUS.md`, `docs/ROADMAP.md`, `docs/CHECKLIST.md`.

## Proof (what the build must show here)

`CGO_ENABLED=0 go build ./...`, `go vet ./...`, `CGO_ENABLED=0 go test ./... -count=1`,
`GOOS=windows GOARCH=amd64 go build ./...`, `make web`, `scripts/e2e.sh`.

A new `internal/e2e` test (or a thorough server test) that, with two orgs A and B and a scoped
operator who can see only A:
- sees only org A in `GET /orgs`, `GET /devices`, `GET /alerts`, `GET /audit`, `GET /summary`;
- gets `404` on `GET /orgs/{B}`, `GET /devices/{B's device}`, `POST …/commands`, `POST …/shell`,
  `POST …/lock`, `POST …/remote-desktop`, `GET …/transcript`, `POST /alerts/{B's alert}/ack` — each
  **before** any side effect (assert no command/alert row was created in B, no audit row written);
- an admin `PUT …/orgs` can grant org B, after which the same operator now sees B;
- admins and all_orgs users are unaffected (every existing test still passes).

Also update `docs/screenshots` in the verify stage with the Edit-access dialog and a scoped
operator's reduced Fleet.
