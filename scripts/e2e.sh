#!/usr/bin/env bash
# scripts/e2e.sh — end-to-end proof with the real binaries (= `make e2e`).
#
# Builds bin/msp-server and bin/msp-agent, bootstraps an admin, an org and an
# enrollment token through the CLI, starts the server on 127.0.0.1:$MSP_E2E_PORT
# (default 18080), enrolls and runs the real Linux agent against it, then drives
# the dashboard JSON API with curl + jq exactly as a technician's browser would:
# device online, uname -a round trip (latency printed), shell-metacharacter
# injection check, script kind, timeout, android refusal, CSRF and role checks,
# audit trail, dashboard shell, remote desktop (RD1: the relay is configured on the
# server and a stub `rustdesk` binary on the agent's PATH answers --get-id, so the
# real agent reports the id and the Connect endpoint answers the rustdesk:// URL),
# and the offline flip with its alert.
#
# Every step prints PASS/FAIL. Exit code 0 and the final line "E2E OK" are the
# verdict; on failure both logs are printed. Requires curl and jq.
set -euo pipefail

R=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
P=${MSP_E2E_PORT:-18080}
B=http://127.0.0.1:$P
H='X-Requested-With: msp'
J='Content-Type: application/json'
ADMIN_EMAIL=admin@example.com
ADMIN_PASS=admin-pass-123
VIEWER_EMAIL=viewer@example.com
VIEWER_PASS=viewer-pass-123

for tool in curl jq; do
	command -v "$tool" >/dev/null 2>&1 || { echo "e2e: $tool is required" >&2; exit 2; }
done

T=$(mktemp -d)
SRV=""
AGT=""
DEV=""

cleanup() {
	local rc=$?
	[ -n "$AGT" ] && kill "$AGT" 2>/dev/null || true
	[ -n "$SRV" ] && kill "$SRV" 2>/dev/null || true
	wait 2>/dev/null || true
	if [ "$rc" -ne 0 ]; then
		echo "--- server.log"; cat "$T/server.log" 2>/dev/null || true
		echo "--- agent.log";  cat "$T/agent.log"  2>/dev/null || true
		echo "E2E FAILED (exit $rc)"
	fi
	rm -rf "$T"
	exit "$rc"
}
trap cleanup EXIT

# step NAME COMMAND...: runs COMMAND, prints PASS/FAIL NAME; a failure ends the run.
step() {
	local name=$1; shift
	if "$@"; then
		echo "PASS $name"
	else
		echo "FAIL $name"
		return 1
	fi
}

api() { curl -sf -b "$T/jar" "$@"; }                       # GET as the logged-in admin
api_post() { curl -sf -b "$T/jar" -H "$H" -H "$J" "$@"; }  # mutating call with CSRF header

# http_code METHOD-ARGS...: prints only the status code (never fails the step by itself)
http_code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }

# queue_cmd JSON [DEVICE_ID]: queues a command, prints its id
queue_cmd() {
	api_post -d "$1" "$B/api/v1/devices/${2:-$DEV}/commands" | jq -r '.command.id'
}

# poll_cmd ID STATUS TIMEOUT_S: waits until command ID has STATUS; any other
# terminal status, or the deadline, fails.
poll_cmd() {
	local id=$1 want=$2 timeout=$3 i st=""
	for ((i = 0; i < timeout * 5; i++)); do
		st=$(api "$B/api/v1/commands/$id" | jq -r '.command.status')
		[ "$st" = "$want" ] && return 0
		case "$st" in
		done | failed | timeout | lost | cancelled)
			echo "command $id ended as $st, wanted $want" >&2
			api "$B/api/v1/commands/$id" | jq -c '.command' >&2
			return 1
			;;
		esac
		sleep 0.2
	done
	echo "command $id still $st after ${timeout}s, wanted $want" >&2
	return 1
}

# run_cmd JSON STATUS TIMEOUT_S: queue + poll; prints the command id
run_cmd() {
	local id
	id=$(queue_cmd "$1")
	[ -n "$id" ] && [ "$id" != null ] || { echo "queue failed for $1" >&2; return 1; }
	poll_cmd "$id" "$2" "$3" || return 1
	echo "$id"
}

