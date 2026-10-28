"""Database models and session helpers (SQLAlchemy 2, async)."""

from __future__ import annotations

import uuid
from collections.abc import AsyncIterator
from datetime import UTC, datetime
from decimal import Decimal
from typing import Any

from sqlalchemy import JSON, DateTime, ForeignKey, Index, Integer, Numeric, String, Text
from sqlalchemy.ext.asyncio import AsyncEngine, AsyncSession, async_sessionmaker, create_async_engine
from sqlalchemy.orm import DeclarativeBase, Mapped, mapped_column

MONEY = Numeric(14, 8)


def utcnow() -> datetime:
    return datetime.now(UTC)


class Base(DeclarativeBase):
    pass


class ApiKey(Base):
    __tablename__ = "api_keys"

    id: Mapped[str] = mapped_column(String(36), primary_key=True, default=lambda: str(uuid.uuid4()))
    # Public lookup id embedded in the key: bar_<key_id>_<secret>.
    key_id: Mapped[str] = mapped_column(String(32), unique=True, index=True)
    service: Mapped[str] = mapped_column(String(16), index=True)
    # sha256 hex of the full plaintext key. The plaintext is never stored.
    key_hash: Mapped[str] = mapped_column(String(64))
    name: Mapped[str] = mapped_column(String(128))
    owner: Mapped[str | None] = mapped_column(String(128), nullable=True)
    scopes: Mapped[list[str]] = mapped_column(JSON, default=list)
    # None = any surface allowed.
    allowed_surfaces: Mapped[list[str] | None] = mapped_column(JSON, nullable=True)
    rpm: Mapped[int] = mapped_column(Integer)
    max_tokens_per_request: Mapped[int] = mapped_column(Integer)
    monthly_budget_usd: Mapped[Decimal] = mapped_column(MONEY)
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), default=utcnow)
    last_used_at: Mapped[datetime | None] = mapped_column(DateTime(timezone=True), nullable=True)
    revoked_at: Mapped[datetime | None] = mapped_column(DateTime(timezone=True), nullable=True)

    @property
    def display_prefix(self) -> str:
        from barista.config import KEY_PREFIXES, Service

        return f"{KEY_PREFIXES[Service(self.service)]}_{self.key_id}_…"


class UsageEvent(Base):
    __tablename__ = "usage_events"
    __table_args__ = (
        Index("ix_usage_key_created", "api_key_id", "created_at"),
        Index("ix_usage_service_created", "service", "created_at"),
    )

    id: Mapped[int] = mapped_column(Integer, primary_key=True, autoincrement=True)
    api_key_id: Mapped[str] = mapped_column(ForeignKey("api_keys.id"))
    service: Mapped[str] = mapped_column(String(16))
    endpoint: Mapped[str] = mapped_column(String(64))
    surface: Mapped[str | None] = mapped_column(String(64), nullable=True)
    agent_id: Mapped[str | None] = mapped_column(String(64), nullable=True)
    model: Mapped[str | None] = mapped_column(String(128), nullable=True)
    input_tokens: Mapped[int] = mapped_column(Integer, default=0)
    output_tokens: Mapped[int] = mapped_column(Integer, default=0)
    cost_usd: Mapped[Decimal] = mapped_column(MONEY, default=Decimal(0))
    # "openrouter" (usage.cost), "pricing" (catalogue), "fallback", or "none" (rejected before upstream).
    cost_source: Mapped[str] = mapped_column(String(16), default="none")
    latency_ms: Mapped[int] = mapped_column(Integer, default=0)
    status_code: Mapped[int] = mapped_column(Integer)
    error_code: Mapped[str | None] = mapped_column(String(64), nullable=True)
    upstream_id: Mapped[str | None] = mapped_column(String(128), nullable=True)
    # Only populated when BARISTA_DEBUG_STORE_BODIES=true.
    request_body: Mapped[Any | None] = mapped_column(Text, nullable=True)
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), default=utcnow, index=True)


class Database:
    def __init__(self, url: str) -> None:
        self.engine: AsyncEngine = create_async_engine(url, pool_pre_ping=True)
        self.sessionmaker = async_sessionmaker(self.engine, expire_on_commit=False)

    async def create_all(self) -> None:
        async with self.engine.begin() as conn:
            await conn.run_sync(Base.metadata.create_all)

    async def session(self) -> AsyncIterator[AsyncSession]:
        async with self.sessionmaker() as session:
            yield session

    async def dispose(self) -> None:
        await self.engine.dispose()
