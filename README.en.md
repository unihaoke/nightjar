# nightjar · Intelligent Middleware Troubleshooting Platform

<p align="center">
  <a href="https://github.com/unihaoke/nightjar/actions/workflows/ci.yml"><img src="https://github.com/unihaoke/nightjar/actions/workflows/ci.yml/badge.svg?branch=master" alt="CI"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-MIT-yellow.svg" alt="License"></a>
  <a href="https://go.dev"><img src="https://img.shields.io/badge/Go-1.23-00ADD8?logo=go&logoColor=white" alt="Go"></a>
  <a href="https://vuejs.org"><img src="https://img.shields.io/badge/Vue-3-4FC08D?logo=vue.js&logoColor=white" alt="Vue"></a>
  <a href="https://www.typescriptlang.org"><img src="https://img.shields.io/badge/TypeScript-strict-3178C6?logo=typescript&logoColor=white" alt="TypeScript"></a>
  <a href="CONTRIBUTING.md"><img src="https://img.shields.io/badge/PRs-welcome-brightgreen.svg" alt="PRs Welcome"></a>
</p>

<p align="center">
  English · <a href="README.md">简体中文</a>
</p>

> A lightweight, AI-driven platform for diagnosing and resolving middleware incidents. It provides unified management, monitoring, alert governance, AI-assisted diagnosis and tiered execution for Redis / Kafka / MySQL / PostgreSQL / Elasticsearch / Nginx, and extends upward to application log alerting and AI code analysis.

This repository is the executable implementation of the [platform design document](docs/DESIGN.md): a **Go backend (Gin + GORM) plus a Vue 3 frontend (Element Plus, responsive for mobile)**. It is a modular monolith that can be deployed on a single host with one command.

---

## 1. Features

