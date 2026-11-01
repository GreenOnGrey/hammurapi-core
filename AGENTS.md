# AGENTS.md — hammurapi-core

Guide for coding agents and developers working in this repository.

## Workspace

Hammurapi is five repositories checked out side by side:

| Repository | What it is |
| --- | --- |
| `hammurapi-core` (this one) | Go backend: one binary with modes and migrations |
| `hammurapi-web` | React SPA |
| `hammurapi` | Public: docs, docker-compose demo stack, self-hosted Helm chart, installer, reusable deploy workflow |
| `hammurapi-infra` | Private: the stand (minikube), charts `hammurapi-core`/`hammurapi-web`, `hammurapi-deploy` |
| `hammurapi-specs` | Source of truth: product, design, arch, tech and qa specifications |

- Every change implements a feature specification from
  `hammurapi-specs/specs/HMR/<GROUP>/FTR.HMR.<GROUP>-NNNN/` (`CMN` — the product, `INFRA` — the stand).
  Read all five areas (product, design, arch, tech, qa) before coding. Test IDs (`IDX-07`, `SCN-05`,
  …) come from the qa spec; name tests and comments after them.
- Refer to specs in comments as `FTR.HMR.CMN-0005 R4`, `FTR.HMR.CMN-0004 arch §3`. The old forms
  `PLT.HMR-…`, `PLT.INFRA-…` and bare `HMR.CMN-…` are obsolete.
- When the implementation deviates from a spec, write the deviation into the spec (version bump,
  "Принятые решения") in the same piece of work.
- Do not commit, tag or push unless asked: the maintainer reviews and releases by tags.

## Stack and layout

Go 1.27 (`go.mod`), Postgres (pgx, goose), Kafka, S3/MinIO, chi.

```text
cmd/hammurapi/        entry point; modes: api | worker | cleaner | migrate
cmd/fakegitlab/       in-memory GitLab for the demo stack (dev only; /fake/* helper endpoints)
migrations/           goose migrations, embedded
internal/app/         wiring per mode (RunAPI, RunWorker, RunOperator, …)
internal/config/      every environment variable
internal/features/*   vertical slices: handlers, service, repository
internal/platform/*   adapters: postgres, kafka, git (GitHub + GitLab), agent, mcp, metrics, …
internal/specdata, internal/cycledata   shared projections
internal/itest/       integration tests (build tag integration)
deploy/versions.env   DEPLOY_WORKFLOW_REF, CHART_VERSION — changed only by PR
```

The agent lives in Nabu (FTR.HMR.CMN-0006, FTR.HMR.CMN-0007): Hammurapi has no agent of its own and does
not start without `NABU_URL`, `NABU_CLIENT_ID`, `NABU_CLIENT_SECRET`. The chat is proxied to the
user's personal agent (`internal/features/nabuconn`: chat proxy, `nabu.*` SSE bridge, `/mcp/nabu`
with Nabu JWTs, the admin section). Scenarios run as runs of service agents (`agentrun.NabuRuns`);
they reach the MCP of the worker (`internal/features/agent/tools.go`, `internal/features/specindex/tools.go`)
with a grant per run. Code tasks (`internal/features/codegen/code.go`) upload a working copy of the
branch to Nabu, the agent works in a sandbox of Nabu, the worker polls the run, takes the changes and
commits them as the bot; the task token is the MCP token of the run. Usage stays in `agent_usage`
(`internal/features/usage`).

## Commands

```sh
make build                          # bin/hammurapi
go test ./...                       # unit tests
go test -tags integration -count=1 ./internal/itest/...   # Postgres: dockertest v4, or
HMR_TEST_DATABASE_URL=postgres://… go test -tags integration ./internal/itest/   # an existing server
golangci-lint run ./... && golangci-lint run --build-tags integration ./internal/itest/
go generate ./internal/platform/git/  # mockgen mocks after changing git.Provider (or other mocked interfaces)
```

CI (`.github/workflows/ci.yml`) runs lint, unit and integration tests, `deploy/sync-ref.sh --check`
and actionlint. Run the same before handing work over.
Tests that need Linux (symlinks, `/proc`) skip on Windows; run them in Linux or WSL.

## Rules learned the hard way

- **Migrations** are expand-only: new files with the next number; never change an applied migration
  except its comments. Every migration has a `Down`. `ALTER TYPE … ADD VALUE` needs
  `-- +goose NO TRANSACTION`. Update the expected version in `internal/itest/migrations_test.go`.
- **Do not run generic formatters over SQL, YAML or shell.** A formatter once rewrote
  `REFERENCES …` into invalid `FOREIGN KEY REFERENCES …` in a migration. Go: `gofmt` only.
- Go regexps are RE2: `\b` is ASCII-only — it never matches after Cyrillic (`Дано\b` fails).
- Every error returned to clients has a stable code (`apperr`); a new code needs a translation in
  `hammurapi-web/locales/*.json` (`errors.<code>`).
- A new environment variable goes to `internal/config/config.go`, `hammurapi/docs/configuration.md`
  and, if the stand needs it, the infra chart.
- Secrets never go to argv, logs or files: environment or stdin only; mask them in logs.
- `fakegitlab` and the `/fake/*` endpoints are for development and demos only.
- Image scanning (Trivy) blocks fixable HIGH/CRITICAL. Accepted findings go to `.trivyignore.yaml`
  with a reason and an `expired_at`; remove the entry when the dependency is fixed.

## Releases

A tag `vX.Y.Z` runs `release.yml`: image `ghcr.io/greenongrey/hammurapi-core` (target `release`,
distroless, no Node.js), SBOM, cosign signature, Trivy, then the reusable `deploy-component.yml` of the `hammurapi`
repo pinned by `DEPLOY_WORKFLOW_REF` (run `deploy/sync-ref.sh` after changing it). `CHART_VERSION`
is the `hammurapi-infra` release whose chart is installed. When a change needs a new chart or a new
deploy workflow, the order is: infra tag → hammurapi tag → bump `deploy/versions.env` here → core tag.
A manual run of `release.yml` from `main` with an existing tag redeploys the signed image.