echo "e2e: repo $R, port $P, tmp $T"

# --- build ------------------------------------------------------------------
step "make build" make -C "$R" build

# --- bootstrap via CLI ------------------------------------------------------
export MSP_DB=$T/msp.db MSP_LISTEN=127.0.0.1:$P MSP_BASE_URL=$B MSP_AGENT_DIR=$R/dist
# Remote desktop (RD1): a relay is configured so agents learn it and Connect works.
export MSP_RUSTDESK_HOST=rmm.example.com MSP_RUSTDESK_KEY=OeVuKk5nlHiXp+7qsR6CbcHBGxhU4PJkNbvoX3P5X0k=
RD_ID=123456789

bootstrap() {
	echo "$ADMIN_PASS" | "$R/bin/msp-server" user add --email "$ADMIN_EMAIL" --role admin --password-stdin >/dev/null &&
		ORG=$("$R/bin/msp-server" org add --name "Acme Dental") &&
		[ "$ORG" = 1 ] &&
		ET=$("$R/bin/msp-server" token new --org "$ORG" --label e2e --max-uses 5 --ttl 24h) &&
		[ "${ET#et_}" != "$ET" ]
}
step "bootstrap admin, org, token via CLI" bootstrap

# --- server -----------------------------------------------------------------
"$R/bin/msp-server" serve >"$T/server.log" 2>&1 &
SRV=$!
wait_ping() {
	local i
	for i in $(seq 1 40); do
		curl -sf "$B/api/agent/v1/ping" | jq -e '.ok==true' >/dev/null 2>&1 && return 0
		kill -0 "$SRV" 2>/dev/null || { echo "server exited early" >&2; return 1; }
		sleep 0.5
	done
	return 1
}
step "server answers ping" wait_ping
step "healthz ok" sh -c "curl -sf $B/healthz | jq -e '.ok==true' >/dev/null"

# --- agent ------------------------------------------------------------------
# A stub RustDesk client on the agent's PATH: the real agent runs `rustdesk --get-id`
# when it collects facts, so the device reports the id without RustDesk being installed.
mkdir -p "$T/stub"
printf '#!/bin/sh\ncase "$1" in --get-id) echo %s ;; *) exit 2 ;; esac\n' "$RD_ID" >"$T/stub/rustdesk"
chmod 0755 "$T/stub/rustdesk"
# A stub apt-get on the agent's PATH (PM1): the real agent's patch scan/install
# runs against this fake, so the e2e never touches the container's packages. A
# simulated dist-upgrade lists two security updates and one regular; any install
# prints a success log. dist-upgrade is matched first (it also contains "upgrade").
cat >"$T/stub/apt-get" <<'APT'
#!/bin/sh
case "$*" in
  *dist-upgrade*)
    echo "Inst libssl3 [3.0.2-0ubuntu1.18] (3.0.2-0ubuntu1.19 Ubuntu:24.04/noble-security [amd64])"
    echo "Inst openssl [3.0.2-0ubuntu1.18] (3.0.2-0ubuntu1.19 Ubuntu:24.04/noble-security [amd64])"
    echo "Inst curl [8.5.0-2ubuntu10.1] (8.5.0-2ubuntu10.6 Ubuntu:24.04/noble-updates [amd64])"
    exit 0 ;;
  *install*|*upgrade*)
    echo "fake apt-get: upgraded the requested packages"
    exit 0 ;;
esac
exit 0
APT
chmod 0755 "$T/stub/apt-get"
# Loopback host => plain http is allowed without --insecure-http.
step "agent enroll" env PATH="$T/stub:$PATH" "$R/bin/msp-agent" enroll --server "$B" --token "$ET" --config "$T/agent.json" --interval 2
step "agent config holds the relay from the enroll response" \
	sh -c "grep -q '\"rustdesk_host\": \"rmm.example.com\"' '$T/agent.json' && grep -q '\"rustdesk_key\": \"$MSP_RUSTDESK_KEY\"' '$T/agent.json'"
PATH="$T/stub:$PATH" "$R/bin/msp-agent" run --config "$T/agent.json" >"$T/agent.log" 2>&1 &
AGT=$!

