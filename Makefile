GOLANGCI_LINT_VERSION := v2.15.0
SQLC := go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1
OAPI := go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.2
COMPOSE := docker compose -f deployments/compose/docker-compose.yml

.PHONY: deploy-check build-go ui-install ui-build ui-dev ui-lint ui-test smoke up-all down-all ps-all logs-all admin image up-temporal run-worker measure run-api run-gateway run-docker-mcp run-prometheus-mcp run-loki-mcp run-kubernetes-mcp run-github-mcp run-pricing-engine-mcp up-docker-proxy dev-db build test test-short lint fmt generate generate-check up up-idp down migrate-up migrate-down migrate-status

# Giao diện web (ADR-0016): mã nguồn React ở web/, bản build ở internal/webui/dist (được
# nhúng vào binary api bằng go:embed). Cần Node; phiên bản phụ thuộc ghim ở web/package-lock.json.
UI_SRC := $(shell find web/src web/public -type f 2>/dev/null) web/index.html web/package.json web/vite.config.ts web/tsconfig.json
UI_DEPS := web/node_modules/.package-lock.json
UI_DIST := internal/webui/dist/index.html

$(UI_DEPS): web/package-lock.json
	cd web && npm ci --ignore-scripts

$(UI_DIST): $(UI_DEPS) $(UI_SRC)
	cd web && npm run build

ui-install: $(UI_DEPS) ## Cài phụ thuộc của giao diện đúng theo package-lock.json

ui-build: $(UI_DIST) ## Build giao diện vào internal/webui/dist (chỉ chạy lại khi mã nguồn đổi)

ui-dev: $(UI_DEPS) ## Giao diện ở http://localhost:5173/ có tải lại nóng; API lấy từ `make run-api`
	cd web && npm run dev

ui-lint: $(UI_DEPS) ## Kiểm kiểu TypeScript + ESLint (luật phụ thuộc giữa feature, cấm HTML thô)
	cd web && npm run typecheck && npm run lint

ui-test: $(UI_DEPS) ## Test của giao diện (vitest, không cần trình duyệt)
	cd web && npm test

build: ui-build build-go ## Build giao diện rồi mọi binary vào ./bin

# Chỉ binary Go, không chạm tới Node. Các process không phục vụ giao diện (gateway, worker,
# MCP server) chạy bằng target này: máy của chúng không cần Node, và công cụ build của
# giao diện (mã bên thứ ba) không chạy cạnh khoá của gateway. Binary api build kiểu này khi
# chưa có bản build của giao diện sẽ trả 503 ở /.
build-go: ## Build mọi binary vào ./bin, không build giao diện
	go build -o bin/ ./cmd/...

test: ## Toàn bộ test, gồm test cần PostgreSQL thật (Docker)
	go test -race ./...

test-short: ## Chỉ test không cần Docker
	go test -race -short ./...

lint: ## go vet + golangci-lint (gồm luật phụ thuộc depguard)
	go vet ./...
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

generate: $(UI_DEPS) ## Sinh lại code: sqlc (migrations/ + queries.sql) và kiểu API cho Go và giao diện (openapi.yaml)
	$(SQLC) generate
	$(OAPI) -config internal/api/oapi-codegen.yaml .docs/api/openapi.yaml
	cd web && npm run generate

generate-check: $(UI_DEPS) ## Lỗi nếu code sinh ra lệch với nguồn (dùng trong CI)
	$(SQLC) diff
	$(OAPI) -generate models -package api .docs/api/openapi.yaml | diff -q - internal/api/types.gen.go
	cd web && npx --no-install openapi-typescript ../.docs/api/openapi.yaml | diff -q - src/api/schema.gen.ts

fmt:
	gofmt -w cmd internal migrations tests

up: ## Khởi động Postgres + Redis cục bộ
	$(COMPOSE) up -d --wait

