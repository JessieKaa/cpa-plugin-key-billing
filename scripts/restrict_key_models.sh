#!/usr/bin/env bash
set -euo pipefail

# Restrict one downstream API key to an allow-list of models and verify it
# end to end.
#
# A key becomes managed only when a subscription plan is bound to it, so this
# script binds a plan with effectively unlimited quota and then binds a routing
# rule whose model allow-list is the requested set. Everything else about the
# key keeps working; only the model check is added.
#
# Usage:
#   scripts/restrict_key_models.sh --key <downstream key> --models <model[,model...]> \
#     [--config <CPA config.yaml>] [--deny-probe <model>] [--base-url <url>] \
#     [--management-key <key>] [--skip-probe] [--rollback]
#
#   --key            Downstream API key to restrict.
#   --models         Allowed model IDs, comma separated, exactly as /v1/models
#                    reports them (for example mmyglm/kimi).
#   --config         CPA config. When given, its api-keys list is synchronized
#                    first, keeping every configured key known to the plugin.
#   --deny-probe     Model expected to be refused. Default: the first model from
#                    /v1/models that is not in the allow-list.
#   --price          Custom price for every allowed model that has none, as
#                    <input>,<output>[,<cache_read>,<cache_write>] in USD per
#                    1M tokens. A managed key is refused with 503
#                    model_price_error when its model has no custom price and no
#                    models.dev reference price, which is normal for a prefixed
#                    model ID.
#   --rollback       Unbind the plan instead of configuring. Use it to undo.
#
# Every probe request stays inside the plugin when it is refused, so only the
# allowed-model probe can reach an upstream provider.

base_url="http://127.0.0.1:8317"
management_key="${CPA_MANAGEMENT_KEY:-}"
config_file=""
downstream_key=""
allowed_models=""
deny_probe=""
price=""
skip_probe=0
rollback=0
failures=0

usage() {
  echo "用法：$0 --key <下游 key> --models <模型[,模型...]> [--config <config.yaml>] [--deny-probe <模型>]" >&2
  echo "         [--base-url <url>] [--management-key <key>] [--price i,o[,cr,cw]] [--skip-probe] [--rollback]" >&2
}

while (( $# > 0 )); do
  case "$1" in
    --key) downstream_key="$2"; shift 2 ;;
    --models) allowed_models="$2"; shift 2 ;;
    --config) config_file="$2"; shift 2 ;;
    --deny-probe) deny_probe="$2"; shift 2 ;;
    --price) price="$2"; shift 2 ;;
    --base-url) base_url="$2"; shift 2 ;;
    --management-key) management_key="$2"; shift 2 ;;
    --skip-probe) skip_probe=1; shift ;;
    --rollback) rollback=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "未知参数：$1" >&2; usage; exit 2 ;;
  esac
done

command -v python3 >/dev/null 2>&1 || { echo "缺少命令：python3" >&2; exit 1; }
base_url="${base_url%/}"
[[ -n "$downstream_key" ]] || { echo "缺少 --key" >&2; usage; exit 2; }
[[ -n "$allowed_models" ]] || { echo "缺少 --models" >&2; usage; exit 2; }
[[ -f "$config_file" || -z "$config_file" ]] || { echo "配置文件不存在：$config_file" >&2; exit 1; }
if [[ -z "$management_key" ]]; then
  [[ -t 0 ]] || { echo "缺少管理密钥：请设置 CPA_MANAGEMENT_KEY 或使用 --management-key" >&2; exit 2; }
  read -rsp "Management key: " management_key
  echo
fi

fail() { printf '  ✗ %s\n' "$*"; failures=$((failures + 1)); }
ok()   { printf '  ✓ %s\n' "$*"; }

scope_of() {
  python3 -c 'import hashlib,sys
print(hashlib.sha256(b"cli-proxy-api:caller-scope:v1\x00" + sys.argv[1].encode()).hexdigest())' "$1"
}

mgmt() { # <method> <path> [--data <json>]
  local method="$1" path="$2"; shift 2
  curl -sS --max-time 30 -X "$method" -H "Authorization: Bearer $management_key" \
    -H "Content-Type: application/json" "$@" "$base_url$path"
}

scope="$(scope_of "$downstream_key")"
short="${downstream_key:0:4}"

if (( rollback )); then
  printf '==> 解绑 %s…（scope %s…）\n' "$short" "${scope:0:12}"
  result="$(mgmt POST "/v0/management/plugins/cpa-key-billing/keys/unbind" --data "{\"scope\":\"$scope\"}")"
  if printf '%s' "$result" | grep -q '"error"'; then
    echo "$result"; exit 1
  fi
  echo "  已解绑。该 Key 恢复为非托管：不限额、不记账、不限制模型。"
  exit 0
