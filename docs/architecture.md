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
| `internal/gateway` | request pipeline, HTTP API, alerts, metrics wiring, docs |

## Request pipeline (`POST /v1/decide` or `/v1/embed`)

1. **Auth (401):** a missing, malformed, unknown, wrong-secret or revoked key is rejected. A valid key for the
   other service gets **403 `wrong_service`**. These rejections aren't recorded, because there's no key to attach
   them to.
2. **Attribution (400):** both `X-AGENT-SURFACE` and `X-AGENT-ID` are required. Every later step writes a usage row,
   including rejections.
3. **Body (422/400):** the request is validated. The model must be on the allow-list.
4. **Scope (403), rate limit (429), max tokens per request (413).**
5. **Reserve (402):** under `pg_advisory_xact_lock(service)`, the gateway checks:
   - key: month spend + live reservations + estimate ≤ key budget
   - service: month spend + live reservations + estimate ≤ service cap

   If both pass, it inserts a reservation.
6. **Upstream:** `provider.data_collection` is forced and `user` defaults to `<service>:<key_id>`. The call is
   detached from the client's context, so a caller who hangs up doesn't lose the cost.
7. **Settle:** under the same lock, the usage row (real cost) is inserted and the reservation deleted. The
   service's before/after spend is computed in the same transaction, so each 80%/100% crossing triggers exactly one
   alert, across replicas.

The token estimate is JSON characters ÷ 4, plus 1000 per decision asset (image). Decisions also reserve
`DECIDE_OUTPUT_TOKEN_ALLOWANCE` output tokens. Cost comes from OpenRouter's `usage.cost` if present, otherwise the
catalogue price (`/models` + `/embeddings/models`, cached for 6h), otherwise `FALLBACK_PRICE_PER_MTOK_USD`.

## Known limits

- The rate limiter is per process. With N replicas, a key gets up to N × rpm. Budgets and caps are exact across
  replicas because they live in Postgres.
- The real cost can exceed the reservation, e.g. a long decision. So spend can end slightly above a cap or budget
  (by at most one call's difference), and the next request is then refused.
- Every budget check sums the month's usage rows (indexed by service/key + time). That's fine at friends-and-agents
  volume. Beyond that, keep a running monthly total.
- Upstream 5xx and transport failures are recorded at $0. If OpenRouter billed a call that then failed, that cost
  won't be seen. Keep an OpenRouter-side credit limit.
