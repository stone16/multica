#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$SCRIPT_DIR/../../../.." && pwd)
CURRENT_POINTER=/tmp/multica-verify-current

usage() {
  cat <<EOF
usage: verify-multica.sh <command>

  launch
  doctor
  drive <sign-in|create-issue|inbox|agents|settings>
  browser start|stop|<op> [--flags]
  read-code --email <email>
  cleanup
EOF
  exit 2
}

load_state() {
  if [ -z "${MULTICA_VERIFY_STATE:-}" ]; then
    if [ ! -f "$CURRENT_POINTER" ]; then
      echo "no verification state. run launch first" >&2
      exit 1
    fi
    MULTICA_VERIFY_STATE=$(cat "$CURRENT_POINTER")
    export MULTICA_VERIFY_STATE
  fi
  if [ ! -f "$MULTICA_VERIFY_STATE/meta.env" ]; then
    echo "missing $MULTICA_VERIFY_STATE/meta.env" >&2
    exit 1
  fi
  set -a
  # shellcheck disable=SC1090
  . "$MULTICA_VERIFY_STATE/meta.env"
  set +a
  STATE_DIR=$MULTICA_VERIFY_STATE
}

go_is_new_enough() {
  local raw major minor
  raw=$(go version 2>/dev/null | awk '{print $3}' | sed 's/^go//')
  major=${raw%%.*}
  minor=${raw#*.}
  minor=${minor%%.*}
  [ "${major:-0}" -gt 1 ] || { [ "$major" -eq 1 ] && [ "${minor:-0}" -ge 26 ]; }
}

port_free() {
  ! ss -ltn | awk '{print $4}' | grep -Eq "(^|:)$1$"
}

pick_port() {
  local port=$1
  while ! port_free "$port"; do
    port=$((port + 1))
  done
  printf '%s\n' "$port"
}

wait_for_http() {
  local url=$1 label=$2 max=$3 needle=${4:-}
  local i body
  for i in $(seq 1 "$max"); do
    body=$(curl -sf "$url" 2>/dev/null || true)
    if [ -n "$body" ] && { [ -z "$needle" ] || printf '%s' "$body" | grep -q "$needle"; }; then
      echo "$label ready (${i}s)"
      return 0
    fi
    sleep 1
  done
  echo "$label did not become ready: $url" >&2
  return 1
}

ensure_postgres() {
  cd "$REPO"
  docker compose up -d postgres
  local i
  for i in $(seq 1 60); do
    if docker compose exec -T postgres pg_isready -U multica -d postgres >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "postgres did not become ready" >&2
  return 1
}

create_database() {
  cd "$REPO"
  local exists
  exists=$(docker compose exec -T postgres psql -U multica -d postgres -Atqc "SELECT 1 FROM pg_database WHERE datname = '${POSTGRES_DB}'")
  if [ "$exists" != "1" ]; then
    docker compose exec -T postgres psql -U multica -d postgres -v ON_ERROR_STOP=1 -c "CREATE DATABASE \"${POSTGRES_DB}\""
  fi
}

drop_database() {
  cd "$REPO"
  docker compose exec -T postgres psql -U multica -d postgres -v ON_ERROR_STOP=1 -c "DROP DATABASE IF EXISTS \"${POSTGRES_DB}\" WITH (FORCE)" >/dev/null
}

kill_recorded() {
  local file=$1 pid
  [ -f "$file" ] || return 0
  pid=$(cat "$file")
  if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
    kill -- -"$pid" 2>/dev/null || kill "$pid" 2>/dev/null || true
    sleep 0.5
    if kill -0 "$pid" 2>/dev/null; then
      kill -9 -- -"$pid" 2>/dev/null || kill -9 "$pid" 2>/dev/null || true
    fi
  fi
}

pid_alive() {
  local pid=$1
  [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null
}

group_owns_port() {
  local pid=$1 port=$2 pgid listeners listener
  pid_alive "$pid" || return 1
  pgid=$(ps -o pgid= -p "$pid" | tr -d ' ')
  listeners=$(ss -ltnp "sport = :$port" 2>/dev/null || true)
  if ! printf '%s' "$listeners" | grep -q "pid="; then
    return 0
  fi
  while read -r listener; do
    if [ -n "$pgid" ] && [ "$(ps -o pgid= -p "$listener" 2>/dev/null | tr -d ' ')" = "$pgid" ]; then
      return 0
    fi
  done < <(printf '%s\n' "$listeners" | grep -oE 'pid=[0-9]+' | cut -d= -f2 | sort -u)
  return 1
}

cmd_launch() {
  if ! command -v docker >/dev/null 2>&1 || ! docker compose version >/dev/null 2>&1; then
    echo "docker compose is required. The repo starts Postgres with docker compose up -d postgres." >&2
    exit 1
  fi
  if ! go_is_new_enough; then
    echo "Go 1.26 or newer is required. go.mod says 1.26.1. Current: $(go version 2>/dev/null || echo missing)" >&2
    exit 1
  fi
  if ! command -v pnpm >/dev/null 2>&1; then
    echo "pnpm is required" >&2
    exit 1
  fi
  if [ ! -d "$REPO/node_modules/playwright" ]; then
    (cd "$REPO" && pnpm install)
  fi

  local run_id port frontend_port
  run_id=${MULTICA_VERIFY_RUN_ID:-$(date +%s)}
  if ! printf '%s' "$run_id" | grep -Eq '^[a-z0-9]+$'; then
    echo "MULTICA_VERIFY_RUN_ID must match [a-z0-9]+" >&2
    exit 1
  fi
  STATE_DIR=/tmp/multica-verify-$run_id
  if [ -d "$STATE_DIR" ]; then
    echo "state already exists: $STATE_DIR" >&2
    exit 1
  fi
  mkdir -p "$STATE_DIR"
  port=$(pick_port 20080)
  frontend_port=$(pick_port $((port + 1000)))
  POSTGRES_DB=multica_verify_$run_id
  EVIDENCE_DIR=${MULTICA_VERIFY_EVIDENCE:-$REPO/.context/verify-multica/$run_id}
  mkdir -p "$EVIDENCE_DIR"
  GIT_REV=$(git -C "$REPO" rev-parse HEAD)

  cat > "$STATE_DIR/meta.env" <<EOF
RUN_ID=$run_id
REPO=$REPO
STATE_DIR=$STATE_DIR
EVIDENCE_DIR=$EVIDENCE_DIR
POSTGRES_DB=$POSTGRES_DB
PORT=$port
FRONTEND_PORT=$frontend_port
FRONTEND_ORIGIN=http://localhost:$frontend_port
NEXT_PUBLIC_API_URL=http://127.0.0.1:$port
NEXT_PUBLIC_WS_URL=ws://127.0.0.1:$port/ws
DATABASE_URL=postgres://multica:multica@127.0.0.1:5432/${POSTGRES_DB}?sslmode=disable
GIT_REV=$GIT_REV
JWT_SECRET=verify-$run_id
EOF
  printf '%s\n' "$STATE_DIR" > "$CURRENT_POINTER"
  export MULTICA_VERIFY_STATE=$STATE_DIR
  load_state
  trap 'status=$?; if [ "$status" -ne 0 ]; then echo "launch failed; cleaning this run" >&2; cmd_cleanup || true; fi; exit "$status"' EXIT

  ensure_postgres
  create_database
  (
    cd "$REPO/server"
    env -u RESEND_API_KEY DATABASE_URL="$DATABASE_URL" go run ./cmd/migrate up
  ) > "$STATE_DIR/migrate.log" 2>&1

  set -m
  (
    cd "$REPO/server"
    env -u RESEND_API_KEY \
      PORT="$PORT" \
      DATABASE_URL="$DATABASE_URL" \
      JWT_SECRET="$JWT_SECRET" \
      FRONTEND_ORIGIN="$FRONTEND_ORIGIN" \
      go run ./cmd/server
  ) > "$STATE_DIR/backend.log" 2>&1 &
  echo $! > "$STATE_DIR/backend.pid"
  (
    cd "$REPO"
    env -u RESEND_API_KEY \
      PORT="$PORT" \
      FRONTEND_PORT="$FRONTEND_PORT" \
      FRONTEND_ORIGIN="$FRONTEND_ORIGIN" \
      NEXT_PUBLIC_API_URL="$NEXT_PUBLIC_API_URL" \
      NEXT_PUBLIC_WS_URL="$NEXT_PUBLIC_WS_URL" \
      DATABASE_URL="$DATABASE_URL" \
      JWT_SECRET="$JWT_SECRET" \
      pnpm dev:web
  ) > "$STATE_DIR/frontend.log" 2>&1 &
  echo $! > "$STATE_DIR/frontend.pid"
  set +m

  if ! wait_for_http "http://127.0.0.1:$PORT/health" backend 180 '"status":"ok"'; then
    tail -80 "$STATE_DIR/backend.log" >&2 || true
    exit 1
  fi
  if ! wait_for_http "http://localhost:$FRONTEND_PORT/login" frontend 180 'Multica'; then
    tail -80 "$STATE_DIR/frontend.log" >&2 || true
    exit 1
  fi
  cmd_doctor
  echo "export MULTICA_VERIFY_STATE=$STATE_DIR"
  echo "evidence $EVIDENCE_DIR"
}

cmd_doctor() {
  load_state
  local failed=0
  local backend_pid frontend_pid head
  backend_pid=$(cat "$STATE_DIR/backend.pid" 2>/dev/null || true)
  frontend_pid=$(cat "$STATE_DIR/frontend.pid" 2>/dev/null || true)
  if ! pid_alive "$backend_pid"; then
    echo "backend pid is not alive" >&2
    failed=1
  fi
  if ! pid_alive "$frontend_pid"; then
    echo "frontend pid is not alive" >&2
    failed=1
  fi
  if ! group_owns_port "$backend_pid" "$PORT"; then
    echo "backend pid does not own :$PORT" >&2
    failed=1
  fi
  if ! group_owns_port "$frontend_pid" "$FRONTEND_PORT"; then
    echo "frontend pid does not own :$FRONTEND_PORT" >&2
    failed=1
  fi
  local health_body login_body
  health_body=$(curl -sf "http://127.0.0.1:$PORT/health" || true)
  if ! printf '%s' "$health_body" | grep -q '"status":"ok"'; then
    echo "backend health failed" >&2
    failed=1
  fi
  login_body=$(curl -sf "$FRONTEND_ORIGIN/login" || true)
  if ! printf '%s' "$login_body" | grep -q 'Multica'; then
    echo "frontend /login is not usable" >&2
    failed=1
  fi
  head=$(git -C "$REPO" rev-parse HEAD)
  if [ "$head" != "$GIT_REV" ]; then
    echo "git HEAD $head does not match launched $GIT_REV" >&2
    failed=1
  fi
  if [ "$POSTGRES_DB" = "multica" ] || ! printf '%s' "$POSTGRES_DB" | grep -Eq '^multica_verify_[a-z0-9]+$'; then
    echo "refusing database $POSTGRES_DB" >&2
    failed=1
  fi
  cd "$REPO"
  if ! docker compose exec -T postgres psql -U multica -d "$POSTGRES_DB" -Atqc "SELECT to_regclass('public.verification_code') IS NOT NULL" | grep -qx t; then
    echo "verification_code table is missing in $POSTGRES_DB" >&2
    failed=1
  fi
  if [ ! -d "$EVIDENCE_DIR" ]; then
    echo "evidence dir missing: $EVIDENCE_DIR" >&2
    failed=1
  fi
  case "$EVIDENCE_DIR" in
    "$STATE_DIR"*) echo "evidence dir must survive cleanup and cannot live in the state dir" >&2; failed=1 ;;
  esac
  if [ "$failed" -ne 0 ]; then
    echo "doctor failed" >&2
    exit 1
  fi
  echo "doctor ok origin=$FRONTEND_ORIGIN db=$POSTGRES_DB rev=$GIT_REV evidence=$EVIDENCE_DIR"
}

read_code() {
  local email=$1 i line
  for i in $(seq 1 40); do
    line=$(grep -F "[DEV] Verification code for ${email}:" "$STATE_DIR/backend.log" | tail -1 || true)
    if [ -n "$line" ]; then
      printf '%s\n' "$line" | awk '{print $NF}'
      return 0
    fi
    sleep 0.5
  done
  echo "no dev verification code for $email" >&2
  return 1
}

cmd_read_code() {
  load_state
  local email=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --email) email=$2; shift 2 ;;
      *) echo "unknown flag $1" >&2; exit 2 ;;
    esac
  done
  [ -n "$email" ] || usage
  read_code "$email"
}