# Chuẩn bị database cục bộ: migrate bằng superuser của compose, rồi tạo tài khoản
# đăng nhập cho từng process. Process từ chối chạy bằng superuser (RLS bị bỏ qua).
# Mật khẩu ở đây chỉ dùng cục bộ.
dev-db: up
	HQ_MIGRATE_DATABASE_URL='postgres://hq:hq@localhost:5432/hq?sslmode=disable' go run ./cmd/migrate up
	$(COMPOSE) exec -T postgres psql -U hq -d hq -v ON_ERROR_STOP=1 -c "\
	  DO \$$\$$ BEGIN \
	    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='hq_api')     THEN CREATE ROLE hq_api     LOGIN PASSWORD 'hq' IN ROLE app_api;     END IF; \
	    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='hq_gateway') THEN CREATE ROLE hq_gateway LOGIN PASSWORD 'hq' IN ROLE app_gateway; END IF; \
	    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='hq_worker')  THEN CREATE ROLE hq_worker  LOGIN PASSWORD 'hq' IN ROLE app_worker;  END IF; \
	  END \$$\$$;"

up-docker-proxy: ## Proxy chỉ đọc trước Docker socket, cho MCP server Docker
	$(COMPOSE) --profile docker up -d docker-proxy

up-temporal: ## Temporal dev server cục bộ (gRPC 7233, UI 8233)
	$(COMPOSE) --profile temporal up -d temporal

up-idp: ## Thêm Keycloak cục bộ
	$(COMPOSE) --profile idp up -d

down:
	$(COMPOSE) down

# --- Platform trong container (chỉ cho máy cục bộ) -------------------------------------
# `make up-all` build image, chạy migration, tạo tài khoản database rồi khởi động api,
# gateway, worker và MCP server Docker cùng hạ tầng (Postgres, Redis, Keycloak, Temporal,
# proxy Docker). Cấu hình lấy từ .env ở gốc repo; compose nội suy TỪNG biến cho đúng
# service được phép giữ nó (xem deployments/compose/docker-compose.yml). Biến đặt trên
# dòng lệnh thắng .env: `HQ_DOCKER_ORG_ID=<id> make up-all`.
# Model chạy bằng adapter anthropic qua gateway: cần HQ_ANTHROPIC_API_KEY trong .env
# (tốn tiền thật) hoặc khoá của org lưu qua API. Claude CLI không có trong container.
ROOTKEY = $$(set -a; . ./.env; set +a; printf %s "$$HQ_SECRETS_ROOT_KEY_FILE")
APPENV = HQ_CONTAINER_UID="$$(id -u)" HQ_CONTAINER_GID="$$(id -g)" HQ_ROOT_KEY_HOST_FILE="$$(realpath "$(ROOTKEY)")"
APPCOMPOSE = $(APPENV) $(COMPOSE) --env-file .env --profile app

image: ## Build image mcp-platform:local (mọi binary)
	docker build -t mcp-platform:local .

up-all: ## Chạy cả platform trong container (cần .env; xem chú thích phía trên)
	@f="$(ROOTKEY)"; if [ -z "$$f" ] || [ ! -f "$$f" ]; then \
	  echo "up-all: HQ_SECRETS_ROOT_KEY_FILE trong .env phải trỏ tới file khoá gốc (tạo bằng: ./bin/hqadmin secrets init -out <file>)"; exit 1; fi
	$(APPCOMPOSE) up -d --build
	@$(APPCOMPOSE) ps --format 'table {{.Service}}\t{{.Status}}\t{{.Ports}}'

down-all: ## Dừng mọi container của platform (giữ volume dữ liệu)
	$(APPENV) $(COMPOSE) --env-file .env --profile app --profile tools --profile idp --profile temporal --profile docker down

ps-all:
	@$(APPCOMPOSE) ps --format 'table {{.Service}}\t{{.Status}}\t{{.Ports}}'

logs-all: ## Log của api, gateway, worker, MCP server Docker (Ctrl-C để thoát)
	$(APPCOMPOSE) logs -f --tail 50 api gateway worker integration-docker

admin: ## hqadmin trong container: make admin ARGS='org create -slug acme -name Acme …'
	@$(APPENV) $(COMPOSE) --env-file .env --profile tools run --rm hqadmin $(ARGS)

# --- Triển khai một máy chủ bằng Docker Compose (ADR-0017) ------------------------------
# File compose của máy chủ nằm ở deployments/production/ và được điều khiển TRÊN MÁY CHỦ bằng
# deployments/production/bin/hqctl (runbook: .docs/runbooks/deploy-compose.md). Ở đây chỉ có
# bước kiểm, chạy được trên máy lập trình viên và trong CI.
deploy-check: ## Kiểm file compose production với file env mẫu (không khởi động gì)
	HQ_IMAGE_TAG=check docker compose -f deployments/production/compose.yaml --env-file deployments/production/hq.env.example config -q
	bash -n deployments/production/bin/hqctl
	sh -n deployments/production/temporal/setup-schema.sh
	sh -n deployments/production/temporal/init-namespace.sh
	sh -n deployments/production/postgres/backup.sh

