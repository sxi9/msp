# Phase 1 build plan

Produced by a three-way architecture panel (judged, then synthesized) and amended with decisions D4/D5 from DECISIONS.md.

## SUMMARY

The final plan keeps the judges' winner (operator-first: one Go server binary, long-poll /sync for sub-second command delivery, Fleet/Device/Alerts/Orgs screens, per-OS install one-liners served by the server) and fixes every critique raised. Transport: agents POST /api/agent/v1/sync carrying metrics plus their own interval_s and a wait_s long-poll (capped at 25 s, under proxy idle timeouts); the server computes online = now - last_seen_at <= 3*interval_s + 15 s per device (deterministic e2e without test flags), returns poll_interval_s=5 as a proxy-cut fallback while commands remain queued, and a sweeper marks sent-but-never-reported commands lost so the console never spins forever. Android is a dependency-free Kotlin app using foregroundServiceType=specialUse (not dataSync, which Android 15 caps at 6 h/day), REQUEST_IGNORE_BATTERY_OPTIMIZATIONS, and cleartextTrafficPermitted=false in release with a debug-only overlay; it shares the protocol by contract (docs/PROTOCOL.md + internal/protocol/fixtures tested on both sides), not by gomobile. The dashboard is Preact 10 + TypeScript bundled by esbuild (one bundler dep, node --test on pure .ts modules via Node 22's native type stripping) and embedded with go:embed behind a committed index.html so go build never needs Node. Security basics: bcrypt cost 12, sha256-at-rest device/enrollment/session tokens, SameSite=Strict cookie + X-Requested-With: msp CSRF header, login/enroll rate limits, CSP and security headers, three roles (admin/operator/viewer), agent refuses http:// unless loopback or --insecure-http (no skip-verify flag), argv-only exec or script-to-0600-file with process-group kill, 64 KiB output caps, append-only audit with actor_type user|device|system and script sha256. The winner's 1,800-line server monolith is split into store (WP1), server core + agent API + alerts/jobs + CLI (WP2) and dashboard API (WP3) with frozen Go signatures in the shared sections; agent core (Linux-buildable, WP4) and Windows-only build-tagged files (WP5) never collide on Linux builds; ten work packages own disjoint paths and include all glue. Every version pinned here was verified in this container: Go 1.24.7, Node 22.23.4 (TS test files run natively), Gradle 8.14.3 on JDK 21 with AGP 8.7.3 / Kotlin 2.0.21 / junit 4.13.2 / org.json 20240303 already in ~/.gradle, Android platform 35 + build-tools 35.0.0 at /opt/android-sdk, esbuild 0.28.2 / preact 10.29.8 / typescript 5.9.3 on the npm registry, modernc.org/sqlite v1.62.0 on the Go proxy, Docker CLI present but daemon down (compose validated with docker compose config). Proof is scripts/e2e.sh plus an in-process Go e2e test: start server, bootstrap admin/org/token via CLI, enroll and run a real Linux agent, see it online through the dashboard JSON API, run `uname -a`, an injection check (argv ["echo","a;id"]), a script, a timeout, an Android refusal, the audit trail, and the offline flip. Phase 1 is ~5,500-6,000 lines.

## STACK

SERVER (cmd/msp-server, internal/server, internal/store): Go 1.24 (container has go1.24.7), stdlib net/http with Go 1.22+ method/pattern routing (no router dep), database/sql + modernc.org/sqlite v1.62.0 (pure Go => CGO_ENABLED=0 everywhere, one static binary, alpine image), golang.org/x/crypto v0.59.0 (bcrypt only; argon2id rejected because 64 MiB per verify is a memory-DoS lever behind a per-IP limiter), golang.org/x/sys v0.50.0 (agent only), log/slog, embed. Why Go: one language for server and desktop agent, shared internal/protocol package so JSON shapes cannot drift, static cross-compiles for every target from this container, and three third-party modules total in the whole Go build. SQLite in WAL mode behind one package (internal/store) with every SQL statement there, so Postgres is a phase-2 second implementation, not a rewrite. Sizing: 2,000 desktop devices at 30 s = 67 syncs/s, each 1-2 small writes, plus 2,000 parked long-poll goroutines (~10 KB each) — inside SQLite/Go's envelope on one box; metrics samples are throttled to one row per device per 60 s and pruned after MSP_RETENTION_DAYS (7).

DESKTOP AGENT (cmd/msp-agent, internal/agent): Go 1.24, same module, dependency golang.org/x/sys only (windows/svc + svc/mgr for the Windows service, windows/registry for MachineGuid and product names, windows API for GetSystemTimes/GlobalMemoryStatusEx/GetDiskFreeSpaceExW/GetSystemPowerStatus, unix.Statfs/Uname on Linux). No gopsutil (large unauditable transitive tree for a root-level agent); Linux metrics read /proc/stat, /proc/meminfo, /proc/uptime, /proc/mounts, /etc/os-release, /sys/class/power_supply, /sys/class/dmi/id. Command runner: os/exec with argv arrays, or script body written to a 0600 file inside a 0700 temp dir and passed as the single file argument to a fixed interpreter argv; never a shell string. ~8 MB static binary for linux/amd64, linux/arm64, windows/amd64; android/arm64 cross-build kept compiling as a CI proof only.

ANDROID AGENT (android/): Kotlin 2.0.21, AGP 8.7.3, Gradle 8.14.3 (system gradle; wrapper jar not committed), JDK 21 in the container with Java 17 bytecode target, compileSdk 35, targetSdk 35, buildToolsVersion "35.0.0" (pinned: AGP 8.7 defaults to 34.0.0), minSdk 26. Zero third-party runtime dependencies: HttpURLConnection + org.json (platform) for the protocol, HandlerThread loop, platform Notification.Builder, BatteryManager/ActivityManager/StatFs for metrics, app-private SharedPreferences for the device token. Test deps only: junit:junit:4.13.2 and org.json:json:20240303 (both already in ~/.gradle/caches). Why not gomobile: needs the NDK (not installed), cgo and a 10+ MB AAR for three HTTP calls, and Android cannot read /proc/stat or run operator shell commands anyway, so the runner (the only large shared logic) does not apply.

DASHBOARD (web/): Preact 10.29.8 + TypeScript 5.9.3, bundled by esbuild 0.28.2 (the only build dependency; no Vite/Babel tree, no CSP modulePreload polyfill problem), hand-written hash router (~40 lines), fetch() against the JSON API with 5 s polling on Fleet and 1 s polling on a running command, one hand-written CSS file with light/dark via prefers-color-scheme. Pure modules (format, status/sort, argv parsing) are tested with `node --test` directly on .ts files (verified: Node 22.23.4 strips types natively; test files avoid enums/parameter properties and import with .ts extensions). Output web/dist/app.js + app.css is embedded into msp-server by go:embed; web/dist/index.html is committed and renders "dashboard bundle not built — run make web" until the bundle exists, so go build and all Go tests work without Node.

OPS (deploy/, scripts/, .github/): multi-stage Dockerfile (node:22-alpine web build -> golang:1.24-alpine server build + agent cross-compile -> alpine:3.20 non-root with wget healthcheck), docker-compose.yml (server + ./data volume, optional commented Caddy for automatic HTTPS), systemd unit for the Linux agent (no ProtectSystem=full: remote-admin scripts must write /etc and /usr), `msp-agent install` registers the Windows service via x/sys/windows/svc/mgr (no sc.exe strings), GitHub Actions CI running the same Makefile targets, agent binaries + SHA256SUMS + install scripts served by the server at /dl/ from MSP_AGENT_DIR.

NOT USED: Rust, React/Vite, websockets/gRPC/MQTT, gopsutil, androidx/coroutines, any ORM, any CSS framework.

## REPO_LAYOUT