fi

if [[ -n "$config_file" ]]; then
  printf '==> 同步配置中的 API Key\n'
  payload="$(python3 -c 'import json,sys,yaml
cfg = yaml.safe_load(open(sys.argv[1])) or {}
keys = [k for k in (cfg.get("api-keys") or []) if isinstance(k, str) and k.strip()]
if sys.argv[2] not in keys:
    keys.append(sys.argv[2])
print(json.dumps({"keys": keys}))' "$config_file" "$downstream_key")"
  result="$(mgmt POST "/v0/management/plugins/cpa-key-billing/keys/sync" --data "$payload")"
  if printf '%s' "$result" | grep -q '"error"'; then
    fail "同步失败：$result"
  else
    ok "已同步 ($result)"
  fi
fi

printf '==> 绑定订阅计划（额度设为实际上限，仅用于开启纳管）\n'
plan_id="$(mgmt GET "/v0/management/plugins/cpa-key-billing/keys" | python3 -c 'import json,sys
for key in json.load(sys.stdin).get("keys") or []:
    if key.get("scope") == sys.argv[1]:
        print(key.get("plan_id") or "")
        break' "$scope")"
if [[ -n "$plan_id" ]]; then
  ok "该 Key 已绑定计划 $plan_id，复用"
else
  plan_name="restrict-$short-plan"
  result="$(mgmt POST "/v0/management/plugins/cpa-key-billing/plans" --data "{\"name\":\"$plan_name\",\"windows\":[{\"name\":\"Unlimited\",\"period_seconds\":86400,\"amount_usd\":1000000,\"request_limit\":1000000000,\"token_limit\":1000000000000}],\"scopes\":[\"$scope\"]}")"
  plan_id="$(printf '%s' "$result" | python3 -c 'import json,sys
try: print((json.load(sys.stdin).get("plan") or {}).get("id",""))
except Exception: print("")')"
  if [[ -z "$plan_id" ]]; then
    fail "创建计划失败：$result"
  else
    ok "已创建并绑定计划 $plan_id"
  fi
fi

printf '==> 绑定模型白名单路由\n'
models_json="$(python3 -c 'import json,sys;print(json.dumps([m.strip() for m in sys.argv[1].split(",") if m.strip()]))' "$allowed_models")"
route_name="restrict-$short-route"
result="$(mgmt POST "/v0/management/plugins/cpa-key-billing/routes" --data "{\"name\":\"$route_name\",\"rule\":{\"models\":$models_json,\"denied_models\":[],\"credential_ids\":[],\"credential_providers\":[]},\"scopes\":[\"$scope\"]}")"
if printf '%s' "$result" | grep -q '"error"'; then
  fail "创建路由失败：$result"
else
  ok "白名单：$allowed_models"
fi

printf '==> 确保白名单模型有价格（纳管后无价会被 503 拒绝）\n'
missing_prices=""
while IFS= read -r model; do
  [[ -n "$model" ]] || continue
  source="$(curl -sS --max-time 20 -H "Authorization: Bearer $management_key" \
    "$base_url/v0/management/plugins/cpa-key-billing/prices?model=$model" | python3 -c 'import json,sys
rows = json.load(sys.stdin)
print(rows[0].get("source", "none") if rows else "none")' 2>/dev/null || echo none)"
  if [[ "$source" == "none" && -n "$price" ]]; then
    price_payload="$(python3 -c 'import json,sys
parts = (sys.argv[1] + ",0,0").split(",")
values = [float(parts[i]) if i < len(parts) and parts[i].strip() else 0.0 for i in range(4)]
print(json.dumps({"model_id": sys.argv[2], "input_per_1m": values[0], "output_per_1m": values[1],
                  "cache_read_per_1m": values[2], "cache_write_per_1m": values[3]}))' "$price" "$model")"
    mgmt PUT "/v0/management/plugins/cpa-key-billing/prices" --data "$price_payload" >/dev/null
    source="$(curl -sS --max-time 20 -H "Authorization: Bearer $management_key" \
      "$base_url/v0/management/plugins/cpa-key-billing/prices?model=$model" | python3 -c 'import json,sys
rows = json.load(sys.stdin)
print(rows[0].get("source", "none") if rows else "none")' 2>/dev/null || echo none)"
  fi
  if [[ "$source" == "none" ]]; then
    missing_prices="$missing_prices $model"
    printf '  ✗ %s 无价格\n' "$model"
  else
    printf '  ✓ %s 价格来源：%s\n' "$model" "$source"
  fi
