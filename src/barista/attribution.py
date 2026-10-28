"""X-AGENT-SURFACE / X-AGENT-ID attribution headers.

They say where a call comes from and which app/agent made it, for spend reports. They are not
auth: the API key stays the auth and budget mechanism.
"""

from __future__ import annotations

import re
from collections.abc import Mapping
from dataclasses import dataclass

from barista.errors import ApiError

SURFACE_HEADER = "X-AGENT-SURFACE"
AGENT_HEADER = "X-AGENT-ID"
SLUG_RE = re.compile(r"^[a-z0-9._-]{1,64}$")


@dataclass(frozen=True)
class Attribution:
    surface: str
    agent_id: str


def validate_slug(field: str, value: str) -> str:
    value = value.strip().lower()
    if not SLUG_RE.match(value):
        raise ValueError(f"{field} must be 1-64 chars of [a-z0-9._-], got {value!r}")
    return value


def read_attribution(headers: Mapping[str, str]) -> Attribution:
    """Parse both headers or raise a 400 naming every missing/invalid one."""
    missing: list[str] = []
    invalid: list[str] = []
    values: dict[str, str] = {}
    for header in (SURFACE_HEADER, AGENT_HEADER):
        raw = (headers.get(header) or "").strip()
        if not raw:
            missing.append(header)
            continue
        value = raw.lower()
        if not SLUG_RE.match(value):
            invalid.append(header)
            continue
        values[header] = value
    if missing:
        raise ApiError(
            400,
            "missing_attribution_header",
            f"Missing required header(s): {', '.join(missing)}. Send {SURFACE_HEADER} (where the call "
            f"comes from, e.g. 'internal') and {AGENT_HEADER} (a stable app/agent name).",
            headers_missing=missing,
        )
    if invalid:
        raise ApiError(
            400,
            "invalid_attribution_header",
            f"Invalid header(s): {', '.join(invalid)}. Values must be 1-64 chars of [a-z0-9._-].",
            headers_invalid=invalid,
        )
    return Attribution(surface=values[SURFACE_HEADER], agent_id=values[AGENT_HEADER])