msp/                                        (clone into its own empty directory, e.g. /home/user/msp; /home/user/vault is an unrelated project)
  go.mod                                    module github.com/sxi9/msp; go 1.24; require modernc.org/sqlite v1.62.0, golang.org/x/crypto v0.59.0, golang.org/x/sys v0.50.0
  go.sum
  VERSION                                   0.1.0 (read by Makefile, scripts, Dockerfile via -X main.version)
  Makefile                                  targets: build test race web agents android android-test e2e compose-check docker clean (exact commands in build_and_test)
  README.md                                 quickstart: build, bootstrap admin/org/token, run server, enroll a Linux agent, open dashboard, run a command; make targets
  .gitignore                                bin/ dist/ *.db *.db-wal *.db-shm data/ web/node_modules/ web/dist/app.js web/dist/app.css android/.gradle/ android/build/ android/app/build/ android/local.properties
  .github/workflows/ci.yml                  jobs: go (vet, test -race, build-agents.sh, upload dist), web (npm ci/test/typecheck/build), android (gradle assembleDebug testDebugUnitTest, upload APK), e2e (scripts/e2e.sh)
  docs/
    PROTOCOL.md                             normative agent<->server + dashboard API contract (= protocol section verbatim); Android codes only from this + fixtures
    ARCHITECTURE.md                         this plan condensed; component diagram in text; package dependency rules
    OPS.md                                  TLS (direct or Caddy), backups, upgrades, agent install per OS, token hygiene, logs, firewall
    SECURITY.md                             threat model (operator account = root on every client), token lifecycle, hashing, CSRF, rate limits, audit, phase-2 preconditions (MFA, command signing)
  cmd/
    msp-server/main.go                      CLI: serve | user add | org add | token new | version; env+flags; wires store+server; TLS; graceful shutdown
    msp-agent/main.go                       CLI: enroll | run | install | uninstall | status | version; exit codes 0/1/2
  internal/
    protocol/
      types.go                              ALL wire structs + JSON tags + constants + Command.Validate() (shared by server, agent, tests, fixtures)
      types_test.go                         unmarshal->marshal round-trip of every fixture, semantic equality via map[string]any
      fixtures/enroll_linux.json            canonical samples (also read by the Kotlin unit test)
      fixtures/enroll_android.json
      fixtures/sync_linux.json
      fixtures/sync_android.json
      fixtures/sync_response.json           one exec + one script command
      fixtures/command_result.json
      fixtures/error.json
    store/
      store.go                              Store struct, Open (modernc DSN, pragmas, migrations), Close, Ping, Now func, HashToken, NewToken, StatusOf, sentinel errors
      types.go                              Org, User, Session, EnrollmentToken, Device, Heartbeat, Command, Alert, AuditEntry, DeviceFilter, Summary, param structs (json tags = API field names)
      migrations/0001_init.sql              full schema (data_model) applied via go:embed in lexical order, recorded in schema_migrations
      orgs.go users.go sessions.go tokens.go devices.go heartbeats.go commands.go alerts.go audit.go summary.go
      store_test.go                         temp-dir DB tests: token consume limits, upsert rotation, claim-exactly-once, result state machine, alert idempotency, status math
    server/                                 ONE Go package `server`; WP2 and WP3 own disjoint files in it
      server.go                             Config, Server, New, Handler (mux assembly, iterates dashboardRoutes), Run (http.Server, TLS, jobs, shutdown)
      middleware.go                         request log, recover, body limit, security headers, csrf, sessionAuth, requireRole, deviceAuth, clientIP
      ratelimit.go                          in-memory per-IP token bucket (login, enroll)
      json.go                               writeJSON, writeError, readJSON, pathInt
      auth.go                               POST /api/v1/login, /logout, GET /api/v1/me, hashPassword, roleRank
      api_agent.go                          GET ping, POST enroll, POST sync (long-poll), POST commands/{id}/result, GET /healthz
      notifier.go                           per-device wake channels: Subscribe(id) (<-chan struct{}, unsubscribe func), Notify(id)
      alerts.go                             evaluateOnce(now): offline/disk_full/memory_high/battery_low open+resolve, MarkLostCommands
      jobs.go                               runJobs(ctx): evaluator every 15 s, pruner hourly
      api_agent_test.go auth_test.go alerts_test.go notifier_test.go
      dashboard.go                          init() registers registerDashboard into dashboardRoutes; summary; shared dash* helpers; install one-liner rendering
      dashboard_orgs.go                     orgs CRUD, enrollment tokens create/list/revoke
      dashboard_devices.go                  devices list/detail/heartbeats/revoke/delete
      dashboard_commands.go                 command create (validate, audit, notify), list, get
      dashboard_alerts.go                   alerts list/ack
      dashboard_audit.go                    audit list
      dashboard_users.go                    users list/create/disable
      dashboard_static.go                   SPA from web.FS with index fallback + no-cache; /dl/ from cfg.AgentDir
      dashboard_test.go                     httptest + temp sqlite: auth/csrf/roles, device JSON shape, command validation, android 400
    agent/                                  ONE Go package `agent`; WP4 owns Linux-buildable files, WP5 owns windows/other build-tagged files
      config.go                             Config (server_url, device_token, device_id, interval_s, ca_file, insecure_http), DefaultPath, Load, Save (0700 dir, 0600 file)
      client.go                             Client: URL scheme policy, TLS/CA pool, 40 s timeout, Bearer, Enroll/Sync/PostResult, ErrUnauthorized
      agent.go                              Enroll(...) and Run(ctx, cfg, version, log): sync loop, backoff, command dispatch (semaphore 2), result retry
      runner.go                             Execute(ctx, protocol.Command) protocol.CommandResult: exec/script, temp dir, limited writers, timeout
      runner_unix.go                        //go:build !windows: setSysProcAttr (Setpgid), killProcess (kill -pgid), shell map sh/bash
      runner_windows.go                     //go:build windows: CREATE_NEW_PROCESS_GROUP, Process.Kill, shell map powershell/cmd  [WP5]
      metrics.go                            collector struct, cpuPct(idleDelta,totalDelta), localIPs, arch, hostname, Facts assembly helpers
      metrics_linux.go                      //go:build linux: /proc + statfs + os-release + dmi + battery
      metrics_windows.go                    //go:build windows: GetSystemTimes, GlobalMemoryStatusEx, drives, RtlGetVersion, power status, registry  [WP5]
      metrics_other.go                      //go:build !linux && !windows: hostname-only stub (keeps android/darwin cross-builds compiling)  [WP5]
      machineid_linux.go machineid_windows.go machineid_other.go   machineID()  [linux: WP4; others: WP5]
      service_linux.go                      Install/Uninstall/IsService/RunAsService via systemd; unit text as const (mirrors deploy/msp-agent.service)
      service_windows.go                    //go:build windows: x/sys/windows/svc + mgr  [WP5]
      service_other.go                      //go:build !linux && !windows: errors.ErrUnsupported  [WP5]
      agent_test.go metrics_test.go runner_test.go service_test.go
    e2e/e2e_test.go                         in-process proof: httptest server + real agent.Run + runner + dashboard JSON API assertions
  web/
    embed.go                                package web; //go:embed dist/*; var FS embed.FS
    dist/index.html                         committed shell: <div id="app">Dashboard bundle not built — run make web</div> + <script src="/app.js"> + <link href="/app.css">
    package.json                            dependencies preact 10.29.8; devDependencies esbuild 0.28.2, typescript 5.9.3; scripts build/test/typecheck
    package-lock.json                       committed (npm ci requires it)
    tsconfig.json                           strict, jsx react-jsx, jsxImportSource preact, moduleResolution bundler, allowImportingTsExtensions, noEmit, exclude *.test.ts
    build.mjs                               esbuild bundle src/main.tsx -> dist/app.js (+ dist/app.css from imported styles.css), minify, target es2020
    src/main.tsx src/router.ts src/api.ts src/types.ts src/mock.ts
    src/lib/format.ts format.test.ts        bytes/percent/relative-time formatting
    src/lib/status.ts status.test.ts        fleet sort (alerting > offline > online > pending), filters, group-by-org
    src/lib/argv.ts argv.test.ts            one-argument-per-line parsing + argv preview
    src/views/Login.tsx Fleet.tsx Device.tsx Alerts.tsx Orgs.tsx Org.tsx Audit.tsx Users.tsx
    src/components/Layout.tsx StatusDot.tsx Bar.tsx Sparkline.tsx CopyBlock.tsx
    src/styles.css
  android/
    settings.gradle.kts build.gradle.kts gradle.properties
    app/build.gradle.kts                    namespace com.sxi9.msp.agent, compileSdk 35, buildToolsVersion 35.0.0, minSdk 26, fixtures system property for tests
    app/src/main/AndroidManifest.xml        INTERNET, FOREGROUND_SERVICE, FOREGROUND_SERVICE_SPECIAL_USE, RECEIVE_BOOT_COMPLETED, POST_NOTIFICATIONS, REQUEST_IGNORE_BATTERY_OPTIMIZATIONS; specialUse service + property
    app/src/main/java/com/sxi9/msp/agent/MainActivity.kt AgentService.kt BootReceiver.kt ApiClient.kt Protocol.kt Metrics.kt Prefs.kt
    app/src/main/res/layout/activity_main.xml values/strings.xml drawable/ic_notification.xml xml/network_security_config.xml (cleartext false)
    app/src/debug/res/xml/network_security_config.xml   cleartext true (debug builds only, for lab/http servers)
    app/src/test/java/com/sxi9/msp/agent/ProtocolTest.kt   JVM test against ../../internal/protocol/fixtures
  deploy/
    Dockerfile docker-compose.yml Caddyfile.example
    msp-agent.service                       systemd unit (identical to the const in service_linux.go)
    install.sh                              Linux installer: download binary + SHA256SUMS from /dl/, verify, msp-agent install
    install.ps1                             Windows installer (admin PowerShell): download exe, verify hash, msp-agent.exe install
  scripts/
    e2e.sh                                  real-binary end-to-end proof; exit code is the verdict; prints command round-trip latency
    build-agents.sh                         cross-compile matrix into dist/ + SHA256SUMS + copies deploy/install.* ; copies to $MSP_AGENT_DIR when set
  bin/ dist/                                gitignored build outputs

## PROTOCOL

