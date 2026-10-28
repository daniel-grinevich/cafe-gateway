# cafe-gateway architecture

## Layout

| package | role |
|---|---|
| `cmd/cafe-gateway` | the CLI: `serve`, `migrate`, `keys`, `usage` (and a hidden `healthcheck` for the image) |
| `cmd/mock-openrouter` | a fake OpenRouter for local compose |
| `internal/config` | reads the environment |
| `internal/keys` | key format: `bar_`/`grd_` + 12-character id + 43-character secret, stored as a sha256 hash |
| `internal/store` | Postgres via pgx; embedded SQL migrations; keys, usage, reservations, reports |
| `internal/openrouter` | upstream client (timeouts, retries with backoff and jitter on 429/5xx), pricing cache |
| `internal/ratelimit` | in-process token bucket per key |
| `internal/gateway` | request pipeline, HTTP API, alerts, OpenRouter key credit poller, metrics wiring, docs |

## Request pipeline (`POST /v1/decide` or `/v1/embed`)

1. **Auth (401):** a missing, malformed, unknown, wrong-secret or revoked key is rejected. A valid key for the
   other service gets **403 `wrong_service`**. These rejections aren't recorded, because there's no key to attach
   them to.
2. **Attribution (400):** both `X-AGENT-SURFACE` and `X-AGENT-ID` are required. Every later step writes a usage row,
   including rejections.
3. **Body (422/400):** the request is validated. The model must be on the allow-list.
4. **Scope (403), rate limit (429), max tokens per request (413).**
5. **Shared key credit floor (402 `upstream_credit_low`):** refused if the last OpenRouter key poll succeeded and
   showed less than `OPENROUTER_MIN_REMAINING_USD` left (see below).
6. **Reserve (402):** under `pg_advisory_xact_lock(service)`, the gateway checks:
   - key: month spend + live reservations + estimate ≤ key budget
   - service: month spend + live reservations + estimate ≤ service cap

   If both pass, it inserts a reservation.
7. **Upstream:** `provider.data_collection` is forced and `user` defaults to `<service>:<key_id>`. The call is
   detached from the client's context, so a caller who hangs up doesn't lose the cost.
8. **Settle:** under the same lock, the usage row (real cost) is inserted and the reservation deleted. The
   service's before/after spend is computed in the same transaction, so each 80%/100% crossing triggers exactly one
   alert, across replicas.

The token estimate is JSON characters ÷ 4, plus 1000 per decision asset (image). Decisions also reserve
`DECIDE_OUTPUT_TOKEN_ALLOWANCE` output tokens. Cost comes from OpenRouter's `usage.cost` if present, otherwise the
catalogue price (`/models` + `/embeddings/models`, cached for 6h), otherwise `FALLBACK_PRICE_PER_MTOK_USD`.

## Shared key, real limit lives at OpenRouter

`OPEN_ROUTER_KEY` is one OpenRouter key with a credit limit, shared by barista, grinder, bean and (later) ledger.
The service caps and key budgets above only count the gateway's own usage rows, so they can't see what bean
spends. The authoritative number is OpenRouter's.

- `RunCreditPoller` (every pod) calls `GET {OPENROUTER_BASE_URL}/key` every `OPENROUTER_POLL_SECONDS` (60 s), one
  attempt per tick, doubling the delay after each failure up to 15 min. The response's `data.limit`,
  `data.limit_remaining` (or `limit - usage` if absent), `data.usage` and `data.usage_daily|weekly|monthly` feed
  the unlabelled `cafe_gateway_openrouter_key_*` gauges, which go on both metrics registries.
- The floor is decided per pod from its own last poll: blocked iff that poll succeeded and remaining < floor. So a
  failed poll fails open (`poll_success` = 0), and adding credit unblocks within one poll interval. Pods poll
  independently, so they can briefly disagree, by at most one interval.
- 80%/95% usage alerts compare each poll with the previous one in the same process, so each pod logs a crossing
  once (and again after a restart, if usage is still above the threshold).
- The floor check runs before the reservation. A refused call writes a usage row (402) like any other rejection.
- The key's `label` (a masked copy of the key) is never decoded, logged or exported.

## Known limits

- The rate limiter is per process. With N replicas, a key gets up to N × rpm. Budgets and caps are exact across
  replicas because they live in Postgres.
- The real cost can exceed the reservation, e.g. a long decision. So spend can end slightly above a cap or budget
  (by at most one call's difference), and the next request is then refused.
- Every budget check sums the month's usage rows (indexed by service/key + time). That's fine at friends-and-agents
  volume. Beyond that, keep a running monthly total.
- Upstream 5xx and transport failures are recorded at $0. If OpenRouter billed a call that then failed, that cost
  won't be seen by the caps; the key credit poll does see it.