ensure_browser_binary() {
  local executable
  executable=$(node --input-type=module -e 'import { chromium } from "playwright"; process.stdout.write(chromium.executablePath())')
  if [ ! -x "$executable" ]; then
    (cd "$REPO" && pnpm exec playwright install chromium)
  fi
}

browser_start() {
  load_state
  if [ -f "$STATE_DIR/browser.pid" ] && pid_alive "$(cat "$STATE_DIR/browser.pid")"; then
    return 0
  fi
  ensure_browser_binary
  rm -f "$STATE_DIR/browser.port" "$EVIDENCE_DIR/failure.png"
  set -m
  node "$SCRIPT_DIR/browser.mjs" "$STATE_DIR" > "$STATE_DIR/browser.log" 2>&1 &
  echo $! > "$STATE_DIR/browser.pid"
  set +m
  local i
  for i in $(seq 1 60); do
    if [ -f "$STATE_DIR/browser.port" ]; then
      return 0
    fi
    sleep 0.5
  done
  echo "browser failed to start" >&2
  cat "$STATE_DIR/browser.log" >&2 || true
  return 1
}

browser_stop() {
  load_state
  if [ -f "$STATE_DIR/browser.port" ]; then
    browser_http quit >/dev/null 2>&1 || true
  fi
  kill_recorded "$STATE_DIR/browser.pid"
  rm -f "$STATE_DIR/browser.port"
}