TRANSPORT: HTTPS (or HTTP behind a TLS reverse proxy / loopback in dev). JSON bodies, Content-Type: application/json. All timestamps RFC3339 UTC strings. Server listens on MSP_LISTEN (default :8080; e2e uses 127.0.0.1:18080). Path families: /api/agent/v1/* (device bearer auth), /api/v1/* (staff session auth), /healthz, /dl/*, / (SPA). Body limits: 256 KiB agent, 512 KiB dashboard (http.MaxBytesReader). Error body everywhere: {"error":"<code>","detail":"<optional text>"}. Agent User-Agent: msp-agent/<version> (<linux|windows|android>/<arch>).

AGENT AUTH
- Enrollment token: "et_" + 43 chars base64url (32 random bytes). Per org, created by admin|operator; plaintext returned once; stored as sha256 hex with token_prefix (first 10 chars) for display; max_uses, expires_at, revoked_at. Used only by POST /enroll.
- Device token: "dt_" + 43 chars base64url, returned once at enrollment, stored on server as sha256 hex in devices.token_hash; sent as `Authorization: Bearer dt_...`. One token per device. Re-enrolling the same (org_id, device_uid) reuses the device row (history kept) and rotates the token. Revoke = token_hash set NULL -> next request 401 {"error":"invalid_device_token"}; the agent logs "token rejected, re-enroll with msp-agent install" and retries every 5 min (never exits).
- Agent URL policy: https:// always; http:// only when host is localhost/127.0.0.0/8/::1 or config insecure_http=true (--insecure-http). --ca-file PEM adds a private CA. There is no skip-verify option.

AGENT ENDPOINTS
1) GET /api/agent/v1/ping (no auth) -> 200 {"ok":true,"version":"0.1.0"}

2) POST /api/agent/v1/enroll (no auth; rate limit 10 requests/min/IP -> 429 {"error":"rate_limited"})
Request:
{"enrollment_token":"et_...","device_uid":"f2c1...","hostname":"ws-reception-01","platform":"linux","arch":"amd64","os_version":"Ubuntu 24.04.1 LTS","agent_version":"0.1.0","interval_s":30,
 "facts":{"kernel":"6.8.0-45-generic","cpu_model":"Intel(R) Core(TM) i5","cpu_count":8,"mem_total_mb":16000,"manufacturer":"Dell Inc.","model":"Latitude 5540","serial":"ABC123","ips":["10.0.0.12"]}}
device_uid: /etc/machine-id | HKLM\SOFTWARE\Microsoft\Cryptography\MachineGuid | Settings.Secure.ANDROID_ID. platform in linux|windows|android, arch in amd64|arm64|386|arm. interval_s clamped 1..3600 (0 -> 30, or 60 for android). Android: hostname = Settings.Global "device_name" or Build.MODEL; os_version "Android 14 (34)"; facts.kernel = System.getProperty("os.version"), cpu_model = Build.HARDWARE.
Response 201: {"device_id":42,"device_token":"dt_...","poll_interval_s":30}
Errors: 400 {"error":"invalid_request","detail":"..."}; 401 {"error":"invalid_enrollment_token"} (unknown, expired, revoked, uses>=max_uses). Side effects: enrollment_tokens.uses += 1 atomically; devices upsert; audit action device.enrolled actor_type=device.

3) POST /api/agent/v1/sync (Bearer dt_). Heartbeat AND command channel.
Request:
{"agent_version":"0.1.0","interval_s":30,"wait_s":25,"uptime_s":86400,
 "metrics":{"cpu_pct":12.5,"mem_total_mb":16000,"mem_used_mb":8120,"disks":[{"mount":"/","total_gb":476.0,"used_gb":201.3}],"battery_pct":87,"battery_charging":false,"ips":["10.0.0.12"]},
 "facts":{...same shape as enroll...}}
- cpu_pct is null on Android and on the agent's first sample; battery_pct/battery_charging absent when no battery; Android sends one disk {"mount":"/data",...}; facts present only on the first sync after start and every 120th sync (server replaces facts_json when present).
- interval_s = the agent's configured interval (desktop 30, android 60, e2e 2); clamped 1..3600. wait_s = how long the server may hold the request; server clamps to 0..25; Android sends 0.
Server: deviceAuth -> RecordSync (last_seen_at, last_ip, agent_version, interval_s, last_metrics_json, denormalized cpu/mem/disk/battery/uptime columns; heartbeats row only if last_sample_at older than 60 s) -> if platform android respond immediately with commands [] -> else subscribe to the device's notifier, ClaimQueuedCommands(device, 10) (queued->sent, sent_at=now); if none and wait_s>0 block until Notify(device) or wait_s elapses or client disconnects, then claim again -> respond.
Response 200: {"poll_interval_s":30,"commands":[{"id":7,"kind":"exec","argv":["uname","-a"],"timeout_s":60},{"id":8,"kind":"script","shell":"sh","body":"df -h\nuptime\n","timeout_s":120}]}
poll_interval_s = 5 while more queued commands remain for the device (fallback for agents whose long-poll is cut by a proxy), else the device's interval_s. commands is [] (never null).
Agent loop: sync -> for each command spawn a goroutine (max 2 concurrent per device) that executes and POSTs the result -> if the response contained commands sync again immediately, else sleep max(0, poll_interval_s - elapsed) where poll_interval_s is clamped to 1..interval_s -> loop. Network/5xx error: exponential backoff 5 s doubling to 300 s, reset on success. 401: sleep 5 min, retry.
Errors: 400 invalid_request, 401 invalid_device_token.

4) POST /api/agent/v1/commands/{id}/result (Bearer dt_; the command must belong to this device and be in status sent)
Request: {"exit_code":0,"stdout":"Linux ws-01 6.8...\n","stderr":"","started_at":"2026-11-13T10:00:01Z","finished_at":"2026-11-13T10:00:01Z","truncated":false,"error":""}
error is "" on normal completion; "timeout after 60s" when the deadline hit (exit_code -1); "spawn: <reason>" when the process could not start (exit_code -1). stdout/stderr each capped at 65536 bytes by the agent (truncated=true).
Response 204. Status becomes timeout (error starts with "timeout"), failed (error non-empty or exit_code != 0), else done. 404 {"error":"not_found"} if the command is not in status sent for this device; 409 {"error":"conflict"} if already finished. Audit command.finished actor_type=device.

STATUS AND TIMING: desktop interval 30 s, Android 60 s, wait_s 25 (agent HTTP timeout 40 s). Device status computed at read time from devices.last_seen_at and interval_s: "pending" if never seen, "online" if now - last_seen_at <= 3*interval_s + 15 s, else "offline" (30 s -> 105 s; 60 s -> 195 s; e2e 2 s -> 21 s). Evaluator runs every 15 s: opens alert type offline (severity high) for offline, non-revoked devices and resolves it when online again; marks commands lost when status=sent and sent_at + timeout_s + 5 min < now (audit command.lost actor_type=system). Command latency: < 1 s while the agent is parked in a sync, <= interval_s otherwise.

COMMAND EXECUTION (desktop agents only; server returns 400 unsupported_on_device when staff queue one for an android device):
- kind "exec": argv array, len >= 1, each arg <= 4096 bytes; agent runs exec.CommandContext(ctx, argv[0], argv[1:]...) (PATH lookup allowed, no shell, metacharacters literal).
- kind "script": shell in sh|bash (Linux) or powershell|cmd (Windows) + body (non-empty, <= 262144 bytes); agent writes body to <tmpdir>/msp-cmd-<id>/script.(sh|ps1|cmd) (dir 0700, file 0600), runs [sh|bash, file] / [powershell.exe, -NoProfile, -NonInteractive, -ExecutionPolicy, Bypass, -File, file] / [cmd.exe, /C, file]; wrong shell for the platform -> error "spawn: shell <x> not available on <platform>"; dir removed afterwards.
- timeout_s default 60, clamp 1..3600; on deadline the process group is killed (Linux Setpgid + kill(-pgid); Windows Process.Kill, tree kill is phase 2). cwd = the temp dir; env = the service's environment unchanged plus MSP_COMMAND_ID=<id>. Only admin|operator can queue; command.queued audit row stores argv or shell + body sha256 + first 200 chars.

DASHBOARD API (/api/v1; session cookie msp_session: HttpOnly, SameSite=Strict, Path=/, Secure when TLS or MSP_SECURE_COOKIES=1, 12 h; cookie value "ss_"+random stored as sha256. Every non-GET request must carry header `X-Requested-With: msp` else 403 {"error":"csrf"}. 401 {"error":"unauthorized"} without a valid session; 403 {"error":"forbidden"} for insufficient role. Roles: viewer < operator < admin.)
POST /api/v1/login {"email","password"} -> 200 {"user":{"id":1,"email":"...","role":"admin"}} + Set-Cookie; 401 {"error":"invalid_credentials"}; 429 after 10 attempts/min/IP. Audit user.login / user.login_failed.
POST /api/v1/logout -> 204.   GET /api/v1/me -> {"user":{...}}
GET /api/v1/summary -> {"orgs":4,"devices":57,"online":51,"offline":5,"pending":1,"open_alerts":3}
GET /api/v1/orgs -> {"orgs":[{"id":1,"name":"Acme Dental","created_at":"...","device_count":12,"online_count":11,"open_alerts":1}]}
POST /api/v1/orgs {"name"} (admin) -> 201 {"org":{...}}; 409 {"error":"conflict"} on duplicate. Audit org.created.
GET /api/v1/orgs/{id} -> {"org":{...},"tokens":[{"id":3,"org_id":1,"token_prefix":"et_Ab12cD3","label":"front desk","max_uses":10,"uses":2,"expires_at":"...","created_by":1,"created_at":"...","revoked_at":null}]}
POST /api/v1/orgs/{id}/tokens {"label":"front desk","max_uses":10,"ttl_hours":168} (admin|operator; defaults 10 and 168; max_uses 1..1000, ttl_hours 1..8760) -> 201 {"token":{...row...},"plaintext":"et_...","install":{"linux":"curl -fsSLo /tmp/msp-install.sh BASE/dl/install.sh && sudo bash /tmp/msp-install.sh --server BASE --token et_...","windows":"irm BASE/dl/install.ps1 -OutFile $env:TEMP\\msp-install.ps1; & $env:TEMP\\msp-install.ps1 -Server BASE -Token et_...","android":"Install BASE/dl/msp-agent.apk, open MSP Agent, enter BASE and the token et_..."}} where BASE = MSP_BASE_URL or request scheme+host. Audit token.created.
POST /api/v1/orgs/{id}/tokens/{tid}/revoke (admin|operator) -> 204. Audit token.revoked.
GET /api/v1/devices?org_id=&status=online|offline|pending&q=<hostname substring> -> {"devices":[DEVICE...]}
DEVICE = {"id":42,"org_id":1,"org_name":"Acme Dental","device_uid":"f2c1...","hostname":"ws-01","platform":"linux","arch":"amd64","os_version":"Ubuntu 24.04.1 LTS","agent_version":"0.1.0","interval_s":30,"status":"online","enrolled_at":"...","last_seen_at":"...","last_ip":"10.0.0.12","revoked":false,"cpu_pct":12.5,"mem_used_pct":50.8,"disk_used_pct":42.3,"battery_pct":null,"battery_charging":null,"uptime_s":86400,"open_alerts":0,"can_run_commands":true}
GET /api/v1/devices/{id} -> {"device":DEVICE,"facts":{...facts object...},"metrics":{...last metrics object...},"alerts":[open ALERT...],"commands":[last 20 COMMAND without stdout/stderr]}
GET /api/v1/devices/{id}/heartbeats?limit=120 -> {"heartbeats":[{"at":"...","cpu_pct":12.5,"mem_used_pct":50.8,"disk_used_pct":42.3,"battery_pct":null,"uptime_s":86400}]} oldest first
POST /api/v1/devices/{id}/commands (admin|operator) {"kind":"exec","argv":["uname","-a"],"timeout_s":60} or {"kind":"script","shell":"sh","body":"df -h\n","timeout_s":120} -> 201 {"command":COMMAND}; 400 {"error":"unsupported_on_device"} for android or revoked devices; 400 {"error":"invalid_request","detail":"..."} on validation failure. Audit command.queued, then Notify(device).
GET /api/v1/devices/{id}/commands?limit=50 -> {"commands":[COMMAND...]} newest first
GET /api/v1/commands/{id} -> {"command":COMMAND}
COMMAND = {"id":7,"device_id":42,"org_id":1,"created_by":1,"created_by_email":"admin@example.com","kind":"exec","argv":["uname","-a"],"shell":"","body":"","timeout_s":60,"status":"done","created_at":"...","sent_at":"...","started_at":"...","finished_at":"...","exit_code":0,"stdout":"Linux ...","stderr":"","truncated":false,"error":""}  (status in queued|sent|done|failed|timeout|lost|cancelled; nullable times are null)
POST /api/v1/devices/{id}/revoke (admin) -> 204 (token_hash NULL; audit device.revoked).   DELETE /api/v1/devices/{id} (admin) -> 204 (cascade; audit device.deleted)
GET /api/v1/alerts?state=open|all&org_id= -> {"alerts":[{"id":9,"org_id":1,"org_name":"...","device_id":42,"hostname":"ws-01","type":"offline","severity":"high","message":"no sync for 6m","opened_at":"...","resolved_at":null,"acknowledged_by":null,"acknowledged_at":null}]} (default open; limit 500)
POST /api/v1/alerts/{id}/ack (admin|operator) -> 204. Audit alert.ack.
GET /api/v1/audit?limit=200&org_id=&before=<id> -> {"entries":[{"id":1,"at":"...","actor_type":"user","actor_id":1,"actor_label":"admin@example.com","org_id":1,"action":"command.queued","target_type":"command","target_id":7,"ip":"127.0.0.1","detail":{...}}]} newest first; actor_label = user email, "device:<id>" or "system".
GET /api/v1/users (admin) -> {"users":[{"id":1,"email":"...","role":"admin","created_at":"...","disabled_at":null}]}
POST /api/v1/users {"email","password","role"} (admin; password >= 10 chars; role in admin|operator|viewer) -> 201 {"user":{...}}; 409 conflict. Audit user.created.   POST /api/v1/users/{id}/disable (admin) -> 204. Audit user.disabled.
GET /healthz -> 200 {"ok":true} when the DB answers SELECT 1, else 503.
GET / and any path not under /api or /dl -> embedded SPA (index.html fallback, Cache-Control: no-cache). GET /dl/<file> -> static file from MSP_AGENT_DIR (no directory listing; 404 when unset or missing). Security headers on every response: Content-Security-Policy "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'", X-Content-Type-Options nosniff, X-Frame-Options DENY, Referrer-Policy no-referrer, Strict-Transport-Security max-age=31536000 when serving TLS.

AUDIT ACTIONS (audit_log.action): user.login, user.login_failed, user.created, user.disabled, org.created, token.created, token.revoked, device.enrolled, device.revoked, device.deleted, command.queued, command.finished, command.lost, alert.ack.

SERVER CLI (cmd/msp-server): `msp-server serve`; `msp-server user add --email E --role admin|operator|viewer --password-stdin` (reads password from stdin); `msp-server org add --name N` (prints the new org id as a bare integer); `msp-server token new --org ID --label L --max-uses N --ttl 24h` (prints only the plaintext token); `msp-server version`. Env (flags of the same name override): MSP_DB (./msp.db), MSP_LISTEN (:8080), MSP_BASE_URL (""), MSP_TLS_CERT + MSP_TLS_KEY (serve TLS when both set), MSP_SECURE_COOKIES (0/1), MSP_TRUST_PROXY (0/1: honour first X-Forwarded-For), MSP_AGENT_DIR (""), MSP_RETENTION_DAYS (7).
AGENT CLI (cmd/msp-agent): `msp-agent enroll --server URL --token et_... [--config PATH] [--ca-file PEM] [--interval 30] [--insecure-http]` (enrolls, writes config, exits); `msp-agent run [--config PATH] [--interval N]` (foreground; under the Windows SCM it runs as the service); `msp-agent install --server URL --token et_... [--ca-file] [--interval] [--insecure-http]` (enroll + write config + register and start the systemd unit / Windows service; root/Administrator); `msp-agent uninstall`; `msp-agent status` (prints config path, device id, server); `msp-agent version`. Config file JSON {"server_url","device_token","device_id","interval_s","ca_file","insecure_http"} at /etc/msp-agent/agent.json (Linux) or %ProgramData%\msp-agent\agent.json (Windows), dir 0700 file 0600. Exit codes: 0 ok, 1 runtime error, 2 bad arguments/config.

## DATA_MODEL

SQLite file MSP_DB opened with DSN `file:<path>?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)`, db.SetMaxOpenConns(4). Migrations: internal/store/migrations/NNNN_name.sql embedded, applied in lexical order inside a transaction, recorded in schema_migrations. All timestamps TEXT RFC3339 UTC ("2026-11-13T10:00:01Z"). Integer autoincrement ids. Secrets stored as sha256 hex only. Booleans INTEGER 0/1.

schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)

orgs(id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL UNIQUE, created_at TEXT NOT NULL)

users(id INTEGER PRIMARY KEY AUTOINCREMENT, email TEXT NOT NULL UNIQUE /*lowercased*/, password_hash TEXT NOT NULL /*bcrypt cost 12*/, role TEXT NOT NULL CHECK(role IN ('admin','operator','viewer')), created_at TEXT NOT NULL, disabled_at TEXT NULL)

