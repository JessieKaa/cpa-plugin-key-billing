#!/usr/bin/env bash
set -euo pipefail

# Deployment preflight for the cpa-key-billing fork (plan §13.3).
#
# This script inspects the EFFECTIVE CLIProxyAPI configuration before a
# deployment and fails when the deployment shape is unsupported:
#
#   - plugin loading is disabled
#   - the cpa-key-billing instance is missing or disabled
#   - Home mode is enabled (Home selection runs before plugin scheduler
#     selection, so credential/provider restrictions cannot apply)
#   - another plugin instance is enabled (CLIProxyAPI adopts one scheduler
#     plugin by priority, so an unreviewed plugin can win selection)
#
# It warns when a competing plugin instance was explicitly acknowledged with
# --allow-plugin, and when Home mode is only asserted rather than observed.
#
# Usage:
#   scripts/preflight_deployment.sh <config.yaml> [options]
#
#   --log-file <path>       CLIProxyAPI startup log captured after the
#                          candidate start.
#   --process <pattern>     Pattern matching the running CLIProxyAPI process
#                          (default: cli-proxy-api).
#   --allow-plugin <id>     Acknowledge another enabled plugin instance after
#                          reviewing that it does not register a scheduler.
#                          Repeat for each additional plugin.
#   --home-disabled         Assert that Home mode is disabled. Use it when the
#                          process and startup log cannot be inspected, such as
#                          a controlled restart with CPA stopped.

readonly plugin_id="cpa-key-billing"
config_file=""
log_file=""
process_pattern="cli-proxy-api"
home_asserted=0
declare -a allowed_plugins=()

usage() {
  echo "用法：$0 <config.yaml> [--log-file <path>] [--process <pattern>]" >&2
  echo "         [--allow-plugin <id>]... [--home-disabled]" >&2
}

[[ ${1:-} != "" ]] || { usage; exit 2; }
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
    --allow-plugin)
      [[ ${2:-} != "" ]] || { echo "--allow-plugin 需要一个插件 ID" >&2; exit 2; }
      allowed_plugins+=("$2"); shift 2 ;;
    --home-disabled)
      home_asserted=1; shift ;;
    *) echo "未知参数：$1" >&2; usage; exit 2 ;;
  esac
done

command -v python3 >/dev/null 2>&1 || { echo "缺少命令：python3" >&2; exit 1; }
command -v jq >/dev/null 2>&1 || { echo "缺少命令：jq" >&2; exit 1; }
[[ -f "$config_file" ]] || { echo "配置文件不存在：$config_file" >&2; exit 1; }

failures=0
warnings=0

fail() { printf '  ✗ %s\n' "$*"; failures=$((failures + 1)); }
warn() { printf '  ! %s\n' "$*"; warnings=$((warnings + 1)); }
ok()   { printf '  ✓ %s\n' "$*"; }

is_allowed_plugin() {
  local candidate="$1" entry
  for entry in ${allowed_plugins[@]+"${allowed_plugins[@]}"}; do
    [[ "$entry" == "$candidate" ]] && return 0
  done
  return 1
}

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

instance="$(printf '%s' "$config_json" | jq -r --arg id "$plugin_id" '(.plugins.configs // {})[$id] // empty')"
if [[ -z "$instance" ]]; then
  fail "plugins.configs.$plugin_id 缺失：该插件实例不会被加载"
elif [[ "$(printf '%s' "$instance" | jq -r '.enabled // false')" != "true" ]]; then
  fail "plugins.configs.$plugin_id.enabled 不为 true：该插件实例被停用"
else
  ok "计费插件实例已启用"
fi

other_plugins="$(printf '%s' "$config_json" | jq -r --arg id "$plugin_id" \
  '(.plugins.configs // {}) | to_entries | map(select(.key != $id and ((.value.enabled // false) == true))) | map(.key) | join(" ")')"