browser_http() {
  load_state
  python3 - "$STATE_DIR" "$@" <<'PY'
import json, sys, urllib.error, urllib.request
state, op, *args = sys.argv[1:]
body = {"op": op}
flags = {"exact"}
ints = {"count", "width", "height", "timeout"}
i = 0
while i < len(args):
    flag = args[i]
    if not flag.startswith("--"):
        sys.stderr.write(f"bad flag {flag}\n")
        sys.exit(2)
    key = flag[2:]
    if key in flags:
        body[key] = True
        i += 1
        continue
    body[key] = args[i + 1]
    if key in ints:
        body[key] = int(body[key])
    i += 2
port = open(state + "/browser.port").read().strip()
request = urllib.request.Request(
    f"http://127.0.0.1:{port}/",
    data=json.dumps(body).encode(),
    headers={"content-type": "application/json"},
)
try:
    with urllib.request.urlopen(request, timeout=90) as response:
        payload = json.load(response)
except urllib.error.HTTPError as error:
    payload = json.load(error)
    sys.stderr.write(payload.get("error", json.dumps(payload)) + "\n")
    sys.exit(1)
if not payload.get("ok"):
    sys.stderr.write(json.dumps(payload) + "\n")
    sys.exit(1)
print(json.dumps(payload))
PY
}