sessions(token_hash TEXT PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE, created_at TEXT NOT NULL, expires_at TEXT NOT NULL, ip TEXT NOT NULL DEFAULT '')  INDEX sessions_expires(expires_at)

enrollment_tokens(id INTEGER PRIMARY KEY AUTOINCREMENT, org_id INTEGER NOT NULL REFERENCES orgs(id), token_hash TEXT NOT NULL UNIQUE, token_prefix TEXT NOT NULL, label TEXT NOT NULL DEFAULT '', max_uses INTEGER NOT NULL, uses INTEGER NOT NULL DEFAULT 0, expires_at TEXT NOT NULL, created_by INTEGER NULL REFERENCES users(id), created_at TEXT NOT NULL, revoked_at TEXT NULL)  INDEX enrollment_tokens_org(org_id)

devices(id INTEGER PRIMARY KEY AUTOINCREMENT, org_id INTEGER NOT NULL REFERENCES orgs(id), device_uid TEXT NOT NULL, hostname TEXT NOT NULL, platform TEXT NOT NULL CHECK(platform IN ('linux','windows','android')), arch TEXT NOT NULL DEFAULT '', os_version TEXT NOT NULL DEFAULT '', agent_version TEXT NOT NULL DEFAULT '', token_hash TEXT NULL UNIQUE /*NULL = revoked*/, interval_s INTEGER NOT NULL DEFAULT 30, enrolled_at TEXT NOT NULL, last_seen_at TEXT NULL, last_sample_at TEXT NULL, last_ip TEXT NOT NULL DEFAULT '', facts_json TEXT NOT NULL DEFAULT '{}', last_metrics_json TEXT NOT NULL DEFAULT '{}', cpu_pct REAL NULL, mem_used_pct REAL NULL, disk_used_pct REAL NULL /*max over disks*/, battery_pct INTEGER NULL, battery_charging INTEGER NULL, uptime_s INTEGER NULL, UNIQUE(org_id, device_uid))  INDEX devices_org(org_id), INDEX devices_last_seen(last_seen_at)

