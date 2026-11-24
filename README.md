# Siliconia_Backend

Go API, background worker and free simulators for the **Siliconics** prototype.
Design docs (BRD, TRS, LLD, development plan) and the issue tracker live in
[raghavkhandelwal11/Siliconia](https://github.com/raghavkhandelwal11/Siliconia); the web app is in
[Siliconia-Frontend](https://github.com/raghavkhandelwal11/Siliconia-Frontend).

> **Free services only.** Everything here runs on free tools and simulators
> (TRS D-20, D-24). Paid providers are planned in the docs repo's `paid-later/`
> folder and must not be added to this repo.

## Run it

Requirements: Docker Desktop, Go 1.27+, Node 20+ (for the web app).

```bash
make up        # PostgreSQL, Redis, migrations, seed, 2× API, worker, sim-notify, Caddy
```

| URL | What |
|---|---|
| http://localhost:8080/api/v1/healthz | API liveness |
| http://localhost:8080/api/v1/readyz | PostgreSQL + Redis readiness |
| http://localhost:8080/test-inbox | Test inbox for simulated SMS/WhatsApp (password from `TEST_INBOX_PASSWORD`) |
| http://localhost:8080/ | Web app, when `npm run dev` runs in Siliconia-Frontend on port 3000 |

Settings live in `deploy/compose/.env` (created from `.env.example` on first `make up`).

## Develop

```bash
make check             # gofmt, go vet, staticcheck, unit tests (race), generated-code check
make test-integration  # also runs PostgreSQL tests (needs `make up`)
make generate          # regenerate sqlc code after editing db/queries
make help              # all targets
```

## Layout

```
cmd/            api · worker · migrate · seed · sim-notify
internal/
  platform/     config · httpx (errors, middleware) · db · redisx · ratelimit · outbox · jobs · migrate · logx
  provider/     adapter interfaces + free adapters (messaging: sim-notify, log)
  sim/          simulators (notify = test inbox; pay, ship, payout, kyc come with M1–M4)
  server/       router (middleware order from LLD 4.1)
  health/       liveness and readiness
  store/        sqlc-generated queries (do not edit)
  <module>/     one folder per product module as it is built (auth, catalogue, …)
db/             migrations (goose, timestamp-named) · queries (sqlc)
api/            openapi.yaml: the HTTP contract the web app generates its client from
deploy/compose/ docker-compose.yml · Caddyfile · .env.example
```

## Rules

- One task (issue in the Siliconia repo) → one branch → one pull request; `ci-success` must pass.
- Migrations are timestamp-named and expand/contract; never edit a merged migration.
- Money is integer paise, rates are basis points, IDs are UUIDv7.
- Every outside service sits behind an interface in `internal/provider` with a free simulator.
