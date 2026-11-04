# hammurapi-core

Backend of **Hammurapi** — a platform for writing product specifications through a
sequence of quality gates (`product → design → arch → tech → qa`), with an AI agent as a
partner and git as the source of truth for content.

One Go binary, several modes:

| Mode | Purpose | Kubernetes |
| --- | --- | --- |
| `api` | User/admin HTTP API, SSE, webhooks, the chat proxied to Nabu, `/mcp/nabu` | Deployment |
| `worker` | Kafka consumer, workflows, runs of the service agents of Nabu and code tasks, MCP of runs (`:8083`) | Deployment |
| `cleaner` | Expired attachments, sessions, locks, unconfirmed imports, pending branch deletions | CronJob |
| `migrate` | goose migrations (embedded) | Job / Helm hook |

Deployment (docker-compose, Helm, installer) and documentation live in the
[`hammurapi`](../hammurapi) repository; the web app lives in [`hammurapi-web`](../hammurapi-web).

## Layout

```text
cmd/hammurapi/        entry point: mode selection and wiring
migrations/           goose SQL, embedded into the binary
internal/app/         dependency assembly and routing per mode
internal/features/    vertical slices: auth, profile, domains, features, gates, approvals,
                      handoff, rules, agent, voice, attachments, imports, admin, feedback, webhooks
internal/specdata/    shared projection of features, gates, history and locks
internal/platform/    adapters: postgres, kafka, git (GitHub + GitLab), agent (scenarios, LLM error
                      classes), nabu (client of Nabu), mcp, storage (S3),
                      whisper, events (NOTIFY → SSE), crypto, logging, telemetry, metrics, httpx
internal/jobs/cleaner the cleaner mode
```

## How the pieces fit

- **Git is the source of truth.** Documents and rules live only in the instance repository and are
  accessed through the provider API (no local clone). Commits are made with the user's own
  OAuth token and carry `Hammurapi-*` trailers.
- **Statuses live in Postgres.** Every push to a `feature/<id>` branch goes webhook → Kafka
  (partitioned by feature id) → `worker`. A commit touching a gate folder that Hammurapi does not
  already know is an edit: event `edited`, and the gate returns to draft. Commits Hammurapi made
  itself (creation, import, deletion) are recorded as `head_commit` first, so they are skipped.
- **Approval is guarded against stale content:** before approving, `api` asks the provider for the
  latest commit of the gate folder and refuses if the projection has not seen it yet.
- **Events** are published with `pg_notify`; every `api` pod listens and fans them out over one SSE
  stream per browser. Agent tokens go straight to the local SSE stream (sticky sessions).
- **Agent (FTR.HMR.CMN-0007):** the agent lives in Nabu; Hammurapi requires `NABU_URL`. Scenarios
  are runs of service agents of Nabu; code tasks upload a working copy of the branch to Nabu, the
  agent works in a sandbox there, and the worker commits the changes as the bot and opens the PR.
  Hammurapi tools are an MCP server with a grant per run. LLM errors are classified and shown in
  plain words; usage is recorded per scenario. There is no delete tool.

## Development

Requires Go 1.27. Docker is needed only for integration tests and images.

```sh
make build            # bin/hammurapi
make test             # unit tests (mockgen mocks, httptest providers)
make test-integration # + Postgres via dockertest v4
make generate         # regenerate mocks (go tool mockgen)
make image            # docker image hammurapi:<version>
```

Run locally against the compose stack from the `hammurapi` repository:

```sh
export $(grep -v '^#' ../hammurapi/.env | xargs)
./bin/hammurapi migrate && ./bin/hammurapi api
```

## Configuration

All settings are environment variables; see `internal/config/config.go` and
`hammurapi/docs/configuration.md`. Besides the variables from the architecture spec, the binary
reads `PUBLIC_URL` (external URL, used for the OAuth callback and `Secure` cookies), `HTTP_ADDR`
(`:8080`), `SERVICE_ADDR` (`:9100`), `MCP_ADDR` (`127.0.0.1:8081`) and `S3_USE_SSL`.

When the SPA and the API live on different subdomains (`web.<domain>`, `api.<domain>`,
FTR.HMR.INFRA-0002), set `PUBLIC_WEB_URL` (where the browser returns after sign-in),
`PUBLIC_API_URL` (OAuth callback, deploy callbacks), `CORS_ALLOWED_ORIGINS` (origins allowed to
call the API with credentials) and `COOKIE_DOMAIN` (shared Domain of the session and CSRF
cookies). All default to a single origin.

## Release

A tag `vX.Y.Z` runs `.github/workflows/release.yml`: lint (golangci-lint) ∥ tests → image
`ghcr.io/greenongrey/hammurapi-core` (target `release` of the `Dockerfile`: the static binary on distroless, SBOM, provenance, cosign
signature) → Trivy scan
(CRITICAL/HIGH with a fix block the deploy) → deploy through the reusable workflow of
`hammurapi` (`.github/workflows/deploy-component.yml`). A manual run with a tag redeploys the signed image without a rebuild; with
`run_id` and `callback_url` it follows the FTR.HMR.CMN-0002 deploy contract.

Versions are pinned in `deploy/versions.env` (`DEPLOY_WORKFLOW_REF`, `CHART_VERSION`) and change by PR; after changing `DEPLOY_WORKFLOW_REF` run
`deploy/sync-ref.sh` (CI checks it). Setup: `installer/docs/hammurapi.md`.

## API

REST/JSON under `/api/v1` (session + `X-CSRF-Token`), `/admin/api/v1`, `/hooks/v1/git`, and
`:9100/{healthz,readyz,metrics}`. Errors are `{"error":{"code","message","details"}}` with stable
codes; the frontend localizes by `code`. See `hammurapi/docs/api.md`.