done < <(python3 -c 'import sys;print("\n".join(m.strip() for m in sys.argv[1].split(",") if m.strip()))' "$allowed_models")
if [[ -n "${missing_prices// /}" ]]; then
  fail "以下模型没有价格，纳管后会被 503 model_price_error 拒绝：${missing_prices# }。请用 --price <input>,<output> 提供每 1M token 的 USD 单价，或先执行：curl -X PUT -H \"Authorization: Bearer <管理密钥>\" -H 'Content-Type: application/json' --data '{\"model_id\":\"<模型>\",\"input_per_1m\":<输入>,\"output_per_1m\":<输出>}' $base_url/v0/management/plugins/cpa-key-billing/prices"
fi

key_status="$(mgmt GET "/v0/management/plugins/cpa-key-billing/keys" | python3 -c 'import json,sys
for key in json.load(sys.stdin).get("keys") or []:
    if key.get("scope") == sys.argv[1]:
        print("%s plan=%s models=%s" % (key.get("status"), key.get("plan_id") or "-", key.get("route_bindings", {}).get("models")))
        break' "$scope")"
printf '==> Key 状态：%s\n' "${key_status:-未找到（同步是否成功？）}"

if (( skip_probe )); then
  printf '\n配置完成（未做请求验证）。回滚：%s --key <key> --models x --rollback\n' "$0"
  exit 0
fi

list_models() {
  curl -sS --max-time 20 -H "Authorization: Bearer $downstream_key" "$base_url/v1/models" |
    python3 -c 'import json,sys
try: print("\n".join(sorted(m["id"] for m in json.load(sys.stdin).get("data") or [])))
except Exception: pass'
}

allowed_probe="$(python3 -c 'import sys;print(sys.argv[1].split(",")[0].strip())' "$allowed_models")"
if [[ -z "$deny_probe" ]]; then
  deny_probe="$(list_models | python3 -c 'import sys
allowed = {m.strip() for m in sys.argv[1].split(",")}
for line in sys.stdin:
    model = line.strip()
    if model and model not in allowed:
        print(model); break' "$allowed_models")"
fi

post_chat() {
  local model="$1" out="$2"
  curl -sS --max-time 60 -o "$out" -w '%{http_code}' -X POST \
    -H "Content-Type: application/json" -H "Authorization: Bearer $downstream_key" \
    --data "{\"model\":\"$model\",\"messages\":[{\"role\":\"user\",\"content\":\"ping\"}],\"max_tokens\":1,\"stream\":false}" \
    "$base_url/v1/chat/completions"
}

tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
events_before="$(mgmt GET "/v0/management/plugins/cpa-key-billing/events?limit=1" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("total"))')"

printf '==> 允许的模型：%s\n' "$allowed_probe"
status="$(post_chat "$allowed_probe" "$tmp/allowed.json")"
if grep -q 'cpa_key_billing_error' "$tmp/allowed.json"; then
  fail "允许的模型被插件拒绝：HTTP $status $(head -c 200 "$tmp/allowed.json")"
else
  ok "未被插件拒绝（HTTP $status）"
fi

if [[ -n "$deny_probe" ]]; then
  printf '==> 白名单外的模型：%s\n' "$deny_probe"
  status="$(post_chat "$deny_probe" "$tmp/denied.json")"
  if [[ "$status" == "403" ]] && grep -qE '"code":"insufficient_quota"|permission_error' "$tmp/denied.json"; then
    ok "返回 403 permission_error"
  else
    fail "预期 403 permission_error，实际 HTTP $status $(head -c 200 "$tmp/denied.json")"
  fi
else
  printf '  ! 没有可用的拒绝探针模型（--deny-probe 未提供且 /v1/models 为空）\n'
fi

events_after="$(mgmt GET "/v0/management/plugins/cpa-key-billing/events?limit=1" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("total"))')"
printf '==> 请求事件计数：%s → %s\n' "${events_before:-?}" "${events_after:-?}"
if (( ${events_after:-0} > ${events_before:-0} )); then
  ok "已记账（新增 $((events_after - events_before)) 条）"
else
  fail "允许的模型请求没有产生事件（纳管后应记账）"
fi

printf '\n回滚：%s --key %s… --models x --rollback\n' "$0" "$short"
if (( failures > 0 )); then
  echo "处理完成，但有 ${failures} 项失败。"
  exit 1
fi
echo "完成：该 Key 现在只能调用 $allowed_models。"
