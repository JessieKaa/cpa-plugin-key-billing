#!/usr/bin/env bash
set -euo pipefail

# Post-deployment verification for a running CLIProxyAPI with cpa-key-billing
# (plan §13.4 step 6).
#
# Every check is read-only: registration, Management response headers, key
# status values, Management authentication, and the former Resource prefix.
# Behavioral checks (managed refusals, real passthrough) are separate, because
# CLIProxyAPI validates the requested model before plugin interceptors run, so
# a request for a model it does not know never reaches the plugin.
#
# Usage:
#   scripts/verify_deployment.sh [--base-url http://127.0.0.1:8317]
#
#   --base-url <url>     Running CLIProxyAPI base URL. Default: http://127.0.0.1:8317
#   --management-key <k> Management key in plaintext. Prefer CPA_MANAGEMENT_KEY
#                        or the interactive prompt: the config stores only a
#                        bcrypt hash, so the plaintext cannot be recovered from it.
#   --check-negative-auth
#                        Also verify that missing and wrong keys are rejected.
#                        Off by default: each rejected attempt counts toward
#                        CLIProxyAPI's 5-failure limit, after which the client IP
#                        is banned from Management for 30 minutes.
#
# Exit status is 1 when any check fails.

base_url="http://127.0.0.1:8317"
management_key="${CPA_MANAGEMENT_KEY:-}"
check_negative_auth=0
failures=0

usage() {
  echo "用法：$0 [--base-url <url>] [--management-key <key>] [--check-negative-auth]" >&2
}

while (( $# > 0 )); do
  case "$1" in
    --base-url) base_url="$2"; shift 2 ;;
    --management-key) management_key="$2"; shift 2 ;;
    --check-negative-auth) check_negative_auth=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "未知参数：$1" >&2; usage; exit 2 ;;
  esac
done

command -v curl >/dev/null 2>&1 || { echo "缺少命令：curl" >&2; exit 1; }
command -v python3 >/dev/null 2>&1 || { echo "缺少命令：python3" >&2; exit 1; }
base_url="${base_url%/}"

fail() { printf '  ✗ %s\n' "$*"; failures=$((failures + 1)); }
ok()   { printf '  ✓ %s\n' "$*"; }

if [[ -z "$management_key" ]]; then
  [[ -t 0 ]] || { echo "缺少管理密钥：请设置 CPA_MANAGEMENT_KEY 或使用 --management-key" >&2; exit 2; }
  read -rsp "Management key: " management_key
  echo
fi
[[ -n "$management_key" ]] || { echo "管理密钥为空" >&2; exit 2; }

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

mgmt_get() { # <body-file> <path> [extra curl args...]
  local body_file="$1" path="$2"; shift 2
  curl -sS --max-time 20 -o "$body_file" -w '%{http_code}' "$@" \
    -H "Authorization: Bearer $management_key" "$base_url$path"
}

json_field() { # <body-file> <dotted.path>
  python3 -c 'import json,sys
try:
    value = json.load(open(sys.argv[1]))
except Exception:
    print("")
    raise SystemExit(0)
for part in sys.argv[2].split("."):
    if part == "":
        continue
    if isinstance(value, list):
        value = value[int(part)] if part.isdigit() and int(part) < len(value) else None
    elif isinstance(value, dict):
        value = value.get(part)
    else:
        value = None
    if value is None:
        break
print("" if value is None else value)' "$1" "$2"
}

printf '==> 插件注册与启用：%s\n' "$base_url"
if [[ "$(mgmt_get "$tmp/plugins.json" "/v0/management/plugins")" != "200" ]]; then
  fail "GET /v0/management/plugins 未返回 200（管理密钥是否正确？）"
else
  case "$(python3 -c 'import json,sys
d=json.load(open(sys.argv[1]))
for plugin in d.get("plugins") or []:
    if plugin.get("id") == "cpa-key-billing":
        print("yes" if plugin.get("registered") and plugin.get("effective_enabled") else "no")
        break
