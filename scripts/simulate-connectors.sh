#!/usr/bin/env sh
set -eu

BASE_URL=${VEQRI_URL:-http://127.0.0.1:7342}
DATA_DIR=${VEQRI_DATA_DIR:-$HOME/.veqri}
TOKEN=${VEQRI_AUTH_TOKEN:-}
if [ -z "$TOKEN" ] && [ -f "$DATA_DIR/admin.token" ]; then
  TOKEN=$(tr -d '\r\n' < "$DATA_DIR/admin.token")
fi
if [ -z "$TOKEN" ] && command -v security >/dev/null 2>&1; then
  TOKEN=$(security find-generic-password -s ai.veqri -a admin-token -w 2>/dev/null || true)
fi
if [ -z "$TOKEN" ]; then
  echo "No admin token found. Start veqri-core first or set VEQRI_AUTH_TOKEN." >&2
  exit 1
fi

for KIND in slack mattermost teams; do
  curl --fail --silent --show-error \
    -H "Authorization: Bearer $TOKEN" \
    -H "X-Veqri-Protocol-Version: 1" \
    -H "Content-Type: application/json" \
    -d "{\"text\":\"Run the deterministic $KIND connector task\",\"actor_id\":\"simulated-user\",\"channel_id\":\"simulated-channel\",\"thread_id\":\"simulated-thread\",\"message_id\":\"$KIND-$(date +%s)\"}" \
    "$BASE_URL/v1/connectors/simulate/$KIND"
  printf '\n'
done
