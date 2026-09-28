#!/usr/bin/env python3

import argparse
import hashlib
import json
import random
import re
import time
from datetime import datetime, timedelta, timezone
from zoneinfo import ZoneInfo
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlparse


ROOT = Path(__file__).resolve().parents[1]
UI_PATH = ROOT / "internal" / "plugin" / "ui.html"
API_BASE = "/v0/management/plugins/cpa-key-billing"
NOW = datetime.now(timezone.utc).replace(minute=0, second=0, microsecond=0)
CALLER_SCOPE_SALT = b"cli-proxy-api:caller-scope:v1\0"


def iso(value):
    return value.isoformat().replace("+00:00", "Z")


PLANS = [
    {"id": "engineering", "name": "研发团队", "windows": [
        {"id": "short", "name": "短时额度", "amount_usd": 15, "request_limit": 100, "token_limit": 1000000, "period_seconds": 18000},
        {"id": "budget", "name": "团队预算", "amount_usd": 300, "period_seconds": 2592000},
    ]},
    {"id": "production", "name": "生产服务", "windows": [
        {"id": "short", "name": "峰值保护", "amount_usd": 0, "request_limit": 200, "token_limit": 0,
         "period_seconds": 7200, "cycle_anchor_at": iso(NOW + timedelta(hours=2))},
        {"id": "medium", "name": "服务额度", "amount_usd": 0, "request_limit": 0, "token_limit": 2000000,
         "period_seconds": 86400, "cycle_anchor_at": iso((NOW + timedelta(days=1)).replace(hour=0))},
        {"id": "budget", "name": "生产预算", "amount_usd": 1000,
         "period_seconds": 2592000, "cycle_anchor_at": iso((NOW + timedelta(days=15)).replace(hour=0))},
    ]},
    {"id": "project-credit", "name": "项目额度", "windows": [
        {"id": "budget", "name": "项目预算", "amount_usd": 100, "period_seconds": 864000},
    ]},
]


QUOTA_CYCLES = {}


