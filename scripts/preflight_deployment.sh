#!/usr/bin/env bash
set -euo pipefail

# Deployment preflight for the cpa-key-billing fork (plan §13.3).
#
# This script inspects the EFFECTIVE CLIProxyAPI configuration before a
# deployment and fails when the deployment shape is unsupported:
#
#   - plugin loading is disabled
#   - the cpa-key-billing instance is missing or disabled
#   - Home mode is verifiably enabled (Home selection runs before plugin
#     scheduler selection, so credential/provider restrictions cannot apply)
#
# It warns when another plugin instance is enabled (a competing scheduler
# would take over credential selection) and when Home mode cannot be
# determined. A startup log, when provided, is checked for successful
# registration of this plugin and for plugin errors, panics, or fusion.
#
# Usage:
#   scripts/preflight_deployment.sh <config.yaml> [--log-file <path>] [--process <pattern>]
#
#   --log-file   CLIProxyAPI startup log captured after the candidate start.
#   --process    Pattern matching the running CLIProxyAPI process (default:
#                cli-proxy-api). Used only to detect an active Home mode.

readonly plugin_id="cpa-key-billing"
config_file=""
log_file=""
process_pattern="cli-proxy-api"

[[ ${1:-} != "" ]] || { echo "用法：$0 <config.yaml> [--log-file <path>] [--process <pattern>]" >&2; exit 2; }
config_file="$1"
shift
while (( $# > 0 )); do
  case "$1" in
    --log-file)
      [[ ${2:-} != "" ]] || { echo "--log-file 需要一个路径" >&2; exit 2; }
      log_file="$2"; shift 2 ;;
    --process)
      [[ ${2:-} != "" ]] || { echo "--process 需要一个模式" >&2; exit 2; }
      process_pattern="$2"; shift 2 ;;
    *) echo "未知参数：$1" >&2; exit 2 ;;
  esac
done

command -v python3 >/dev/null 2>&1 || { echo "缺少命令：python3" >&2; exit 1; }
[[ -f "$config_file" ]] || { echo "配置文件不存在：$config_file" >&2; exit 1; }

failures=0
warnings=0

fail() { printf '  ✗ %s\n' "$*"; failures=$((failures + 1)); }
warn() { printf '  ! %s\n' "$*"; warnings=$((warnings + 1)); }
ok()   { printf '  ✓ %s\n' "$*"; }

# --- Effective configuration -------------------------------------------------

printf '==> 检查 CLIProxyAPI 配置：%s\n' "$config_file"

config_json="$(python3 - "$config_file" <<'PY'
import json, sys, yaml
try:
    with open(sys.argv[1], "r", encoding="utf-8") as handle:
        config = yaml.safe_load(handle) or {}
except Exception as error:
    print(json.dumps({"error": str(error)}))
    raise SystemExit(0)
print(json.dumps(config))
PY
)" || true

if [[ -z "$config_json" ]] || printf '%s' "$config_json" | jq -er '.error' >/dev/null 2>&1; then
  echo "配置不是有效的 YAML：$config_file" >&2
  exit 1
fi

plugins_enabled="$(printf '%s' "$config_json" | jq -r '.plugins.enabled // false')"
if [[ "$plugins_enabled" != "true" ]]; then
  fail "plugins.enabled 不为 true：动态插件加载被关闭，计费插件不会运行"
else
  ok "动态插件加载已开启"
fi

instance="$(printf '%s' "$config_json" | jq -r --arg id "$plugin_id" '.plugins.configs[$id] // empty')"
if [[ -z "$instance" ]]; then
  fail "plugins.configs.$plugin_id 缺失：该插件实例不会被加载"
elif [[ "$(printf '%s' "$instance" | jq -r '.enabled // false')" != "true" ]]; then
  fail "plugins.configs.$plugin_id.enabled 不为 true：该插件实例被停用"
else
  ok "计费插件实例已启用"
fi

other_plugins="$(printf '%s' "$config_json" | jq -r --arg id "$plugin_id" \
  '.plugins.configs | to_entries | map(select(.key != $id and ((.value.enabled // false) == true))) | map(.key) | join(" ")')"
if [[ -n "$other_plugins" ]]; then
  warn "存在其他已启用的插件实例：$other_plugins。CLIProxyAPI 只会采用一个调度器插件；请逐个确认它们不注册调度器，否则本插件的凭据路由限制不会生效"
else
  ok "没有其他已启用的插件实例"
fi

state_file="$(printf '%s' "$instance" | jq -r '.state_file // ""')"
if [[ -n "$state_file" ]]; then
  if [[ -f "$state_file" ]]; then
    ok "状态数据库存在：$state_file"
  else
    warn "状态数据库尚不存在，首次启动会创建：$state_file"
  fi
fi

# --- Home mode ----------------------------------------------------------------

printf '==> 检查 Home 模式（Home 选择先于插件调度，凭据路由限制不生效）\n'
home_detected=0
if command -v pgrep >/dev/null 2>&1; then
  # Home mode is enabled by the -home-jwt flag or HOME_JWT environment
  # variable; it never appears in config.yaml.
  while IFS= read -r pid; do
    [[ -n "$pid" ]] || continue
    if tr '\0' ' ' < "/proc/$pid/cmdline" 2>/dev/null | grep -q -- '-home-jwt'; then
      home_detected=1
      break
    fi
    if tr '\0' '\n' < "/proc/$pid/environ" 2>/dev/null | grep -q '^HOME_JWT='; then
      home_detected=1
      break
    fi
  done < <(pgrep -f "$process_pattern" 2>/dev/null || true)
fi
if (( home_detected )); then
  fail "运行中的 CLIProxyAPI 启用了 Home 模式（-home-jwt/HOME_JWT）：Home 选择先于插件调度，凭据路由限制无法生效"
elif command -v pgrep >/dev/null 2>&1 && pgrep -f "$process_pattern" >/dev/null 2>&1; then
  ok "运行中的 CLIProxyAPI 未检测到 Home 模式"
else
  warn "CLIProxyAPI 当前未运行，无法从进程确认 Home 模式；部署前请确认启动参数没有 -home-jwt，且未设置 HOME_JWT"
fi

# --- Startup log --------------------------------------------------------------

if [[ -n "$log_file" ]]; then
  printf '==> 检查启动日志：%s\n' "$log_file"
  [[ -f "$log_file" ]] || { echo "日志文件不存在：$log_file" >&2; exit 1; }
  if grep -q 'pluginhost: plugin registered' "$log_file" && grep -q "$plugin_id" <(grep 'pluginhost: plugin registered' "$log_file"); then
    ok "日志中找到 $plugin_id 的注册记录"
  else
    fail "日志中没有 $plugin_id 的注册记录：插件可能没有加载"
  fi
  if grep -qiE 'plugin.*(panic|fus)|panic.*plugin' "$log_file"; then
    fail "日志中存在插件 panic 或熔断记录"
  else
    ok "日志中没有插件 panic 或熔断记录"
  fi
fi

# --- Result -------------------------------------------------------------------

printf '\n'
if (( failures > 0 )); then
  echo "预检失败：${failures} 项错误，${warnings} 项警告。请先处理错误再部署。"
  exit 1
fi
echo "预检通过：${warnings} 项警告。"
if (( warnings > 0 )); then
  echo "警告项需要人工确认后才能部署。"
fi
exit 0
