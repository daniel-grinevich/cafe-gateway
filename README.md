# cafe-gateway

A small Go gateway in front of OpenRouter. It gives each friend and app its own API key, rate limit and monthly
budget, and puts a hard monthly cap on the whole bill. One codebase and one image
(`ghcr.io/daniel-grinevich/cafe-gateway`) run as two services:

| service | endpoint | OpenRouter API | default model | keys |
|---|---|---|---|---|
| **barista** | `POST /v1/decide` | Decisions (`/api/alpha/decisions`) | `perplexity/pplx-decider-flash-v1.2` | `bar_…` |
| **grinder** | `POST /v1/embed` | Embeddings (`/api/v1/embeddings`) | `qwen/qwen3-embedding-8b` | `grd_…` |

`BARISTA_SERVICE=barista|grinder` picks the service. Each deployment serves only its own model endpoint, plus
`/health`, `/ready`, `/metrics`, `/docs`, `/openapi.json`, `GET /v1/usage` and the admin API. Both deployments share
one Postgres database.

Tailnet URLs: `https://barista.tailf9a785.ts.net` and `https://grinder.tailf9a785.ts.net` (production).

## For friends: calling it

You get a key from the owner. It's shown once, so keep it secret. A `bar_` key only works on barista and a `grd_`
key only works on grinder. Every model call needs two headers, which the usage reports group by:

- `X-AGENT-SURFACE`: where the call comes from, e.g. `cli`, `slack`, `bean`, `internal`
- `X-AGENT-ID`: a stable name for your app or agent, e.g. `my-script`

Both are 1–64 characters of `[a-z0-9._-]` (they get lowercased). A missing header returns `400`.

**barista (decisions):**

```sh
curl -s https://barista.tailf9a785.ts.net/v1/decide \
  -H "Authorization: Bearer $BARISTA_KEY" \
  -H "X-AGENT-SURFACE: cli" -H "X-AGENT-ID: my-script" \
  -H "Content-Type: application/json" \
  -d '{
        "state": "We have 3 open bugs and a demo tomorrow.",
        "questions": {
          "ship":     {"type": "noul", "instructions": "Should we ship today?"},
          "priority": {"type": "choice", "instructions": "Which first?", "criteria": ["bug 1", "bug 2", "bug 3"]}
        }
      }'
```

The body goes to OpenRouter's Decisions API unchanged (`model`, `state`, `questions`, `assets`, `session_id`, …).
The response is OpenRouter's `{id, model, answers, usage}`.

**grinder (embeddings):**

```sh
curl -s https://grinder.tailf9a785.ts.net/v1/embed \
  -H "Authorization: Bearer $GRINDER_KEY" \
  -H "X-AGENT-SURFACE: cli" -H "X-AGENT-ID: my-script" \
  -H "Content-Type: application/json" \
  -d '{"input": ["espresso", "latte"]}'
```

This is an OpenAI-style embeddings request (`input` is a string or up to 256 strings; `dimensions` and similar are
passed through). The response is OpenRouter's `{data: [{embedding, index}], usage}`.

**Your usage and spend this month:**

```sh
curl -s "https://barista.tailf9a785.ts.net/v1/usage?by=agent" -H "Authorization: Bearer $BARISTA_KEY"
# by = surface | agent | model | day | status; month=2026-10 for another month
```

Every successful call returns `X-Cost-Usd` (what that call cost) and `X-Budget-Remaining-Usd`.

**Errors** come back as `{"error": {"code": "...", "message": "..."}}`:

| status | code | meaning |
|---|---|---|
| 400 | `missing_header`, `invalid_header` | send `X-AGENT-SURFACE` and `X-AGENT-ID` |
| 400 | `model_not_allowed` | the response lists `allowed_models` |
| 401 | `missing_api_key`, `invalid_api_key`, `revoked_api_key` | |
| 402 | `budget_exceeded` | your key's monthly budget is used up (it resets on the 1st, UTC) |
| 402 | `service_budget_exceeded` | the service hit its global monthly cap |
| 403 | `wrong_service` | a `grd_` key on barista, or the reverse |
| 413 | `too_many_tokens` | the request is larger than your key's per-request limit |
| 422 | `invalid_request` | the body is malformed |
| 429 | `rate_limited` | wait for `Retry-After` |
| 502/503 | `upstream_*` | OpenRouter is down, rate limiting, or refused the gateway |

## For the owner: keys and usage

The CLI runs against `DATABASE_URL`. In the cluster, use
`kubectl -n cafe-gateway-production exec deploy/barista -- cafe-gateway …`. Locally, use
`docker compose run --rm barista …`.

```sh
cafe-gateway migrate                                             # apply schema migrations (concurrent-safe)
cafe-gateway keys create --service barista --name alice --rpm 30 --budget 5 [--max-tokens 8000]
cafe-gateway keys create --service grinder --name bean            # defaults: 30 rpm, $1/month, 8000 tokens
cafe-gateway keys list [--service grinder] [--json]
cafe-gateway keys revoke <key_id | bar_…_… prefix>
cafe-gateway usage --month [2026-10] [--service barista] [--key <key_id>] [--by surface|agent|key|model|day|service|status]
```

Keys are shown once and stored only as a sha256 hash. Revoking a key takes effect on its next request.

The same operations are available over HTTP with `Authorization: Bearer $BARISTA_ADMIN_TOKEN`. The admin API is
off when the token is unset.

