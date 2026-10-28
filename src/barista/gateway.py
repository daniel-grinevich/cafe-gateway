"""The request pipeline shared by /v1/decide and /v1/embed.

auth (401) -> scope (403) -> attribution headers (400) -> allowed surface (403) -> rate limit (429)
-> model allow-list (400) -> max tokens (413) -> key budget / service cap (402) -> upstream
-> cost -> usage row. Every step after auth writes a usage row, rejections included.
"""

from __future__ import annotations

import json
import logging
import time
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from decimal import Decimal
from typing import Any

from fastapi import Request
from fastapi.responses import JSONResponse
from sqlalchemy import select

from barista.attribution import Attribution, read_attribution
from barista.config import Service, Settings
from barista.db import ApiKey, Database, UsageEvent, utcnow
from barista.errors import ApiError
from barista.keys import parse_key, verify_key
from barista.openrouter import OpenRouterClient, UpstreamResponse, UpstreamUnavailable
from barista.pricing import PricingCache
from barista.ratelimit import RateLimiter
from barista.usage import month_spend

log = logging.getLogger(__name__)

# Approximation used before the call, when real token counts aren't known yet.
CHARS_PER_TOKEN = 4
TOKENS_PER_IMAGE = 1000


@dataclass
class AppState:
    service: Service
    settings: Settings
    db: Database
    client: OpenRouterClient
    pricing: PricingCache
    limiter: RateLimiter


def get_state(request: Request) -> AppState:
    return request.app.state.barista


def _bearer(request: Request) -> str | None:
    auth = request.headers.get("authorization")
    if auth and auth.lower().startswith("bearer "):
        return auth[7:].strip() or None
    return (request.headers.get("x-api-key") or "").strip() or None


async def authenticate(request: Request, state: AppState) -> ApiKey:
    plaintext = _bearer(request)
    if plaintext is None:
        raise ApiError(
            401,
            "missing_api_key",
            "Send your key as 'Authorization: Bearer <key>' or 'X-API-Key: <key>'.",
            headers={"WWW-Authenticate": "Bearer"},
        )
    parsed = parse_key(plaintext)
    if parsed is None:
        raise ApiError(401, "invalid_api_key", "API key is malformed or unknown.")
    if parsed.service is not state.service:
        raise ApiError(
            401,
            "wrong_service",
            f"This is a {parsed.service.value} key; it doesn't work on {state.service.value}.",
        )
    async with state.db.sessionmaker() as session:
        stmt = select(ApiKey).where(ApiKey.key_id == parsed.key_id)
        api_key = (await session.execute(stmt)).scalar_one_or_none()
    if (
        api_key is None
        or not verify_key(plaintext, api_key.key_hash)
        or api_key.service != state.service.value
    ):
        raise ApiError(401, "invalid_api_key", "API key is malformed or unknown.")
    if api_key.revoked_at is not None:
        raise ApiError(401, "revoked_api_key", "This API key has been revoked.")
    return api_key


def require_scope(key: ApiKey, scope: str) -> None:
    if scope not in (key.scopes or []):
        raise ApiError(403, "insufficient_scope", f"This key lacks the '{scope}' scope.")


def check_surface(key: ApiKey, attribution: Attribution) -> None:
    if key.allowed_surfaces is not None and attribution.surface not in key.allowed_surfaces:
        raise ApiError(
            403,
            "surface_not_allowed",
            f"This key may only be used from surfaces: {', '.join(key.allowed_surfaces)}.",
        )


