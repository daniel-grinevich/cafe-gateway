"""Async OpenRouter client: decisions, embeddings and the model catalogue.

Retries 429/5xx and transport errors with exponential backoff + jitter. The upstream key
never leaves this module and is never logged.
"""

from __future__ import annotations

import asyncio
import logging
import random
from dataclasses import dataclass
from typing import Any

import httpx

from barista.config import Settings

log = logging.getLogger(__name__)

RETRY_STATUSES = frozenset({429, 500, 502, 503, 504, 524, 529})


@dataclass
class UpstreamResponse:
    status: int
    body: Any
    attempts: int


class UpstreamUnavailable(Exception):
    """Transport-level failure after all retries."""


class OpenRouterClient:
    def __init__(self, settings: Settings, *, transport: httpx.AsyncBaseTransport | None = None) -> None:
        self._settings = settings
        headers = {
            "HTTP-Referer": settings.openrouter_app_url,
            "X-Title": settings.openrouter_app_title,
        }
        if settings.open_router_key is not None:
            headers["Authorization"] = f"Bearer {settings.open_router_key.get_secret_value()}"
        self._http = httpx.AsyncClient(
            headers=headers,
            timeout=httpx.Timeout(
                settings.upstream_read_timeout_s, connect=settings.upstream_connect_timeout_s
            ),
            transport=transport,
        )

    @property
    def configured(self) -> bool:
        return self._settings.open_router_key is not None

    async def aclose(self) -> None:
        await self._http.aclose()

    async def decide(self, payload: dict[str, Any]) -> UpstreamResponse:
        return await self._post(self._settings.openrouter_decisions_url, payload)

    async def embed(self, payload: dict[str, Any]) -> UpstreamResponse:
        return await self._post(f"{self._settings.openrouter_base_url}/embeddings", payload)

    async def list_models(self) -> list[dict[str, Any]]:
        """Merge the general catalogue with the embeddings catalogue (embedding models aren't
        always listed in /models)."""
        models: dict[str, dict[str, Any]] = {}
        base = self._settings.openrouter_base_url
        for url in (f"{base}/models?output_modalities=all", f"{base}/embeddings/models"):
            try:
                resp = await self._http.get(url)
                resp.raise_for_status()
                for model in resp.json().get("data", []):
                    if isinstance(model, dict) and "id" in model:
                        models.setdefault(model["id"], model)
            except (httpx.HTTPError, ValueError) as exc:
                log.warning("model catalogue fetch failed for %s: %s", url, type(exc).__name__)
        return list(models.values())

    async def _post(self, url: str, payload: dict[str, Any]) -> UpstreamResponse:
        settings = self._settings
        attempt = 0
        while True:
            attempt += 1
            try:
                resp = await self._http.post(url, json=payload)
            except httpx.TransportError as exc:
                if attempt > settings.upstream_max_retries:
                    raise UpstreamUnavailable(type(exc).__name__) from exc
                await self._backoff(attempt, None)
                continue
            if resp.status_code in RETRY_STATUSES and attempt <= settings.upstream_max_retries:
                await self._backoff(attempt, resp.headers.get("retry-after"))
                continue
            try:
                body: Any = resp.json()
            except ValueError:
                body = {"error": {"message": resp.text[:500]}}
            return UpstreamResponse(status=resp.status_code, body=body, attempts=attempt)

    async def _backoff(self, attempt: int, retry_after: str | None) -> None:
        delay = self._settings.upstream_backoff_base_s * 2 ** (attempt - 1)
        if retry_after:
            try:
                delay = max(delay, min(float(retry_after), 10.0))
            except ValueError:
                pass
        await asyncio.sleep(delay * (1 + random.random() * 0.25))
