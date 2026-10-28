"""Key management shared by the CLI and the admin HTTP endpoints."""

from __future__ import annotations

from dataclasses import dataclass
from decimal import Decimal
from typing import Any

from sqlalchemy import select
from sqlalchemy.ext.asyncio import AsyncSession

from barista.attribution import validate_slug
from barista.config import Service, Settings
from barista.db import ApiKey, utcnow
from barista.keys import generate_key, hash_key

ALL_SCOPES = frozenset({"decide", "embed", "usage"})


@dataclass(frozen=True)
class CreatedKey:
    key: ApiKey
    plaintext: str


def _check_scopes(service: Service, scopes: list[str]) -> list[str]:
    allowed = set(service.default_scopes)
    bad = [s for s in scopes if s not in allowed]
    if bad:
        raise ValueError(f"scopes {bad} are not valid for {service.value}; allowed: {sorted(allowed)}")
    return sorted(set(scopes))


async def create_key(
    session: AsyncSession,
    settings: Settings,
    *,
    service: Service,
    name: str,
    owner: str | None = None,
    rpm: int | None = None,
    monthly_budget_usd: Decimal | None = None,
    max_tokens_per_request: int | None = None,
    scopes: list[str] | None = None,
    allowed_surfaces: list[str] | None = None,
) -> CreatedKey:
    if not name.strip():
        raise ValueError("name must not be empty")
    rpm = settings.default_rpm if rpm is None else rpm
    budget = settings.default_monthly_budget_usd if monthly_budget_usd is None else monthly_budget_usd
    max_tokens = settings.default_max_tokens_per_request if max_tokens_per_request is None else max_tokens_per_request
    if rpm < 1 or max_tokens < 1 or budget < 0:
        raise ValueError("rpm and max_tokens must be >= 1 and budget >= 0")
    surfaces = None
    if allowed_surfaces:
        surfaces = sorted({validate_slug("surface", s) for s in allowed_surfaces})
    key_id, plaintext = generate_key(service)
    key = ApiKey(
        key_id=key_id,
        service=service.value,
        key_hash=hash_key(plaintext),
        name=name.strip(),
        owner=owner,
        scopes=_check_scopes(service, scopes or service.default_scopes),
        allowed_surfaces=surfaces,
        rpm=rpm,
        max_tokens_per_request=max_tokens,
        monthly_budget_usd=budget,
    )
    session.add(key)
    await session.commit()
    return CreatedKey(key=key, plaintext=plaintext)


async def list_keys(
    session: AsyncSession, *, service: Service | None = None, include_revoked: bool = True
) -> list[ApiKey]:
    stmt = select(ApiKey).order_by(ApiKey.created_at)
    if service is not None:
        stmt = stmt.where(ApiKey.service == service.value)
    if not include_revoked:
        stmt = stmt.where(ApiKey.revoked_at.is_(None))
    return list((await session.execute(stmt)).scalars())


async def find_key(session: AsyncSession, key_id: str) -> ApiKey | None:
    # Accept the bare key_id or a pasted display prefix like "bar_abc123def456_…".
    parts = key_id.split("_")
    if len(parts) >= 2 and parts[0] in ("bar", "grd"):
        key_id = parts[1]
    return (await session.execute(select(ApiKey).where(ApiKey.key_id == key_id))).scalar_one_or_none()


async def revoke_key(session: AsyncSession, key_id: str) -> ApiKey | None:
    key = await find_key(session, key_id)
    if key is None:
        return None
    if key.revoked_at is None:
        key.revoked_at = utcnow()
        await session.commit()
    return key


def key_to_dict(key: ApiKey) -> dict[str, Any]:
    return {
        "key_id": key.key_id,
        "prefix": key.display_prefix,
        "service": key.service,
        "name": key.name,
        "owner": key.owner,
        "scopes": key.scopes,
        "allowed_surfaces": key.allowed_surfaces,
        "rpm": key.rpm,
        "max_tokens_per_request": key.max_tokens_per_request,
        "monthly_budget_usd": str(key.monthly_budget_usd),
        "created_at": key.created_at.isoformat() if key.created_at else None,
        "last_used_at": key.last_used_at.isoformat() if key.last_used_at else None,
        "revoked_at": key.revoked_at.isoformat() if key.revoked_at else None,
    }
