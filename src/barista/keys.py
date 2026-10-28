"""API key format, generation and hashing.

A key looks like ``bar_<key_id>_<secret>`` (barista) or ``grd_<key_id>_<secret>`` (grinder).
The prefix shows the scope, ``key_id`` is a public lookup id stored in clear, and only the
sha256 of the whole key is stored, so a leaked database can't be replayed.
"""

from __future__ import annotations

import hashlib
import hmac
import re
import secrets
from dataclasses import dataclass

from barista.config import KEY_PREFIXES, Service

_KEY_RE = re.compile(r"^(?P<prefix>[a-z]{3})_(?P<key_id>[a-z0-9]{12})_(?P<secret>[A-Za-z0-9_-]{32,64})$")
_ID_ALPHABET = "abcdefghijklmnopqrstuvwxyz0123456789"


@dataclass(frozen=True)
class ParsedKey:
    service: Service
    key_id: str
    plaintext: str


def hash_key(plaintext: str) -> str:
    return hashlib.sha256(plaintext.encode()).hexdigest()


def verify_key(plaintext: str, key_hash: str) -> bool:
    return hmac.compare_digest(hash_key(plaintext), key_hash)


def generate_key(service: Service) -> tuple[str, str]:
    """Return (key_id, plaintext). The plaintext is shown once and never stored."""
    key_id = "".join(secrets.choice(_ID_ALPHABET) for _ in range(12))
    secret = secrets.token_urlsafe(32)[:43]
    return key_id, f"{service.key_prefix}_{key_id}_{secret}"


def parse_key(plaintext: str) -> ParsedKey | None:
    match = _KEY_RE.match(plaintext)
    if not match:
        return None
    for service, prefix in KEY_PREFIXES.items():
        if prefix == match["prefix"]:
            return ParsedKey(service=service, key_id=match["key_id"], plaintext=plaintext)
    return None