sign_in_steps() {
  local email=$1
  browser_http goto --path /login
  browser_http expect-text --text Multica --exact
  browser_http fill --role textbox --name Email --value "$email" --exact
  browser_http click --role button --name Continue --exact
  browser_http expect-text --text "Check your email" --exact
  local code
  code=$(read_code "$email")
  browser_http otp --value "$code"
  browser_http expect-url --pattern '/issues/?($|\?)' --timeout 30000
  browser_http expect-text --text "No issues yet" --exact
}

open_new_issue() {
  browser_http hover --selector "[data-slot='sidebar-header'] [data-slot='tooltip-trigger']"
  browser_http expect-text --text "New issue" --exact
  browser_http click-selector --selector "[data-slot='sidebar-header'] [data-slot='tooltip-trigger']"
  browser_http expect --role dialog --name "New Issue" --exact
}

cmd_drive() {
  local feature=${1:-}
  [ -n "$feature" ] || usage
  cmd_doctor >/dev/null
  browser_start
  trap browser_stop EXIT
  case "$feature" in
    sign-in) drive_sign_in ;;
    create-issue) drive_create_issue ;;
    inbox) drive_inbox ;;
    agents) drive_agents ;;
    settings) drive_settings ;;
    *) usage ;;
  esac
  trap - EXIT
  browser_stop
}

