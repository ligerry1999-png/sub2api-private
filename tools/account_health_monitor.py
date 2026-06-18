#!/usr/bin/env python3
"""
Sub2API account health monitor.

This script runs outside the main service. It reads account status from the
Sub2API database through the running Docker container, builds a short Chinese
health report, and optionally sends it to a Feishu custom bot webhook.
"""

from __future__ import annotations

import argparse
import base64
import hashlib
import hmac
import json
import os
import subprocess
import sys
import time
import urllib.error
import urllib.request
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any


DEFAULT_ENV_FILE = Path(__file__).with_suffix(".env")


ACCOUNT_SQL = r"""
WITH account_group_names AS (
  SELECT
    ag.account_id,
    string_agg(g.name, ', ' ORDER BY ag.priority, g.id) AS group_names
  FROM account_groups ag
  JOIN groups g ON g.id = ag.group_id AND g.deleted_at IS NULL
  GROUP BY ag.account_id
)
SELECT COALESCE(json_agg(row_to_json(t)), '[]'::json)
FROM (
  SELECT
    a.id,
    a.name,
    a.platform,
    a.type,
    a.status,
    a.schedulable,
    a.error_message,
    a.temp_unschedulable_reason,
    a.concurrency,
    a.priority,
    COALESCE(agn.group_names, '') AS group_names,
    COALESCE(a.credentials->>'email', a.extra->>'email', '') AS email,
    COALESCE(a.credentials->>'plan_type', a.extra->>'plan_type', '') AS plan_type,
    COALESCE(a.credentials->>'chatgpt_account_id', a.extra->>'chatgpt_account_id', '') AS chatgpt_account_id,
    to_char(a.rate_limit_reset_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"') AS rate_limit_reset_at,
    EXTRACT(EPOCH FROM a.rate_limit_reset_at)::bigint AS rate_limit_reset_epoch,
    to_char(a.overload_until AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"') AS overload_until,
    EXTRACT(EPOCH FROM a.overload_until)::bigint AS overload_until_epoch,
    to_char(a.temp_unschedulable_until AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"') AS temp_unschedulable_until,
    EXTRACT(EPOCH FROM a.temp_unschedulable_until)::bigint AS temp_unschedulable_until_epoch,
    to_char(a.expires_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"') AS expires_at,
    EXTRACT(EPOCH FROM a.expires_at)::bigint AS expires_at_epoch,
    to_char(a.last_used_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"') AS last_used_at,
    EXTRACT(EPOCH FROM a.last_used_at)::bigint AS last_used_at_epoch,
    to_char(a.updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"') AS updated_at,
    EXTRACT(EPOCH FROM a.updated_at)::bigint AS updated_at_epoch
  FROM accounts a
  LEFT JOIN account_group_names agn ON agn.account_id = a.id
  WHERE a.deleted_at IS NULL
  ORDER BY a.platform, a.id
) t;
"""


@dataclass
class Config:
    container: str
    env_file: Path
    state_file: Path
    feishu_webhook: str
    feishu_secret: str
    timezone_name: str
    max_items: int
    warn_expire_days: int
    require_group: bool
    send_when_ok: bool
    send_only_on_change: bool


def load_env_file(path: Path) -> dict[str, str]:
    values: dict[str, str] = {}
    if not path.exists():
        return values
    for raw_line in path.read_text(encoding="utf-8").splitlines():
        line = raw_line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        key = key.strip()
        value = value.strip().strip('"').strip("'")
        values[key] = value
    return values


def env_bool(values: dict[str, str], key: str, default: bool) -> bool:
    raw = os.environ.get(key, values.get(key))
    if raw is None or raw == "":
        return default
    return raw.strip().lower() in {"1", "true", "yes", "y", "on"}


def env_int(values: dict[str, str], key: str, default: int) -> int:
    raw = os.environ.get(key, values.get(key))
    if raw is None or raw == "":
        return default
    try:
        return int(raw)
    except ValueError:
        return default