else:
    print("missing")' "$tmp/plugins.json")" in
    yes) ok "已注册且启用" ;;
    no) fail "插件已列出，但未注册或未启用" ;;
    *) fail "管理接口未列出 cpa-key-billing" ;;
  esac
fi

printf '==> 管理响应头与 Key 状态\n'
if [[ "$(mgmt_get "$tmp/keys.json" "/v0/management/plugins/cpa-key-billing/keys" -D "$tmp/keys.headers")" != "200" ]]; then
  fail "GET .../keys 未返回 200"
else
  for expected in 'cache-control: private, no-store' 'pragma: no-cache' 'referrer-policy: no-referrer' 'x-content-type-options: nosniff'; do
    if grep -qiE "^${expected%%:*}: *${expected#*: }" "$tmp/keys.headers"; then
      ok "$expected"
    else
      fail "响应头缺少 $expected"
    fi
  done
  if broken="$(python3 -c 'import json,sys
allowed = {"unmanaged", "managed", "external-managed", "deleted"}
broken = [k.get("scope", "?") for k in json.load(open(sys.argv[1])).get("keys") or [] if k.get("status") not in allowed]
print(" ".join(broken))' "$tmp/keys.json")"; then
    if [[ -z "$broken" ]]; then
      ok "所有 Key 均带有效状态枚举"
    else
      fail "以下 Key 缺少有效状态：$broken"
    fi
  fi
fi

if [[ -s "$tmp/keys.json" ]]; then
  python3 -c 'import json,sys
keys = json.load(open(sys.argv[1])).get("keys") or []
if not keys:
    print("  ! 尚未同步任何 API Key")
for key in keys:
    print("  · %s status=%s plan=%s concurrency=%s" % (key.get("preview") or "?", key.get("status"), key.get("plan_id") or "-", key.get("concurrency_limit")))' "$tmp/keys.json"
fi
events_total="$(mgmt_get "$tmp/events.json" "/v0/management/plugins/cpa-key-billing/events?limit=1" >/dev/null && json_field "$tmp/events.json" total)"
errors_total="$(mgmt_get "$tmp/errors.json" "/v0/management/plugins/cpa-key-billing/errors?limit=1" >/dev/null && json_field "$tmp/errors.json" total)"
printf '==> 事件计数：请求 %s，错误 %s\n' "${events_total:-?}" "${errors_total:-?}"

printf '==> 旧 Resource 前缀\n'
for path in ui profile subscription routing prices analysis events errors auth-files; do
  target="$base_url/v0/resource/plugins/cpa-key-billing/$path"
  anon="$(curl -sS --max-time 10 -o /dev/null -w '%{http_code}' "$target")"
  authorized="$(curl -sS --max-time 10 -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $management_key" "$target")"
  if [[ "$anon" == "404" && "$authorized" == "404" ]]; then
    ok "/$path 404"
  else
    fail "/$path 匿名=$anon 管理=$authorized，预期均为 404"
  fi
done

if (( check_negative_auth )); then
  printf '==> 管理认证（负向用例）\n'
  no_key="$(curl -sS --max-time 10 -o /dev/null -w '%{http_code}' "$base_url/v0/management/plugins/cpa-key-billing/keys")"
  wrong_key="$(curl -sS --max-time 10 -o /dev/null -w '%{http_code}' -H "Authorization: Bearer wrong-management-key" "$base_url/v0/management/plugins/cpa-key-billing/keys")"
  for pair in "无密钥:$no_key" "错误密钥:$wrong_key"; do
    label="${pair%%:*}"; status="${pair#*:}"
    if [[ "$status" == "401" || "$status" == "403" ]]; then
      ok "$label HTTP $status"
    else
      fail "$label HTTP $status，预期 401/403"
    fi
  done
else
  printf '==> 管理认证（负向用例跳过）\n'
  printf '  ! 未传 --check-negative-auth：每次失败尝试都计入 5 次上限，超过后该 IP 被封 30 分钟\n'
fi

printf '\n'
if (( failures > 0 )); then
  echo "验证失败：${failures} 项。"
  exit 1
fi
echo "验证通过。"