heartbeats(id INTEGER PRIMARY KEY AUTOINCREMENT, device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE, at TEXT NOT NULL, cpu_pct REAL NULL, mem_used_pct REAL NULL, disk_used_pct REAL NULL, battery_pct INTEGER NULL, uptime_s INTEGER NULL)  INDEX heartbeats_device_at(device_id, at)   -- one row per device per >= 60 s; pruned after MSP_RETENTION_DAYS

commands(id INTEGER PRIMARY KEY AUTOINCREMENT, device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE, org_id INTEGER NOT NULL REFERENCES orgs(id), created_by INTEGER NOT NULL REFERENCES users(id), kind TEXT NOT NULL CHECK(kind IN ('exec','script')), argv_json TEXT NULL, shell TEXT NULL, body TEXT NULL, body_sha256 TEXT NULL, timeout_s INTEGER NOT NULL, status TEXT NOT NULL CHECK(status IN ('queued','sent','done','failed','timeout','lost','cancelled')), created_at TEXT NOT NULL, sent_at TEXT NULL, started_at TEXT NULL, finished_at TEXT NULL, exit_code INTEGER NULL, stdout TEXT NOT NULL DEFAULT '', stderr TEXT NOT NULL DEFAULT '', truncated INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '')  INDEX commands_device_status(device_id, status), INDEX commands_status_sent(status, sent_at), INDEX commands_created(created_at)

alerts(id INTEGER PRIMARY KEY AUTOINCREMENT, org_id INTEGER NOT NULL REFERENCES orgs(id), device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE, type TEXT NOT NULL CHECK(type IN ('offline','disk_full','memory_high','battery_low')), severity TEXT NOT NULL CHECK(severity IN ('low','medium','high')), message TEXT NOT NULL, opened_at TEXT NOT NULL, resolved_at TEXT NULL, acknowledged_by INTEGER NULL REFERENCES users(id), acknowledged_at TEXT NULL)  CREATE UNIQUE INDEX alerts_open ON alerts(device_id, type) WHERE resolved_at IS NULL; INDEX alerts_org_resolved(org_id, resolved_at)

audit_log(id INTEGER PRIMARY KEY AUTOINCREMENT, at TEXT NOT NULL, actor_type TEXT NOT NULL CHECK(actor_type IN ('user','device','system')), actor_id INTEGER NULL, org_id INTEGER NULL, action TEXT NOT NULL, target_type TEXT NOT NULL DEFAULT '', target_id INTEGER NULL, ip TEXT NOT NULL DEFAULT '', detail_json TEXT NOT NULL DEFAULT '{}')  INDEX audit_at(at), INDEX audit_org_at(org_id, at)   -- append-only: the Store exposes only Audit and ListAudit

Alert thresholds (server Config, env-overridable in phase 2): offline = status offline (3*interval_s + 15 s), severity high; disk_full = disk_used_pct >= 90, medium; memory_high = mem_used_pct >= 95, low; battery_low = battery_pct <= 15 and not charging, low. Each alert type has at most one open row per device (partial unique index) and auto-resolves when the condition clears; ack is a flag on the open row.

STORE GO API (package github.com/sxi9/msp/internal/store; every method takes ctx context.Context first; implemented by WP1, consumed by WP2/WP3/WP9 exactly as written):
var ErrNotFound, ErrInvalidToken, ErrConflict error
type Store struct { db *sql.DB; Now func() time.Time /* defaults to time.Now().UTC; tests override */ }
func Open(path string) (*Store, error); func (s *Store) Close() error; func (s *Store) Ping(ctx) error
func HashToken(plain string) string /*sha256 hex*/; func NewToken(prefix string) string /*prefix + base64.RawURLEncoding of 32 random bytes*/; func StatusOf(lastSeen *time.Time, intervalS int, now time.Time) string /*pending|online|offline*/
Types (json tags are exactly the API field names in the protocol section): Org{ID, Name, CreatedAt, DeviceCount, OnlineCount, OpenAlerts}; User{ID, Email, Role, CreatedAt, DisabledAt *time.Time, PasswordHash `json:"-"`}; EnrollmentToken{ID, OrgID, TokenPrefix, Label, MaxUses, Uses, ExpiresAt, CreatedBy *int64, CreatedAt, RevokedAt *time.Time}; Device{ID, OrgID, OrgName, DeviceUID, Hostname, Platform, Arch, OSVersion, AgentVersion, IntervalS, Status, EnrolledAt, LastSeenAt *time.Time, LastIP, Revoked bool, CPUPct *float64, MemUsedPct *float64, DiskUsedPct *float64, BatteryPct *int, BatteryCharging *bool, UptimeS *int64, OpenAlerts int, CanRunCommands bool, FactsJSON `json:"-"`, LastMetricsJSON `json:"-"`}; Heartbeat{ID `json:"-"`, DeviceID `json:"-"`, At, CPUPct *float64, MemUsedPct *float64, DiskUsedPct *float64, BatteryPct *int, UptimeS *int64}; Command{ID, DeviceID, OrgID, CreatedBy, CreatedByEmail, Kind, Argv []string, Shell, Body, TimeoutS, Status, CreatedAt, SentAt/StartedAt/FinishedAt *time.Time, ExitCode *int, Stdout, Stderr, Truncated bool, Error}; Alert{ID, OrgID, OrgName, DeviceID, Hostname, Type, Severity, Message, OpenedAt, ResolvedAt *time.Time, AcknowledgedBy *int64, AcknowledgedAt *time.Time}; AuditEntry{ID, At, ActorType, ActorID *int64, ActorLabel, OrgID *int64, Action, TargetType, TargetID *int64, IP, Detail map[string]any}; DeviceFilter{OrgID *int64; Status, Query string}; Summary{Orgs, Devices, Online, Offline, Pending, OpenAlerts int}; UpsertDeviceParams{OrgID int64; DeviceUID, Hostname, Platform, Arch, OSVersion, AgentVersion string; IntervalS int; FactsJSON string}; CreateCommandParams{DeviceID, OrgID, CreatedBy int64; Kind string; Argv []string; Shell, Body string; TimeoutS int}
Methods:
CreateOrg(ctx, name string) (Org, error) /*ErrConflict*/; ListOrgs(ctx) ([]Org, error) /*with counts using StatusOf*/; GetOrg(ctx, id int64) (Org, error)
CreateUser(ctx, email, passwordHash, role string) (User, error); GetUserByEmail(ctx, email string) (User, error); GetUser(ctx, id int64) (User, error); ListUsers(ctx) ([]User, error); DisableUser(ctx, id int64) error; CountUsers(ctx) (int, error)
CreateSession(ctx, userID int64, ip string, ttl time.Duration) (plain string, err error); GetSession(ctx, plain string) (User, error) /*ErrNotFound when missing, expired or user disabled*/; DeleteSession(ctx, plain string) error; PruneSessions(ctx) (int64, error)
CreateEnrollmentToken(ctx, orgID int64, label string, maxUses int, ttl time.Duration, createdBy *int64) (plain string, tok EnrollmentToken, err error); ListEnrollmentTokens(ctx, orgID int64) ([]EnrollmentToken, error); ConsumeEnrollmentToken(ctx, plain string) (EnrollmentToken, error) /*single UPDATE ... WHERE token_hash=? AND revoked_at IS NULL AND expires_at>now AND uses<max_uses; ErrInvalidToken when 0 rows*/; RevokeEnrollmentToken(ctx, orgID, id int64) error
UpsertDevice(ctx, p UpsertDeviceParams) (dev Device, plainToken string, err error) /*INSERT ... ON CONFLICT(org_id, device_uid) DO UPDATE rotating token_hash*/; GetDeviceByToken(ctx, plain string) (Device, error) /*ErrInvalidToken*/; GetDevice(ctx, id int64) (Device, error); ListDevices(ctx, f DeviceFilter) ([]Device, error) /*status filter applied in Go via StatusOf; order org_name, hostname*/; RecordSync(ctx, deviceID int64, ip string, req protocol.SyncRequest, sampleEvery time.Duration) error /*one tx*/; RevokeDevice(ctx, id int64) error; DeleteDevice(ctx, id int64) error
ListHeartbeats(ctx, deviceID int64, limit int) ([]Heartbeat, error) /*oldest first*/; PruneHeartbeats(ctx, olderThan time.Duration) (int64, error)
CreateCommand(ctx, p CreateCommandParams) (Command, error); ClaimQueuedCommands(ctx, deviceID int64, max int) ([]protocol.Command, error) /*tx: SELECT queued ORDER BY id LIMIT max; UPDATE status='sent', sent_at*/; HasQueuedCommands(ctx, deviceID int64) (bool, error); GetCommand(ctx, id int64) (Command, error); ListCommands(ctx, deviceID int64, limit int) ([]Command, error) /*newest first*/; CompleteCommand(ctx, id, deviceID int64, res protocol.CommandResult) (status string, err error) /*ErrNotFound unless device matches; ErrConflict unless status=sent*/; MarkLostCommands(ctx, grace time.Duration) ([]Command, error)
OpenAlert(ctx, orgID, deviceID int64, typ, severity, message string) (created bool, err error) /*INSERT OR IGNORE against alerts_open*/; ResolveAlert(ctx, deviceID int64, typ string) error; ListAlerts(ctx, openOnly bool, orgID *int64, limit int) ([]Alert, error); AckAlert(ctx, id, userID int64) error; CountOpenAlerts(ctx) (int, error)
Audit(ctx, e AuditEntry) error /*At defaults to Now()*/; ListAudit(ctx, limit int, orgID *int64, beforeID int64) ([]AuditEntry, error)
Summary(ctx) (Summary, error)