if [[ -z "$other_plugins" ]]; then
  ok "没有其他已启用的插件实例"
else
  unreviewed=""
  for other in $other_plugins; do
    if is_allowed_plugin "$other"; then
      warn "插件实例 $other 已通过 --allow-plugin 确认不注册调度器"
    else
      unreviewed="$unreviewed $other"
    fi
  done
  if [[ -n "${unreviewed// /}" ]]; then
    fail "存在未确认的插件实例：${unreviewed# }。CLIProxyAPI 只采用一个调度器插件；确认其不注册调度器后，用 --allow-plugin <id> 逐个确认，否则本插件的凭据路由限制可能失效"
  fi
fi

state_file="$(printf '%s' "$instance" | jq -r '.state_file // ""')"
if [[ -n "$state_file" ]]; then
  if [[ -f "$state_file" ]]; then
    ok "状态数据库存在：$state_file"
  else
    warn "状态数据库尚不存在，首次启动会创建：$state_file"
  fi
fi

# --- Startup log --------------------------------------------------------------

log_home=0
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
  if grep -qiE 'home mode' "$log_file"; then
    log_home=1
  fi
fi

# --- Home mode ----------------------------------------------------------------

printf '==> 检查 Home 模式（Home 选择先于插件调度，凭据路由限制不生效）\n'
home_evidence=""
process_seen=0
process_unreadable=0
# HOME_JWT set in the preflight's own environment is direct evidence that the
# instance is started from an environment with Home mode enabled.
if [[ -n "${HOME_JWT:-}" ]]; then
  home_evidence="环境变量 HOME_JWT"
elif command -v pgrep >/dev/null 2>&1; then
  # Home mode is enabled by the -home-jwt flag or HOME_JWT environment variable;
  # it never appears in config.yaml. Candidates whose command line contains this
  # script are the preflight itself, its subshells, and its wrapping shell.
  while IFS= read -r pid; do
    [[ -n "$pid" ]] || continue
    # A candidate that exited between pgrep and this read is not an unreadable
    # process, so it must not downgrade the check.
    [[ -d "/proc/$pid" ]] || continue
    # Read cmdline and environ with the redirection errors suppressed so a
    # process that exits mid-scan is quiet rather than noisy.
    if ! cmdline=$( { tr '\0' ' ' < "/proc/$pid/cmdline"; } 2>/dev/null ); then
      [[ -d "/proc/$pid" ]] || continue
      process_unreadable=1
      continue
    fi
    if [[ "$cmdline" == *"$0"* ]]; then
      continue
    fi
    process_seen=1
    if [[ "$cmdline" == *"-home-jwt"* ]]; then
      home_evidence="进程参数 -home-jwt"
      break
    fi
    if ! environ=$( { tr '\0' '\n' < "/proc/$pid/environ"; } 2>/dev/null ); then
      [[ -d "/proc/$pid" ]] || continue
      process_unreadable=1
      continue
    fi
    if grep -q '^HOME_JWT=' <<<"$environ"; then
      home_evidence="环境变量 HOME_JWT"
      break
    fi
  done < <(pgrep -f "$process_pattern" 2>/dev/null | grep -vxE "$$|$PPID" || true)
fi

if (( log_home )); then
  fail "启动日志显示 Home 模式已启用：Home 选择先于插件调度，凭据路由限制无法生效"
elif [[ -n "$home_evidence" ]]; then
  fail "检测到 Home 模式已启用（$home_evidence）：Home 选择先于插件调度，凭据路由限制无法生效"
elif (( process_seen )) && (( ! process_unreadable )); then
  ok "运行中的 CLIProxyAPI 未检测到 Home 模式"
elif (( home_asserted )); then
  warn "已按 --home-disabled 声明 Home 模式关闭；预检无法从进程或日志独立验证"
else
  fail "无法确认 Home 模式已关闭：请在 CPA 运行时预检、提供启动日志、或在确认后传入 --home-disabled"
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
