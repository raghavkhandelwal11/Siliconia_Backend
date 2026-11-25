# Observability (T5.4)

Built in, no account needed:

- **Synthetic checks** run in the worker every 5 minutes (`health:synthetic`): API readiness,
  catalogue, search, a product page and each simulator. Results go to `synthetic_runs`.
  Two failures in a row open an incident and send `health_alert` to every admin; a pass resolves it.
- **Admin health page** (`/admin/health`) shows last result, 24 h latency and 30-day uptime per check.
- **Daily jobs**: payment reconciliation against the gateway settlement (`recon_issues`),
  Electrons ledger invariant, retention purge. Failures open incidents the same way.

Optional free services (need your own accounts; nothing is created for you):

| Service | Free tier | Setup |
|---|---|---|
| Grafana Cloud (logs) | 50 GB logs/month | Create a stack, then run Alloy with `config.alloy` and set `GRAFANA_LOKI_URL`, `GRAFANA_LOKI_USER`, `GRAFANA_CLOUD_TOKEN`. |
| Sentry (errors) | 5k errors/month | Create a Go and a Next.js project; put the DSNs in `SENTRY_DSN` (backend) and `NEXT_PUBLIC_SENTRY_DSN` (frontend) when the SDKs are wired. |
| UptimeRobot (external uptime) | 50 monitors, 5-minute checks | Add HTTP monitors for `https://<host>/readyz` and the home page; alert to your email. |

Keep tokens in `deploy/compose/.env` (git-ignored), never in this folder.