## ANDROID_APPROACH

Decision: a separate, dependency-free Kotlin app that speaks the protocol; no Go code on Android (gomobile needs the NDK, cgo and a 10+ MB AAR; Android cannot read /proc/stat nor run operator shell commands, so the runner never applies). What is shared is the contract: docs/PROTOCOL.md and internal/protocol/fixtures/*.json. ProtocolTest.kt (JVM unit test, no device) builds enroll/sync JSON and checks key sets and JSON types against enroll_android.json / sync_android.json, and parses sync_response.json; the Go side round-trips the same files, so a shape change breaks both suites until both agents are updated. Phase 1 Android scope: inventory + sync (metrics, battery) only; the server never queues commands for platform android and the dashboard hides the console (can_run_commands=false).

App (package com.sxi9.msp.agent, ~550 lines Kotlin + XML + Gradle):
- Gradle: settings.gradle.kts (pluginManagement google()/mavenCentral()/gradlePluginPortal(); dependencyResolutionManagement google()/mavenCentral(); rootProject.name="msp-agent"; include(":app")); root build.gradle.kts plugins com.android.application 8.7.3 and org.jetbrains.kotlin.android 2.0.21 apply false; gradle.properties: android.useAndroidX=true, org.gradle.jvmargs=-Xmx2g -Dfile.encoding=UTF-8, kotlin.compiler.execution.strategy=in-process (the out-of-process Kotlin daemon fails in this container), kotlin.code.style=official; app/build.gradle.kts: namespace/applicationId com.sxi9.msp.agent, compileSdk 35, buildToolsVersion "35.0.0", minSdk 26, targetSdk 35, versionCode 1, versionName from ../../VERSION, Java 17 source/target + jvmTarget 17, isMinifyEnabled=false, testOptions.unitTests.isReturnDefaultValues=true and a system property MSP_FIXTURES=${rootDir}/../internal/protocol/fixtures for tests, dependencies only testImplementation junit:junit:4.13.2 and org.json:json:20240303 (all already in ~/.gradle/caches; no download beyond what is cached).
- Manifest: INTERNET, FOREGROUND_SERVICE, FOREGROUND_SERVICE_SPECIAL_USE, RECEIVE_BOOT_COMPLETED, POST_NOTIFICATIONS, REQUEST_IGNORE_BATTERY_OPTIMIZATIONS; application android:networkSecurityConfig="@xml/network_security_config" (main: cleartextTrafficPermitted="false"; src/debug overlay: "true" so lab/http servers work with the debug APK only); MainActivity exported launcher; AgentService exported=false, android:foregroundServiceType="specialUse" with <property android:name="android.app.PROPERTY_SPECIAL_USE_FGS_SUBTYPE" android:value="IT management agent reporting device health to the device owner's IT provider"/> (dataSync is capped at 6 h/day on Android 15); BootReceiver exported=true for BOOT_COMPLETED.
- MainActivity: LinearLayout with server URL EditText (default https://), enrollment token EditText, buttons Enroll / Start / Stop / Allow background (ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS with package: URI), status TextView (device id, last sync result from Prefs). Requests POST_NOTIFICATIONS on API 33+. Enroll runs on a Thread: ApiClient.enroll -> Prefs(server_url, device_token, device_id, interval_s=60) -> startForegroundService(AgentService).
- AgentService: notification channel "msp_agent" (IMPORTANCE_LOW), startForeground(1, notification, FOREGROUND_SERVICE_TYPE_SPECIAL_USE) on API 34+, plain startForeground below; HandlerThread loop: facts on first run and every 120th (PROTOCOL.md 3.3); build sync JSON with wait_s 0 and interval_s 60; POST; update notification text "Last sync HH:mm"; schedule next after poll_interval_s clamped 15..600; IOException -> backoff 30 s doubling to 600 s; 401 -> notification "Token rejected — open app to re-enroll" and retry every 5 min; START_STICKY; onDestroy quits the thread.
- Metrics.kt: cpu_pct = JSONObject.NULL; mem_total_mb/mem_used_mb from ActivityManager.MemoryInfo (used = total - avail); disks = [{"mount":"/data","total_gb","used_gb"}] from StatFs(Environment.getDataDirectory()) one decimal; battery_pct from BatteryManager.getIntProperty(BATTERY_PROPERTY_CAPACITY), battery_charging from BatteryManager.isCharging; ips from NetworkInterface (IPv4, non-loopback); hostname = Settings.Global.getString(resolver, "device_name") ?: Build.MODEL; device_uid = Settings.Secure.ANDROID_ID; os_version = "Android ${Build.VERSION.RELEASE} (${Build.VERSION.SDK_INT})"; arch from Build.SUPPORTED_ABIS[0] (arm64-v8a->arm64, x86_64->amd64, else "arm"); facts {kernel: System.getProperty("os.version"), cpu_model: Build.HARDWARE, cpu_count: Runtime.availableProcessors(), mem_total_mb, manufacturer: Build.MANUFACTURER, model: Build.MODEL, serial: "", ips}.
- Protocol.kt: pure Kotlin (no android.* imports): buildEnrollJson(...), buildSyncJson(...), parseEnrollResponse(String): EnrollResult(deviceId: Long, deviceToken: String, pollIntervalS: Int), parseSyncResponse(String): Int (poll_interval_s; commands ignored), parseError(String): String.
- ApiClient.kt: HttpURLConnection, 15 s connect/read timeouts, Content-Type/Accept application/json, User-Agent msp-agent/0.1.0 (android/<arch>), Authorization Bearer; throws UnauthorizedException on 401, IOException otherwise. System trust store only: production needs a publicly trusted certificate (Caddy/Let's Encrypt); self-signed CAs must be installed as user certificates.
- Prefs.kt: SharedPreferences "msp_agent" MODE_PRIVATE (app-private storage; Keystore wrapping is phase 2).
- Build/verify in container: `cd android && ANDROID_HOME=/opt/android-sdk gradle --no-daemon --console=plain :app:assembleDebug :app:testDebugUnitTest`; APK at android/app/build/outputs/apk/debug/app-debug.apk (debug keystore exists at ~/.android/debug.keystore); `/opt/android-sdk/build-tools/35.0.0/aapt dump badging ...` proves package, sdk versions, permissions and the service. No emulator/instrumentation tests. Doze: a specialUse foreground service survives Doze but networking may be deferred; the 60 s interval with a 195 s online window plus the battery-optimization exemption keeps most phones from flapping; residual flapping is accepted and documented.

## BUILD_AND_TEST

Verified container facts (2026-11-13): go1.24.7 linux/amd64; node v22.23.4 / npm 10.9.9 (node --test on .ts files works natively, verified with a sample); Gradle 8.14.3 on OpenJDK 21.0.12 with proxy settings in JAVA_TOOL_OPTIONS; ~/.gradle/caches already holds AGP 8.7.3, Kotlin 2.0.21, kotlin-stdlib 2.0.21, junit 4.13.2, org.json 20240303; /opt/android-sdk has platforms/android-35, build-tools/34.0.0 and 35.0.0, cmdline-tools, platform-tools (ANDROID_HOME is NOT set: export it); npm registry reachable (esbuild 0.28.2, preact 10.29.8, typescript 5.9.3 resolve); proxy.golang.org reachable (modernc.org/sqlite v1.62.0, x/sys v0.50.0, x/crypto v0.59.0); jq, curl, systemd-analyze present; docker CLI present, daemon DOWN; 4 CPUs, 15 GB RAM; hostname "vm". Clone the repo into its own empty directory: R=/home/user/msp (the cwd /home/user/vault is unrelated). CGO is never needed.

GO (server + desktop agent + protocol + store + in-process e2e)
  cd $R && go mod download
  cd $R && go vet ./... && CGO_ENABLED=0 go build ./...
  cd $R && go test ./... -count=1 -p 4                                 # includes internal/e2e (httptest server + real agent.Run + runner)
  cd $R && go test -race ./internal/server ./internal/agent ./internal/e2e -count=1
  cd $R && CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=$(cat VERSION)" -o bin/msp-server ./cmd/msp-server
  cd $R && CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=$(cat VERSION)" -o bin/msp-agent  ./cmd/msp-agent
  (= `make build`; `make test` runs vet + go test; `make race`)

CROSS-COMPILE (= `make agents` = scripts/build-agents.sh; proves Windows/arm64/android code compiles; Windows cannot execute here)
  cd $R && CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.version=$(cat VERSION)" -o dist/msp-agent-linux-amd64 ./cmd/msp-agent
  cd $R && CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -trimpath -ldflags "-s -w -X main.version=$(cat VERSION)" -o dist/msp-agent-linux-arm64 ./cmd/msp-agent
  cd $R && CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.version=$(cat VERSION)" -o dist/msp-agent-windows-amd64.exe ./cmd/msp-agent
  cd $R && CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build -o /dev/null ./cmd/msp-agent        # compile-only proof of the !linux&&!windows stubs
  cd $R && CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.version=$(cat VERSION)" -o dist/msp-server-linux-amd64 ./cmd/msp-server
  cd $R && GOOS=windows go vet ./internal/agent/ ./cmd/msp-agent/                                 # type-checks the windows build tag
  cd $R/dist && sha256sum * > SHA256SUMS && cp ../deploy/install.sh ../deploy/install.ps1 . ; [ -n "$MSP_AGENT_DIR" ] && cp -r $R/dist/. "$MSP_AGENT_DIR"/

WEB (= `make web`)
  cd $R/web && npm ci
  cd $R/web && npm test                      # node --test src/lib/*.test.ts  (native TS stripping; tests use only erasable syntax and .ts import extensions)
  cd $R/web && npm run typecheck             # tsc --noEmit
  cd $R/web && npm run build                 # node build.mjs -> dist/app.js + dist/app.css (< 150 KB together)
  cd $R && make build                        # rebuild server so go:embed picks up the bundle

ANDROID (= `make android` / `make android-test`)
  cd $R/android && ANDROID_HOME=/opt/android-sdk ANDROID_SDK_ROOT=/opt/android-sdk gradle --no-daemon --console=plain :app:assembleDebug :app:testDebugUnitTest
  ls -l $R/android/app/build/outputs/apk/debug/app-debug.apk
  /opt/android-sdk/build-tools/35.0.0/aapt dump badging $R/android/app/build/outputs/apk/debug/app-debug.apk | grep -E "package:|sdkVersion|uses-permission|service"
  (first run 2-4 min; all plugin/library versions are already in the Gradle cache)

OPS CHECKS
  cd $R && docker compose -f deploy/docker-compose.yml config > /dev/null      # daemon not required
  cd $R && systemd-analyze verify deploy/msp-agent.service || true            # warns only; container has no systemd
  cd $R && bash -n deploy/install.sh scripts/e2e.sh scripts/build-agents.sh
  (docker build / compose up only where a daemon exists: `docker build -f deploy/Dockerfile -t msp-server:dev .`)

END-TO-END PROOF WITH REAL BINARIES (= `make e2e` = scripts/e2e.sh; exit 0 and the line "E2E OK" are the verdict; total run ~90 s)
  export R=/home/user/msp; T=$(mktemp -d); P=18080; B=http://127.0.0.1:$P; H='X-Requested-With: msp'; J='Content-Type: application/json'
  cd $R && make build
  export MSP_DB=$T/msp.db MSP_LISTEN=127.0.0.1:$P MSP_BASE_URL=$B MSP_AGENT_DIR=$R/dist
  echo 'admin-pass-123' | $R/bin/msp-server user add --email admin@example.com --role admin --password-stdin
  ORG=$($R/bin/msp-server org add --name "Acme Dental")                                   # prints 1
  ET=$($R/bin/msp-server token new --org $ORG --label e2e --max-uses 5 --ttl 24h)         # prints et_...
  $R/bin/msp-server serve > $T/server.log 2>&1 & SRV=$!; trap 'kill $AGT $SRV 2>/dev/null; cat $T/server.log' EXIT
  until curl -sf $B/api/agent/v1/ping | jq -e '.ok==true' >/dev/null; do sleep 0.5; done
  $R/bin/msp-agent enroll --server $B --token "$ET" --config $T/agent.json --interval 2    # loopback => http allowed
  $R/bin/msp-agent run --config $T/agent.json > $T/agent.log 2>&1 & AGT=$!
  curl -sf -c $T/jar -H "$H" -H "$J" -d '{"email":"admin@example.com","password":"admin-pass-123"}' $B/api/v1/login | jq -e '.user.role=="admin"'
  for i in $(seq 1 20); do DEV=$(curl -sf -b $T/jar "$B/api/v1/devices" | jq -r '.devices[] | select(.status=="online" and .platform=="linux") | .id'); [ -n "$DEV" ] && break; sleep 1; done; test -n "$DEV"
  curl -sf -b $T/jar "$B/api/v1/devices/$DEV" | jq -e --arg h "$(hostname)" '.device.hostname==$h and .device.can_run_commands==true and .metrics.mem_total_mb>0'
  S=$(date +%s%N); CMD=$(curl -sf -b $T/jar -H "$H" -H "$J" -d '{"kind":"exec","argv":["uname","-a"],"timeout_s":30}' $B/api/v1/devices/$DEV/commands | jq -r .command.id)
  for i in $(seq 1 30); do ST=$(curl -sf -b $T/jar $B/api/v1/commands/$CMD | jq -r .command.status); [ "$ST" = done ] && break; sleep 0.2; done
  echo "round trip $(( ($(date +%s%N)-S)/1000000 )) ms"; curl -sf -b $T/jar $B/api/v1/commands/$CMD | jq -e '.command.status=="done" and .command.exit_code==0 and (.command.stdout|startswith("Linux"))'
  # injection check: argv with shell metacharacters is literal
  CMD=$(curl -sf -b $T/jar -H "$H" -H "$J" -d '{"kind":"exec","argv":["echo","a;id"],"timeout_s":30}' $B/api/v1/devices/$DEV/commands | jq -r .command.id); (poll as above) ; curl -sf -b $T/jar $B/api/v1/commands/$CMD | jq -e '.command.stdout=="a;id\n"'
  # script kind
  CMD=$(curl -sf -b $T/jar -H "$H" -H "$J" -d '{"kind":"script","shell":"sh","body":"printf hello\n","timeout_s":30}' $B/api/v1/devices/$DEV/commands | jq -r .command.id); (poll) ; ... jq -e '.command.stdout=="hello"'
  # timeout
  CMD=$(curl -sf -b $T/jar -H "$H" -H "$J" -d '{"kind":"exec","argv":["sleep","5"],"timeout_s":1}' $B/api/v1/devices/$DEV/commands | jq -r .command.id); poll up to 10 s for status=="timeout"; jq -e '.command.exit_code==-1 and (.command.error|startswith("timeout"))'
  # android refusal: enroll a fake android device directly, then try to queue a command
  AID=$(curl -sf -H "$J" -d "{\"enrollment_token\":\"$ET\",\"device_uid\":\"android-e2e\",\"hostname\":\"Pixel 8\",\"platform\":\"android\",\"arch\":\"arm64\",\"os_version\":\"Android 14 (34)\",\"agent_version\":\"0.1.0\",\"interval_s\":60,\"facts\":{}}" $B/api/agent/v1/enroll | jq -r .device_id)
  test "$(curl -s -o /dev/null -w '%{http_code}' -b $T/jar -H "$H" -H "$J" -d '{"kind":"exec","argv":["id"]}' $B/api/v1/devices/$AID/commands)" = 400
  # csrf + role checks
  test "$(curl -s -o /dev/null -w '%{http_code}' -b $T/jar -H "$J" -d '{"kind":"exec","argv":["id"]}' $B/api/v1/devices/$DEV/commands)" = 403      # missing X-Requested-With
  # audit trail distinguishes user and device actors
  curl -sf -b $T/jar "$B/api/v1/audit?limit=200" | jq -e '[.entries[]|select(.action=="command.queued" and .actor_type=="user")]|length>=4' ; curl -sf -b $T/jar "$B/api/v1/audit?limit=200" | jq -e '[.entries[]|select(.action=="command.finished" and .actor_type=="device")]|length>=4 and ([.entries[]|select(.action=="device.enrolled")]|length>=2)'
  # dashboard shell served
  curl -sf $B/ | grep -q app.js
  # offline flip: window is 3*2+15 = 21 s; evaluator ticks every 15 s
  kill $AGT; sleep 25; curl -sf -b $T/jar "$B/api/v1/devices/$DEV" | jq -e '.device.status=="offline"'
  for i in $(seq 1 20); do curl -sf -b $T/jar "$B/api/v1/alerts?state=open" | jq -e --argjson d $DEV '[.alerts[]|select(.device_id==$d and .type=="offline")]|length==1' >/dev/null && break; sleep 1; done
  kill $SRV; echo "E2E OK"
Manual visual check: with `make web` done, open http://127.0.0.1:18080/ in a browser, log in as admin@example.com, see the Fleet page grouped by "Acme Dental" with the container host online, open it, run `uname -a` from the console.

CI (.github/workflows/ci.yml, ubuntu-latest): job go (actions/setup-go 1.24, go vet, go test -race, scripts/build-agents.sh, upload dist/); job web (setup-node 22, npm ci, npm test, npm run typecheck, npm run build, upload web/dist); job android (setup-java 17 temurin, android-actions/setup-android@v3 with packages platforms;android-35 build-tools;35.0.0, gradle --no-daemon :app:assembleDebug :app:testDebugUnitTest, upload APK); job e2e (setup-go, make build, scripts/e2e.sh, then sudo ./bin/msp-agent install --server http://127.0.0.1:18080 --token ... --insecure-http against a running server and `systemctl is-active msp-agent`).

## RISKS

1) Windows and Android cannot be executed in this container. Windows code is gated only by GOOS=windows vet/build with arithmetic kept in Linux-tested helpers (cpuPct, pctRound) and the service handler isolated in one file; expect one round of fixes on a real Windows VM (struct sizes, registry keys, SCM behaviour) before client rollout. Android is verified by APK build + JVM protocol tests only; the first phone install will surface UI/permission issues. Pilot order: Linux, then one Windows box, then phones.
2) Android background execution: Doze and OEM battery killers (Samsung, Xiaomi, Huawei) can defer the specialUse service's network loop; the 195 s online window and the battery-optimisation exemption absorb most of it, but false offline alerts on phones will happen. Phase 2: WorkManager fallback and a per-org mobile offline threshold. If the app is ever published on Play, the specialUse subtype justification must be reviewed.
3) Any operator/admin account is root/SYSTEM on every client machine by design. Phase-1 mitigations: bcrypt, SameSite=Strict + custom header CSRF, login/enroll rate limits, three roles, append-only audit with actor_type and script hashes, Secure cookies behind TLS. Still required before scaling to many clients: MFA, command signing, dashboard IP allowlisting (docs/SECURITY.md states this plainly).
4) Claim-on-fetch command delivery: a sync response lost between server and agent leaves the command in status sent until the lost sweeper (sent_at + timeout_s + 5 min). No command is ever executed twice; a lost one must be re-queued by staff. An ack step is phase 2.
5) Long-poll through corporate proxies/load balancers: wait_s is capped at 25 s, the agent client timeout is 40 s, and poll_interval_s=5 plus heartbeat-carried commands are the fallback, so worst case is one interval of latency, not loss. Each parked sync is one goroutine and one socket; at 5,000 devices that is ~50-100 MB RAM and well within one box.
6) SQLite single writer: every sync does one UPDATE (plus one INSERT per 60 s) inside one transaction; busy_timeout 5000 and WAL are mandatory from the first commit or agents will see 5xx under load. Heartbeats grow ~1,440 rows/device/day; the hourly pruner and MSP_RETENTION_DAYS are not optional. Postgres is a contained phase-2 change because all SQL lives in internal/store.
7) Build fragility in the container: npm ci must download esbuild's platform binary and preact (registry.npmjs.org bypasses the proxy, verified); the first Gradle run uses only cached AGP 8.7.3/Kotlin 2.0.21 artefacts but any version drift triggers multi-hundred-MB proxy downloads; the Kotlin out-of-process daemon fails here (in-process pinned); Docker has no daemon, so the Dockerfile is only validated in CI.
8) Parallel-WP integration hazards (all mitigated by frozen names in the shared sections): WP3 relies on WP2's unexported helper names in the same package; WP5 must match WP4's per-platform contract names; WP9's in-process test needs server.EvaluateNow from WP2; package-lock.json must be committed by WP6 or the Dockerfile's npm ci fails. WP9 owns the integration run and reports drift to owners rather than editing their files.
9) Enrollment token leakage lets an attacker enroll rogue devices into that org until max_uses/expiry; defaults (10 uses, 7 days) and the revoke button limit the window; device tokens are per device and bounded to that device's own sync/result calls (they cannot read or command anything).
10) Windows timeout kills only the direct child (no job object), so a PowerShell script that spawns a long-running process can leave it running; Linux uses process groups. Job objects are phase 2.
11) Secrets at rest: agent.json (0600 root), ProgramData ACLs, app-private SharedPreferences — anyone with root/admin on the endpoint can impersonate only that device. Keystore wrapping on Android is phase 2.
12) Self-inflicted outages: operators can run scripts that break networking or the agent; the audit log and captured output are the only safety nets. Script library with review is phase 2.
13) Scope creep magnets: Device page sparklines/presets and the Org enrollment dialog in WP6; Fleet + Device console ship first, Alerts/Orgs/Audit/Users in that order so a partial WP6 is still demoable.
14) Privacy/compliance: hostnames, IPs and command output of client machines live in the MSP's database; retention is 7 days for metrics and unbounded for commands/audit; the MSP needs a data-processing clause with clients.

## CUT_LIST

NOT built in phase 1 (do not add): WebSocket/SSE/gRPC/MQTT or any persistent agent connection beyond the 25 s long-poll; dashboard live push (polling only); Postgres implementation (SQL isolated for it); command acknowledgement/re-delivery protocol and command cancellation after sent (status exists, no UI/agent support); Windows job-object process-tree kill; device token rotation with overlap (revoke + re-enroll only); argon2id; mTLS; command signing; MFA/TOTP, SSO/OIDC, LDAP, password reset emails, invitations; client-org (org-scoped) user logins and per-org RBAC (all staff see all orgs; roles admin/operator/viewer only); per-org or editable alert thresholds, cpu_high and consecutive-breach counters, maintenance windows, email/Slack/SMS/webhook notifications; metric rollups, charts beyond the 120-sample sparklines, reports, SLA/uptime, exports, client portal; scheduled/recurring scripts, script library, bulk run across devices, file transfer, remote desktop/VNC/interactive terminal; patch/update management, software inventory, services/process lists, event log/syslog collection, SNMP/printer/network monitoring; agent auto-update, signed binaries/code signing, MSI/EXE installer (install subcommand + install.sh/ps1 are the installers), macOS/iOS agents, Windows on ARM, non-systemd Linux; Android command execution of any kind, MDM/device-owner features (lock, wipe, app install), QR enrollment, WorkManager fallback, EncryptedSharedPreferences/Keystore wrapping, release signing, Play distribution, instrumentation/emulator tests; ACME/autocert in the server (Caddy example instead), API keys/public REST API, PSA/ticketing integrations, Prometheus/OpenTelemetry, Kubernetes/Helm, multi-node/HA server, audit export/SIEM; Gradle wrapper jar in the repo; i18n, accessibility audit, theming beyond prefers-color-scheme. Phase 2 (next session set): Postgres + Notifier via LISTEN/NOTIFY, command ack, token rotation, MFA, alert thresholds per org + email/webhooks, metric rollups, QR enrollment, WorkManager fallback, agent self-update with signed binaries, Windows job objects, org-scoped client logins. Phase 3: Android device-owner/MDM mode, patch management and software deployment, remote shell session, macOS agent, client portal.

## AMENDMENT A1 (overrides anything above that conflicts): BUILT-IN COMMAND KINDS, INCLUDING ANDROID, AND REMOTE LOCK

Command kinds are: "exec", "script", "lock", "ping", "message".
- exec / script: desktop only (platform linux|windows). Shape exactly as in the protocol above.
- lock: all platforms. Shape {"id":9,"kind":"lock","timeout_s":30}. No argv, no body.
- ping: all platforms. {"id":10,"kind":"ping","timeout_s":30}. Agent replies exit_code 0, stdout "pong".
- message: all platforms. {"id":11,"kind":"message","text":"Please call the helpdesk","timeout_s":30}. Desktop: Linux runs notify-send if present else writes to the result with exit 0; Windows: msg.exe via the console session (best effort). Android: shows a notification with the text.

SERVER RULES (WP2 store/validation, WP3 dashboard API):
- commands table gets a `kind` CHECK IN ('exec','script','lock','ping','message') and an optional `text` column for message.
- POST /api/v1/devices/{id}/commands validates kind per platform: exec/script rejected with 400 {"error":"unsupported_on_platform"} when the device platform is android. lock/ping/message accepted for every platform.
- Role: viewer can never create commands. operator and admin can. lock additionally writes audit action "device.lock_requested" with actor user id.
- Tenant isolation: the device must belong to the technician's visible org set (the existing org scoping rule); lock on a device outside it is 404 like any other device access. There is NO endpoint that can lock a device that is not enrolled under this server's tenant tree.
- Android sync: REPLACE "if platform android respond immediately with commands []" with: Android (wait_s is 0) is served the same ClaimQueuedCommands(device, 10) without blocking, so queued lock/ping/message reach it on its next sync. The result endpoint works identically.
- Store: ClaimQueuedCommands returns the kind and text fields; the agent-facing JSON includes "text" only for message.

DESKTOP AGENT (WP4 runner, WP5 windows files):
- runner dispatches on kind. exec/script as specified. Unknown kind -> result exit_code -1, error "unsupported kind: X".
- lock on Linux: run argv ["loginctl","lock-sessions"]; if that exits non-zero try ["xdg-screensaver","lock"]; report the first that succeeds, else failed.
- lock on Windows: the agent runs as a service in session 0, where LockWorkStation does nothing. Implement in runner_windows.go: find the active console session (WTSGetActiveConsoleSessionId), WTSQueryUserToken for it, CreateProcessAsUser running `rundll32.exe user32.dll,LockWorkStation` with CREATE_NO_WINDOW; if no user is logged on return exit 0 with stdout "no interactive session". When not running as a service (dev mode), call user32 LockWorkStation directly. Use golang.org/x/sys/windows only.
- ping and message as described above.

ANDROID (WP7): REPLACE the cut-list line "Android command execution of any kind" with: Android supports exactly the kinds lock, ping, message and rejects everything else with exit_code -1, error "unsupported kind". lock uses DevicePolicyManager.lockNow() through a DeviceAdminReceiver (android.app.action.DEVICE_ADMIN_ENABLED, res/xml/device_admin.xml with <force-lock/>). The enrollment screen has a "Grant device admin" button (ACTION_ADD_DEVICE_ADMIN intent) and shows whether admin is active; if lock is requested without admin, result is exit_code 1, error "device admin not granted". message shows a high-importance notification. Each sync processes returned commands sequentially and POSTs results before the next sync.

DASHBOARD (WP6): Device page shows a "Lock device" button (operator/admin only, hidden for viewer and for devices with status pending) with an in-page confirm step (no window.confirm), which POSTs kind lock, and a "Send message" input. Run command box stays desktop-only; on Android devices it is replaced with the lock/ping/message controls. Command history renders kind as a chip.

DOCS: docs/PROTOCOL.md (WP0) documents all five kinds and the per-platform rules above. docs/SECURITY.md (WP8) has a section "Remote lock scope" restating: only enrolled devices in the technician's org tree, never free-text, audited.

FUTURE (not built now, keep names stable): remote shell over WebSocket relay (phase 2), remote desktop via a self-hosted branded RustDesk relay/ID server and client, with devices.rustdesk_id reserved as a nullable TEXT column added NOW in the schema so phase 3 needs no migration.
