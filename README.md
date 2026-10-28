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
        "state": {"bugs": ["login fails on Safari", "CSV export drops a column", "typo on pricing page"],
                  "demo": "tomorrow 10:00"},
        "questions": {
          "ship": {
            "type": "noul",
            "instructions": "Should we ship today?",
            "criteria": {"true": "None of the bugs affects the demo.", "false": "A bug would break the demo."}
          },
          "first": {
            "type": "choice",
            "instructions": "Which bug should we fix first?",
            "criteria": {
              "login":   "Users can't sign in.",
              "export":  "Exports are wrong but usable.",
              "pricing": "Cosmetic only."
            }
          },
          "urgency": {
            "type": "score",
            "instructions": "How urgent is the login bug?",
            "criteria": ["Can wait for the next release", "Fix this week", "Fix before the demo"]
          }
        }
      }'
```

The body goes to OpenRouter's Decisions API unchanged: `model`, `state`, `questions`, `assets`, `session_id`, and so
on. `state`, `instructions` and every criterion can be a string, an object or an array. The schema of each question
type (from [OpenRouter's API reference](https://openrouter.ai/docs/api/api-reference/alphadecisions/submit-a-decisions-request)):

| `type` | required fields | `criteria` | answer |
|---|---|---|---|
| `noul` (yes/no) | `instructions` | optional object `{"true": …, "false": …}` | `"noul": 0.96`, the probability that the answer is true (0–1) |
| `choice` | `instructions`, `criteria` | **object** `{"<option>": "<when to pick it>", …}`, not a list | `"choice": "<option>"`, with `probabilities` per option and `confidence` |
| `score` | `instructions`, `criteria` | ordered **list**, lowest first, at least 1 item | `"score": 1.99`, the expected index from 0 to n−1, with `legend` (`{"0": …}`), `probabilities` and `confidence` |
| `box` | `instructions` | none; reference an image asset | `"box": {"top", "bottom", "left", "right"}` with `confidence` |

Images go in `assets` as an object keyed by reference:
`"assets": {"shot1": {"type": "image", "format": "png", "data_base64": "<base64, no data: prefix>"}}`. Write
`"shot1"` as a whole string wherever the image belongs (in `state`, `instructions` or `criteria`). Every asset must be
referenced.

The response is OpenRouter's `{id, model, provider, answers, usage}`. If OpenRouter rejects the request, you get
`400 upstream_rejected` with OpenRouter's reason in `message` and `upstream_detail`.

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
| 400 | `upstream_rejected` | OpenRouter refused the body (e.g. a `choice` with list criteria); `message` and `upstream_detail` say why |
| 401 | `missing_api_key`, `invalid_api_key`, `revoked_api_key` | |
| 402 | `budget_exceeded` | your key's monthly budget is used up (it resets on the 1st, UTC) |
| 402 | `service_budget_exceeded` | the service hit its global monthly cap |
| 402 | `upstream_credit_low` | the shared OpenRouter key is almost out of credit; calls resume once the owner tops it up |
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
curl -s $URL/v1/admin/spend -H "Authorization: Bearer $BARISTA_ADMIN_TOKEN"   # month spend vs cap, per service, + the OpenRouter key's real credit
```

`cafe-gateway usage` also prints the key's live credit under `openrouter_key`.

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
  - Labels are per service only; key names never appear, because this port is reachable over the tailnet.
  - Example alert: `cafe_gateway_cap_used_ratio > 0.8`.
- **Per-user metrics on an internal-only port**, `BARISTA_INTERNAL_METRICS_ADDR` (default `:9090`, `off` disables).
  `GET /metrics` there returns everything above plus the per-key series below. Scrape it inside the cluster and
  never route it through an ingress.

  | metric | type | labels |
  |---|---|---|
  | `cafe_gateway_user_requests_total` | counter | `service, key_name, key_id, surface, agent_id, model, status` |
  | `cafe_gateway_user_errors_total` | counter | the same + `error_code` (status ≥ 400) |
  | `cafe_gateway_user_request_duration_seconds` | histogram (0.25, 1, 2.5, 5, 10, 30 s) | `service, key_name, key_id, model` |
  | `cafe_gateway_user_tokens_total` | counter | `service, key_name, key_id, surface, agent_id, model, direction` (`input`/`output`) |
  | `cafe_gateway_user_spend_usd_total` | counter | `service, key_name, key_id, surface, agent_id, model` |
  | `cafe_gateway_user_budget_usd` | gauge | `service, key_name, key_id` (active keys) |
  | `cafe_gateway_user_budget_remaining_usd` | gauge | `service, key_name, key_id` (active keys) |
  | `cafe_gateway_cap_remaining_usd` | gauge | `service` |

  Cardinality is bounded:
  - Requests without a valid key for that service all get `key_name`, `key_id`, `surface` and `agent_id` set to
    `unauthenticated`. That covers a missing, unknown, forged or revoked key, or another service's key.
  - Values that are missing get `none`.
  - After a key's first 50 distinct (`surface`, `agent_id`) pairs, further pairs are counted under `other`.
  - `model` is always an allow-listed model or `none`, and `status` is the HTTP status.
  - The counters are per pod and start at 0 on restart, so use `sum(rate(...))` / `increase(...)`.
  - The budget and cap gauges are recomputed from Postgres every 30 s and after each request. Use `max` across
    pods.
