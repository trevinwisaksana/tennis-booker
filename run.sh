#!/usr/bin/env bash
# Usage: ./run.sh check|selftest|start|stop|status|wait
#
# Auth per account (X = A or B), any of:
#   BOOKER_EMAIL_X + BOOKER_PASSWORD_X  site email/password login (re-logs in when the session dies)
#   BOOKER_COOKIE_X                      raw `cookie:` header from DevTools
#   cookie.txt / cookie2.txt             same, as files
set -euo pipefail
cd "$(dirname "$0")"

ACCOUNTS=(A B)
declare -A START=([A]=06:00 [B]=07:00)
declare -A FILE=([A]=cookie.txt [B]=cookie2.txt)
COURTS=${BOOKER_COURT_IDS:-372,373}

cookie_env() { local v="BOOKER_COOKIE_$1"; printf '%s' "${!v:-}"; }
email_env() { local v="BOOKER_EMAIL_$1"; printf '%s' "${!v:-}"; }
pass_env() { local v="BOOKER_PASSWORD_$1"; printf '%s' "${!v:-}"; }
log_of() { echo "booker-$(echo "$1" | tr 'A-Z' 'a-z').log"; }
pid_of() { echo ".booker-$(echo "$1" | tr 'A-Z' 'a-z').pid"; }

build() { go build -o booker . ; }

check() {
  local ok=0
  for a in "${ACCOUNTS[@]}"; do
    local src raw
    if [[ -n "$(email_env "$a")" && -n "$(pass_env "$a")" ]]; then
      echo "account $a: email/password login configured"
    fi
    if [[ -n "$(cookie_env "$a")" ]]; then src="env BOOKER_COOKIE_$a"; raw=$(cookie_env "$a")
    elif [[ -f "${FILE[$a]}" ]]; then src="${FILE[$a]}"; raw=$(cat "${FILE[$a]}")
    elif [[ -n "$(email_env "$a")" && -n "$(pass_env "$a")" ]]; then continue
    else echo "account $a: MISSING (no BOOKER_EMAIL_$a/BOOKER_PASSWORD_$a, BOOKER_COOKIE_$a or ${FILE[$a]})"; ok=1; continue; fi
    if grep -qE '(^|[; ])[A-Za-z0-9_]*_session=' <<<"$raw"; then
      echo "account $a: $src contains a *_session cookie"
    else
      echo "account $a: $src has NO *_session cookie"; ok=1
    fi
  done
  return $ok
}

launch() {  # launch <account> <foreground:0|1>
  local a=$1 fg=$2
  (
    export BOOKER_LABEL="$a" BOOKER_START_TIME="${START[$a]}" BOOKER_COURT_IDS="$COURTS"
    c=$(cookie_env "$a")
    if [[ -n "$c" ]]; then export BOOKER_COOKIE="$c"; unset BOOKER_COOKIE_FILE || true
    else export BOOKER_COOKIE_FILE="${FILE[$a]}"; unset BOOKER_COOKIE || true; fi
    [[ -f "${FILE[$a]}" || -n "$c" ]] || unset BOOKER_COOKIE_FILE
    export BOOKER_EMAIL="$(email_env "$a")" BOOKER_PASSWORD="$(pass_env "$a")"
    unset BOOKER_COOKIE_A BOOKER_COOKIE_B BOOKER_EMAIL_A BOOKER_EMAIL_B BOOKER_PASSWORD_A BOOKER_PASSWORD_B
    wrap=()
    command -v caffeinate >/dev/null && wrap=(caffeinate -dis)
    if [[ $fg == 1 ]]; then
      exec "${wrap[@]}" ./booker
    else
      nohup "${wrap[@]}" ./booker >>"$(log_of "$a")" 2>&1 &
      echo $! >"$(pid_of "$a")"
    fi
  )
}

running() { [[ -f "$(pid_of "$1")" ]] && kill -0 "$(cat "$(pid_of "$1")")" 2>/dev/null; }

case "${1:-}" in
  check) check ;;
  selftest)
    build; check
    rc=0
    for a in "${ACCOUNTS[@]}"; do BOOKER_SELFTEST=1 launch "$a" 1 || rc=1; done
    exit $rc ;;
  start)
    build; check
    for a in "${ACCOUNTS[@]}"; do
      if running "$a"; then echo "account $a already running (pid $(cat "$(pid_of "$a")"))"; continue; fi
      launch "$a" 0
    done
    for a in "${ACCOUNTS[@]}"; do
      for _ in $(seq 60); do grep -q "armed:" "$(log_of "$a")" 2>/dev/null && break; sleep 1; done
      if grep -q "armed:" "$(log_of "$a")"; then grep "armed:" "$(log_of "$a")" | tail -1
      else echo "account $a NOT armed; tail of $(log_of "$a"):"; tail -5 "$(log_of "$a")"; exit 1; fi
    done ;;
  stop)
    for a in "${ACCOUNTS[@]}"; do running "$a" && kill "$(cat "$(pid_of "$a")")" && echo "stopped $a"; rm -f "$(pid_of "$a")"; done ;;
  status)
    for a in "${ACCOUNTS[@]}"; do
      if running "$a"; then echo "account $a running (pid $(cat "$(pid_of "$a")"))"; else echo "account $a not running"; fi
      tail -2 "$(log_of "$a")" 2>/dev/null || true
    done ;;
  wait)  # block until both daemons exit (use with BOOKER_ONESHOT=1)
    for a in "${ACCOUNTS[@]}"; do while running "$a"; do sleep 5; done; done
    grep -h "NOTIFY" booker-a.log booker-b.log || true ;;
  *) echo "usage: $0 check|selftest|start|stop|status|wait"; exit 2 ;;
esac