```sh
curl -s -X POST $URL/v1/admin/keys -H "Authorization: Bearer $BARISTA_ADMIN_TOKEN" \
  -d '{"service": "barista", "name": "alice", "rpm": 30, "monthly_budget_usd": "5"}'
curl -s $URL/v1/admin/keys -H "Authorization: Bearer $BARISTA_ADMIN_TOKEN"
curl -s -X POST $URL/v1/admin/keys/<key_id>/revoke -H "Authorization: Bearer $BARISTA_ADMIN_TOKEN"
curl -s "$URL/v1/admin/usage?by=surface&service=grinder" -H "Authorization: Bearer $BARISTA_ADMIN_TOKEN"
curl -s $URL/v1/admin/spend -H "Authorization: Bearer $BARISTA_ADMIN_TOKEN"   # month spend vs cap, per service
```

## Money safety

- **Global monthly cap per service** (`BARISTA_MONTHLY_CAP_USD`, `GRINDER_MONTHLY_CAP_USD`, both $10 by default,
  UTC calendar month). A request that would cross the cap gets `402`. A cap of `0` blocks every call; there is no
  "unlimited" setting.
- **Per-key monthly budget, rpm and max tokens per request.**
- **Reserve, then reconcile.** Before calling OpenRouter, the gateway estimates the call's cost and reserves it in
  Postgres. The estimate is input tokens plus an output allowance, priced from OpenRouter's model catalogue or a
  pessimistic $1/Mtok fallback. Both the key budget and the service cap must cover recorded spend, plus everything
  already in flight, plus this estimate. A Postgres advisory lock serializes the check, so concurrent requests on
  any replica can't share the same remaining budget. When the call returns, the reservation is replaced by the real
  cost in the same locked transaction: OpenRouter's `usage.cost` if present, otherwise catalogue price × tokens. A
  crashed pod's reservations stop counting after `RESERVATION_TTL_S`.
- **Alerts at 80% and 100%** of each service cap and each key budget. These are structured `WARN` JSON log lines
  (`"event": "cafe_gateway_cap_threshold"` / `"cafe_gateway_key_budget_threshold"`), and each crossing is logged
  exactly once. Refusals at the cap log `"event": "cafe_gateway_cap_refused"`.
- **Prometheus metrics on `/metrics`:**
  - `cafe_gateway_month_spend_usd`, `cafe_gateway_monthly_cap_usd`, `cafe_gateway_cap_used_ratio`,
    `cafe_gateway_reserved_usd`
  - `cafe_gateway_cost_usd_total`, `cafe_gateway_requests_total`, `cafe_gateway_cap_threshold_alerts_total`
  - Labels are per service only; key names never appear.
  - Example alert: `cafe_gateway_cap_used_ratio > 0.8`.
- **`data_collection: deny`** is forced onto every OpenRouter request (`OPENROUTER_DATA_COLLECTION`). A caller can't
  override it.
- **Give the gateway its own OpenRouter key with an OpenRouter-side credit limit.** The gateway's caps protect
  against callers. The credit limit on the key itself (openrouter.ai → Keys → credit limit) is the last line of
  defense if the gateway is misconfigured or has a bug. Set it no higher than the sum of the two service caps.

## Configuration

See [`.env.example`](.env.example) for every variable and its default. In the cluster:

- secrets: `OPEN_ROUTER_KEY`, `DATABASE_URL`, `BARISTA_ADMIN_TOKEN`, `POSTGRES_PASSWORD` (Infisical `/cafe-gateway`)
- config: `BARISTA_MODELS`, `GRINDER_MODELS`, `BARISTA_MONTHLY_CAP_USD`, `GRINDER_MONTHLY_CAP_USD`,
  `OPENROUTER_DATA_COLLECTION`, `BARISTA_DEBUG_STORE_BODIES`
- per deployment: `BARISTA_SERVICE`

## Development

You need Docker. Go 1.25 is optional, since everything runs in containers.

```sh
docker compose up --build -d      # barista :8051, grinder :8052, Postgres :5444, mock OpenRouter (no real spend)
docker compose run --rm barista keys create --service barista --name me
curl -s localhost:8051/v1/decide -H "Authorization: Bearer bar_…" -H "X-AGENT-SURFACE: cli" -H "X-AGENT-ID: me" \
  -d '{"state": "s", "questions": {"q": {"type": "noul"}}}'
open http://localhost:8051/docs
```

Tests need a Postgres whose user can `CREATE DATABASE`. Each test gets its own throwaway database. OpenRouter is
always mocked (`httptest`).

```sh
BARISTA_TEST_ADMIN_DSN=postgres://cafe_gateway:cafe_gateway@localhost:5444/cafe_gateway?sslmode=disable \
  go test -race ./...
golangci-lint run ./...
```

## Deploying

cafe-gateway runs in production only; there is no staging environment. GitHub Actions
(`.github/workflows/deploy-production.yml`) does the release:

1. Tag a commit on `main` with `vX.Y.Z` and push the tag: `git tag v1.2.3 <commit> && git push origin v1.2.3`.
2. CI checks that the commit is on `main` and builds the arm64 image. It pushes `X.Y.Z`, plus `prod-latest` and
   `prod-<sha>`.
3. CI bumps `newTag` to `X.Y.Z` in [rainbow-road](https://github.com/daniel-grinevich/rainbow-road)
   `production/cafe-gateway/kustomization.yaml`, using `RAINBOW_ROAD_PAT`.
4. Sync the `cafe-gateway-production` app by hand in ArgoCD (production is manual sync). Syncing rolls both the
   barista and grinder deployments.

Both deployments run `cafe-gateway migrate` as an init container. It is idempotent and takes an advisory lock.

See [docs/architecture.md](docs/architecture.md) for the request pipeline and known limits.