# --- login ------------------------------------------------------------------
login() {
	curl -sf -c "$T/jar" -H "$H" -H "$J" -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASS\"}" "$B/api/v1/login" |
		jq -e '.user.role=="admin"' >/dev/null
}
step "login as admin" login

# --- device online ----------------------------------------------------------
wait_online() {
	local i
	for i in $(seq 1 20); do
		DEV=$(api "$B/api/v1/devices" | jq -r '.devices[] | select(.status=="online" and .platform=="linux") | .id' | head -n1)
		[ -n "$DEV" ] && return 0
		sleep 1
	done
	return 1
}
step "linux device online" wait_online
echo "device id $DEV"

device_detail() {
	api "$B/api/v1/devices/$DEV" |
		jq -e --arg h "$(hostname)" '.device.hostname==$h and .device.can_run_commands==true and .metrics.mem_total_mb>0' >/dev/null
}
step "device detail (hostname, can_run_commands, metrics)" device_detail

# --- uname -a round trip ----------------------------------------------------
uname_cmd() {
	local s id
	s=$(date +%s%N)
	id=$(run_cmd '{"kind":"exec","argv":["uname","-a"],"timeout_s":30}' done 30) || return 1
	echo "round trip $(( ($(date +%s%N) - s) / 1000000 )) ms (command $id)"
	api "$B/api/v1/commands/$id" |
		jq -e '.command.status=="done" and .command.exit_code==0 and (.command.stdout|startswith("Linux"))' >/dev/null
}
step "exec uname -a" uname_cmd

# --- injection check: argv with shell metacharacters stays literal ----------
injection_cmd() {
	local id
	id=$(run_cmd '{"kind":"exec","argv":["echo","a;id"],"timeout_s":30}' done 30) || return 1
	api "$B/api/v1/commands/$id" | jq -e '.command.stdout=="a;id\n"' >/dev/null
}
step "exec argv is literal (no shell)" injection_cmd

# --- script kind ------------------------------------------------------------
script_cmd() {
	local id
	id=$(run_cmd '{"kind":"script","shell":"sh","body":"printf hello\n","timeout_s":30}' done 30) || return 1
	api "$B/api/v1/commands/$id" | jq -e '.command.stdout=="hello" and .command.exit_code==0' >/dev/null
}
step "script sh" script_cmd

# --- built-in ping kind (A1) ------------------------------------------------
ping_cmd() {
	local id
	id=$(run_cmd '{"kind":"ping","timeout_s":30}' done 30) || return 1
	api "$B/api/v1/commands/$id" | jq -e '.command.stdout=="pong" and .command.exit_code==0' >/dev/null
}
step "ping kind" ping_cmd

# --- timeout ----------------------------------------------------------------
timeout_cmd() {
	local id
	id=$(run_cmd '{"kind":"exec","argv":["sleep","5"],"timeout_s":1}' timeout 10) || return 1
	api "$B/api/v1/commands/$id" | jq -e '.command.exit_code==-1 and (.command.error|startswith("timeout"))' >/dev/null
}
step "exec timeout" timeout_cmd

# --- android refusal --------------------------------------------------------
android_enroll() {
	AID=$(curl -sf -H "$J" -d "{\"enrollment_token\":\"$ET\",\"device_uid\":\"android-e2e\",\"hostname\":\"Pixel 8\",\"platform\":\"android\",\"arch\":\"arm64\",\"os_version\":\"Android 14 (34)\",\"agent_version\":\"0.1.0\",\"interval_s\":60,\"facts\":{}}" "$B/api/agent/v1/enroll" | jq -r '.device_id')
	[ -n "$AID" ] && [ "$AID" != null ]
}
step "android device enrolls" android_enroll
step "exec on android is refused (400)" \
	test "$(http_code -b "$T/jar" -H "$H" -H "$J" -d '{"kind":"exec","argv":["id"]}' "$B/api/v1/devices/$AID/commands")" = 400
step "lock on android is accepted (201)" \
	test "$(http_code -b "$T/jar" -H "$H" -H "$J" -d '{"kind":"lock","timeout_s":30}' "$B/api/v1/devices/$AID/commands")" = 201