def estimate_tokens(value: Any) -> int:
    text = value if isinstance(value, str) else json.dumps(value, ensure_ascii=False, separators=(",", ":"))
    return max(1, -(-len(text) // CHARS_PER_TOKEN))


@dataclass
class Billable:
    endpoint: str
    scope: str
    model: str
    estimated_tokens: int
    payload: dict[str, Any]
    call: Callable[[dict[str, Any]], Awaitable[UpstreamResponse]]
    # (input_tokens, output_tokens, reported_cost) from the upstream body.
    read_usage: Callable[[Any], tuple[int, int, Any]]


class Gateway:
    def __init__(self, state: AppState) -> None:
        self.state = state

    def resolve_model(self, requested: str | None) -> str:
        allowed = self.state.settings.models_for(self.state.service)
        if requested is None:
            return allowed[0]
        if requested not in allowed:
            raise ApiError(
                400, "model_not_allowed", f"Model '{requested}' is not allowed here.", allowed_models=allowed
            )
        return requested

    def apply_provider_prefs(self, payload: dict[str, Any], key: ApiKey) -> dict[str, Any]:
        settings = self.state.settings
        provider = payload.get("provider")
        provider = dict(provider) if isinstance(provider, dict) else {}
        if settings.openrouter_data_collection:
            # Forced, not defaulted: a caller can't opt the user's account into data collection.
            provider["data_collection"] = settings.openrouter_data_collection
        if provider:
            payload["provider"] = provider
        # Stable per-key end-user id for OpenRouter's abuse tooling, unless the caller set one.
        payload.setdefault("user", f"{self.state.service.value}:{key.key_id}")
        return payload

    async def handle(self, request: Request, key: ApiKey, build: Callable[[], Billable]) -> JSONResponse:
        state = self.state
        settings = state.settings
        started = time.perf_counter()
        attribution: Attribution | None = None
        model: str | None = None
        endpoint = request.url.path
        debug_body = await request.body() if settings.barista_debug_store_bodies else None

        async def record(status: int, error_code: str | None = None, **fields: Any) -> None:
            await self._record(
                key,
                endpoint=endpoint,
                attribution=attribution,
                model=model,
                status=status,
                error_code=error_code,
                latency_ms=int((time.perf_counter() - started) * 1000),
                request_body=debug_body.decode(errors="replace") if debug_body is not None else None,
                **fields,
            )

        try:
            attribution = read_attribution(request.headers)
            check_surface(key, attribution)
            billable = build()
            model = billable.model
            require_scope(key, billable.scope)
            retry_after = state.limiter.try_acquire(key.id, key.rpm)
            if retry_after is not None:
                raise ApiError(
                    429,
                    "rate_limited",
                    f"Rate limit of {key.rpm} requests/minute exceeded.",
                    headers={"Retry-After": RateLimiter.retry_after_header(retry_after)},
                    retry_after_s=round(retry_after, 2),
                )
            if billable.estimated_tokens > key.max_tokens_per_request:
                raise ApiError(
                    413,
                    "too_many_tokens",
                    f"Request is ~{billable.estimated_tokens} tokens; this key allows "
                    f"{key.max_tokens_per_request} per request.",
                    estimated_tokens=billable.estimated_tokens,
                    max_tokens_per_request=key.max_tokens_per_request,
                )
            remaining = await self._check_budgets(key, billable)
            if not state.client.configured:
                raise ApiError(503, "upstream_not_configured", "OPEN_ROUTER_KEY is not configured.")
            payload = self.apply_provider_prefs(billable.payload, key)
            try:
                upstream = await billable.call(payload)
            except UpstreamUnavailable as exc:
                raise ApiError(502, "upstream_unavailable", "Could not reach OpenRouter.") from exc
            if upstream.status >= 400:
                raise self._map_upstream_error(upstream)
            inp, out, reported = billable.read_usage(upstream.body)
            cost = await state.pricing.cost(billable.model, inp, out, reported)
        except ApiError as exc:
            await record(exc.status, exc.code)
            raise
        await record(
            200,
            input_tokens=inp,
            output_tokens=out,
            cost_usd=cost.usd,
            cost_source=cost.source,
            upstream_id=upstream.body.get("id") if isinstance(upstream.body, dict) else None,
        )
        headers = {"X-Barista-Cost-Usd": f"{cost.usd:.8f}"}
        if remaining is not None:
            headers["X-Barista-Budget-Remaining-Usd"] = f"{max(Decimal(0), remaining - cost.usd):.6f}"
        return JSONResponse(upstream.body, headers=headers)

    async def _check_budgets(self, key: ApiKey, billable: Billable) -> Decimal | None:
        state = self.state
        estimate = await state.pricing.estimate(billable.model, billable.estimated_tokens)
        async with state.db.sessionmaker() as session:
            spent = await month_spend(session, api_key_id=key.id)
            remaining = key.monthly_budget_usd - spent
            if spent + estimate > key.monthly_budget_usd:
                raise ApiError(
                    402,
                    "budget_exceeded",
                    f"This key's monthly budget of ${key.monthly_budget_usd} is used up "
                    f"(spent ${spent:.4f} this month). It resets on the 1st (UTC).",
                    budget_usd=str(key.monthly_budget_usd),
                    spent_usd=f"{spent:.8f}",
                )
            cap = state.settings.monthly_cap_for(state.service)
            if cap > 0:
                service_spent = await month_spend(session, service=state.service.value)
                if service_spent + estimate > cap:
                    log.warning("service monthly cap reached for %s", state.service.value)
                    raise ApiError(
                        402,
                        "service_budget_exceeded",
                        f"The {state.service.value} service has hit its global monthly cap. "
                        "Try again next month or ask the owner to raise it.",
                    )
        return remaining

    @staticmethod
    def _map_upstream_error(upstream: UpstreamResponse) -> ApiError:
        message = "OpenRouter returned an error."
        body = upstream.body
        if isinstance(body, dict) and isinstance(body.get("error"), dict):
            message = str(body["error"].get("message") or message)
        status = upstream.status
        if status in (400, 404, 413, 422):
            # Caller's request was bad; pass the upstream reason through.
            return ApiError(status, "upstream_rejected", message, upstream_status=status)
        if status in (401, 402, 403):
            # Our OpenRouter key/credits, not the caller's fault. Don't leak details.
            log.error("OpenRouter refused the gateway's key: %s %s", status, message)
            return ApiError(502, "upstream_auth", "The gateway's upstream account was refused.")
        if status == 429:
            return ApiError(
                503, "upstream_rate_limited", "OpenRouter is rate limiting; retry shortly.",
                headers={"Retry-After": "5"},
            )
        return ApiError(502, "upstream_error", message, upstream_status=status)

    async def _record(
        self,
        key: ApiKey,
        *,
        endpoint: str,
        attribution: Attribution | None,
        model: str | None,
        status: int,
        error_code: str | None,
        latency_ms: int,
        request_body: str | None,
        input_tokens: int = 0,
        output_tokens: int = 0,
        cost_usd: Decimal = Decimal(0),
        cost_source: str = "none",
        upstream_id: str | None = None,
    ) -> None:
        try:
            async with self.state.db.sessionmaker() as session:
                session.add(
                    UsageEvent(
                        api_key_id=key.id,
                        service=self.state.service.value,
                        endpoint=endpoint,
                        surface=attribution.surface if attribution else None,
                        agent_id=attribution.agent_id if attribution else None,
                        model=model,
                        input_tokens=input_tokens,
                        output_tokens=output_tokens,
                        cost_usd=cost_usd,
                        cost_source=cost_source,
                        latency_ms=latency_ms,
                        status_code=status,
                        error_code=error_code,
                        upstream_id=upstream_id,
                        request_body=request_body,
                    )
                )
                db_key = await session.get(ApiKey, key.id)
                if db_key is not None:
                    db_key.last_used_at = utcnow()
                await session.commit()
        except Exception:
            # Never fail a paid request because logging failed, but make it loud.
            log.exception("failed to record usage for key %s", key.key_id)
