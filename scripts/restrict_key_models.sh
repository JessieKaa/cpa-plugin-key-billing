#!/usr/bin/env bash
set -euo pipefail

# Restrict one downstream API key to an allow-list of models and a quota, then
# verify the result end to end.
#
# A key becomes managed only when a subscription plan is bound to it, so the
# plan carries the quota and the routing rule carries the model allow-list.
#
# Usage:
#   scripts/restrict_key_models.sh --key <downstream key> --models <model[,model...]> \
#     --price <input>,<output>[,<cache_read>[,<cache_write>]] [--amount <USD>] \
#     [--period <seconds>] [--config <CPA config.yaml>] [--deny-probe <model>] \
#     [--base-url <url>] [--management-key <key>] [--quota-probe] [--skip-probe] [--rollback]
#
#   --key          Downstream API key to restrict.
#   --models       Allowed model IDs, comma separated, exactly as /v1/models
#                  reports them (for example mmyglm/kimi).
#   --price        Custom price in USD per 1M tokens for every allowed model.
#                  Required for a model without a models.dev reference price: a
#                  managed key is refused with 503 model_price_error otherwise.
#   --amount       Quota for the plan window in USD. Default 1000000 (in effect
#                  unlimited). 0 disables the money dimension, so use a small
#                  positive value for a real cap.
#   --period       Quota window length in seconds. Default 86400 (one day).
#                  The cycle starts at the first admitted request.
#   --config       CPA config. When given, its api-keys list is synchronized
#                  first so every configured key stays known to the plugin.
#   --deny-probe   Model expected to be refused. Default: the first model from
#                  /v1/models that is not in the allow-list.
#   --quota-probe  Also prove the money cap blocks: read the accumulated spend,
#                  lower the window amount to a nominal value, expect 429, then
#                  restore the amount. Refusals never reach an upstream.
#   --rollback     Unbind the plan instead of configuring. Use it to undo.
#
# Only the allowed-model probes can reach an upstream provider.

base_url="http://127.0.0.1:8317"
management_key="${CPA_MANAGEMENT_KEY:-}"
config_file=""
downstream_key=""
allowed_models=""
deny_probe=""
price=""
amount="1000000"
period="86400"
skip_probe=0
quota_probe=0
rollback=0
failures=0

usage() {
  echo "用法：$0 --key <下游 key> --models <模型[,模型...]> --price <输入>,<输出>[,<缓存读>[,<缓存写>]]" >&2
  echo "         [--amount <USD>] [--period <秒>] [--config <config.yaml>] [--deny-probe <模型>]" >&2
  echo "         [--base-url <url>] [--management-key <key>] [--quota-probe] [--skip-probe] [--rollback]" >&2
}

while (( $# > 0 )); do
  case "$1" in
    --key) downstream_key="$2"; shift 2 ;;
    --models) allowed_models="$2"; shift 2 ;;
    --price) price="$2"; shift 2 ;;
    --amount) amount="$2"; shift 2 ;;
    --period) period="$2"; shift 2 ;;
    --config) config_file="$2"; shift 2 ;;
    --deny-probe) deny_probe="$2"; shift 2 ;;
    --base-url) base_url="$2"; shift 2 ;;
    --management-key) management_key="$2"; shift 2 ;;
    --quota-probe) quota_probe=1; shift ;;
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
python3 -c 'import sys
parts = [p.strip() for p in sys.argv[1].split(",")]
if not 2 <= len(parts) <= 4 or any(not p for p in parts):
    raise SystemExit(1)
values = [float(p) for p in parts]
if any(v < 0 for v in values):
    raise SystemExit(1)' "$price" ||
  { echo "--price 需要 <输入>,<输出>[,<缓存读>[,<缓存写>]]，单位为 USD / 1M token" >&2; exit 2; }
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

mgmt() { # <method> <path> [curl args...]
  local method="$1" path="$2"; shift 2
  curl -sS --max-time 30 -X "$method" -H "Authorization: Bearer $management_key" \
    -H "Content-Type: application/json" "$@" "$base_url$path"
}

key_view() { # prints "<status> <planId>" for the target scope, or "" when absent
  mgmt GET "/v0/management/plugins/cpa-key-billing/keys" | python3 -c 'import json,sys
for key in json.load(sys.stdin).get("keys") or []:
    if key.get("scope") == sys.argv[1]:
        print("%s %s" % (key.get("status"), key.get("plan_id") or "-"))
        break' "$scope"
}

scope="$(scope_of "$downstream_key")"
short="${downstream_key:0:4}"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT

if (( rollback )); then
  printf '==> 解绑 %s…（scope %s…）\n' "$short" "${scope:0:12}"
  result="$(mgmt POST "/v0/management/plugins/cpa-key-billing/keys/unbind" --data "{\"scope\":\"$scope\"}")"
  if printf '%s' "$result" | grep -q '"error"'; then echo "$result"; exit 1; fi
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
  if printf '%s' "$result" | grep -q '"error"'; then fail "同步失败：$result"; else ok "已同步 $result"; fi
fi

printf '==> 订阅计划与额度\n'
plan_id="$(key_view | awk '{print $2}')"
if [[ "$plan_id" != "-" && -n "$plan_id" ]]; then
  ok "该 Key 已绑定计划 $plan_id，沿用其额度设置"
  plan_id="$(key_view | awk '{print $2}')"
else
  window_name="Window"
  [[ "$period" == "86400" ]] && window_name="Daily"
  result="$(mgmt POST "/v0/management/plugins/cpa-key-billing/plans" --data "{\"name\":\"restrict-$short-plan\",\"windows\":[{\"name\":\"$window_name\",\"period_seconds\":$period,\"amount_usd\":$amount,\"request_limit\":0,\"token_limit\":0}],\"scopes\":[\"$scope\"]}")"
  plan_id="$(printf '%s' "$result" | python3 -c 'import json,sys
try: print((json.load(sys.stdin).get("plan") or {}).get("id",""))
except Exception: print("")')"
  if [[ -z "$plan_id" ]]; then
    fail "创建计划失败：$result"
  else
    ok "已创建并绑定计划 $plan_id：$amount USD / $period 秒"
  fi
fi

printf '==> 绑定模型白名单路由\n'
models_json="$(python3 -c 'import json,sys;print(json.dumps([m.strip() for m in sys.argv[1].split(",") if m.strip()]))' "$allowed_models")"
result="$(mgmt POST "/v0/management/plugins/cpa-key-billing/routes" --data "{\"name\":\"restrict-$short-route\",\"rule\":{\"models\":$models_json,\"denied_models\":[],\"credential_ids\":[],\"credential_providers\":[]},\"scopes\":[\"$scope\"]}")"
if printf '%s' "$result" | grep -q '"error"'; then fail "创建路由失败：$result"; else ok "白名单：$allowed_models"; fi

printf '==> 白名单模型定价（无价会被 503 model_price_error 拒绝）\n'
missing_prices=""
price_of() {
  curl -sS --max-time 20 -H "Authorization: Bearer $management_key" \
    "$base_url/v0/management/plugins/cpa-key-billing/prices?model=$1" | python3 -c 'import json,sys
rows = json.load(sys.stdin)
print(rows[0].get("source", "none") if rows else "none")' 2>/dev/null || echo none
}
while IFS= read -r model; do
  [[ -n "$model" ]] || continue
  source="$(price_of "$model")"
  if [[ "$source" == "none" ]]; then
    mgmt PUT "/v0/management/plugins/cpa-key-billing/prices" --data "$(python3 -c 'import json,sys
parts = (sys.argv[1] + ",0,0").split(",")
values = [float(parts[i]) if i < len(parts) and parts[i].strip() else 0.0 for i in range(4)]
print(json.dumps({"model_id": sys.argv[2], "input_per_1m": values[0], "output_per_1m": values[1],
                  "cache_read_per_1m": values[2], "cache_write_per_1m": values[3]}))' "$price" "$model")" >/dev/null
    source="$(price_of "$model")"
  fi
  if [[ "$source" == "none" ]]; then
    missing_prices="$missing_prices $model"
    printf '  ✗ %s 无价格\n' "$model"
  else
    printf '  ✓ %s 价格来源：%s\n' "$model" "$source"
  fi
done < <(python3 -c 'import sys;print("\n".join(m.strip() for m in sys.argv[1].split(",") if m.strip()))' "$allowed_models")
if [[ -n "${missing_prices// /}" ]]; then
  fail "以下模型没有价格：${missing_prices# }。用 --price <输入>,<输出> 提供 USD / 1M token 单价"
fi

printf '==> Key 状态\n'
status_line="$(key_view)"
printf '  %s\n' "${status_line:-未找到（同步是否成功？）}"
mgmt GET "/v0/management/plugins/cpa-key-billing/keys" | python3 -c 'import json,sys
for key in json.load(sys.stdin).get("keys") or []:
    if key.get("scope") != sys.argv[1]:
        continue
    if not key.get("windows"):
        print("  尚无活动额度周期（首次放行后开始计时）")
    for window in key.get("windows") or []:
        dims = "，".join("%s %s/%s%s" % (d.get("metric"), d.get("used"), d.get("limit"),
                                        "（已拦截）" if d.get("blocked") else "")
                        for d in window.get("dimensions") or [])
        print("  额度窗口 %s（%s 秒）：%s" % (window.get("name"), window.get("period_seconds"), dims or "无限制"))
    break' "$scope"