def build_config(args: argparse.Namespace) -> Config:
    env_file = Path(args.env_file or os.environ.get("MONITOR_ENV") or DEFAULT_ENV_FILE)
    values = load_env_file(env_file)
    state_file = Path(os.environ.get("STATE_FILE", values.get("STATE_FILE", "/opt/sub2api-monitor/state.json")))
    return Config(
        container=os.environ.get("SUB2API_CONTAINER", values.get("SUB2API_CONTAINER", "sub2api-private")),
        env_file=env_file,
        state_file=state_file,
        feishu_webhook=os.environ.get("FEISHU_WEBHOOK", values.get("FEISHU_WEBHOOK", "")),
        feishu_secret=os.environ.get("FEISHU_SECRET", values.get("FEISHU_SECRET", "")),
        timezone_name=os.environ.get("TIMEZONE", values.get("TIMEZONE", "Asia/Shanghai")),
        max_items=env_int(values, "MAX_ITEMS", 20),
        warn_expire_days=env_int(values, "WARN_EXPIRE_DAYS", 7),
        require_group=env_bool(values, "REQUIRE_GROUP", True),
        send_when_ok=env_bool(values, "SEND_WHEN_OK", True),
        send_only_on_change=env_bool(values, "SEND_ONLY_ON_CHANGE", False),
    )


def load_local_timezone(name: str) -> timezone:
    try:
        from zoneinfo import ZoneInfo

        return ZoneInfo(name)  # type: ignore[return-value]
    except Exception:
        return timezone(timedelta(hours=8))