| Milestone | Scope | Status |
|-----------|-------|--------|
| M1 Core loop | Instance management + monitoring + alert rules/notifications + RBAC + audit trail | Implemented |
| M2 AI diagnosis | AI diagnosis center + knowledge base + six engineering guardrails + cost governance | Implemented |
| M3 Logs & code analysis | Log integration (Filebeat → platform Kafka) + log alert rules + external AI analysis service integration + approval loop for high-risk actions | Implemented (the fix executor is a dry-run preview; see [Known Limitations](#7-known-limitations)) |

Core coverage: Redis / MySQL / PostgreSQL / Kafka / Elasticsearch support onboarding, monitoring, threshold alerts and AI diagnosis; Nginx supports onboarding, monitoring and alerts (no AI diagnosis); RabbitMQ is onboarding-only in this release.

**Integration Center**: pick a component, fill in its address and credentials in the UI, and the platform handles "exporter exposure → Prometheus scraping → instance onboarding → recommended alert rules", mirroring the "data collection → integration center" experience of cloud consoles. Scraping targets are served via Prometheus **HTTP service discovery (http_sd, `GET /api/sd/integrations`, 30s refresh)**, so new integrations never require a Prometheus restart. Optionally mounting `docker.sock` lets the platform start exporter containers with one click. See [docs/INTEGRATION.md](docs/INTEGRATION.md).

**Log integration**: the Integration Center also offers a log-type integration. The platform uses **Ansible to deploy Filebeat idempotently** on target servers (existing installs are skipped; the "overwrite Filebeat" switch explicitly re-downloads/reinstalls for upgrades or repairs). Filebeat ships logs to the **built-in Kafka** (a single KRaft node); the backend consumes topic `mwops-logs` with consumer group `mwops-log-ingest` and reuses the existing log-event pipeline (error fingerprinting / notifications / AI analysis entry). It installs no exporter, never touches Prometheus, and does not need `docker.sock`. See [docs/LOG_INTEGRATION.md](docs/LOG_INTEGRATION.md).

**Log alert rules and AI code analysis**: processing parameters are configured per rule in the UI (`log_alert_rules`), keyed by "service + error fingerprint + severity" — dedup window, cooldown, notification channels, AI toggle and priority (lower wins). There are **no built-in default rules**: a log raises an alert only when it matches an enabled rule (unmatched logs are neither stored, notified nor analyzed). Separate exclusion rules (`log_alert_exclusions`) suppress framework noise by substring or `/regex/` and take precedence over all rules.

Matched events are handled by a post-processing worker ([logalert_worker.go](middleware-ops/internal/service/logalert_worker.go), periodically scanning `analysis_state=pending`; replicas use conditional updates for leader election). It sends notifications and then submits **redacted error details** to an **external AI analysis service** configured on the "AI Settings" page. **The platform never clones or caches business code.** When analysis completes, the service calls back (`POST /api/ai/analysis/callback`, token-authenticated and idempotent); polling is used as a fallback. The task state machine is `pending → running → awaiting → done / failed / disabled`, and the result is a three-point conclusion (file/line location / root cause / immediate mitigation / fix suggestion). The protocol contract and acceptance checklist live in [docs/AI_CODE_ANALYSIS_API.md](docs/AI_CODE_ANALYSIS_API.md).

---

## 2. Quick Start

### 2.1 One-command deployment (recommended)

```bash
cp .env.example .env
# Must change: JWT_SECRET (>=32 random chars), ADMIN_PASSWORD, DB_PASSWORD, REDIS_PASSWORD
docker compose up -d --build
```

Open `http://<host>:8000` and log in with the admin account from `.env` (default `admin`). **Change the password immediately after first login.**

Bundled components: PostgreSQL 15 + pgvector, Redis 7, Kafka (log bus, enabled by default), Prometheus, Grafana, the Go backend, and Nginx serving the frontend.

> The monitoring stack lives entirely in this platform: managed projects no longer need their own Prometheus / Grafana / exporters. Grafana datasources are injected via provisioning; import dashboards using the IDs shown on integration cards.

> Tables are created by GORM AutoMigrate at startup (`database.auto_migrate=true`). Scripts under `deploy/postgres/init/` only create extensions and database parameters — never pre-create tables manually (constraint naming mismatches break migration). For DBA-managed schemas use [docs/SCHEMA.sql](docs/SCHEMA.sql) (constraint names follow GORM conventions) and set `auto_migrate=false`.

### 2.2 Local development

Prerequisites: Go 1.23+, Node.js 20+ (CI uses 22), PostgreSQL 15 (Redis / Prometheus optional).

```bash
# 1) Database
createdb middleware_ops

# 2) Backend (reads configs/config.yaml; degrades gracefully without Redis/Prometheus)
cd middleware-ops
go mod tidy
go run ./cmd/server -config configs/config.yaml     # listens on :8080

# 3) Frontend (proxies /api to 127.0.0.1:8080)
cd ../middleware-ops-web
npm install
npm run dev                                          # listens on :5173
```

### 2.3 Offline mode with zero external dependencies

The platform is deliberately designed to run with dependencies missing, which makes local demos and end-to-end testing possible:

| Dependency | Fallback | How to connect the real thing |
|------------|----------|-------------------------------|
| Redis | `redis.addr` empty → in-process cache + in-memory queue | Set `redis.addr` (password/DB supported) |
| Prometheus | `prometheus.base_url` empty → built-in deterministic metrics simulator | Set `prometheus.base_url` |
| Third-party LLM | Disabled → rule engine producing structured semi-automatic conclusions | Configure `ai_engine.third_party.*` |
| Local LLM | `kind: mock` → in-process deterministic engine | `kind: ollama` + `base_url` |
| pgvector | Disabled → vectors stored as text, cosine similarity in the application layer | Build with `-tags pgvector` and `CREATE EXTENSION vector` |

The metrics simulator is deterministic (same instance + metric + time bucket → same value), so charts, rule evaluation and AI context always stay consistent.

---

## 3. Documentation

The full documentation index is at **[docs/README.md](docs/README.md)**. Common entry points:

| Document | Content |
|----------|---------|
| [docs/DESIGN.md](docs/DESIGN.md) | Design baseline; chapter 14 maps the design to the actual implementation |
| [docs/INTEGRATION.md](docs/INTEGRATION.md) | Integration Center: end-to-end onboarding tutorial, deployment modes, bring-your-own-exporter guide, troubleshooting |
| [docs/LOG_INTEGRATION.md](docs/LOG_INTEGRATION.md) | Authoritative log-pipeline reference: Filebeat → Kafka topology, idempotent deployment, self-checks, config reference |
| [docs/AI_CODE_ANALYSIS_API.md](docs/AI_CODE_ANALYSIS_API.md) | External AI service OpenAPI v1 contract, callback HMAC (Appendix A), platform acceptance checklist (Appendix B) |
| [docs/API.md](docs/API.md) | Platform HTTP API reference and error-code table |
| [docs/OPERATIONS.md](docs/OPERATIONS.md) | Deployment, configuration reference, backup/restore, upgrades and on-call runbooks |
| [docs/POSTMORTEM.md](docs/POSTMORTEM.md) | Incident postmortems from real delivery engagements (historical archive, kept as-is) |

> Note: documentation body text is written in Chinese by project convention; file names are in English.

---

## 4. Repository Layout

```text
.
├── .github/                        # CI, issue/PR templates, Dependabot
├── docs/                           # Design / API / operations docs (index: docs/README.md)
├── middleware-ops/                 # Backend (Go 1.23, modular monolith)
│   ├── cmd/server/                 # Entry point: config → logging → DB → cache → engines → services → router → scheduler
│   ├── cmd/renderdump/             # Debug tool: dump rendered Ansible artifacts
│   ├── configs/config.yaml         # Default configuration (commented)
│   └── internal/
│       ├── config/                 # viper config + env overrides + startup validation
│       ├── model/ · db/            # Entities; GORM connection, migrations, pgvector build tags
│       ├── repository/             # Data access (audit tables are append-only — no Update/Delete)
│       ├── engine/guardrail/       # Six guardrails: budget / loop_guard / timeout / permscope / quality / cost
│       ├── monitor/                # Prometheus query wrapper (no self-built collector) + deterministic simulator
│       ├── logpipe/                # Kafka consumption (group mwops-log-ingest) + Filebeat event parsing
│       ├── integration/            # Integration templates + collector/Ansible rendering (pure, unit-tested)
│       ├── docker/                 # Minimal Docker Engine API client (one-click exporters)
│       ├── service/                # Business services: diagnosis, alert convergence, log post-processing, external AI
│       ├── handler/ · router/      # HTTP layer (permission points and risk tiers declared on routes)
│       ├── middleware/             # Gin middleware (tracing/recovery/rate-limit/auth/data-scope)
│       └── pkg/cache/ · utils/     # Redis/in-memory cache; JWT/crypto/password utilities
├── middleware-ops-web/             # Frontend (Vue 3 + Vite + TS + Element Plus, light/dark themes, responsive)
├── deploy/                         # Postgres init, Prometheus config & alert rules, Grafana provisioning, Ansible tools
├── scripts/                        # onboard.sh, doctor.sh, smoke-test.ps1 (end-to-end smoke)
├── docker-compose.yml              # One-command deployment
├── Makefile                        # Developer commands (make help)
├── CONTRIBUTING.md · SECURITY.md · CODE_OF_CONDUCT.md · CHANGELOG.md
└── LICENSE
```

---

## 5. Design Highlights

### 5.1 Six engineering guardrails around AI

| Guardrail | Location | Key behavior |
|-----------|----------|--------------|
| ① Context budget | `engine/guardrail/budget.go` | Configurable 8K input / 2K output; reports which dimensions were truncated; time series downsampled to mean/P95/slope/turnpoints/anomalous segments |
| ② Loop prevention | `loop_guard.go` | Step cap; same tool + same argument fingerprint repeated twice terminates; tool allowlist; per-step decision card; no automatic retry on tool failure |
| ③ Timeout & degradation | `timeout.go` | 10s tool / 120s task timeouts; missing sources are dropped and flagged; third-party → local → rule-engine degradation with circuit breaking |
| ④ Permission isolation | `permscope.go` | AI gets read-only tools only (no write methods even in types); data scoping enforced in repository queries; AI-generated SQL is forced read-only with LIMIT, single statement and table allowlist |
| ⑤ Quality | `quality.go` | Mandatory structured output (root cause/evidence/confidence/advice/impact/open questions); speculation labeled without hard evidence; 24-case fault evaluation set |
| ⑥ Cost governance | `cost.go` | Deterministic 24h cache per instance + question signature; per-user/platform daily budgets; concurrency ≤4; circuit breaking on abnormal spikes |

### 5.2 Security

- **RBAC**: four built-in roles (admin / ops / dev / readonly) plus custom roles; permission points are declared on routes and enforced server-side.
- **Data scoping**: isolation by environment (dev/staging/prod) and group, applied in repository queries — never via prompt instructions.
- **Action tiers**: L0 read-only / L1 low-risk / L2 high-risk; every L2 action requires approval (mandatory in production, auto-rejected after 30 minutes), and requester and approver must be different people.
- **Verifiable audit**: `audit_logs` expose no update/delete methods anywhere; a hash chain `hash_self = SHA256(hash_prev | canonical content)` plus daily snapshots makes tampering locatable.
- **Credentials**: connection passwords are stored AES-256-GCM encrypted; the master key comes from an environment variable or an auto-generated 0600 key file; ciphertext is never returned by the API.
- **Egress compliance**: outbound traffic is denied by default; services must be explicitly allowlisted, and stack traces are force-redacted (IPs, phone numbers, request IDs, paths, emails, credentials).

### 5.3 API conventions

- Unified response: `{code, message, data}`; pagination via `page` / `page_size` (default 20, max 100).
- Error code ranges: 400x parameters, 401x authentication, 403x permissions, 404x not found, 500x system.
- Auth: `Authorization: Bearer <JWT>`, also set as a SameSite cookie; permissions are rebuilt from the database per request, so role changes take effect immediately.
- Streaming: `POST /api/ai/diagnose` uses SSE with event types `meta` / `data` / `done` / `error`.
- Ingest hooks: `POST /api/hooks/logs`, `POST /api/hooks/alerts` use the `X-Hook-Token` service token, separate from user JWTs.

See [docs/API.md](docs/API.md) for the full API reference.

---

## 6. Verification

```bash
# Full pre-commit gate (backend fmt/vet/tests + frontend build/smoke)
make all-check

# Individual targets
make backend-lint     # gofmt -l + go vet
make backend-test     # go test ./...
make frontend-build   # type check + production build
make frontend-smoke   # headless-browser smoke against built assets

# End-to-end smoke (requires the backend on :8080; use pwsh for PS 7, powershell for PS 5.1)
make verify
```

Without `make` (e.g. Windows), run `gofmt -l . && go vet ./... && go test ./...` under `middleware-ops/`, and `npm run build` / `npm run smoke` under `middleware-ops-web/`.

Unit tests focus on authorization cases (RBAC points/tiers, data-scope filtering, read-only tool enforcement), alert dedup fingerprints, the six guardrails, egress redaction, external AI integration (protocol adaptation / callback idempotency), and database schema invariants (constraint naming matching the GORM strategy; column names avoiding PostgreSQL reserved words).

---

## 7. Known Limitations

1. **The fix executor is a dry-run preview**: `service/dryRunExecutor` returns impact previews and parameter validation only — it never mutates managed middleware. Real execution requires implementing and injecting the `service.Executor` interface; approved L2 actions are executed manually following the preview and then recorded.
2. **Code analysis requires an external AI analysis service**: the platform neither clones nor caches business code, and does not perform AST analysis, code knowledge graphs or vector reranking. It triggers per rule, sends redacted payloads outbound, and converges three-point conclusions via callback/polling. Deep code indexing is future work.
3. **No causal correlation**: alert convergence covers rule-level (real-time) and semantic clustering (offline, display-only). Topology-based causal correlation is future work.
4. **Notification channels require external configuration**: without webhooks, Feishu/WeCom/DingTalk/email notifications are logged only; the main pipeline is unaffected.
5. **Default builds do not enable native pgvector types**: vectors are stored as text with application-layer cosine similarity (acceptable up to ~200 instances); build with `-tags pgvector` for ANN indexes.
6. **No manual chunk splitting in the frontend**: `vite.config.ts` deliberately avoids `manualChunks`. element-plus and dayjs reference each other, and forced splitting creates chunk cycles that trigger ES module TDZ (a white screen). See INC-003 in [docs/POSTMORTEM.md](docs/POSTMORTEM.md).

---

## 8. Contributing

Issues and pull requests are welcome. Before submitting:

- Keep the backend `gofmt`-clean and ensure `go vet ./...` and `go test ./...` pass; run `make all-check`.
- Follow [Conventional Commits](https://www.conventionalcommits.org/): `<type>(<scope>): <subject>`.
- Update the relevant documents under `docs/` for behavior changes, and register them in [docs/README.md](docs/README.md) to avoid dead links.

See [CONTRIBUTING.md](CONTRIBUTING.md) for details, [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) for community standards, and [SECURITY.md](SECURITY.md) for private vulnerability reporting. Real incident postmortems are collected in [docs/POSTMORTEM.md](docs/POSTMORTEM.md).

## 9. License

Released under the [MIT License](LICENSE). Metrics collection relies on the Prometheus ecosystem and official middleware exporters (no collector binaries are included in this repository).
