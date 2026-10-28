-- cafe-gateway: API keys, per-request usage, and in-flight cost reservations.

CREATE TABLE api_keys (
    id                     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    key_id                 varchar(32) NOT NULL UNIQUE,
    service                varchar(16) NOT NULL CHECK (service IN ('barista', 'grinder')),
    -- sha256 hex of the full key. The plaintext is shown once and never stored.
    key_hash               varchar(64) NOT NULL,
    name                   varchar(128) NOT NULL,
    scopes                 text[] NOT NULL,
    rpm                    integer NOT NULL CHECK (rpm >= 1),
    max_tokens_per_request integer NOT NULL CHECK (max_tokens_per_request >= 1),
    monthly_budget_usd     numeric(14, 8) NOT NULL CHECK (monthly_budget_usd >= 0),
    created_at             timestamptz NOT NULL DEFAULT now(),
    last_used_at           timestamptz,
    revoked_at             timestamptz
);

-- One row per authenticated request, rejections included.
CREATE TABLE usage (
    id            bigserial PRIMARY KEY,
    api_key_id    uuid NOT NULL REFERENCES api_keys (id),
    service       varchar(16) NOT NULL,
    endpoint      varchar(64) NOT NULL,
    surface       varchar(64),
    agent_id      varchar(64),
    model         varchar(128),
    input_tokens  integer NOT NULL DEFAULT 0,
    output_tokens integer NOT NULL DEFAULT 0,
    cost_usd      numeric(14, 8) NOT NULL DEFAULT 0,
    -- openrouter (usage.cost), pricing (catalogue), fallback, or none (no upstream call).
    cost_source   varchar(16) NOT NULL DEFAULT 'none',
    latency_ms    integer NOT NULL DEFAULT 0,
    status_code   integer NOT NULL,
    error_code    varchar(64),
    upstream_id   varchar(128),
    -- Only populated when BARISTA_DEBUG_STORE_BODIES=true.
    request_body  text,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX usage_key_created ON usage (api_key_id, created_at);
CREATE INDEX usage_service_created ON usage (service, created_at);

-- Estimated cost of requests sent to OpenRouter but not yet recorded in usage. Budget and cap checks count these,
-- so concurrent requests (on any replica) can't all spend the same remaining budget. A crashed pod's rows stop
-- counting at expires_at.
CREATE TABLE reservations (
    id         bigserial PRIMARY KEY,
    service    varchar(16) NOT NULL,
    api_key_id uuid NOT NULL REFERENCES api_keys (id),
    usd        numeric(14, 8) NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);

CREATE INDEX reservations_service ON reservations (service, expires_at);