def run_psql_json(container: str, sql: str) -> list[dict[str, Any]]:
    cmd = [
        "docker",
        "exec",
        "-i",
        container,
        "sh",
        "-lc",
        (
            'PGPASSWORD="$DATABASE_PASSWORD" '
            'psql -h "$DATABASE_HOST" -p "$DATABASE_PORT" '
            '-U "$DATABASE_USER" -d "$DATABASE_DBNAME" '
            "-X -q -t -A -v ON_ERROR_STOP=1 -f -"
        ),
    ]
    result = subprocess.run(
        cmd,
        input=sql,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    if result.returncode != 0:
        raise RuntimeError(f"读取数据库失败：{result.stderr.strip() or result.stdout.strip()}")
    output = result.stdout.strip()
    if not output:
        return []
    parsed = json.loads(output)
    if not isinstance(parsed, list):
        raise RuntimeError("数据库返回格式异常，不是账号列表。")
    return parsed


def epoch(value: Any) -> int | None:
    if value is None or value == "":
        return None
    try:
        return int(value)
    except (TypeError, ValueError):
        return None


def fmt_epoch(ts: int | None, tz: timezone) -> str:
    if not ts:
        return "-"
    return datetime.fromtimestamp(ts, timezone.utc).astimezone(tz).strftime("%m-%d %H:%M")


def fmt_remaining(until: int | None, now_ts: int) -> str:
    if not until or until <= now_ts:
        return ""
    seconds = until - now_ts
    if seconds < 60:
        return f"{seconds}秒"
    if seconds < 3600:
        return f"{seconds // 60}分钟"
    if seconds < 86400:
        return f"{seconds // 3600}小时{(seconds % 3600) // 60}分钟"
    return f"{seconds // 86400}天{(seconds % 86400) // 3600}小时"


def account_label(account: dict[str, Any]) -> str:
    name = str(account.get("name") or "").strip()
    email = str(account.get("email") or "").strip()
    plan = str(account.get("plan_type") or "").strip()
    label = name or email or f"账号#{account.get('id')}"
    if email and email not in label:
        label = f"{label} / {email}"
    if plan:
        label = f"{label} [{plan}]"
    return label


def analyze_accounts(
    accounts: list[dict[str, Any]],
    cfg: Config,
    local_tz: timezone,
) -> tuple[dict[str, Any], list[dict[str, Any]]]:
    now_ts = int(time.time())
    expiring_before = now_ts + cfg.warn_expire_days * 86400

    summary = {
        "total": len(accounts),
        "available": 0,
        "issues": 0,
        "rate_limited": 0,
        "overloaded": 0,
        "temp_unschedulable": 0,
        "disabled": 0,
        "errors": 0,
        "expired": 0,
        "expiring": 0,
        "no_group": 0,
    }
    analyzed: list[dict[str, Any]] = []

    for account in accounts:
        status = str(account.get("status") or "").lower()
        schedulable = bool(account.get("schedulable", True))
        group_names = str(account.get("group_names") or "").strip()
        error_message = str(account.get("error_message") or "").strip()
        temp_reason = str(account.get("temp_unschedulable_reason") or "").strip()

        rate_limit_until = epoch(account.get("rate_limit_reset_epoch"))
        overload_until = epoch(account.get("overload_until_epoch"))
        temp_until = epoch(account.get("temp_unschedulable_until_epoch"))
        expires_at = epoch(account.get("expires_at_epoch"))

        reasons: list[str] = []
        if status and status not in {"active", "enabled", "normal"}:
            reasons.append(f"状态={status}")
            summary["disabled"] += 1
        if not schedulable:
            reasons.append("不可调度")
            summary["disabled"] += 1
        if rate_limit_until and rate_limit_until > now_ts:
            reasons.append(f"限流中，约{fmt_remaining(rate_limit_until, now_ts)}后恢复")
            summary["rate_limited"] += 1
        if overload_until and overload_until > now_ts:
            reasons.append(f"过载保护，约{fmt_remaining(overload_until, now_ts)}后恢复")
            summary["overloaded"] += 1
        if temp_until and temp_until > now_ts:
            detail = f"临时不可用，约{fmt_remaining(temp_until, now_ts)}后恢复"
            if temp_reason:
                detail = f"{detail}：{temp_reason[:80]}"
            reasons.append(detail)
            summary["temp_unschedulable"] += 1
        if error_message:
            reasons.append(f"错误：{error_message[:120]}")
            summary["errors"] += 1
        if expires_at and expires_at < now_ts:
            reasons.append("订阅/凭证已过期")
            summary["expired"] += 1
        elif expires_at and expires_at <= expiring_before:
            reasons.append(f"{cfg.warn_expire_days}天内到期：{fmt_epoch(expires_at, local_tz)}")
            summary["expiring"] += 1
        if cfg.require_group and not group_names:
            reasons.append("未加入任何分组")
            summary["no_group"] += 1

        is_available = len(reasons) == 0
        if is_available:
            summary["available"] += 1
        else:
            summary["issues"] += 1

        analyzed.append(
            {
                "account": account,
                "is_available": is_available,
                "reasons": reasons,
                "signature": "|".join(reasons),
            }
        )

    return summary, analyzed


def summarize_by_group(accounts: list[dict[str, Any]]) -> list[str]:
    counts: dict[str, int] = {}
    for account in accounts:
        groups = str(account.get("group_names") or "未分组").split(",")
        for group in groups:
            name = group.strip() or "未分组"
            counts[name] = counts.get(name, 0) + 1
    return [f"{name}:{count}" for name, count in sorted(counts.items())]


def build_report(
    summary: dict[str, Any],
    analyzed: list[dict[str, Any]],
    cfg: Config,
    local_tz: timezone,
) -> str:
    now_text = datetime.now(local_tz).strftime("%Y-%m-%d %H:%M:%S")
    accounts = [item["account"] for item in analyzed]
    issue_items = [item for item in analyzed if not item["is_available"]]

    lines = [
        "Sub2API 账号健康报告",
        f"时间：{now_text}",
        (
            "概况："
            f"总数 {summary['total']}，"
            f"可用 {summary['available']}，"
            f"需处理 {summary['issues']}，"
            f"限流 {summary['rate_limited']}，"
            f"过载 {summary['overloaded']}，"
            f"错误 {summary['errors']}，"
            f"未分组 {summary['no_group']}"
        ),
    ]

    group_summary = summarize_by_group(accounts)
    if group_summary:
        lines.append("分组：" + "；".join(group_summary[:12]))

    if not issue_items:
        lines.append("结论：当前账号池没有发现需要处理的问题。")
        return "\n".join(lines)

    lines.append("")
    lines.append(f"需要处理的账号（最多显示 {cfg.max_items} 条）：")
    for item in issue_items[: cfg.max_items]:
        account = item["account"]
        groups = str(account.get("group_names") or "未分组").strip()
        last_used = fmt_epoch(epoch(account.get("last_used_at_epoch")), local_tz)
        reasons = "；".join(item["reasons"])
        lines.append(f"- #{account.get('id')} {account_label(account)}")
        lines.append(f"  分组：{groups}｜上次使用：{last_used}｜问题：{reasons}")

    omitted = len(issue_items) - cfg.max_items
    if omitted > 0:
        lines.append(f"... 还有 {omitted} 个账号未展开。")

    return "\n".join(lines)


def issue_signature(summary: dict[str, Any], analyzed: list[dict[str, Any]]) -> str:
    parts = [
        f"total={summary['total']}",
        f"available={summary['available']}",
        f"issues={summary['issues']}",
    ]
    for item in analyzed:
        if not item["is_available"]:
            account = item["account"]
            parts.append(f"{account.get('id')}:{item['signature']}")
    raw = "\n".join(parts)
    return hashlib.sha256(raw.encode("utf-8")).hexdigest()


def should_send(cfg: Config, summary: dict[str, Any], analyzed: list[dict[str, Any]], force: bool) -> tuple[bool, str]:
    if force:
        return True, "手动强制发送"
    if summary["issues"] == 0 and not cfg.send_when_ok:
        return False, "全部正常，配置为不发送正常报告"
    if not cfg.send_only_on_change:
        return True, "定时报送"

    signature = issue_signature(summary, analyzed)
    previous = {}
    if cfg.state_file.exists():
        try:
            previous = json.loads(cfg.state_file.read_text(encoding="utf-8"))
        except Exception:
            previous = {}
    if previous.get("signature") == signature:
        return False, "账号状态无变化"
    return True, "账号状态有变化"


def save_state(cfg: Config, summary: dict[str, Any], analyzed: list[dict[str, Any]]) -> None:
    cfg.state_file.parent.mkdir(parents=True, exist_ok=True)
    state = {
        "updated_at": int(time.time()),
        "signature": issue_signature(summary, analyzed),
        "summary": summary,
    }
    cfg.state_file.write_text(json.dumps(state, ensure_ascii=False, indent=2), encoding="utf-8")


def feishu_payload(text: str, secret: str) -> dict[str, Any]:
    payload: dict[str, Any] = {"msg_type": "text", "content": {"text": text}}
    if secret:
        timestamp = str(int(time.time()))
        string_to_sign = f"{timestamp}\n{secret}".encode("utf-8")
        sign = base64.b64encode(hmac.new(string_to_sign, b"", digestmod=hashlib.sha256).digest()).decode("utf-8")
        payload["timestamp"] = timestamp
        payload["sign"] = sign
    return payload


def send_feishu(webhook: str, secret: str, text: str) -> None:
    if not webhook:
        raise RuntimeError("还没有配置 FEISHU_WEBHOOK，无法发送飞书。")
    data = json.dumps(feishu_payload(text, secret), ensure_ascii=False).encode("utf-8")
    request = urllib.request.Request(
        webhook,
        data=data,
        headers={"Content-Type": "application/json; charset=utf-8"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(request, timeout=15) as response:
            body = response.read().decode("utf-8", errors="replace")
            if response.status >= 300:
                raise RuntimeError(f"飞书返回 HTTP {response.status}: {body}")
            parsed = json.loads(body)
            if parsed.get("code", 0) != 0:
                raise RuntimeError(f"飞书发送失败：{body}")
    except urllib.error.URLError as exc:
        raise RuntimeError(f"飞书网络请求失败：{exc}") from exc


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Monitor Sub2API account health and send Feishu report.")
    parser.add_argument("--env-file", help="Path to .env config file.")
    parser.add_argument("--dry-run", action="store_true", help="Print report only; do not send Feishu.")
    parser.add_argument("--send", action="store_true", help="Send report to Feishu if rules allow.")
    parser.add_argument("--force", action="store_true", help="Ignore change rules and force sending.")
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    cfg = build_config(args)
    local_tz = load_local_timezone(cfg.timezone_name)

    try:
        accounts = run_psql_json(cfg.container, ACCOUNT_SQL)
        summary, analyzed = analyze_accounts(accounts, cfg, local_tz)
        report = build_report(summary, analyzed, cfg, local_tz)
        print(report)

        if args.dry_run or not args.send:
            return 0

        ok_to_send, reason = should_send(cfg, summary, analyzed, args.force)
        if not ok_to_send:
            print(f"跳过发送：{reason}")
            save_state(cfg, summary, analyzed)
            return 0

        send_feishu(cfg.feishu_webhook, cfg.feishu_secret, report)
        save_state(cfg, summary, analyzed)
        print(f"已发送飞书：{reason}")
        return 0
    except Exception as exc:
        print(f"监控失败：{exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