def refresh_key_quota(key):
    plan = next((item for item in PLANS if item["id"] == key["plan_id"]), None)
    previous = QUOTA_CYCLES.get(key["scope"], {})
    cycles = {}
    now = datetime.now(timezone.utc)
    key.update(plan_name=plan["name"] if plan else "", unlimited=plan is None, blocked=False, windows=[])
    key.pop("retry_at", None)
    for window in plan["windows"] if plan else []:
        cycle = previous.get(window["id"], {})
        started = (cycle.get("plan_id") == key["plan_id"]
                   and cycle.get("period_seconds") == window["period_seconds"]
                   and cycle.get("cycle_anchor_at") == window.get("cycle_anchor_at")
                   and (key.get("deleted_at") or datetime.fromisoformat(cycle["end_at"].replace("Z", "+00:00")) > now))
        if started:
            cycles[window["id"]] = cycle
        else:
            cycle = {}
        if not started and window.get("cycle_anchor_at"):
            anchor = datetime.fromisoformat(window["cycle_anchor_at"].replace("Z", "+00:00"))
            period = timedelta(seconds=window["period_seconds"])
            start = anchor + ((now - anchor) // period) * period
            cycle = dict(start_at=iso(start), end_at=iso(start+period))
            started = True
        dimensions = [dict(metric=metric, limit=limit, used=used, remaining=max(0, limit-used),
                           used_percent=min(100, used / limit * 100), blocked=used >= limit)
                      for metric, limit, used in [
                          ("amount_usd", window.get("amount_usd", 0), cycle.get("spent_usd", 0)),
                          ("tokens", window.get("token_limit", 0), cycle.get("used_tokens", 0)),
                          ("requests", window.get("request_limit", 0), cycle.get("used_requests", 0))] if limit > 0]
        view = dict(id=window["id"], name=window["name"], period_seconds=window["period_seconds"],
                    started=started, blocked=any(dimension["blocked"] for dimension in dimensions),
                    dimensions=dimensions)
        if window.get("cycle_anchor_at"):
            view["cycle_anchor_at"] = window["cycle_anchor_at"]
        if started:
            view.update(start_at=cycle["start_at"], end_at=cycle["end_at"])
        if view["blocked"]:
            key["blocked"] = True
            key["retry_at"] = max(key.get("retry_at", ""), view["end_at"])
        key["windows"].append(view)
    QUOTA_CYCLES[key["scope"]] = cycles


AUTOMATION_CREDENTIAL_REF = "sha256:" + "f" * 64
UNAVAILABLE_CREDENTIAL_REF = "sha256:" + "1" * 64

CREDENTIALS = [
    {"ref": "sha256:" + "a" * 64, "source": "auth-files", "provider": "codex", "display_name": "dev-team@example.com", "status": "active", "disabled": False, "unavailable": False},
    {"ref": "sha256:" + "b" * 64, "source": "auth-files", "provider": "claude", "display_name": "platform@example.com", "status": "active", "disabled": False, "unavailable": False},
    {"ref": "sha256:" + "c" * 64, "source": "ai-providers", "provider": "codex", "display_name": "sk-proxy…7f3a", "status": "active", "disabled": False, "unavailable": False},
    {"ref": "sha256:" + "d" * 64, "source": "ai-providers", "provider": "deepseek", "display_name": "sk-live…91b2", "status": "disabled", "disabled": True, "unavailable": False},
    {"ref": "sha256:" + "e" * 64, "source": "auth-files", "provider": "xai", "display_name": "disabled@example.com", "status": "disabled", "disabled": True, "unavailable": False},
    {"ref": AUTOMATION_CREDENTIAL_REF, "source": "auth-files", "provider": "codex", "display_name": "automation@example.com", "status": "active", "disabled": False, "unavailable": False},
    {"ref": UNAVAILABLE_CREDENTIAL_REF, "source": "auth-files", "provider": "kimi", "display_name": "research@example.com", "status": "error", "disabled": False, "unavailable": True},
]
SYNCED_CREDENTIAL_REFS = set()

ROUTES = [
    {"id": "coding", "name": "代码开发", "rule": {"models": ["gpt-5.6-sol", "gpt-5.5", "codex/deepseek-v4-flash-vision-exp"], "credential_ids": [], "credential_providers": [{"source": "auth-files", "provider": "codex"}]}},
    {"id": "analytics", "name": "数据分析", "rule": {"models": ["claude/deepseek-v4-pro", "claude/deepseek-v4-flash"], "credential_ids": ["sha256:" + "b" * 64], "credential_providers": []}},
    {"id": "economy", "name": "轻量任务", "rule": {"models": ["gpt-5.6-luna"], "credential_ids": [], "credential_providers": [{"source": "auth-files", "provider": "codex"}]}},
    {
        "id": "ci",
        "name": "持续集成",
        "rule": {
            "models": ["gpt-5.6-luna", "gpt-5.6-terra"],
            "credential_ids": [],
            "credential_providers": [{"source": "auth-files", "provider": "codex"}],
            "denied_models": ["gpt-5.6-sol"],
            "denied_credential_ids": ["sha256:" + "a" * 64],
            "denied_credential_providers": [{"source": "ai-providers", "provider": "deepseek"}],
        },
    },
    {
        "id": "text-only",
        "name": "文本服务",
        "rule": {
            "models": [], "credential_ids": [], "credential_providers": [],
            "denied_models": ["gpt-image-2"],
            "denied_credential_ids": ["sha256:" + "d" * 64, "sha256:" + "e" * 64],
            "denied_credential_providers": [],
        },
    },
]

AUTH_FILES = [
    {
        "auth_index": "auth-demo-codex-plus",
        "name": "codex-dev-team@example.com.json",
        "category": "codex",
        "email": "dev-team@example.com",
        "disabled": False,
        "unavailable": False,
        "quota_supported": True,
    },
    {
        "auth_index": "auth-demo-claude",
        "name": "claude-platform@example.com.json",
        "category": "claude",
        "email": "platform@example.com",
        "disabled": False,
        "unavailable": False,
        "quota_supported": True,
    },
    {
        "auth_index": "auth-demo-codex-pro",
        "name": "codex-automation@example.com.json",
        "category": "codex",
        "email": "automation@example.com",
        "disabled": False,
        "unavailable": False,
        "quota_supported": True,
    },
    {
        "auth_index": "auth-demo-antigravity",
        "name": "antigravity-ai-lab@example.com.json",
        "category": "antigravity",
        "email": "ai-lab@example.com",
        "disabled": False,
        "unavailable": False,
        "quota_supported": True,
    },
    {
        "auth_index": "auth-demo-kimi",
        "name": "kimi-research@example.com.json",
        "category": "kimi",
        "email": "research@example.com",
        "disabled": False,
        "unavailable": True,
        "quota_supported": True,
    },
    {
        "auth_index": "auth-demo-xai-active",
        "name": "xai-research@example.com.json",
        "category": "xai",
        "email": "research@example.com",
        "disabled": False,
        "unavailable": False,
        "quota_supported": True,
    },
]

AUTH_FILE_CREDENTIAL_REFS = {
    "auth-demo-codex-plus": "sha256:" + "a" * 64,
    "auth-demo-claude": "sha256:" + "b" * 64,
    "auth-demo-codex-pro": AUTOMATION_CREDENTIAL_REF,
    "auth-demo-kimi": UNAVAILABLE_CREDENTIAL_REF,
    "auth-demo-xai-active": "sha256:" + "e" * 64,
}

for auth_file in AUTH_FILES:
    auth_file["cache_revision"] = iso(NOW - timedelta(minutes=5))

AUTH_CATEGORY_ORDER = {"claude": 0, "antigravity": 1, "codex": 2, "xai": 3, "kimi": 4}
AUTH_FILES.sort(
    key=lambda item: (
        AUTH_CATEGORY_ORDER.get(item["category"], 5),
        item["category"].lower(),
        item["name"].lower(),
        item["auth_index"],
    )
)


def quota_row(label, remaining_percent, reset_seconds, **extra):
    return {
        "label": label,
        "remaining_percent": remaining_percent,
        "reset_at": iso(NOW + timedelta(seconds=reset_seconds)),
        **extra,
    }


AUTH_FILE_QUOTAS = {
    "auth-demo-codex-pro": {
        "plan": "pro-20x",
        "rate_limit_reset_credits_available_count": 2,
        "rate_limit_reset_credits": [
            {"expires_at": iso(NOW + timedelta(days=13, hours=14))},
            {"expires_at": iso(NOW + timedelta(days=14, hours=10))},
        ],
        "quota": [
            quota_row("周限额", 62, 432000),
            quota_row(
                "GPT-5.3-Codex-Spark 5 小时限额",
                100,
                18000,
            ),
            quota_row(
                "GPT-5.3-Codex-Spark 周限额",
                100,
                604800,
            ),
        ],
    },
    "auth-demo-codex-plus": {
        "plan": "plus",
        "rate_limit_reset_credits_available_count": 1,
        "rate_limit_reset_credits": [{"expires_at": iso(NOW + timedelta(days=7))}],
        "quota": [
            quota_row("5 小时限额", 35, 14400),
            quota_row("周限额", 90, 518400),
        ],
    },
    "auth-demo-claude": {
        "plan": "Team",
        "quota": [
            quota_row("5 小时限额", 76, 12600),
            quota_row("周限额", 59, 388800),
            {
                "label": "额外用量",
                "used": 12.5,
                "limit": 100,
                "remaining_percent": 87.5,
                "currency": "USD",
            },
        ],
    },
    "auth-demo-antigravity": {
        "plan": "Google AI Pro",
        "quota": [
            quota_row(
                "5 小时限额",
                82,
                64800,
                group_label="Gemini Models",
            ),
            quota_row(
                "周限额",
                93,
                64800,
                group_label="Gemini Models",
            ),
        ],
    },
    "auth-demo-kimi": {
        "quota": [
            quota_row("5 小时限额", 48, 7200),
            quota_row("周限额", 69, 345600),
        ],
    },
    "auth-demo-xai-active": {
        "quota": [
            quota_row("周限额", 78, 410400),
            {
                "label": "月度额度",
                "used": 8.5,
                "limit": 50,
                "remaining_percent": 83,
                "currency": "USD",
                "reset_at": iso(NOW + timedelta(seconds=1814400)),
            },
        ],
    },
}


def auth_file_quota(query):
    auth_index = query.get("auth_index", [""])[0]
    quota = AUTH_FILE_QUOTAS.get(auth_index)
    if quota is None:
        return None
    auth_file = next(item for item in AUTH_FILES if item["auth_index"] == auth_index)
    result = {
        "auth_revision": auth_file["cache_revision"],
        "fetched_at": iso(NOW),
        **quota,
    }
    english = json.loads((UI_PATH.parent / "locales/en.json").read_text())
    chinese = json.loads((UI_PATH.parent / "locales/zh-CN.json").read_text())
    labels = {value: key for key, value in chinese.items() if key.startswith("backend.") and "{" not in value}
    result["quota"] = [dict(row) for row in result["quota"]]
    for row in result["quota"]:
        key = labels.get(row.get("label", ""))
        if key:
            row["label"] = english[key]
            row["label_message"] = {"message_key": key}
    return result


KEY_PROFILES = [
    {"label": "代码审查机器人", "plan_id": "engineering", "spent_usd": 128.64, "concurrency_limit": 5, "current_concurrency": 2,
     "route_bindings": {"route_ids": ["coding", "analytics", "economy", "text-only"], "models": [], "credential_ids": [], "credential_providers": []}},
    {"label": "CI 构建服务", "plan_id": "engineering", "spent_usd": 84.27, "concurrency_limit": 10, "current_concurrency": 3,
     "route_bindings": {"route_ids": ["ci"], "models": ["gpt-5.6-terra", "gpt-5.5"],
                        "credential_ids": [AUTOMATION_CREDENTIAL_REF], "credential_providers": [],
                        "denied_models": ["gpt-5.6-sol", "gpt-image-2"], "denied_credential_ids": ["sha256:" + "a" * 64], "denied_credential_providers": []}},
    {"label": "数据分析平台", "plan_id": "production", "spent_usd": 368.91, "concurrency_limit": 5, "current_concurrency": 1,
     "route_bindings": {"route_ids": ["analytics"], "models": [], "credential_ids": [], "credential_providers": []}},
    {"label": "客服助手", "plan_id": "production", "spent_usd": 241.36, "concurrency_limit": 8, "current_concurrency": 2,
     "route_bindings": {"route_ids": ["analytics"], "models": ["gpt-5.5"], "denied_models": ["gpt-image-2"], "credential_ids": [], "credential_providers": []}},
    {"label": "文档生成", "plan_id": "engineering", "spent_usd": 56.48, "concurrency_limit": 3, "current_concurrency": 0,
     "route_bindings": {"route_ids": ["text-only"], "models": [], "credential_ids": [], "credential_providers": []}},
    {"label": "预发布环境", "plan_id": "project-credit", "spent_usd": 43.72, "concurrency_limit": 2, "current_concurrency": 1,
     "route_bindings": {"route_ids": ["economy"], "models": [], "credential_ids": [], "credential_providers": []}},
    {"label": "内部工具", "plan_id": "", "spent_usd": 0, "concurrency_limit": 0, "current_concurrency": 1,
     "route_bindings": {"route_ids": [], "models": [], "credential_ids": [], "credential_providers": []}},
    {"label": "临时测试", "plan_id": "project-credit", "spent_usd": 87.19, "concurrency_limit": 1, "current_concurrency": 0,
     "route_bindings": {"route_ids": [], "models": ["gpt-5.5"], "credential_ids": ["sha256:" + "c" * 64], "credential_providers": []}},
]


def make_key(index):
    profile = KEY_PROFILES[index - 1]
    plan = next((item for item in PLANS if item["id"] == profile["plan_id"]), None)
    result = {
        "scope": hashlib.sha256(CALLER_SCOPE_SALT + f"sk-demo-{index:04d}".encode()).hexdigest(),
        "preview": f"sk-demo…{index:04d}",
        "label": profile["label"],
        "in_config": True,
        "plan_id": profile["plan_id"],
        "concurrency_limit": profile["concurrency_limit"],
        "current_concurrency": profile["current_concurrency"],
        "route_bindings": profile["route_bindings"],
    }
    cycles = {}
    for position, window in enumerate(plan["windows"] if plan and index != 4 else []):
        ratio = profile["spent_usd"] / plan["windows"][-1]["amount_usd"]
        if index == 2 and position == 0 or index == 3 and position < 2:
            ratio = 1.05
        end = (datetime.fromisoformat(window["cycle_anchor_at"].replace("Z", "+00:00"))
               if window.get("cycle_anchor_at") else NOW + timedelta(seconds=window["period_seconds"] * (0.3 + index * 0.04)))
        cycles[window["id"]] = dict(plan_id=result["plan_id"], period_seconds=window["period_seconds"],
            cycle_anchor_at=window.get("cycle_anchor_at"),
            spent_usd=window["amount_usd"]*ratio,
            used_requests=int(window.get("request_limit", 1000)*ratio),
            used_tokens=int(window.get("token_limit", 10000000)*ratio),
            start_at=iso(end-timedelta(seconds=window["period_seconds"])), end_at=iso(end))
    QUOTA_CYCLES[result["scope"]] = cycles
    refresh_key_quota(result)
    return result


KEYS = [make_key(index) for index in range(1, len(KEY_PROFILES) + 1)]
KEYS[5]["deleted_at"] = iso(NOW - timedelta(days=3))
KEYS[-2]["in_config"] = False
KEYS[-1]["in_config"] = False
LIVE_KEYS = [key for key in KEYS if not key.get("deleted_at")]


def key_status(key):
    if key.get("deleted_at"):
        return "deleted"
    if not key.get("plan_id"):
        return "unmanaged"
    if not key.get("in_config"):
        return "external-managed"
    return "managed"

PRICES = [
    {
        "model_id": "gpt-5.6-sol",
        "input_per_1m": 4,
        "output_per_1m": 20,
        "cache_read_per_1m": 0.4,
        "source": "custom",
        "long_context": {
            "threshold_input_tokens": 272000,
            "input_per_1m": 8,
            "output_per_1m": 30,
            "cache_read_per_1m": 0.8,
        },
    },
    {
        "model_id": "gpt-5.5",
        "input_per_1m": 5,
        "output_per_1m": 30,
        "cache_read_per_1m": 0.5,
        "source": "custom",
        "long_context": {
            "threshold_input_tokens": 272000,
            "input_per_1m": 10,
            "output_per_1m": 45,
            "cache_read_per_1m": 1,
        },
    },
    {
        "model_id": "gpt-5.6-luna",
        "input_per_1m": 0.2,
        "output_per_1m": 1.2,
        "cache_read_per_1m": 0.02,
        "source": "custom",
    },
    {
        "model_id": "gpt-5.6-terra",
        "input_per_1m": 2,
        "output_per_1m": 12,
        "cache_read_per_1m": 0.2,
        "source": "custom",
    },
    {
        "model_id": "gpt-image-2",
        "input_per_1m": 5,
        "output_per_1m": 30,
        "cache_read_per_1m": 1.25,
        "source": "custom",
    },
    {
        "model_id": "claude/deepseek-v4-pro",
        "input_per_1m": 0.435,
        "output_per_1m": 0.87,
        "cache_read_per_1m": 0.003625,
        "source": "custom",
    },
    {
        "model_id": "claude/deepseek-v4-flash",
        "input_per_1m": 0.28,
        "output_per_1m": 0.42,
        "source": "custom",
    },
]

def make_cost(uncached, cache_read, cache_write, output, rates, tiered=False, long_context=False, multiplier=1):
    input_price, read_price, write_price, output_price = (rate * multiplier for rate in rates)
    parts = {
        "uncached_input_usd": uncached * input_price / 1_000_000,
        "cache_read_usd": cache_read * read_price / 1_000_000,
        "cache_write_usd": cache_write * write_price / 1_000_000,
        "output_usd": output * output_price / 1_000_000,
    }
    return {
        **parts,
        "multiplier": multiplier,
        "total_usd": sum(parts.values()),
        "uncached_input_tokens": uncached,
        "cache_read_tokens": cache_read,
        "cache_write_tokens": cache_write,
        "billed_output_tokens": output,
        "tiered": tiered,
        "long_context": long_context,
        "threshold_input_tokens": 272000 if tiered else 0,
        "applied_input_per_1m": input_price,
        "applied_cache_read_per_1m": read_price,
        "applied_cache_write_per_1m": write_price,
        "applied_output_per_1m": output_price,
    }


def event_sample(
    key_index,
    source,
    provider,
    model,
    executor,
    effort,
    tier,
    latency_ms,
    ttft_ms,
    reasoning_tokens,
    tokens,
    rates,
    *,
    billing_model="",
    response_model="",
    response_tier="",
    long_context=False,
    failed=False,
    multiplier=1,
):
    uncached, cache_read, cache_write, output = tokens
    if failed:
        uncached = cache_read = cache_write = output = 0
    return {
        "key_index": key_index,
        "source": source,
        "provider": provider,
        "executor_type": executor,
        "reasoning_effort": effort,
        "service_tier": tier,
        "response_service_tier": response_tier,
        "upstream_model": model,
        "response_model": response_model,
        "billing_model": billing_model or model,
        "failed": failed,
        "latency_ms": latency_ms,
        "ttft_ms": ttft_ms,
        "accounting_quality": "" if failed else "complete",
        "price_source": "custom",
        "cost": make_cost(
            uncached,
            cache_read,
            cache_write,
            output,
            rates,
            model.startswith("gpt-5."),
            long_context,
            multiplier if not failed else 1,
        ),
        "reasoning_tokens": 0 if failed else reasoning_tokens,
    }


# Numeric usage and timing values are sampled from a real export. All identities
# below are synthetic and intentionally unrelated to the source records.
SUCCESS_EVENT_SAMPLES = [
    event_sample(0, "codex · dev-team@example.com", "codex", "gpt-5.6-sol", "CodexExecutor", "high", "priority", 11513, 8209, 266, (712, 91648, 4096, 425), (4, 0.4, 5, 20), multiplier=2.5, response_tier="priority"),
    event_sample(5, "codex · dev-team@example.com", "codex", "codex-auto-review", "CodexWebsocketsExecutor", "low", "auto", 2516, 1431, 10, (1030, 49920, 0, 75), (0.2, 0.02, 0.25, 1.2), response_model="gpt-5.6-luna", response_tier="default"),
    event_sample(1, "codex · dev-team@example.com", "codex", "gpt-5.5", "CodexWebsocketsExecutor", "medium", "auto", 2417, 1103, 0, (1194, 95616, 0, 73), (5, 0.5, 5, 30)),
    event_sample(1, "codex · dev-team@example.com", "codex", "gpt-5.6-sol", "CodexWebsocketsExecutor", "high", "auto", 5306, 2121, 21, (798, 169984, 0, 201), (4, 0.4, 5, 20)),
    event_sample(2, "claude · platform@example.com", "claude", "deepseek-v4-pro", "ClaudeExecutor", "high", "auto", 10061, 637, 0, (38049, 0, 0, 474), (0.435, 0.003625, 0.435, 0.87), billing_model="claude/deepseek-v4-pro", response_model="deepseek-v4-pro"),
    event_sample(7, "codex · sk-proxy…7f3a", "codex", "gpt-5.5", "CodexWebsocketsExecutor", "high", "auto", 7288, 4130, 188, (8502, 45440, 0, 309), (5, 0.5, 5, 30)),
    event_sample(6, "codex · dev-team@example.com", "codex", "gpt-5.6-terra", "CodexWebsocketsExecutor", "medium", "auto", 10120, 3379, 81, (1300, 69376, 0, 477), (2, 0.2, 2.5, 12)),
    event_sample(4, "codex · dev-team@example.com", "codex", "gpt-5.6-sol", "CodexWebsocketsExecutor", "high", "auto", 2609, 1881, 6, (1368, 237824, 0, 46), (4, 0.4, 5, 20)),
    event_sample(5, "codex · dev-team@example.com", "codex", "gpt-5.6-luna", "CodexWebsocketsExecutor", "low", "auto", 5002, 2464, 61, (1261, 149248, 0, 147), (0.2, 0.02, 0.25, 1.2)),
    event_sample(7, "codex · sk-proxy…7f3a", "codex", "gpt-5.5", "CodexWebsocketsExecutor", "high", "auto", 7634, 4412, 94, (24196, 19840, 0, 275), (5, 0.5, 5, 30)),
    event_sample(6, "codex · dev-team@example.com", "codex", "gpt-image-2", "CodexExecutor", "", "auto", 43679, 43476, 0, (1658, 0, 0, 915), (5, 1.25, 5, 30)),
    event_sample(3, "claude · platform@example.com", "claude", "deepseek-v4-pro", "ClaudeExecutor", "high", "auto", 16431, 909, 0, (66079, 32768, 0, 535), (0.435, 0.003625, 0.435, 0.87), billing_model="claude/deepseek-v4-pro"),
    event_sample(0, "codex · dev-team@example.com", "codex", "gpt-5.6-sol", "CodexExecutor", "high", "auto", 19542, 17836, 681, (835, 282752, 0, 802), (8, 0.8, 10, 30), long_context=True),
    event_sample(6, "codex · dev-team@example.com", "codex", "gpt-5.6-luna", "CodexWebsocketsExecutor", "low", "auto", 5074, 3149, 49, (28615, 31488, 0, 124), (0.2, 0.02, 0.25, 1.2)),
]

FAILURE_EVENT_SAMPLES = [
    event_sample(1, "codex · dev-team@example.com", "codex", "gpt-5.5", "CodexWebsocketsExecutor", "high", "auto", 4338, 237, 0, (0, 0, 0, 0), (5, 0.5, 5, 30), failed=True),
]

EVENT_SAMPLES = SUCCESS_EVENT_SAMPLES * 2 + FAILURE_EVENT_SAMPLES


def make_request_events():
    entries = []
    for index, sample in enumerate(EVENT_SAMPLES):
        key = KEYS[sample["key_index"]]
        entries.append({
            "id": str(index + 1),
            "at": iso(NOW - timedelta(hours=index * 22, minutes=(index % 4) * 11)),
            "scope": key["scope"],
            "preview": key["preview"],
            "label": key["label"],
            **{name: value for name, value in sample.items() if name != "key_index"},
        })
    return entries


REQUEST_EVENTS = make_request_events()

def source_filter_token(scope, source):
    payload = "filter:v1\0source\0" + scope + "\0" + source
    return hashlib.sha256(payload.encode()).hexdigest()

def source_filter_options(scope, sources):
    return [{"value": source_filter_token(scope, source), "label": source} for source in sources]

def event_snapshot(query):
    return int(query.get("snapshot_id", [str(max((int(entry["id"]) for entry in REQUEST_EVENTS), default=0))])[0])

def request_event_view(query, scope=""):
    snapshot = event_snapshot(query)
    selected_key = "" if scope else query.get("api_key", [""])[0]
    selected_model = query.get("model", [""])[0]
    selected_source = query.get("source", [""])[0]
    selected_provider = query.get("provider", [""])[0]
    selected_executor = query.get("executor", [""])[0]
    selected_failed = query.get("failed", [""])[0]
    offset = max(0, int(query.get("offset", ["0"])[0] or 0))
    limit = max(0, int(query.get("limit", ["0"])[0] or 0))
    time_matched = filter_event_time([entry for entry in REQUEST_EVENTS
                                     if int(entry["id"]) <= snapshot and (not scope or entry["scope"] == scope)], query)
    time_matched.sort(key=lambda entry: (entry["at"], int(entry["id"])), reverse=True)
    source_values = sorted({entry.get("source", "") for entry in time_matched} - {""}, key=str.lower)
    filter_options = {
        "models": sorted({entry.get("billing_model") or entry.get("upstream_model", "")
                          for entry in time_matched} - {""}, key=str.lower),
        "source_options": source_filter_options(scope, source_values),
        "providers": sorted({entry.get("provider", "") for entry in time_matched} - {""}, key=str.lower),
        "executors": sorted({entry.get("executor_type", "") for entry in time_matched} - {""}, key=str.lower),
    }
    if selected_source:
        selected_source = next((source for source in source_values
                                if source_filter_token(scope, source) == selected_source), "\0")
    counts = {"all": 0, "normal": 0, "failed": 0}
    matched = []
    for entry in time_matched:
        if selected_key and entry.get("scope") != selected_key:
            continue
        if selected_model and (entry.get("billing_model") or entry.get("upstream_model")) != selected_model:
            continue
        if selected_source and entry.get("source") != selected_source:
            continue
        if selected_provider and entry.get("provider") != selected_provider:
            continue
        if selected_executor and entry.get("executor_type") != selected_executor:
            continue
        failed = bool(entry.get("failed"))
        counts["all"] += 1
        counts["failed" if failed else "normal"] += 1
        if selected_failed and failed != (selected_failed == "true"):
            continue
        matched.append(entry)
    page = matched[offset:offset + limit] if limit else matched[offset:]
    if scope:
        page = [{key: value for key, value in entry.items()
                 if key not in {"scope", "auth_index", "preview", "label"}} for entry in page]
    result = {"entries": page, "total": len(matched), "snapshot_id": str(snapshot), "status_counts": counts}
    if offset == 0:
        result["filter_options"] = filter_options
    return result


def filter_event_time(entries, query):
    from_raw = query.get("from", [""])[0]
    to_raw = query.get("to", [""])[0]
    from_time = datetime.fromisoformat(from_raw.replace("Z", "+00:00")) if from_raw else None
    to_time = datetime.fromisoformat(to_raw.replace("Z", "+00:00")) if to_raw else None
    return [entry for entry in entries if
            (not from_time or datetime.fromisoformat(entry["at"].replace("Z", "+00:00")) >= from_time) and
            (not to_time or datetime.fromisoformat(entry["at"].replace("Z", "+00:00")) < to_time)]


def refresh_route_counts():
    for route in ROUTES:
        bound = [key for key in KEYS if route["id"] in key["route_bindings"]["route_ids"]]
        route["bound_key_count"] = len(bound)
        route["deleted_key_count"] = sum(bool(key.get("deleted_at")) for key in bound)
        route["fully_unrestricted_keys"] = sum(
            not key.get("deleted_at")
            and len(key["route_bindings"]["route_ids"]) == 1
            and not key["route_bindings"]["models"]
            and not key["route_bindings"]["credential_ids"]
            and not key["route_bindings"]["credential_providers"]
            and not any(key["route_bindings"].get(field) for field in ("denied_models", "denied_credential_ids", "denied_credential_providers"))
            for key in bound
        )


# transport=True is a bare executor error: the body is the message itself
# rather than an upstream payload.
def request_error(event_index, message, status=0, error_type="", code="", transport=False):
    event = REQUEST_EVENTS[event_index]
    event["failed"] = True
    body = message
    if not transport:
        error = {"message": message}
        if error_type:
            error["type"] = error_type
        if code:
            error["code"] = code
        if 400 <= status <= 599:
            error["status"] = status
        body = json.dumps({"error": error}, ensure_ascii=False, separators=(",", ":"))
    event["error_body"] = body
    return {
        "id": event["id"],
        "at": event["at"],
        "scope": event["scope"],
        "preview": event["preview"],
        "label": event["label"],
        "source": event["source"],
        "provider": event["provider"],
        "executor_type": event["executor_type"],
        "upstream_model": event["upstream_model"],
        "billing_model": event["billing_model"],
        "latency_ms": event["latency_ms"],
        "ttft_ms": event["ttft_ms"],
        "status_code": status,
        "error_type": code or error_type,
        "body": body,
    }


ERRORS = [
    request_error(
        28,
        "Responses websocket connection limit reached (60 minutes). Create a new websocket connection to continue.",
        status=400,
        error_type="invalid_request_error",
        code="websocket_connection_limit_reached",
    ),
    request_error(27, "The model is not supported.", status=400, error_type="invalid_request_error"),
    request_error(26, "websocket: close 1012"),
    request_error(
        25,
        "upstream request timed out",
        status=504,
        error_type="timeout_error",
        code="upstream_timeout",
    ),
    request_error(
        24,
        "websocket: close 1006 (abnormal closure): unexpected EOF",
        code="websocket_abnormal_closure",
        transport=True,
    ),
    request_error(
        23,
        'Post "https://api.deepseek.com/anthropic/v1/messages?beta=true": context canceled',
        code="context_canceled",
        transport=True,
    ),
]

PLUGIN_LOGS = [
    {
        "id": 3,
        "at": iso(NOW - timedelta(minutes=2)),
        "level": "debug",
        "message": "route " + json.dumps({"key": "代码审查机器人 · sk-demo…0001", "model": "gpt-5.6-sol", "model_policy": "restricted", "model_result": "allow", "credential_policy": "restricted", "credential_result": "selected", "selected_credential": "codex · dev-team@example.com", "outcome": "succeeded", "status": 200}, ensure_ascii=False, separators=(",", ":")),
    },
    {
        "id": 2,
        "at": iso(NOW - timedelta(minutes=11)),
        "level": "info",
        "message": (
            "已加载计费数据库 /srv/cli-proxy-api/plugins/cpa-key-billing-state-v1.db："
            "8 个 API Key、3 个订阅计划、29 条请求事件。已启用。"
        ),
    },
    {
        "id": 1,
        "at": iso(NOW - timedelta(hours=12, minutes=40)),
        "level": "info",
        "message": "已同步 CLIProxyAPI 的 API Key 列表：新增 1 个。",
    },
]


def seed_paginated_history():
    """Provide multiple real pages of synthetic history in the default preview."""
    event_samples = list(REQUEST_EVENTS)
    error_samples = {entry["id"]: entry for entry in ERRORS}
    log_samples = list(PLUGIN_LOGS)
    REQUEST_EVENTS.clear()
    ERRORS.clear()
    PLUGIN_LOGS.clear()
    for batch in range(40):
        for index, sample in enumerate(event_samples):
            sequence = batch * len(event_samples) + index
            identity = {
                "id": str(sequence + 1),
                "at": iso(NOW - timedelta(minutes=sequence)),
            }
            REQUEST_EVENTS.append({**sample, **identity})
            if sample["id"] in error_samples:
                ERRORS.append({**error_samples[sample["id"]], **identity})
        for index, sample in enumerate(log_samples):
            sequence = batch * len(log_samples) + index
            PLUGIN_LOGS.append({
                **sample,
                "id": 40 * len(log_samples) - sequence,
                "at": iso(NOW - timedelta(minutes=sequence)),
            })


def error_view(query, scope=""):
    snapshot = event_snapshot(query)
    rows = filter_event_time([entry for entry in ERRORS
                             if int(entry["id"]) <= snapshot and (not scope or entry["scope"] == scope)], query)
    rows.sort(key=lambda entry: (entry["at"], int(entry["id"])), reverse=True)
    selected = {
        "api_key": "" if scope else query.get("api_key", [""])[0],
        "model": query.get("model", [""])[0],
        "source": query.get("source", [""])[0],
        "provider": query.get("provider", [""])[0],
        "executor": query.get("executor", [""])[0],
        "status_code": query.get("status_code", [""])[0],
        "error_type": query.get("error_type", [""])[0],
    }
    source_values = sorted({entry["source"] for entry in rows})
    if selected["source"]:
        selected["source"] = next((source for source in source_values
                                   if source_filter_token(scope, source) == selected["source"]), "\0")
    filtered = []
    counts = {}
    empty_type = query.get("error_type_empty", [""])[0] == "true"
    for entry in rows:
        if selected["api_key"] and entry["scope"] != selected["api_key"]:
            continue
        if selected["model"] and entry["billing_model"] != selected["model"]:
            continue
        if selected["source"] and entry["source"] != selected["source"]:
            continue
        if selected["provider"] and entry["provider"] != selected["provider"]:
            continue
        if selected["executor"] and entry["executor_type"] != selected["executor"]:
            continue
        if selected["status_code"] and str(entry["status_code"]) != selected["status_code"]:
            continue
        error_type = entry["error_type"]
        counts[error_type] = counts.get(error_type, 0) + 1
        if empty_type:
            if error_type:
                continue
        elif selected["error_type"] and error_type != selected["error_type"]:
            continue
        filtered.append(entry)
    offset = max(0, int(query.get("offset", ["0"])[0] or 0))
    limit = max(0, int(query.get("limit", ["0"])[0] or 0))
    page = filtered[offset:offset + limit] if limit else filtered[offset:]
    if scope:
        page = [{key: value for key, value in entry.items()
                 if key not in {"scope", "preview", "label", "auth_index"}} for entry in page]
    result = {"entries": page, "total": len(filtered), "snapshot_id": str(snapshot), "error_type_counts": counts}
    if offset == 0:
        result["filter_options"] = {
            "models": sorted({entry["billing_model"] for entry in rows}),
            "source_options": source_filter_options(scope, source_values),
            "providers": sorted({entry["provider"] for entry in rows}),
            "executors": sorted({entry["executor_type"] for entry in rows}),
            "status_codes": sorted({entry["status_code"] for entry in rows if entry["status_code"]}),
            "error_types": sorted({entry["error_type"] for entry in rows if entry["error_type"]}),
        }
    return result


def analysis_view(query, scope=""):
    rows = filter_event_time([entry for entry in REQUEST_EVENTS if not scope or entry["scope"] == scope], query)
    selected = query.get("api_key", [""])[0]
    if selected and not scope:
        rows = [entry for entry in rows if entry["scope"] == selected]

    def distribution(field, label_field=None, unknown="未知"):
        grouped = {}
        for entry in rows:
            key = entry.get(field, "") or unknown
            label = entry.get(label_field, "") if label_field else key
            if not label and field == "scope":
                label = entry.get("preview", "")
            item = grouped.setdefault(key, {"key": key, "label": label or key,
                                            "total_tokens": 0, "requests": 0,
                                            "cost_usd": 0})
            if field == "scope":
                item["preview"] = entry.get("preview", "")
            cost = entry.get("cost", {})
            item["total_tokens"] += sum(cost.get(name, 0) for name in (
                "uncached_input_tokens", "cache_read_tokens", "cache_write_tokens", "billed_output_tokens"))
            item["requests"] += 1
            item["cost_usd"] += cost.get("total_usd", 0)
        token_total = sum(item["total_tokens"] for item in grouped.values())
        request_total = sum(item["requests"] for item in grouped.values())
        for item in grouped.values():
            denominator = token_total if token_total else request_total
            numerator = item["total_tokens"] if token_total else item["requests"]
            item["percent"] = numerator * 100 / max(1, denominator)
        return sorted(grouped.values(), key=lambda item: (-item["total_tokens"], -item["requests"], item["label"]))

    requests = len(rows)
    failed = sum(int(entry.get("failed", False)) for entry in rows)
    input_tokens = sum(
        sum(entry.get("cost", {}).get(name, 0) for name in (
            "uncached_input_tokens", "cache_read_tokens", "cache_write_tokens"
        )) for entry in rows
    )
    cache_read_tokens = sum(entry.get("cost", {}).get("cache_read_tokens", 0) for entry in rows)
    cache_write_tokens = sum(entry.get("cost", {}).get("cache_write_tokens", 0) for entry in rows)
    output_tokens = sum(entry.get("cost", {}).get("billed_output_tokens", 0) for entry in rows)
    cost = {
        "input_usd": sum(entry.get("cost", {}).get("uncached_input_usd", 0) for entry in rows),
        "cache_read_usd": sum(entry.get("cost", {}).get("cache_read_usd", 0) for entry in rows),
        "cache_write_usd": sum(entry.get("cost", {}).get("cache_write_usd", 0) for entry in rows),
        "output_usd": sum(entry.get("cost", {}).get("output_usd", 0) for entry in rows),
    }
    cost["total_usd"] = sum(cost[field] for field in (
        "input_usd", "cache_read_usd", "cache_write_usd", "output_usd"
    ))

    from_time = datetime.fromisoformat(
        query.get("from", [iso(NOW - timedelta(days=30))])[0].replace("Z", "+00:00")
    )
    to_time = datetime.fromisoformat(
        query.get("to", [iso(NOW)])[0].replace("Z", "+00:00")
    )
    bucket_size = timedelta(hours=1)
    bucket_start = from_time
    if to_time - from_time > timedelta(days=1):
        bucket_size = timedelta(days=1)
        browser_zone = ZoneInfo(query.get("timezone", ["UTC"])[0])
        local_from = from_time.astimezone(browser_zone)
        bucket_start = local_from.replace(hour=0, minute=0, second=0, microsecond=0)
    buckets = []
    cursor = bucket_start
    while cursor < to_time:
        buckets.append({"time": cursor, "requests": 0,
                        "input_tokens": 0, "output_tokens": 0,
                        "cache_read_tokens": 0, "cache_write_tokens": 0,
                        "total_cost": 0})
        cursor += bucket_size
    for entry in rows:
        at = datetime.fromisoformat(entry["at"].replace("Z", "+00:00"))
        if at < bucket_start or not buckets:
            continue
        index = len(buckets) - 1
        for candidate in range(1, len(buckets)):
            if at < buckets[candidate]["time"]:
                index = candidate - 1
                break
        item = buckets[index]
        item["requests"] += 1
        entry_cost = entry.get("cost", {})
        item["input_tokens"] += entry_cost.get("uncached_input_tokens", 0)
        item["output_tokens"] += entry_cost.get("billed_output_tokens", 0)
        item["cache_read_tokens"] += entry_cost.get("cache_read_tokens", 0)
        item["cache_write_tokens"] += entry_cost.get("cache_write_tokens", 0)
        item["total_cost"] += entry_cost.get("total_usd", 0)

    def trend(value):
        return [{"time": iso(item["time"]), "value": value(item)} for item in buckets]

    def total_input(item):
        return item["input_tokens"] + item["cache_read_tokens"] + item["cache_write_tokens"]

    trends = {
        "requests": trend(lambda item: item["requests"]),
        "total_tokens": trend(lambda item: total_input(item) + item["output_tokens"]),
        "input_tokens": trend(lambda item: item["input_tokens"]),
        "output_tokens": trend(lambda item: item["output_tokens"]),
        "cache_read_tokens": trend(lambda item: item["cache_read_tokens"]),
        "cache_write_tokens": trend(lambda item: item["cache_write_tokens"]),
        "cache_rate": trend(lambda item: item["cache_read_tokens"] * 100 / total_input(item)
                            if total_input(item) else 0),
        "total_cost": trend(lambda item: item["total_cost"]),
    }

    return {
        "summary": {
            "requests": requests,
            "succeeded": requests - failed,
            "failed": failed,
            "success_rate": (requests - failed) * 100 / requests if requests else 0,
            "total_tokens": input_tokens + output_tokens,
            "input_tokens": input_tokens,
            "output_tokens": output_tokens,
            "cache_read_tokens": cache_read_tokens,
            "cache_write_tokens": cache_write_tokens,
            "cache_rate": cache_read_tokens * 100 / input_tokens if input_tokens else 0,
            "cost": cost,
        },
        "trends": trends,
        "usage_distribution": {
            "api_keys": [] if scope or selected else distribution("scope", "label"),
            "models": distribution("billing_model", unknown="未知模型"),
            "sources": distribution("source", unknown="未知来源"),
        },
    }


for price in PRICES:
    price["in_models"] = True
PRICES.extend([
    {
        "model_id": "demo-reference", "source": "reference",
        "input_per_1m": 1.5, "output_per_1m": 3, "in_models": True,
    },
    {
        "model_id": "demo-free", "source": "custom",
        "input_per_1m": 0, "output_per_1m": 0, "in_models": True,
    },
    {
        "model_id": "demo-unpriced", "source": "none",
        "input_per_1m": 0, "output_per_1m": 0, "in_models": True,
    },
    {
        "model_id": "demo-retired-custom", "source": "custom",
        "input_per_1m": 1, "output_per_1m": 2, "in_models": False,
    },
])
REFERENCE_PRICES = {
    price["model_id"]: dict(price)
    for price in PRICES if price["source"] == "reference"
}


def price_status():
    return {
        "metadata": {
            "source_url": "https://models.dev/catalog.json",
            "content_hash": "dummy-ui-reference-prices-hash",
            "version": 1,
            "model_count": len(REFERENCE_PRICES),
            "fetched_at": "2026-09-05T00:00:00Z",
            "usable": True,
        },
    }


def model_prices(query, include_custom):
    models = set(query.get("model", []))
    rows = {row["model_id"]: row for row in PRICES}
    names = models | ({row["model_id"] for row in PRICES if row["source"] == "custom"} if include_custom else set())
    return [dict(rows.get(model, {"model_id": model, "source": "none", "input_per_1m": 0, "output_per_1m": 0}),
                 in_models=model in models) for model in sorted(names)]


def credential_labels(refs):
    return {item["ref"]: item["provider"] + " · " + item["display_name"]
            for item in CREDENTIALS if item["ref"] in refs}


def key_rows():
    for key in LIVE_KEYS:
        refresh_key_quota(key)
    return [dict(key, status=key_status(key),
                 route_names={route["id"]: route["name"] for route in ROUTES
                                  if route["id"] in key["route_bindings"]["route_ids"]},
                 credential_labels=credential_labels(key["route_bindings"]["credential_ids"] + key["route_bindings"].get("denied_credential_ids", []))) for key in KEYS]


def route_rows():
    return [dict(route, credential_labels=credential_labels(route["rule"].get("credential_ids", []) + route["rule"].get("denied_credential_ids", []))) for route in ROUTES]


def payload_for(path, query):
    if path == f"{API_BASE}/keys":
        refresh_route_counts()
        return {"keys": key_rows()}
    if path == f"{API_BASE}/plans":
        return {"plans": PLANS}
    if path == f"{API_BASE}/routes":
        refresh_route_counts()
        return {"routes": route_rows()}
    if path == f"{API_BASE}/credentials":
        return {"credentials": CREDENTIALS}
    if path == f"{API_BASE}/prices/reference/status":
        return price_status()
    if path == f"{API_BASE}/prices":
        return model_prices(query, include_custom=query.get("include_custom", ["true"])[0] == "true")
    if path == f"{API_BASE}/events/keys":
        scopes = {event["scope"] for event in filter_event_time(REQUEST_EVENTS, query)}
        return [{field: key[field] for field in ("scope", "preview", "label", "deleted_at") if field in key}
                for key in KEYS if key["scope"] in scopes]
    if path == f"{API_BASE}/events":
        return request_event_view(query)
    if path == f"{API_BASE}/errors":
        return error_view(query)
    if path == f"{API_BASE}/analysis":
        return analysis_view(query)
    if path == f"{API_BASE}/plugin-logs":
        counts = {level: sum(entry["level"] == level for entry in PLUGIN_LOGS)
                  for level in ("debug", "info", "error")}
        levels = query.get("level", ["all"])[0].split(",")
        before = int(query.get("before_id", ["0"])[0])
        limit = int(query.get("limit", ["100"])[0])
        entries = [entry for entry in PLUGIN_LOGS
                   if ("all" in levels or entry["level"] in levels)
                   and (not before or entry["id"] < before)]
        return {"entries": entries[:limit], "level_counts": counts,
                "next_before_id": entries[limit - 1]["id"] if len(entries) > limit else 0}
    if path == f"{API_BASE}/auth-files":
        return {"files": AUTH_FILES}
    if path == f"{API_BASE}/auth-files/quota":
        return auth_file_quota(query)
    if path == f"{API_BASE}/prices/reference":
        term = query.get("q", [""])[0].lower()
        return {"prices": [
            {**price, "provider_id": "demo", "model_id": model, "is_canonical": True}
            for model, price in REFERENCE_PRICES.items() if term in model.lower()
        ]}
    if path in {"/v0/management/config", "/v0/management/api-keys"}:
        config = {"api-keys": [f"sk-demo-{index:04d}" for index in range(1, len(LIVE_KEYS) + 1)]}
        if path == "/v0/management/config":
            config.update({
                "gemini-api-key": [],
                "interactions-api-key": [],
                "xai-api-key": [],
                "vertex-api-key": [],
                "codex-api-key": [{"api-key": "sk-dummy-codex", "prefix": "codex"}],
                "claude-api-key": [{"api-key": "sk-dummy-claude", "prefix": "claude"}],
                "openai-compatibility": [{
                    "name": "DeepSeek",
                    "disabled": False,
                    "api-key-entries": [{"api-key": "sk-dummy-deepseek"}],
                }],
            })
        return config
    if path == "/v1/models":
        return {"data": [{"id": row["model_id"]} for row in PRICES if row.get("in_models")] + [
            {"id": "codex/deepseek-v4-flash-vision-exp"},
        ]}
    return None


class Handler(BaseHTTPRequestHandler):
    def send_response(self, code, message=None):
        time.sleep(random.uniform(0.4, 0.6))
        super().send_response(code, message)

    def send_html(self, body):
        encoded = body.encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Cache-Control", "no-store")
        self.send_header("Content-Length", str(len(encoded)))
        self.end_headers()
        self.wfile.write(encoded)

    def send_json(self, status, payload):
        if 200 <= status < 300 and getattr(self, "mutation_view", None) is not None:
            path = urlparse(self.path).path
            view = {}
            if path in {f"{API_BASE}/plans", f"{API_BASE}/routes"} or path.startswith(f"{API_BASE}/keys/"):
                refresh_route_counts()
                view["keys"] = key_rows()
                if path == f"{API_BASE}/plans":
                    view["plans"] = PLANS
                if path in {f"{API_BASE}/routes", f"{API_BASE}/keys/routes"}:
                    view["routes"] = route_rows()
            elif path in {f"{API_BASE}/prices", f"{API_BASE}/prices/reference/refresh"}:
                view["prices"] = model_prices({"model": self.mutation_view.get("models", [])}, include_custom=True)
                view["metadata"] = price_status()["metadata"]
            elif path == f"{API_BASE}/plugin-logs":
                view["logs_cleared"] = True
            payload = dict(payload, view=view)
        body = json.dumps(payload, ensure_ascii=False).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        self.mutation_view = None
        parsed = urlparse(self.path)
        if parsed.path == "/favicon.ico":
            self.send_response(204)
            self.end_headers()
            return
        if parsed.path in ("/", "/ui"):
            body = UI_PATH.read_text()
            catalogs = {language: json.loads((UI_PATH.parent / "locales" / f"{language}.json").read_text())
                        for language in ("en", "zh-CN")}
            script = "const BILLING_MESSAGES = " + json.dumps(catalogs).replace("<", "\\u003c") + ";\n"
            script += (UI_PATH.parent / "i18n.js").read_text()
            body = body.replace("// BILLING_I18N", script)
            self.send_html(body)
            return
        payload = payload_for(parsed.path, parse_qs(parsed.query))
        if payload is None:
            self.send_json(404, {"error": {"message": "dummy backend: route not found"}})
            return
        self.send_json(200, payload)

    def do_POST(self):
        self.handle_mutation()

    def reset_auth_quota(self, parsed, files):
        query = parse_qs(parsed.query, keep_blank_values=True)
        auth_index = query.get("auth_index", [""])[0]
        auth_file = next((item for item in files if item["auth_index"] == auth_index), None)
        quota = AUTH_FILE_QUOTAS.get(auth_index)
        if not re.fullmatch(r"[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}", self.headers.get("X-Quota-Reset-ID", "")):
            self.send_json(400, {"error": {"message": "Invalid quota reset request ID"}})
        elif auth_file is None or quota is None:
            self.send_json(404, {"error": {"message": "Auth file does not exist"}})
        elif auth_file["category"] != "codex" or auth_file.get("disabled"):
            self.send_json(422, {"error": {"message": "This auth file cannot reset quotas"}})
        elif query.get("auth_revision") != [auth_file["cache_revision"]] or query.get("auth_name") != [auth_file["name"]]:
            self.send_json(409, {"error": {"message": "Auth file changed; refresh the auth file list and try again"}})
        elif quota.get("rate_limit_reset_credits_available_count", 0) <= 0:
            self.send_json(502, {"error": {"message": "No reset credits available"}})
        else:
            quota["rate_limit_reset_credits_available_count"] -= 1
            if quota.get("rate_limit_reset_credits"):
                quota["rate_limit_reset_credits"].pop(0)
            for row in quota["quota"]:
                row["remaining_percent"] = 100
            self.send_json(200, {"reset": True})

    def do_PATCH(self):
        self.handle_mutation()

    def do_PUT(self):
        self.handle_mutation()

    def do_DELETE(self):
        self.handle_mutation()

    def handle_mutation(self):
        parsed = urlparse(self.path)
        self.mutation_view = None
        length = int(self.headers.get("Content-Length", "0"))
        request_body = b""
        if length:
            request_body = self.rfile.read(length)
        if parse_qs(parsed.query).get("view") == ["1"]:
            self.mutation_view = json.loads(request_body or b"{}")
            request_body = json.dumps(self.mutation_view.get("data") or {}).encode()
        route = self.command, parsed.path
        if route == ("POST", f"{API_BASE}/auth-files/quota/reset"):
            self.reset_auth_quota(parsed, AUTH_FILES)
        elif route == ("DELETE", f"{API_BASE}/plugin-logs"):
            cleared = len(PLUGIN_LOGS)
            PLUGIN_LOGS.clear()
            self.send_json(200, {"cleared": cleared})
        elif route == ("POST", f"{API_BASE}/prices/reference/refresh"):
            self.send_json(200, {"metadata": price_status()["metadata"], "changed": False})
        elif route == ("PUT", f"{API_BASE}/prices"):
            body = json.loads(request_body or b"{}")
            row = next((price for price in PRICES if price["model_id"] == body.get("model_id")), None)
            if row is None:
                row = {"in_models": False}
                PRICES.append(row)
            row.update(body)
            row["source"] = "custom"
            self.send_json(200, {"price": row})
        elif route == ("DELETE", f"{API_BASE}/prices"):
            model = parse_qs(parsed.query).get("model_id", [""])[0]
            row = next((price for price in PRICES if price["model_id"] == model), None)
            if row is None or row["source"] != "custom":
                self.send_json(404, {"error": {"message": "自定义价不存在"}})
                return
            if not row["in_models"]:
                PRICES.remove(row)
            else:
                row.update(REFERENCE_PRICES.get(model) or {
                    "source": "none", "input_per_1m": 0, "output_per_1m": 0,
                    "cache_read_per_1m": None, "cache_write_per_1m": None,
                    "long_context": None,
                })
            self.send_json(200, {"deleted": model})
        elif route == ("POST", f"{API_BASE}/keys/reset"):
            body = json.loads(request_body or b"{}")
            targets = [key for key in KEYS if not key.get("deleted_at") and key["plan_id"]
                       and (body.get("mode") == "global" or key["scope"] in body.get("scopes", []))]
            counts = {"keys": 0, "windows": 0}
            for key in targets:
                count = len(QUOTA_CYCLES.get(key["scope"], {}))
                counts["keys"] += bool(count)
                counts["windows"] += count
                QUOTA_CYCLES.pop(key["scope"], None)
                refresh_key_quota(key)
            self.send_json(200, counts)
        elif route == ("POST", f"{API_BASE}/keys/concurrency"):
            body = json.loads(request_body or b"{}")
            for key in KEYS:
                if key["scope"] == body.get("scope"):
                    key["concurrency_limit"] = body.get("concurrency_limit", 0)
                    break
            self.send_json(200, {"ok": True})
        elif route == ("POST", f"{API_BASE}/keys/sync"):
            self.send_json(200, {"added": 0, "deleted": 0})
        elif route == ("POST", f"{API_BASE}/credentials/sync"):
            body = json.loads(request_body or b"{}")
            CREDENTIALS[:] = [
                item for item in CREDENTIALS
                if item["ref"] not in SYNCED_CREDENTIAL_REFS
            ]
            SYNCED_CREDENTIAL_REFS.clear()
            for item in body.get("credentials", []):
                ref = item["ref"]
                SYNCED_CREDENTIAL_REFS.add(ref)
                CREDENTIALS.append({
                    "ref": ref,
                    "source": "ai-providers",
                    "provider": item["provider"],
                    "display_name": item["display_name"],
                    "status": "disabled" if item.get("disabled") else "active",
                    "disabled": bool(item.get("disabled")),
                    "unavailable": False,
                })
            self.send_json(200, {"credentials": CREDENTIALS})
        elif route == ("DELETE", f"{API_BASE}/routes"):
            route_id = parse_qs(parsed.query).get("id", [""])[0]
            affected = 0
            unrestricted = 0
            deleted = 0
            ROUTES[:] = [item for item in ROUTES if item["id"] != route_id]
            for key in KEYS:
                route_ids = key["route_bindings"]["route_ids"]
                if route_id in route_ids:
                    key["route_bindings"]["route_ids"] = [item for item in route_ids if item != route_id]
                    affected += 1
                    deleted += bool(key.get("deleted_at"))
                    unrestricted += not key.get("deleted_at") and not any(key["route_bindings"].values())
            self.send_json(200, {"deleted": route_id, "affected_keys": affected, "deleted_keys": deleted, "fully_unrestricted_keys": unrestricted})
        elif route == ("POST", f"{API_BASE}/routes"):
            body = json.loads(request_body or b"{}")
            route_id = f"route-dummy-{len(ROUTES)}"
            scopes = set(body.get("scopes", []))
            stored = {"id": route_id, "name": body.get("name", "新路由"), "rule": body.get("rule", {})}
            ROUTES.append(stored)
            for key in KEYS:
                if key["scope"] in scopes:
                    key["route_bindings"]["route_ids"].append(route_id)
            self.send_json(201, {"route": stored})
        elif route == ("PATCH", f"{API_BASE}/routes"):
            body = json.loads(request_body or b"{}")
            route_id = body.get("id", "")
            stored = next((item for item in ROUTES if item["id"] == route_id), None)
            if stored is None:
                self.send_json(404, {"error": {"message": "dummy backend: route not found"}})
                return
            stored.update({key: body[key] for key in ("name", "rule") if key in body})
            if "scopes" in body:
                scopes = set(body["scopes"])
                for key in KEYS:
                    route_ids = [item for item in key["route_bindings"]["route_ids"] if item != route_id]
                    if key["scope"] in scopes:
                        route_ids.append(route_id)
                    key["route_bindings"]["route_ids"] = route_ids
            refresh_route_counts()
            self.send_json(200, {"route": stored})
        elif route in {("POST", f"{API_BASE}/plans"), ("PATCH", f"{API_BASE}/plans")}:
            body = json.loads(request_body or b"{}")
            plan_id = body.get("id") or "plan-" + str(time.time_ns())
            stored = next((item for item in PLANS if item["id"] == plan_id), None)
            if self.command == "POST":
                stored = {"id": plan_id}
                PLANS.append(stored)
            if stored is None:
                self.send_json(404, {"error": {"message": "dummy backend: plan not found"}})
                return
            stored.update({key: body[key] for key in ("name", "windows") if key in body})
            for index, window in enumerate(stored["windows"]):
                window.setdefault("id", str(time.time_ns()) + "-" + str(index))
            stored["windows"].sort(key=lambda window: window["period_seconds"])
            if "scopes" in body:
                scopes = set(body["scopes"])
                for key in KEYS:
                    if key["scope"] in scopes and key["plan_id"] != plan_id:
                        key["plan_id"] = plan_id
                        QUOTA_CYCLES.pop(key["scope"], None)
                    elif key["scope"] not in scopes and key["plan_id"] == plan_id:
                        key["plan_id"] = ""
                        QUOTA_CYCLES.pop(key["scope"], None)
            for key in KEYS:
                refresh_key_quota(key)
            self.send_json(201 if self.command == "POST" else 200, {"plan": stored})
        elif route == ("PUT", f"{API_BASE}/keys/routes"):
            body = json.loads(request_body or b"{}")
            bindings = body.get("bindings", {})
            for key in KEYS:
                if key["scope"] == body.get("scope"):
                    key["route_bindings"] = {
                        "route_ids": bindings.get("route_ids", []),
                        "models": bindings.get("models", []),
                        "credential_ids": bindings.get("credential_ids", []),
                        "credential_providers": bindings.get("credential_providers", []),
                        "denied_models": bindings.get("denied_models", []),
                        "denied_credential_ids": bindings.get("denied_credential_ids", []),
                        "denied_credential_providers": bindings.get("denied_credential_providers", []),
                    }
                    break
            refresh_route_counts()
            self.send_json(200, {"ok": True})
        elif route == ("DELETE", f"{API_BASE}/plans"):
            plan_id = parse_qs(parsed.query).get("id", [""])[0]
            PLANS[:] = [plan for plan in PLANS if plan["id"] != plan_id]
            for key in KEYS:
                if key["plan_id"] == plan_id:
                    key["plan_id"] = ""
                    refresh_key_quota(key)
            self.send_json(200, {"deleted": plan_id})
        elif route in {("POST", f"{API_BASE}/keys/bind"), ("POST", f"{API_BASE}/keys/unbind")}:
            body = json.loads(request_body or b"{}")
            for key in KEYS:
                if key["scope"] == body.get("scope"):
                    plan_id = body.get("plan_id", "")
                    if key["plan_id"] != plan_id:
                        key["plan_id"] = plan_id
                        QUOTA_CYCLES.pop(key["scope"], None)
                        refresh_key_quota(key)
            self.send_json(200, {"ok": True})
        elif route == ("POST", f"{API_BASE}/keys/label"):
            body = json.loads(request_body or b"{}")
            for key in KEYS:
                if key["scope"] == body.get("scope"):
                    key["label"] = body.get("label", "").strip()
                    break
            self.send_json(200, {"ok": True})
        else:
            self.send_json(404, {"error": {"message": "dummy backend: route not found"}})

    def log_message(self, message, *args):
        print(f"{self.address_string()} - {message % args}")


def main():
    parser = argparse.ArgumentParser(description="Serve the standalone billing UI with deterministic dummy data.")
    parser.add_argument("--port", type=int, default=8765)
    args = parser.parse_args()
    seed_paginated_history()
    server = ThreadingHTTPServer(("127.0.0.1", args.port), Handler)
    print(
        f"Frontend dummy backend: http://127.0.0.1:{server.server_port}/ui",
        flush=True,
    )
    print("Data is reset on every restart. Press Ctrl-C to stop.", flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