# (HQ_MIGRATE_DATABASE_URL), không phải tài khoản của process ứng dụng.
migrate-up:
	go run ./cmd/migrate up

migrate-down: ## Lùi một migration; chỉ dùng trong phát triển
	go run ./cmd/migrate down

migrate-status:
	go run ./cmd/migrate status

# Chạy process cục bộ với cấu hình trong .env. Mỗi process chỉ nhận những biến nó
# được phép giữ: api không có khoá gốc của Secrets, khoá ký ngữ cảnh hay khoá LLM;
# server MCP không có DSN hay khoá nào ngoài khoá ký ngữ cảnh.
LOADENV = set -a; . ./.env; set +a

run-api: build ## API (cần `make dev-db`, `make up-idp`)
	@$(LOADENV); env -u HQ_SECRETS_ROOT_KEY_FILE -u HQ_SERVICE_SIGNING_KEY -u HQ_ANTHROPIC_API_KEY -u HQ_ADMIN_DATABASE_URL -u HQ_MIGRATE_DATABASE_URL \
	  HQ_HTTP_ADDR="$$HQ_API_HTTP_ADDR" HQ_DATABASE_URL="$$HQ_API_DATABASE_URL" ./bin/api

run-gateway: build-go ## MCP Gateway (process duy nhất giữ khoá gốc của Secrets và khoá LLM)
	@$(LOADENV); env -u HQ_SECRETS_PUBLIC_KEY -u HQ_ENABLE_CLAUDE_CLI -u HQ_ADMIN_DATABASE_URL -u HQ_MIGRATE_DATABASE_URL \
	  HQ_HTTP_ADDR="$$HQ_GATEWAY_HTTP_ADDR" HQ_DATABASE_URL="$$HQ_GATEWAY_DATABASE_URL" ./bin/gateway

run-worker: build-go ## Execution Worker (cần `make up-temporal`); không nhận khoá nào của Secrets hay LLM
	@$(LOADENV); env -u HQ_SECRETS_ROOT_KEY_FILE -u HQ_SECRETS_PUBLIC_KEY -u HQ_SERVICE_SIGNING_KEY -u HQ_SERVICE_TOKEN_KEY -u HQ_ANTHROPIC_API_KEY -u HQ_ADMIN_DATABASE_URL -u HQ_MIGRATE_DATABASE_URL \
	  HQ_HTTP_ADDR="$$HQ_WORKER_HTTP_ADDR" HQ_DATABASE_URL="$$HQ_WORKER_DATABASE_URL" ./bin/worker

run-docker-mcp: build-go ## Managed MCP server Docker chỉ đọc (cần `make up-docker-proxy`)
	@$(LOADENV); env -i PATH="$$PATH" HQ_ENV="$$HQ_ENV" HQ_HTTP_ADDR="$$HQ_DOCKER_MCP_HTTP_ADDR" HQ_DOCKER_API_URL="$$HQ_DOCKER_API_URL" \
	  HQ_DOCKER_ORG_ID="$$HQ_DOCKER_ORG_ID" HQ_SERVICE_SIGNING_KEY="$$HQ_SERVICE_SIGNING_KEY" ./bin/integration-docker

run-prometheus-mcp: build-go ## Managed MCP server Prometheus chỉ đọc (HQ_PROMETHEUS_URL do người vận hành đặt)
	@$(LOADENV); env -i PATH="$$PATH" HQ_ENV="$$HQ_ENV" HQ_HTTP_ADDR="$$HQ_PROMETHEUS_MCP_HTTP_ADDR" HQ_PROMETHEUS_URL="$$HQ_PROMETHEUS_URL" \
	  HQ_PROMETHEUS_BEARER_TOKEN_FILE="$$HQ_PROMETHEUS_BEARER_TOKEN_FILE" \
	  HQ_PROMETHEUS_ORG_ID="$$HQ_PROMETHEUS_ORG_ID" HQ_SERVICE_SIGNING_KEY="$$HQ_SERVICE_SIGNING_KEY" ./bin/integration-prometheus

