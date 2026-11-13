#!/usr/bin/env bash
# scripts/e2e.sh — end-to-end proof with the real binaries (= `make e2e`).
#
# Builds bin/msp-server and bin/msp-agent, bootstraps an admin, an org and an
# enrollment token through the CLI, starts the server on 127.0.0.1:$MSP_E2E_PORT
# (default 18080), enrolls and runs the real Linux agent against it, then drives
# the dashboard JSON API with curl + jq exactly as a technician's browser would:
# device online, uname -a round trip (latency printed), shell-metacharacter
# injection check, script kind, timeout, android refusal, CSRF and role checks,
# audit trail, dashboard shell, and the offline flip with its alert.
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
# Loopback host => plain http is allowed without --insecure-http.
step "agent enroll" "$R/bin/msp-agent" enroll --server "$B" --token "$ET" --config "$T/agent.json" --interval 2
"$R/bin/msp-agent" run --config "$T/agent.json" >"$T/agent.log" 2>&1 &
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
