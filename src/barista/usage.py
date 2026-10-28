"""Usage logging and monthly aggregation, shared by the HTTP API and the CLI."""

from __future__ import annotations

import re
from dataclasses import dataclass
from datetime import UTC, datetime
from decimal import Decimal
from typing import Any, Literal

from sqlalchemy import func, select
from sqlalchemy.ext.asyncio import AsyncSession

from barista.db import ApiKey, UsageEvent

GroupBy = Literal["surface", "agent", "model", "key", "day", "service", "status"]
GROUP_BYS: tuple[str, ...] = ("surface", "agent", "model", "key", "day", "service", "status")

_MONTH_RE = re.compile(r"^\d{4}-(0[1-9]|1[0-2])$")


def month_bounds(month: str | None, now: datetime | None = None) -> tuple[str, datetime, datetime]:
    """Return (YYYY-MM, start, end) in UTC. ``month`` defaults to the current month."""
    now = now or datetime.now(UTC)
    if month is None:
        year, mon = now.year, now.month
    else:
        if not _MONTH_RE.match(month):
            raise ValueError("month must be YYYY-MM")
        year, mon = int(month[:4]), int(month[5:])
    start = datetime(year, mon, 1, tzinfo=UTC)
    end = datetime(year + 1, 1, 1, tzinfo=UTC) if mon == 12 else datetime(year, mon + 1, 1, tzinfo=UTC)
    return f"{year:04d}-{mon:02d}", start, end


@dataclass(frozen=True)
class UsageFilter:
    month: str | None = None
    service: str | None = None
    api_key_id: str | None = None
    surface: str | None = None
    agent_id: str | None = None


async def month_spend(session: AsyncSession, *, api_key_id: str | None = None, service: str | None = None) -> Decimal:
    _, start, end = month_bounds(None)
    stmt = select(func.coalesce(func.sum(UsageEvent.cost_usd), 0)).where(
        UsageEvent.created_at >= start, UsageEvent.created_at < end
    )
    if api_key_id is not None:
        stmt = stmt.where(UsageEvent.api_key_id == api_key_id)
    if service is not None:
        stmt = stmt.where(UsageEvent.service == service)
    return Decimal(str((await session.execute(stmt)).scalar_one()))


def _group_column(group_by: str) -> Any:
    match group_by:
        case "surface":
            return UsageEvent.surface
        case "agent":
            return UsageEvent.agent_id
        case "model":
            return UsageEvent.model
        case "key":
            return ApiKey.name + " (" + ApiKey.key_id + ")"
        case "service":
            return UsageEvent.service
        case "status":
            return UsageEvent.status_code
        case "day":
            return func.date(UsageEvent.created_at)
        case _:
            raise ValueError(f"group_by must be one of {', '.join(GROUP_BYS)}")


async def summarize(session: AsyncSession, flt: UsageFilter, group_by: str | None = None) -> dict[str, Any]:
    month, start, end = month_bounds(flt.month)
    aggregates = (
        func.count(UsageEvent.id),
        func.coalesce(func.sum(UsageEvent.input_tokens), 0),
        func.coalesce(func.sum(UsageEvent.output_tokens), 0),
        func.coalesce(func.sum(UsageEvent.cost_usd), 0),
    )

    def _where(stmt: Any) -> Any:
        stmt = stmt.where(UsageEvent.created_at >= start, UsageEvent.created_at < end)
        if flt.service:
            stmt = stmt.where(UsageEvent.service == flt.service)
        if flt.api_key_id:
            stmt = stmt.where(UsageEvent.api_key_id == flt.api_key_id)
        if flt.surface:
            stmt = stmt.where(UsageEvent.surface == flt.surface)
        if flt.agent_id:
            stmt = stmt.where(UsageEvent.agent_id == flt.agent_id)
        return stmt

    def _row(requests: int, inp: int, out: int, cost: Any) -> dict[str, Any]:
        return {
            "requests": int(requests),
            "input_tokens": int(inp),
            "output_tokens": int(out),
            "cost_usd": str(Decimal(str(cost)).quantize(Decimal("0.00000001"))),
        }

    total = (await session.execute(_where(select(*aggregates)))).one()
    result: dict[str, Any] = {
        "month": month,
        "filters": {
            k: v for k, v in {"service": flt.service, "surface": flt.surface, "agent_id": flt.agent_id}.items() if v
        },
        "totals": _row(*total),
    }
    if group_by:
        col = _group_column(group_by).label("group")
        stmt = _where(select(col, *aggregates).join(ApiKey, ApiKey.id == UsageEvent.api_key_id))
        stmt = stmt.group_by(col).order_by(func.sum(UsageEvent.cost_usd).desc())
        rows = (await session.execute(stmt)).all()
        result["group_by"] = group_by
        result["groups"] = [
            {"group": None if g is None else str(g), **_row(r, i, o, c)} for g, r, i, o, c in rows
        ]
    return result
