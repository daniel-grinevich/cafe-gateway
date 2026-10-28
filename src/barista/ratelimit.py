"""In-process token bucket per API key.

Each service runs as its own deployment and keys are service-scoped, so a key is only ever
counted by one process. With more than one replica per service this would need to move to
Postgres or Redis (see docs/architecture.md).
"""

from __future__ import annotations

import math
import time
from collections.abc import Callable
from dataclasses import dataclass


@dataclass
class _Bucket:
    tokens: float
    updated: float


class RateLimiter:
    def __init__(self, clock: Callable[[], float] = time.monotonic) -> None:
        self._clock = clock
        self._buckets: dict[str, _Bucket] = {}

    def try_acquire(self, key: str, rpm: int) -> float | None:
        """Take one token. Return None when allowed, else seconds until a token is available."""
        if rpm <= 0:
            return 60.0
        capacity = float(rpm)
        rate = rpm / 60.0
        now = self._clock()
        bucket = self._buckets.get(key)
        if bucket is None:
            bucket = self._buckets[key] = _Bucket(tokens=capacity, updated=now)
        else:
            bucket.tokens = min(capacity, bucket.tokens + (now - bucket.updated) * rate)
            bucket.updated = now
        if bucket.tokens >= 1.0:
            bucket.tokens -= 1.0
            return None
        return (1.0 - bucket.tokens) / rate

    @staticmethod
    def retry_after_header(seconds: float) -> str:
        return str(max(1, math.ceil(seconds)))