run-loki-mcp: build-go ## Managed MCP server Loki chỉ đọc (HQ_LOKI_URL do người vận hành đặt)
	@$(LOADENV); env -i PATH="$$PATH" HQ_ENV="$$HQ_ENV" HQ_HTTP_ADDR="$$HQ_LOKI_MCP_HTTP_ADDR" HQ_LOKI_URL="$$HQ_LOKI_URL" \
	  HQ_LOKI_BEARER_TOKEN_FILE="$$HQ_LOKI_BEARER_TOKEN_FILE" HQ_LOKI_TENANT_ID="$$HQ_LOKI_TENANT_ID" HQ_LOKI_ALLOW_MULTI_TENANT="$$HQ_LOKI_ALLOW_MULTI_TENANT" \
	  HQ_LOKI_ORG_ID="$$HQ_LOKI_ORG_ID" HQ_SERVICE_SIGNING_KEY="$$HQ_SERVICE_SIGNING_KEY" ./bin/integration-loki

run-kubernetes-mcp: build-go ## Managed MCP server Kubernetes chỉ đọc (HQ_KUBERNETES_URL do người vận hành đặt)
	@$(LOADENV); env -i PATH="$$PATH" HQ_ENV="$$HQ_ENV" HQ_HTTP_ADDR="$$HQ_KUBERNETES_MCP_HTTP_ADDR" HQ_KUBERNETES_URL="$$HQ_KUBERNETES_URL" \
	  HQ_KUBERNETES_BEARER_TOKEN_FILE="$$HQ_KUBERNETES_BEARER_TOKEN_FILE" HQ_KUBERNETES_CA_FILE="$$HQ_KUBERNETES_CA_FILE" \
	  HQ_KUBERNETES_NAMESPACES="$$HQ_KUBERNETES_NAMESPACES" \
	  HQ_KUBERNETES_ORG_ID="$$HQ_KUBERNETES_ORG_ID" HQ_SERVICE_SIGNING_KEY="$$HQ_SERVICE_SIGNING_KEY" ./bin/integration-kubernetes

run-github-mcp: build-go ## Managed MCP server GitHub chỉ đọc (token và allowlist do người vận hành đặt)
	@$(LOADENV); env -i PATH="$$PATH" HQ_ENV="$$HQ_ENV" HQ_HTTP_ADDR="$$HQ_GITHUB_MCP_HTTP_ADDR" HQ_GITHUB_API_URL="$$HQ_GITHUB_API_URL" \
	  HQ_GITHUB_TOKEN_FILE="$$HQ_GITHUB_TOKEN_FILE" HQ_GITHUB_REPOSITORIES="$$HQ_GITHUB_REPOSITORIES" \
	  HQ_GITHUB_ORG_ID="$$HQ_GITHUB_ORG_ID" HQ_SERVICE_SIGNING_KEY="$$HQ_SERVICE_SIGNING_KEY" ./bin/integration-github

run-pricing-engine-mcp: build-go ## Managed MCP server Pricing Engine chỉ đọc (HQ_PRICING_ENGINE_URL và file API key do người vận hành đặt)
	@$(LOADENV); env -i PATH="$$PATH" HQ_ENV="$$HQ_ENV" HQ_HTTP_ADDR="$$HQ_PRICING_ENGINE_MCP_HTTP_ADDR" HQ_PRICING_ENGINE_URL="$$HQ_PRICING_ENGINE_URL" \
	  HQ_PRICING_ENGINE_API_KEY_FILE="$$HQ_PRICING_ENGINE_API_KEY_FILE" HQ_PRICING_ENGINE_API_KEY="$$HQ_PRICING_ENGINE_API_KEY" \
	  HQ_PRICING_ENGINE_ORG_ID="$$HQ_PRICING_ENGINE_ORG_ID" HQ_SERVICE_SIGNING_KEY="$$HQ_SERVICE_SIGNING_KEY" ./bin/integration-pricing-engine

measure: ## Đo độ trễ thêm của Gateway, kích thước step và dữ liệu ghi mỗi execution (cần Docker)
	HQ_MEASURE=1 go test ./tests/api -run TestMeasure -count=1 -v 2>&1 | grep -A8 '=== Số đo'


smoke: ## Thử nhanh platform cục bộ; xem đầu scripts/smoke-local.sh (đặt HQ_SMOKE_MODEL + giá để chạy một execution thật)
	@./scripts/smoke-local.sh
