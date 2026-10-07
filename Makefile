COMPOSE ?= docker compose
API_DIR ?= buddi-api
WEB_DIR ?= buddi-web
GO ?= go

ifneq (,$(wildcard $(API_DIR)/.env))
include $(API_DIR)/.env
endif

MODEL ?= $(or $(BUDDI_MODEL),qwen3:0.6b)
EMBED_MODEL ?= $(or $(BUDDI_EMBED_MODEL),nomic-embed-text)
TEST_DATABASE_URI ?= postgres://buddi:buddi@localhost:5432/buddi_test?sslmode=disable

export

.DEFAULT_GOAL := help

.PHONY: help dev dev-watch up infra db ollama obs models bootstrap api api-watch web \
        down clean logs ps build web-build vet fmt test test-live test-db

help:
	@echo Buddi targets
	@echo   make dev        start infra (db, ollama, obs) then the API and the web UI
	@echo   make dev-watch  the same, with the API on live reload (air)
	@echo   make up         start infrastructure only
	@echo   make models     pull $(MODEL) and $(EMBED_MODEL) into Ollama
	@echo   make api        run the API (reads $(API_DIR)/.env)
	@echo   make web        run the web dev server on http://localhost:5173
	@echo   make down       stop infrastructure (keeps volumes)
	@echo   make clean      stop infrastructure and delete volumes
	@echo   make test       run the API test suite
	@echo   make test-live  run the suite against a disposable test database
	@echo.
	@echo   API      http://localhost:$(or $(PORT),8000)
	@echo   Web      http://localhost:5173
	@echo   Jaeger   http://localhost:16686
	@echo   Ollama   http://localhost:11434

dev: bootstrap up
	@echo API on http://localhost:$(or $(PORT),8000)  Web on http://localhost:5173  Jaeger on http://localhost:16686
	$(MAKE) -j2 api web

dev-watch: bootstrap up
	@echo API on http://localhost:$(or $(PORT),8000)  Web on http://localhost:5173  Jaeger on http://localhost:16686
	$(MAKE) -j2 api-watch web

up: db ollama obs

infra: up

db:
	$(COMPOSE) up -d --wait db

ollama:
	$(COMPOSE) --profile llm up -d --wait ollama

obs:
	$(COMPOSE) --profile obs up -d

models: ollama
	$(COMPOSE) --profile llm exec -T ollama ollama pull $(MODEL)
	$(COMPOSE) --profile llm exec -T ollama ollama pull $(EMBED_MODEL)

bootstrap:
	cd $(API_DIR) && $(GO) mod download
	cd $(WEB_DIR) && npm install

api:
	cd $(API_DIR) && $(GO) run ./cmd/server

api-watch:
	cd $(API_DIR) && air

web:
	cd $(WEB_DIR) && npm run dev

down:
	$(COMPOSE) --profile llm --profile obs down

clean:
	$(COMPOSE) --profile llm --profile obs down -v

logs:
	$(COMPOSE) --profile llm --profile obs logs -f --tail=100

ps:
	$(COMPOSE) --profile llm --profile obs ps

build:
	cd $(API_DIR) && $(GO) build ./...

web-build:
	cd $(WEB_DIR) && npm run build

vet:
	cd $(API_DIR) && $(GO) vet ./...

fmt:
	cd $(API_DIR) && gofmt -w .

test:
	cd $(API_DIR) && $(GO) test ./... -count=1 -timeout 900s

test-live: export BUDDI_TEST_DATABASE_URI := $(TEST_DATABASE_URI)
test-live: test-db
	cd $(API_DIR) && $(GO) test ./... -count=1 -timeout 900s

test-db: db
	$(COMPOSE) exec -T db psql -U buddi -d buddi -c "SELECT 'CREATE DATABASE buddi_test' WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname='buddi_test')\gexec"