# --- patch status + install (PM1, against the fake apt-get) ------------------
wait_patches() {
	local i p
	for ((i = 0; i < 20; i++)); do
		p=$(api "$B/api/v1/devices/$DEV" | jq -r '.device.pending_patches')
		[ "$p" = 3 ] && return 0
		sleep 1
	done
	echo "pending_patches never reached 3 (last $p)" >&2
	return 1
}
step "device reports pending updates from the fake apt" wait_patches
step "patch report: 2 security of 3, full report on the device detail" \
	sh -c "curl -sf -b $T/jar $B/api/v1/devices/$DEV | jq -e '.device.security_patches==2 and .device.pending_patches==3 and ([.patches.updates[]|select(.security==true)]|length==2)' >/dev/null"
patch_install() {
	local id
	id=$(api_post -d '{"action":"install","mode":"security"}' "$B/api/v1/devices/$DEV/patch" | jq -r '.command.id')
	[ -n "$id" ] && [ "$id" != null ] || { echo "patch queue failed" >&2; return 1; }
	poll_cmd "$id" done 30 || return 1
	api "$B/api/v1/commands/$id" | jq -e '.command.status=="done" and .command.kind=="patch" and (.command.stdout|contains("fake apt-get: upgraded"))' >/dev/null
}
step "patch install security via fake apt completes done with the log" patch_install
step "patch.install_requested audited" \
	sh -c "curl -sf -b $T/jar '$B/api/v1/audit?limit=200' | jq -e '[.entries[]|select(.action==\"patch.install_requested\" and .actor_type==\"user\")]|length>=1' >/dev/null"
step "patch install with an id not in the pending list is 400" \
	test "$(http_code -b "$T/jar" -H "$H" -H "$J" -d '{"action":"install","mode":"ids","ids":["openssl","ghost"]}' "$B/api/v1/devices/$DEV/patch")" = 400
step "patch on android is 400 unsupported_on_platform" \
	sh -c "curl -s -b $T/jar -H '$H' -H '$J' -d '{\"action\":\"install\",\"mode\":\"security\"}' $B/api/v1/devices/$AID/patch | jq -e '.error==\"unsupported_on_platform\"' >/dev/null"

# --- csrf + role checks -----------------------------------------------------
step "missing X-Requested-With is 403" \
	test "$(http_code -b "$T/jar" -H "$J" -d '{"kind":"exec","argv":["id"]}' "$B/api/v1/devices/$DEV/commands")" = 403
step "no session is 401" \
	test "$(http_code -H "$H" -H "$J" -d '{"kind":"exec","argv":["id"]}' "$B/api/v1/devices/$DEV/commands")" = 401

viewer_setup() {
	api_post -d "{\"email\":\"$VIEWER_EMAIL\",\"password\":\"$VIEWER_PASS\",\"role\":\"viewer\"}" "$B/api/v1/users" | jq -e '.user.role=="viewer"' >/dev/null &&
		curl -sf -c "$T/jar-viewer" -H "$H" -H "$J" -d "{\"email\":\"$VIEWER_EMAIL\",\"password\":\"$VIEWER_PASS\"}" "$B/api/v1/login" | jq -e '.user.role=="viewer"' >/dev/null
}
step "create viewer and log in" viewer_setup
step "viewer cannot queue commands (403)" \
	test "$(http_code -b "$T/jar-viewer" -H "$H" -H "$J" -d '{"kind":"exec","argv":["id"]}' "$B/api/v1/devices/$DEV/commands")" = 403
step "viewer can read the fleet (200)" test "$(http_code -b "$T/jar-viewer" "$B/api/v1/devices")" = 200

# --- audit trail distinguishes user and device actors -----------------------
audit_check() {
	local a
	a=$(api "$B/api/v1/audit?limit=200")
	echo "$a" | jq -e '[.entries[]|select(.action=="command.queued" and .actor_type=="user")]|length>=5' >/dev/null &&
		echo "$a" | jq -e '[.entries[]|select(.action=="command.finished" and .actor_type=="device")]|length>=5' >/dev/null &&
		echo "$a" | jq -e '[.entries[]|select(.action=="device.enrolled" and .actor_type=="device")]|length>=2' >/dev/null &&
		echo "$a" | jq -e '[.entries[]|select(.action=="user.login" and .actor_type=="user")]|length>=2' >/dev/null
}
step "audit trail (user vs device actors)" audit_check