if (( skip_probe )); then
  printf '\n配置完成（未做请求验证）。回滚：%s --key <key> --models x --price 0,0 --rollback\n' "$0"
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

post_chat() { # <model> <out-file> -> http status
  curl -sS --max-time 60 -o "$2" -w '%{http_code}' -X POST \
    -H "Content-Type: application/json" -H "Authorization: Bearer $downstream_key" \
    --data "{\"model\":\"$1\",\"messages\":[{\"role\":\"user\",\"content\":\"ping\"}],\"max_tokens\":1,\"stream\":false}" \
    "$base_url/v1/chat/completions"
}

events_total() {
  mgmt GET "/v0/management/plugins/cpa-key-billing/events?limit=1" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("total"))'
}

events_before="$(events_total)"
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

events_after="$(events_total)"
printf '==> 请求事件计数：%s → %s\n' "${events_before:-?}" "${events_after:-?}"
if (( ${events_after:-0} > ${events_before:-0} )); then
  ok "已记账（新增 $((events_after - events_before)) 条）"
else
  fail "允许的模型请求没有产生事件（纳管后应记账）"
fi

if (( quota_probe )); then
  printf '==> 额度上限探针（临时把上限降到极小值，验证会 429，随后恢复）\n'
  spent="0"
  for _ in $(seq 1 30); do
    spent="$(mgmt GET "/v0/management/plugins/cpa-key-billing/keys" | python3 -c 'import json,sys
for key in json.load(sys.stdin).get("keys") or []:
    if key.get("scope") != sys.argv[1]:
        continue
    for window in key.get("windows") or []:
        for dimension in window.get("dimensions") or []:
            if dimension.get("metric") == "amount_usd":
                print(dimension.get("used"))
                raise SystemExit(0)
    raise SystemExit(0)
print(0)' "$scope")"
  [[ -n "$spent" ]] || spent=0
    python3 -c 'import sys; sys.exit(0 if float(sys.argv[1]) > 0 else 1)' "$spent" && break
    sleep 0.5
  done
  if python3 -c 'import sys; sys.exit(0 if float(sys.argv[1]) > 0 else 1)' "$spent"; then
    original_windows="$(mgmt GET "/v0/management/plugins/cpa-key-billing/plans" | python3 -c 'import json,sys
for plan in json.load(sys.stdin).get("plans") or []:
    if plan.get("id") == sys.argv[1]:
        print(json.dumps(plan.get("windows") or []))
        break' "$plan_id")"
    patch() { # <amount>
      mgmt PATCH "/v0/management/plugins/cpa-key-billing/plans" --data "$(python3 -c 'import json,sys
windows = json.loads(sys.argv[1])
for window in windows:
    window["amount_usd"] = float(sys.argv[2])
print(json.dumps({"id": sys.argv[3], "windows": windows}))' "$original_windows" "$1" "$plan_id")" >/dev/null
    }
    patch 0.000001
    status="$(post_chat "$allowed_probe" "$tmp/quota.json")"
    patch "$amount"
    if [[ "$status" == "429" ]] && grep -q 'rate_limit_error' "$tmp/quota.json"; then
      ok "额度上限生效：返回 429 rate_limit_error（零上游成本）"
    else
      fail "预期 429 rate_limit_error，实际 HTTP $status $(head -c 200 "$tmp/quota.json")"
    fi
    blocked="$(mgmt GET "/v0/management/plugins/cpa-key-billing/keys" | python3 -c 'import json,sys
for key in json.load(sys.stdin).get("keys") or []:
    if key.get("scope") == sys.argv[1]:
        print((key.get("windows") or [{}])[0].get("blocked", False))
        break
else: print(False)' "$scope")"
    if [[ "$blocked" == "False" || "$blocked" == "false" ]]; then
      ok "上限已恢复为 $amount USD，Key 不再被拦截"
    else
      fail "上限未恢复，请手动执行：PATCH /plans 把 amount_usd 改回 $amount（计划 $plan_id）"
    fi
  else
    fail "首个请求未记录消费（spent=$spent），无法验证额度上限，已跳过"
  fi
fi

printf '\n回滚：%s --key %s… --models x --price 0,0 --rollback\n' "$0" "$short"
if (( failures > 0 )); then
  echo "处理完成，但有 ${failures} 项失败。"
  exit 1
fi
echo "完成：该 Key 只能调用 $allowed_models，额度 $amount USD / $period 秒。"
