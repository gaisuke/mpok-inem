#!/usr/bin/env bash
# GitHub device-code login for the gh CLI on this headless VPS.
# Prints the user_code for the human, then polls until authorized and stores
# the token via `gh auth login --with-token` (token is never echoed).
set -u
CLIENT_ID="178c6fc778ccc68e1d6a"   # gh CLI's public OAuth client id
SCOPE="repo workflow read:org"
DEV_JSON=$(curl -4 -s -X POST https://github.com/login/device/code \
  -H "Accept: application/json" \
  -d "client_id=${CLIENT_ID}" -d "scope=${SCOPE}")
USER_CODE=$(python3 -c "import json,sys;print(json.load(sys.stdin)['user_code'])" <<<"$DEV_JSON")
DEVICE_CODE=$(python3 -c "import json,sys;print(json.load(sys.stdin)['device_code'])" <<<"$DEV_JSON")
VERIFY_URI=$(python3 -c "import json,sys;print(json.load(sys.stdin).get('verification_uri','https://github.com/login/device'))" <<<"$DEV_JSON")
INTERVAL=$(python3 -c "import json,sys;print(json.load(sys.stdin).get('interval',5))" <<<"$DEV_JSON")
echo "==== ACTION REQUIRED ===="
echo "CODE: ${USER_CODE}"
echo "URL:  ${VERIFY_URI}"
echo "========================"
echo "polling every ${INTERVAL}s ..."
for i in $(seq 1 120); do
  sleep "${INTERVAL}"
  RESP=$(curl -4 -s -X POST https://github.com/login/oauth/access_token \
    -H "Accept: application/json" \
    -d "client_id=${CLIENT_ID}" -d "device_code=${DEVICE_CODE}" \
    -d "grant_type=urn:ietf:params:oauth:grant-type:device_code")
  STATUS=$(python3 -c "import json,sys;d=json.load(sys.stdin);print(d.get('error',''))" <<<"$RESP")
  if [ -z "$STATUS" ]; then
    TOKEN=$(python3 -c "import json,sys;print(json.load(sys.stdin)['access_token'])" <<<"$RESP")
    if printf '%s' "$TOKEN" | gh auth login --with-token 2>/tmp/gh_login_err; then
      gh auth setup-git 2>/dev/null
      echo "AUTH_OK"
      gh auth status 2>&1 | sed -n '1,6p'
      exit 0
    else
      echo "AUTH_STORE_FAILED"; cat /tmp/gh_login_err; exit 1
    fi
  fi
  case "$STATUS" in
    authorization_pending) : ;;                 # keep waiting
    slow_down) INTERVAL=$((INTERVAL + 5)) ;;
    expired_token|access_denied) echo "AUTH_FAILED: $STATUS"; exit 1 ;;
    *) : ;;
  esac
done
echo "AUTH_TIMEOUT"; exit 1