drive_sign_in() {
  sign_in_steps "sign-in@example.com"
  browser_http expect-text --text "sign-in's Workspace" --exact
  browser_http screenshot --path sign-in/issues.png
  browser_http snapshot --path sign-in/issues.aria.txt
  printf '%s\n' '{"feature":"sign-in","email":"sign-in@example.com"}' > "$EVIDENCE_DIR/sign-in/proof.json"
  echo "drive sign-in ok $EVIDENCE_DIR/sign-in"
}

drive_create_issue() {
  local email=create-issue@example.com
  sign_in_steps "$email"
  browser_http expect-text --text "create-issue's Workspace" --exact
  browser_http screenshot --path create-issue/empty.png
  open_new_issue
  browser_http type --role textbox --name "Issue title" --value "Release checklist" --exact
  browser_http click --role button --name "Create Issue" --exact
  browser_http expect-text --text "Issue created" --exact
  browser_http expect --role link --name "Release checklist"
  browser_http screenshot --path create-issue/board.png
  browser_http snapshot --path create-issue/board.aria.txt
  browser_http click --role link --name "Release checklist"
  browser_http expect-url --pattern '/issues/[0-9a-f-]{8,}' --timeout 30000
  browser_http expect-text --text "Release checklist" --exact
  browser_http screenshot --path create-issue/detail.png
  browser_http click --role link --name Issues --exact
  browser_http expect-url --pattern '/issues/?($|\?)' --timeout 30000
  open_new_issue
  browser_http type --role textbox --name "Issue title" --value "Discard me" --exact
  browser_http hover --selector "[data-slot='dialog-content'] [data-slot='tooltip-trigger'] >> nth=1"
  browser_http expect-text --text Close --exact
  browser_http click-selector --selector "[data-slot='dialog-content'] [data-slot='tooltip-trigger'] >> nth=1"
  browser_http expect-count --role dialog --name "New Issue" --count 0
  browser_http expect-count --role link --name "Discard me" --count 0
  browser_http expect --role link --name "Release checklist"
  browser_http viewport --width 390 --height 844
  browser_http screenshot --path create-issue/board-mobile.png
  browser_http viewport --width 1280 --height 800
  browser_http issue-api-proof
  python3 - "$EVIDENCE_DIR/create-issue/api-issues.json" <<'PY'
import json, sys
data = json.load(open(sys.argv[1]))
titles = [issue["title"] for issue in data["issues"]]
if "Release checklist" not in titles or "Discard me" in titles:
    raise SystemExit(f"unexpected titles {titles}")
print("api titles", titles)
PY
  printf '%s\n' '{"feature":"create-issue","email":"create-issue@example.com","title":"Release checklist"}' > "$EVIDENCE_DIR/create-issue/proof.json"
  echo "drive create-issue ok $EVIDENCE_DIR/create-issue"
}