# --- remote desktop (RD1) ---------------------------------------------------
step "rustdesk info is enabled with host and key" \
	sh -c "curl -sf -b $T/jar $B/api/v1/rustdesk | jq -e --arg h \"\$MSP_RUSTDESK_HOST\" --arg k \"\$MSP_RUSTDESK_KEY\" '.enabled==true and .host==\$h and .key==\$k' >/dev/null"
step "device reports rustdesk_id from the stub client" \
	sh -c "curl -sf -b $T/jar $B/api/v1/devices/$DEV | jq -e --arg id $RD_ID '.device.rustdesk_id==\$id and .facts.rustdesk_id==\$id' >/dev/null"
remote_desktop() {
	api_post -X POST "$B/api/v1/devices/$DEV/remote-desktop" |
		jq -e --arg id "$RD_ID" --arg h "$MSP_RUSTDESK_HOST" '.url=="rustdesk://connect/"+$id and .rustdesk_id==$id and .host==$h and (.key|length>0)' >/dev/null
}
step "remote desktop answers rustdesk://connect/<id> (200)" remote_desktop
step "remote desktop on the android device is 409 no_rustdesk_id" \
	sh -c "curl -s -b $T/jar -H '$H' -X POST $B/api/v1/devices/$AID/remote-desktop | jq -e '.error==\"no_rustdesk_id\"' >/dev/null"
step "viewer cannot open remote desktop (403)" \
	test "$(http_code -b "$T/jar-viewer" -H "$H" -X POST "$B/api/v1/devices/$DEV/remote-desktop")" = 403
step "remote desktop without CSRF header is 403" \
	test "$(http_code -b "$T/jar" -X POST "$B/api/v1/devices/$DEV/remote-desktop")" = 403
step "remote desktop on an unknown device is 404" \
	test "$(http_code -b "$T/jar" -H "$H" -X POST "$B/api/v1/devices/999999/remote-desktop")" = 404
step "audit has exactly one device.remote_desktop_opened by the admin" \
	sh -c "curl -sf -b $T/jar '$B/api/v1/audit?limit=200' | jq -e --arg id $RD_ID --argjson d $DEV '[.entries[]|select(.action==\"device.remote_desktop_opened\")]|length==1 and (.[0].actor_type==\"user\" and .[0].actor_label==\"$ADMIN_EMAIL\" and .[0].target_id==\$d and .[0].detail.rustdesk_id==\$id and .[0].ip==\"127.0.0.1\")' >/dev/null"

# --- dashboard shell served -------------------------------------------------
step "dashboard shell served" sh -c "curl -sf $B/ | grep -q app.js"
step "unknown API path is JSON 404" test "$(http_code -b "$T/jar" "$B/api/v1/nope")" = 404

# --- offline flip: window is 3*2+15 = 21 s; evaluator ticks every 15 s ------
kill "$AGT"
wait "$AGT" 2>/dev/null || true
AGT=""
echo "agent stopped; waiting 25 s for the offline window"
sleep 25
step "device offline after the window" \
	sh -c "curl -sf -b $T/jar $B/api/v1/devices/$DEV | jq -e '.device.status==\"offline\"' >/dev/null"

offline_alert() {
	local i
	for i in $(seq 1 20); do
		api "$B/api/v1/alerts?state=open" |
			jq -e --argjson d "$DEV" '[.alerts[]|select(.device_id==$d and .type=="offline")]|length==1' >/dev/null && return 0
		sleep 1
	done
	return 1
}
step "offline alert opened" offline_alert
step "summary counts the offline device" sh -c "curl -sf -b $T/jar $B/api/v1/summary | jq -e '.offline>=1 and .open_alerts>=1' >/dev/null"

kill "$SRV"
wait "$SRV" 2>/dev/null || true
SRV=""
echo "E2E OK"
