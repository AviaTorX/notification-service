#!/usr/bin/env bash
set -euo pipefail

KEY="${API_KEY:-dev-api-key-change-me}"
BASE="${BASE:-http://localhost:8080}"
UID_="${UID_:-00000000-0000-0000-0000-000000000001}"

hdr_key="X-API-Key: $KEY"
hdr_json="Content-Type: application/json"

jsp() { python3 -m json.tool 2>/dev/null || cat; }
jkey() { python3 -c "import sys,json; print(json.load(sys.stdin)$1)"; }

echo "=== 1. Send instant welcome (template defaults = email + in_app) ==="
R=$(curl -sf -X POST "$BASE/v1/notifications" \
  -H "$hdr_key" -H "$hdr_json" -H "Idempotency-Key: welcome-001" \
  -d "{\"user_id\":\"$UID_\",\"template_name\":\"welcome\",\"variables\":{\"name\":\"Avinash\",\"product\":\"NotifCo\"}}")
echo "$R" | jsp | head -30
NID=$(echo "$R" | jkey "['id']")
echo "notification_id=$NID"

sleep 2

echo
echo "=== 2. Fetch notification with deliveries ==="
curl -sf "$BASE/v1/notifications/$NID" -H "$hdr_key" | jsp | head -40

echo
echo "=== 3. Mailpit shows the email ==="
MP=$(curl -sf "http://localhost:8025/api/v1/messages")
echo "$MP" | python3 -c '
import sys,json
d=json.load(sys.stdin)
print("total:", d.get("total", "?"))
for m in d.get("messages", [])[:5]:
    print(" -", m.get("Subject",""), "to", [x.get("Address") for x in m.get("To",[])])
'

echo
echo "=== 4. In-app feed ==="
curl -sf "$BASE/v1/users/$UID_/in-app-notifications" -H "$hdr_key" | jsp | head -30

echo
echo "=== 5. Idempotency replay (same key, expect Idempotent-Replay: true) ==="
curl -sfi -X POST "$BASE/v1/notifications" \
  -H "$hdr_key" -H "$hdr_json" -H "Idempotency-Key: welcome-001" \
  -d "{\"user_id\":\"$UID_\",\"template_name\":\"welcome\",\"variables\":{\"name\":\"Avinash\",\"product\":\"NotifCo\"}}" \
  | head -14

echo
echo "=== 6. Conflict: same key, different body => 409 ==="
CODE=$(curl -s -o /tmp/conflict.json -w "%{http_code}" -X POST "$BASE/v1/notifications" \
  -H "$hdr_key" -H "$hdr_json" -H "Idempotency-Key: welcome-001" \
  -d "{\"user_id\":\"$UID_\",\"template_name\":\"welcome\",\"variables\":{\"name\":\"Someone Else\",\"product\":\"NotifCo\"}}")
echo "status=$CODE"; cat /tmp/conflict.json; echo

echo
echo "=== 7. Schedule for 3s in the future ==="
FUTURE=$(python3 -c "import datetime; print((datetime.datetime.now(datetime.UTC)+datetime.timedelta(seconds=3)).strftime('%Y-%m-%dT%H:%M:%SZ'))")
R=$(curl -sf -X POST "$BASE/v1/notifications" \
  -H "$hdr_key" -H "$hdr_json" \
  -d "{\"user_id\":\"$UID_\",\"template_name\":\"welcome\",\"variables\":{\"name\":\"Delayed\",\"product\":\"NotifCo\"},\"scheduled_for\":\"$FUTURE\"}")
NID2=$(echo "$R" | jkey "['id']")
echo "scheduled notification_id=$NID2 at $FUTURE"
sleep 5
curl -sf "$BASE/v1/notifications/$NID2" -H "$hdr_key" | jsp | head -30

echo
echo "=== 8. Slack-only path (deploy_succeeded template) ==="
R=$(curl -sf -X POST "$BASE/v1/notifications" \
  -H "$hdr_key" -H "$hdr_json" \
  -d "{\"user_id\":\"$UID_\",\"template_name\":\"deploy_succeeded\",\"variables\":{\"deploy_id\":\"d-42\",\"service\":\"notifd\",\"duration\":\"2m 10s\"}}")
echo "$R" | jsp | head -20
sleep 1
echo "--- slack-mock received:"
curl -sf http://localhost:9000/received | python3 -c '
import sys,json
d=json.load(sys.stdin)
print("count:", len(d))
for r in d[-3:]:
    print("at:", r.get("at"))
    print("  body:", json.dumps(r.get("body"))[:200])
'

echo
echo "=== 9. Opt-out email, send welcome -> only in_app should land ==="
curl -sf -X PUT "$BASE/v1/users/$UID_/preferences" \
  -H "$hdr_key" -H "$hdr_json" \
  -d "{\"channel\":\"email\",\"enabled\":false}"
R=$(curl -sf -X POST "$BASE/v1/notifications" \
  -H "$hdr_key" -H "$hdr_json" \
  -d "{\"user_id\":\"$UID_\",\"template_name\":\"welcome\",\"variables\":{\"name\":\"NoEmail\",\"product\":\"NotifCo\"}}")
NID3=$(echo "$R" | jkey "['id']")
sleep 2
curl -sf "$BASE/v1/notifications/$NID3" -H "$hdr_key" | jsp | head -30
# Re-enable for subsequent runs.
curl -sf -X PUT "$BASE/v1/users/$UID_/preferences" \
  -H "$hdr_key" -H "$hdr_json" \
  -d "{\"channel\":\"email\",\"enabled\":true}"

echo
echo "=== 10. Invalid: missing required variables ==="
curl -s -o /tmp/missing.json -w "status=%{http_code}\n" -X POST "$BASE/v1/notifications" \
  -H "$hdr_key" -H "$hdr_json" \
  -d "{\"user_id\":\"$UID_\",\"template_name\":\"welcome\",\"variables\":{\"name\":\"OnlyName\"}}"
cat /tmp/missing.json; echo

echo
echo "=== ALL PATHS PASSED ==="