drive_inbox() {
  sign_in_steps "inbox@example.com"
  browser_http click --role link --name Inbox --exact
  browser_http expect-url --includes /inbox --timeout 30000
  browser_http expect --role heading --name Inbox --exact
  browser_http expect-text --text "No notifications" --exact
  browser_http expect-text --text "Your inbox is empty" --exact
  browser_http screenshot --path inbox/empty.png
  browser_http snapshot --path inbox/empty.aria.txt
  printf '%s\n' '{"feature":"inbox","email":"inbox@example.com"}' > "$EVIDENCE_DIR/inbox/proof.json"
  echo "drive inbox ok $EVIDENCE_DIR/inbox"
}

drive_agents() {
  sign_in_steps "agents@example.com"
  browser_http click --role link --name Agents --exact
  browser_http expect-url --includes /agents --timeout 30000
  browser_http expect --role heading --name Agents --exact
  browser_http expect-text --text "No agents yet" --exact
  browser_http expect --role button --name "Create Agent" --exact
  browser_http screenshot --path agents/empty.png
  browser_http snapshot --path agents/empty.aria.txt
  printf '%s\n' '{"feature":"agents","email":"agents@example.com"}' > "$EVIDENCE_DIR/agents/proof.json"
  echo "drive agents ok $EVIDENCE_DIR/agents"
}

drive_settings() {
  sign_in_steps "settings@example.com"
  browser_http click --role link --name Settings --exact
  browser_http expect-url --includes /settings --timeout 30000
  browser_http expect --role heading --name Settings --exact
  browser_http expect --role heading --name Profile --exact
  browser_http screenshot --path settings/profile.png
  browser_http click --role tab --name Members --exact
  browser_http expect-text --text "Members (" 
  browser_http screenshot --path settings/members.png
  browser_http snapshot --path settings/members.aria.txt
  printf '%s\n' '{"feature":"settings","email":"settings@example.com"}' > "$EVIDENCE_DIR/settings/proof.json"
  echo "drive settings ok $EVIDENCE_DIR/settings"
}

cmd_browser() {
  local sub=${1:-}
  [ -n "$sub" ] || usage
  shift
  case "$sub" in
    start) browser_start ;;
    stop) browser_stop ;;
    *) browser_http "$sub" "$@" ;;
  esac
}

cmd_cleanup() {
  load_state || true
  if [ -n "${STATE_DIR:-}" ] && [ -d "${STATE_DIR:-}" ]; then
    browser_stop || true
    kill_recorded "$STATE_DIR/frontend.pid"
    kill_recorded "$STATE_DIR/backend.pid"
    if [ -n "${POSTGRES_DB:-}" ]; then
      drop_database || true
    fi
    rm -rf "$STATE_DIR"
  fi
  if [ -f "$CURRENT_POINTER" ] && [ "$(cat "$CURRENT_POINTER")" = "${MULTICA_VERIFY_STATE:-}" ]; then
    rm -f "$CURRENT_POINTER"
  fi
  if [ -n "${EVIDENCE_DIR:-}" ] && [ -d "$EVIDENCE_DIR" ]; then
    echo "evidence kept $EVIDENCE_DIR"
  fi
}

main() {
  local cmd=${1:-}
  [ -n "$cmd" ] || usage
  shift
  case "$cmd" in
    launch) cmd_launch "$@" ;;
    doctor) cmd_doctor "$@" ;;
    drive) cmd_drive "$@" ;;
    browser) cmd_browser "$@" ;;
    read-code) cmd_read_code "$@" ;;
    cleanup) cmd_cleanup "$@" ;;
    *) usage ;;
  esac
}

main "$@"
