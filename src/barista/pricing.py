"""Cost computation: OpenRouter's usage.cost first, then cached catalogue pricing, then a
pessimistic fallback so budgets still apply when both are missing."""

from __future__ import annotations

import asyncio
import logging
import time
from dataclasses import dataclass
from decimal import Decimal, InvalidOperation
from typing import Any

from barista.config import Settings
from barista.openrouter import OpenRouterClient

log = logging.getLogger(__name__)

MTOK = Decimal(1_000_000)


@dataclass(frozen=True)
class ModelPrice:
    prompt: Decimal  # USD per input token
    completion: Decimal  # USD per output token
    request: Decimal  # USD per request


@dataclass(frozen=True)
class Cost:
    usd: Decimal
    source: str  # openrouter | pricing | fallback


def _dec(value: Any) -> Decimal:
    try:
        return Decimal(str(value)) if value not in (None, "") else Decimal(0)
    except InvalidOperation:
        return Decimal(0)


class PricingCache:
    def __init__(self, settings: Settings, client: OpenRouterClient) -> None:
        self._settings = settings
        self._client = client
        self._prices: dict[str, ModelPrice] = {}
        self._fetched_at = 0.0
        self._lock = asyncio.Lock()

    def set_prices(self, prices: dict[str, ModelPrice]) -> None:
        self._prices = dict(prices)
        self._fetched_at = time.monotonic()

    async def price(self, model: str) -> ModelPrice | None:
        if time.monotonic() - self._fetched_at > self._settings.pricing_ttl_s:
            async with self._lock:
                if time.monotonic() - self._fetched_at > self._settings.pricing_ttl_s:
                    await self._refresh()
        return self._prices.get(model)

    async def _refresh(self) -> None:
        if not self._client.configured:
            self._fetched_at = time.monotonic()
            return
        prices: dict[str, ModelPrice] = {}
        for model in await self._client.list_models():
            pricing = model.get("pricing") or {}
            price = ModelPrice(
                prompt=_dec(pricing.get("prompt")),
                completion=_dec(pricing.get("completion")),
                request=_dec(pricing.get("request")),
            )
            prices[model["id"]] = price
            if slug := model.get("canonical_slug"):
                prices.setdefault(slug, price)
        if prices:
            self._prices = prices
        # Even on failure, wait a TTL before retrying so a dead catalogue doesn't add latency to every call.
        self._fetched_at = time.monotonic()

    def fallback_per_token(self) -> Decimal:
        return self._settings.fallback_price_per_mtok_usd / MTOK

    async def estimate(self, model: str, input_tokens: int) -> Decimal:
        price = await self.price(model)
        if price is None:
            return self.fallback_per_token() * input_tokens
        return price.prompt * input_tokens + price.request

    async def cost(self, model: str, input_tokens: int, output_tokens: int, reported: Any) -> Cost:
        if reported is not None:
            return Cost(_dec(reported), "openrouter")
        price = await self.price(model)
        if price is None:
            per_token = self.fallback_per_token()
            return Cost(per_token * (input_tokens + output_tokens), "fallback")
        usd = price.prompt * input_tokens + price.completion * output_tokens + price.request
        return Cost(usd, "pricing")