- **`data_collection: deny`** is forced onto every OpenRouter request (`OPENROUTER_DATA_COLLECTION`). A caller can't
  override it.
- **Shared key, real limit lives at OpenRouter.** `OPEN_ROUTER_KEY` has a credit limit (currently $10) and is
  shared by barista, grinder, bean and later ledger. The gateway's caps only see the gateway's own spend, so every
  pod also polls OpenRouter's [`GET /api/v1/key`](https://openrouter.ai/docs/api/reference/limits) every
  `OPENROUTER_POLL_SECONDS` (default 60; `0` turns polling and the floor off), backing off up to 15 min while
  polls fail. It reads `limit`, `limit_remaining`, `usage`, `usage_daily`, `usage_weekly` and `usage_monthly`.
  - **Safety floor:** when the last successful poll shows less than `OPENROUTER_MIN_REMAINING_USD` (default $0.50)
    left, model calls get `402 upstream_credit_low` without reaching OpenRouter. Crossing the floor logs one
    `WARN` (`"event": "cafe_gateway_openrouter_credit_low"`). Calls resume on the first poll after credit is added
    (`"event": "cafe_gateway_openrouter_credit_ok"`). The floor fails open: if the latest poll failed, nothing is
    blocked (OpenRouter still enforces the hard limit), and `poll_success` drops to 0.
  - **Alerts:** `WARN` logs when the key's usage crosses 80% and 95% of its limit
    (`"event": "cafe_gateway_openrouter_key_threshold"`, `threshold_pct`), once per crossing per pod.
  - **Metrics** (on both `/metrics` ports, no labels; every pod reports the same values, so use `max()`):
    `cafe_gateway_openrouter_key_limit_usd`, `_usage_usd`, `_remaining_usd`, `_remaining_ratio`,
    `_usage_daily_usd`, `_usage_weekly_usd`, `_usage_monthly_usd`, `_min_remaining_usd`, `_blocked` (0/1),
    `_poll_success` (0/1), `_last_poll_timestamp_seconds` (last *successful* poll), and the counter
    `cafe_gateway_openrouter_key_alerts_total{threshold_pct}`. Each gauge is absent until the first poll, and
    limit/remaining/ratio stay absent for a key with no limit. Example alerts:
    `max(cafe_gateway_openrouter_key_remaining_ratio) < 0.2`,
    `time() - max(cafe_gateway_openrouter_key_last_poll_timestamp_seconds) > 600`.
  - `GET /v1/admin/spend` and `cafe-gateway usage` show the same numbers under `openrouter_key`, with a `status`
    of `ok`, `warning` (≥ 80% used), `critical` (≥ 95%), `below_floor` or `unlimited`.

## Configuration

See [`.env.example`](.env.example) for every variable and its default. In the cluster:

- secrets: `OPEN_ROUTER_KEY`, `DATABASE_URL`, `BARISTA_ADMIN_TOKEN`, `POSTGRES_PASSWORD` (Infisical `/cafe-gateway`)
- config: `BARISTA_MODELS`, `GRINDER_MODELS`, `BARISTA_MONTHLY_CAP_USD`, `GRINDER_MONTHLY_CAP_USD`,
  `OPENROUTER_DATA_COLLECTION`, `BARISTA_DEBUG_STORE_BODIES`, `OPENROUTER_POLL_SECONDS`,
  `OPENROUTER_MIN_REMAINING_USD`
- per deployment: `BARISTA_SERVICE`

## Development

You need Docker. Go 1.25 is optional, since everything runs in containers.

```sh
docker compose up --build -d      # barista :8051, grinder :8052, Postgres :5444, mock OpenRouter (no real spend)
docker compose run --rm barista keys create --service barista --name me
curl -s localhost:8051/v1/decide -H "Authorization: Bearer bar_…" -H "X-AGENT-SURFACE: cli" -H "X-AGENT-ID: me" \
  -d '{"state": "s", "questions": {"q": {"type": "noul", "instructions": "Is s a letter?"}}}'
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
