"""Runtime configuration, read from environment variables (see .env.example)."""

from __future__ import annotations

from decimal import Decimal
from enum import StrEnum
from functools import lru_cache
from typing import Annotated

from pydantic import Field, SecretStr, field_validator
from pydantic_settings import BaseSettings, NoDecode, SettingsConfigDict


class Service(StrEnum):
    """The two gateways served from this codebase.

    barista = OpenRouter Decisions API, grinder = OpenRouter embeddings.
    """

    BARISTA = "barista"
    GRINDER = "grinder"

    @property
    def key_prefix(self) -> str:
        return KEY_PREFIXES[self]

    @property
    def default_scopes(self) -> list[str]:
        return ["decide", "usage"] if self is Service.BARISTA else ["embed", "usage"]


KEY_PREFIXES: dict[Service, str] = {Service.BARISTA: "bar", Service.GRINDER: "grd"}


def _split_csv(value: object) -> object:
    if isinstance(value, str):
        return [item.strip() for item in value.split(",") if item.strip()]
    return value


class Settings(BaseSettings):
    model_config = SettingsConfigDict(env_file=None, extra="ignore")

    # Which gateway this process serves. Same image, two deployments.
    barista_service: Service = Service.BARISTA

    database_url: str = "postgresql+asyncpg://barista:barista@localhost:5444/barista"
    open_router_key: SecretStr | None = None
    barista_admin_token: SecretStr | None = None

    openrouter_base_url: str = "https://openrouter.ai/api/v1"
    # The Decisions endpoint lives outside /api/v1.
    openrouter_decisions_url: str = "https://openrouter.ai/api/alpha/decisions"
    # Sent as HTTP-Referer / X-Title so usage is attributed to this app on openrouter.ai.
    openrouter_app_url: str = "https://github.com/daniel-grinevich/barista"
    openrouter_app_title: str = "barista"
    # Provider preference forced onto every upstream request. "deny" = only providers that
    # don't store/train on prompts. Set to "" to stop forcing it.
    openrouter_data_collection: str = "deny"

    upstream_connect_timeout_s: float = 5.0
    upstream_read_timeout_s: float = 60.0
    upstream_max_retries: int = 2
    upstream_backoff_base_s: float = 0.5

    # Model allow-lists; the first entry is the default model.
    barista_models: Annotated[list[str], NoDecode] = Field(
        default_factory=lambda: ["perplexity/pplx-decider-flash-v1.2"]
    )
    grinder_models: Annotated[list[str], NoDecode] = Field(
        default_factory=lambda: ["qwen/qwen3-embedding-8b"]
    )

    # Global monthly caps per service (USD), protecting the OpenRouter bill. 0 disables.
    barista_monthly_cap_usd: Decimal = Decimal("20")
    grinder_monthly_cap_usd: Decimal = Decimal("20")

    # Defaults for newly created keys.
    default_rpm: int = 30
    default_max_tokens_per_request: int = 8000
    default_monthly_budget_usd: Decimal = Decimal("1")

    # Pricing fallback when neither OpenRouter usage.cost nor the model catalogue has a price.
    # Deliberately pessimistic so budgets still bite.
    fallback_price_per_mtok_usd: Decimal = Decimal("1")
    pricing_ttl_s: int = 6 * 3600

    max_embed_inputs: int = 256

    # Opt-in: store request bodies on usage rows. Off by default for privacy.
    barista_debug_store_bodies: bool = False

    @field_validator("barista_models", "grinder_models", mode="before")
    @classmethod
    def _csv(cls, value: object) -> object:
        return _split_csv(value)

    @field_validator("barista_models", "grinder_models")
    @classmethod
    def _non_empty(cls, value: list[str]) -> list[str]:
        if not value:
            raise ValueError("model allow-list must not be empty")
        return value

    def models_for(self, service: Service) -> list[str]:
        return self.barista_models if service is Service.BARISTA else self.grinder_models

    def monthly_cap_for(self, service: Service) -> Decimal:
        return self.barista_monthly_cap_usd if service is Service.BARISTA else self.grinder_monthly_cap_usd


@lru_cache
def get_settings() -> Settings:
    return Settings()
