#!/usr/bin/env bash
# Thử nhanh platform đang chạy trên máy cục bộ (make up-all, hoặc make run-*).
#
#   make smoke                         # phần KHÔNG tốn tiền: sức khoẻ, đăng nhập, tool call qua Gateway
#   HQ_SMOKE_MODEL=<id model của provider> \
#   HQ_SMOKE_PRICE_IN=<USD / 1M token vào> HQ_SMOKE_PRICE_OUT=<USD / 1M token ra> \
#     make smoke                       # thêm phần TỐN TIỀN THẬT: một execution của Agent
#
# Phần tốn tiền dùng HQ_ANTHROPIC_API_KEY trong .env làm khoá của organization: script gửi
# khoá tới API cục bộ (http://127.0.0.1:8080), API kiểm nó với provider rồi mã hoá và lưu.
# Khoá không được in ra. Model và giá do bạn đưa vào: source không chứa tên model hay giá.
# Budget của Agent thử là 0.20 USD cho mỗi execution.
set -euo pipefail
cd "$(dirname "$0")/.."

API=${HQ_SMOKE_API:-http://127.0.0.1:8080}
GW=${HQ_SMOKE_GATEWAY:-http://127.0.0.1:8081}
KC=${HQ_SMOKE_KEYCLOAK:-http://localhost:8180}
USER_NAME=${HQ_SMOKE_USER:-dev}
USER_PASS=${HQ_SMOKE_PASSWORD:-dev}
OUT=$(mktemp)
trap 'rm -f "$OUT"' EXIT

say()  { printf '\n== %s\n' "$*"; }
ok()   { printf '   ok   %s\n' "$*"; }
fail() { printf '   LỖI  %s\n' "$*" >&2; exit 1; }
# field <biểu thức python trên biến d>: đọc một trường từ JSON trong $OUT.
field() { python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); print(eval(sys.argv[2]) or "")' "$OUT" "$1" 2>/dev/null || true; }
# call <METHOD> <path> [json body]: gọi Management API, in mã HTTP, thân vào $OUT.
call() {
  local method=$1 path=$2 body=${3:-}
  local args=(-s -o "$OUT" -w '%{http_code}' -X "$method" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json')
  if [ "$method" = POST ]; then args+=(-H "Idempotency-Key: smoke-$(date +%s%N)"); fi
  if [ -n "$body" ]; then args+=(--data-binary "$body"); fi
  curl "${args[@]}" "$API$path"
}
must() { # must <mã mong đợi> <METHOD> <path> [body]
  local want=$1; shift
  local got; got=$(call "$@")
  [ "$got" = "$want" ] || fail "$1 $2 → HTTP $got (muốn $want): $(head -c 400 "$OUT")"
}

say "1. Sức khoẻ của ba process"
for pair in "api $API" "gateway $GW" "worker ${HQ_SMOKE_WORKER:-http://127.0.0.1:8082}"; do
  set -- $pair
  code=$(curl -s -o /dev/null -w '%{http_code}' "$2/readyz" || true)
  [ "$code" = 200 ] && ok "$1 sẵn sàng" || fail "$1 ($2/readyz) trả $code; xem: make logs-all"
done

say "2. Đăng nhập ($USER_NAME) qua Keycloak"
TOKEN=$(curl -s -X POST "$KC/realms/hq/protocol/openid-connect/token" -d grant_type=password -d client_id=hq-cli \
  -d "username=$USER_NAME" -d "password=$USER_PASS" -d scope=openid | python3 -c 'import sys,json; print(json.load(sys.stdin).get("access_token",""))')
[ -n "$TOKEN" ] || fail "không lấy được token từ $KC (Keycloak đã chạy chưa?)"
must 200 GET /v1/organization
ok "organization: $(field 'd["slug"]') ($(field 'd["id"]'))"

say "3. Project"
must 200 GET /v1/projects
PROJECT=$(field 'next((p["id"] for p in d["data"] if p["slug"]=="ops"), "")')
if [ -z "$PROJECT" ]; then
  must 201 POST /v1/projects '{"slug":"ops","name":"Ops","environment":"non_prod"}'
  PROJECT=$(field 'd["id"]')
fi
ok "project ops: $PROJECT"

say "4. Tool call qua Gateway (MCP): docker.list_containers"
must 200 GET /v1/mcp-servers
SERVER=$(field 'next((s["id"] for s in d["data"] if s["slug"]=="docker"), "")')
if [ -z "$SERVER" ]; then
  echo "   bỏ qua: chưa có MCP server 'docker' trong organization này."
  echo "   Đăng ký: make admin ARGS='mcp-server register -slug docker -name Docker -endpoint http://integration-docker:8090/mcp -org <slug org>'"
else
  code=$(curl -s -o "$OUT" -w '%{http_code}' -X POST "$GW/mcp/v1/projects/$PROJECT" -H "Authorization: Bearer $TOKEN" \
    -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
    -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"docker.list_containers","arguments":{}}}')
  [ "$code" = 200 ] || fail "Gateway trả HTTP $code: $(head -c 300 "$OUT")"
  [ "$(field 'd["result"].get("isError", False)')" != True ] || fail "tool trả lỗi: $(field 'd["result"]["content"][0]["text"]')"
  ok "tool chạy; $(field 'len(json.loads(d["result"]["content"][0]["text"]).get("containers", []))') container"
  must 200 GET '/v1/audit-logs?action=mcp_tool.call&limit=1'
  ok "audit: $(field 'd["data"][0]["action"]') / $(field 'd["data"][0]["outcome"]')"
fi

if [ -z "${HQ_SMOKE_MODEL:-}" ]; then
  say "Xong phần không tốn tiền."
  echo "   Để chạy một execution của Agent (tốn tiền thật trên khoá Anthropic), đặt:"
  echo "     HQ_SMOKE_MODEL=<id model>  HQ_SMOKE_PRICE_IN=<USD/1M token vào>  HQ_SMOKE_PRICE_OUT=<USD/1M token ra>  make smoke"
  exit 0
fi
: "${HQ_SMOKE_PRICE_IN:?đặt HQ_SMOKE_PRICE_IN (USD cho 1 triệu token vào, theo bảng giá của provider)}"
: "${HQ_SMOKE_PRICE_OUT:?đặt HQ_SMOKE_PRICE_OUT (USD cho 1 triệu token ra)}"

say "5. Model provider của organization (adapter anthropic, khoá lấy từ .env)"
must 200 GET /v1/model-providers
PROVIDER=$(field 'next((p["id"] for p in d["data"] if p["slug"]=="anthropic-smoke"), "")')
if [ -z "$PROVIDER" ]; then
  KEY=$(set -a; . ./.env; set +a; printf %s "${HQ_ANTHROPIC_API_KEY:-}")
  [ -n "$KEY" ] || fail "HQ_ANTHROPIC_API_KEY chưa có trong .env"
  # Thân request đi qua stdin của curl, không nằm trên dòng lệnh (không lộ trong danh sách process).
  code=$(python3 -c 'import json,sys; print(json.dumps({"slug":"anthropic-smoke","adapter":"anthropic","api_key":sys.argv[1]}))' "$KEY" |
    curl -s -o "$OUT" -w '%{http_code}' -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' --data-binary @- "$API/v1/model-providers")
  unset KEY
  [ "$code" = 201 ] || fail "tạo provider → HTTP $code: $(field 'd.get("detail")') (422: khoá bị provider từ chối; 503: không kiểm được khoá)"
  PROVIDER=$(field 'd["id"]')
fi
ok "provider: $PROVIDER"

say "6. Model $HQ_SMOKE_MODEL và giá"
must 200 GET /v1/models
MODEL=$(PM=$HQ_SMOKE_MODEL PV=$PROVIDER field 'next((m["id"] for m in d["data"] if m["provider_id"]==__import__("os").environ["PV"] and m["provider_model"]==__import__("os").environ["PM"]), "")')
if [ -z "$MODEL" ]; then
  must 201 POST /v1/models "$(python3 -c 'import json,sys; print(json.dumps({"provider_id":sys.argv[1],"provider_model":sys.argv[2],"display_name":sys.argv[2],"tier":"standard","capabilities":{"tool_calling":True,"context_window":200000,"max_output_tokens":4096}}))' "$PROVIDER" "$HQ_SMOKE_MODEL")"
  MODEL=$(field 'd["id"]')
  must 201 POST "/v1/models/$MODEL/pricing" "$(python3 -c 'import json,sys,datetime; print(json.dumps({"currency":"USD","input_per_mtok":sys.argv[1],"output_per_mtok":sys.argv[2],"effective_from":datetime.datetime.now(datetime.timezone.utc).isoformat()}))' "$HQ_SMOKE_PRICE_IN" "$HQ_SMOKE_PRICE_OUT")"
fi
ok "model: $MODEL"

say "7. Agent DevOps chỉ đọc (3 tool Docker, budget 0.20 USD)"
must 200 GET "/v1/projects/$PROJECT/agents"
AGENT=$(field 'next((a["id"] for a in d["data"] if a["slug"]=="devops-smoke"), "")')
if [ -z "$AGENT" ]; then
  [ -n "$SERVER" ] || fail "cần MCP server 'docker' đã đăng ký (xem bước 4)"
  must 201 POST "/v1/projects/$PROJECT/agents" "$(python3 -c '
import json,sys
print(json.dumps({"slug":"devops-smoke","name":"DevOps (smoke)",
  "system_prompt":"Bạn là trợ lý DevOps chỉ đọc. Dùng tool để xem container Docker rồi trả lời ngắn gọn bằng tiếng Việt. Không suy đoán khi chưa gọi tool.",
  "config":{"model":{"policy":"fixed","fixed_model_id":sys.argv[1]},
            "limits":{"max_iterations":6,"max_tool_calls":8,"timeout_seconds":120,"budget":{"amount":"0.20","currency":"USD"}},
            "tools":[{"server_id":sys.argv[2],"allow":["list_containers","inspect_container","get_container_logs"]}]}}))' "$MODEL" "$SERVER")"
  AGENT=$(field 'd["id"]')
  must 200 PUT "/v1/agents/$AGENT/active-version" '{"version":1}'
fi
ok "agent: $AGENT"

say "8. Execution (gọi model thật)"
must 202 POST "/v1/projects/$PROJECT/executions" "$(python3 -c 'import json,sys; print(json.dumps({"mode":"single_agent","agent_id":sys.argv[1],"input":{"message":sys.argv[2]}}))' \
  "$AGENT" "${HQ_SMOKE_PROMPT:-Liệt kê các container đang chạy trên máy này và cho biết container nào không ở trạng thái running.}")"
EXEC=$(field 'd["id"]')
ok "execution: $EXEC (theo dõi trên Temporal: http://localhost:8233)"
status=queued
for _ in $(seq 1 60); do
  must 200 GET "/v1/executions/$EXEC"
  status=$(field 'd["status"]')
  case "$status" in completed|failed|cancelled|timed_out|budget_exceeded) break;; esac
  sleep 2
done
echo "   trạng thái: $status"
python3 - "$OUT" <<'PY'
import json,sys
d=json.load(open(sys.argv[1]))
out=d.get("output") or {}
print("   chi phí ước tính:", d.get("estimated_cost") or d.get("usage") or "(xem /v1/usage/records)")
if d.get("error"): print("   lỗi:", json.dumps(d["error"], ensure_ascii=False))
msg=out.get("message") if isinstance(out, dict) else None
if msg: print("\n--- Trả lời của Agent ---\n"+msg[:3000])
PY
must 200 GET "/v1/executions/$EXEC/tasks"
TASK=$(field 'd["data"][0]["id"]')
must 200 GET "/v1/tasks/$TASK/steps"
echo "   các bước: $(field '", ".join(s["kind"]+("("+s["tool_name"]+")" if s.get("tool_name") else "")+"="+s["status"] for s in d["data"])')"
[ "$status" = completed ] || fail "execution kết thúc ở trạng thái $status"
say "Xong: execution hoàn thành."
