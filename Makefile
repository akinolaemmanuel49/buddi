COMPOSE ?= docker compose
API_DIR ?= buddi-api
WEB_DIR ?= buddi-web
GO ?= go
PORT ?= 8000

# Recipes deliberately avoid shell-specific syntax - no `$$(...)`, no backslash
# joins, no parentheses in echo text.
#
# GNU Make on Windows does not reliably run recipes through one shell: it
# direct-execs a line with no shell metacharacters, and pointing SHELL at git's
# bash instead resolves to /usr/bin/bash in a different filesystem root, where
# `cd buddi-api` fails. Anything evaluated by make itself (`$(shell)`, `$(if)`,
# `$(error)`) sidesteps the recipe shell entirely, so that is what is used below.

ifneq (,$(wildcard $(API_DIR)/.env))
include $(API_DIR)/.env
endif

# The model is whatever buddi-api/.env says, so the two cannot drift. The default
# here only applies before .env exists, and matches .env.example.
MODEL ?= $(or $(BUDDI_MODEL),qwen3:4b-instruct-2507-q4_K_M)
EMBED_MODEL ?= $(or $(BUDDI_EMBED_MODEL),nomic-embed-text)
TEST_DATABASE_URI ?= postgres://buddi:buddi@localhost:5432/buddi_test?sslmode=disable

# Note the absence of a blanket `export` here.
#
# Including .env makes its values make variables, and exporting all of them
# globally leaks the developer's real configuration into every recipe - including
# `go test`, where the config tests assert *defaults* and therefore fail with
# every value present. `make test` was broken for that reason before this file
# was touched.
#
# Only the API targets need them in the process environment; see below.

.DEFAULT_GOAL := help

.PHONY: help dev dev-watch up infra db ollama obs models model-info bootstrap api api-watch web \
        down clean logs ps build web-build vet fmt fmt-check check test test-cover test-live test-db \
        test-model

# .ONESHELL hands the whole recipe to a single shell invocation. Scoped to help,
# which is a block of bare echo lines, each of which would otherwise be direct-exec'd.
# Scoped deliberately: applied globally it changes how every other recipe runs.
.ONESHELL: help

help:
	@echo Buddi targets
	@echo   make dev          start infra then the API and the web UI
	@echo   make dev-watch    the same, with the API on live reload via air
	@echo   make up           start infrastructure only
	@echo.
	@echo   make models       pull $(MODEL) and $(EMBED_MODEL) into Ollama
	@echo   make model-info   list what Ollama has, and whether $(MODEL) is present
	@echo.
	@echo   make api          run the API, reading $(API_DIR)/.env
	@echo   make web          run the web dev server on http://localhost:5173
	@echo   make down         stop infrastructure, keeping volumes
	@echo   make clean        stop infrastructure and delete volumes
	@echo   make logs         tail infrastructure logs
	@echo.
	@echo   make check        gofmt check, go vet, and the full test suite
	@echo   make test         run the API test suite, no external services
	@echo   make test-cover   run the suite and report coverage per package
	@echo   make test-model   run the model-output tests against a live Ollama
	@echo   make test-live    run the suite against a disposable test database
	@echo.
	@echo   API      http://localhost:$(PORT)
	@echo   Web      http://localhost:5173
	@echo   Jaeger   http://localhost:16686
	@echo   Ollama   http://localhost:11434

dev: bootstrap up
	@echo API on http://localhost:$(PORT)  Web on http://localhost:5173  Jaeger on http://localhost:16686
	$(MAKE) -j2 api web

dev-watch: bootstrap up
	@echo API on http://localhost:$(PORT)  Web on http://localhost:5173  Jaeger on http://localhost:16686
	$(MAKE) -j2 api-watch web

up: db ollama obs

infra: up

db:
	$(COMPOSE) up -d --wait db

ollama:
	$(COMPOSE) --profile llm up -d --wait ollama

obs:
	$(COMPOSE) --profile obs up -d

# Pulls the models the API will ask for. Idempotent: Ollama skips a model it
# already holds, so this is safe to re-run after editing BUDDI_MODEL.
models: ollama
	$(COMPOSE) --profile llm exec -T ollama ollama pull $(MODEL)
	$(COMPOSE) --profile llm exec -T ollama ollama pull $(EMBED_MODEL)

# Reports what the runtime holds alongside the configured model. A missing model is
# the failure that presents as "the AI is broken", so the configured name is printed
# first and the list follows for comparison.
#
# No automated presence test: it needs a search whose tool differs between cmd and
# sh, and a check that silently reports MISSING on Linux is worse than none. Compare
# the two lines by eye, or run `make models`, which is idempotent either way.
model-info:
	@echo configured: $(MODEL)
	@echo embeddings: $(EMBED_MODEL)
	@$(COMPOSE) --profile llm exec -T ollama ollama list

bootstrap:
	cd $(API_DIR) && $(GO) mod download
	cd $(WEB_DIR) && npm install

api: export
	cd $(API_DIR) && $(GO) run ./cmd/server

api-watch: export
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

# The gate to run before committing: format check, vet, then the suite. Order
# matters, since a gofmt failure makes the rest noise.
#
# The gofmt step is evaluated by make rather than by a recipe shell, so it works
# the same on cmd and sh. Recursive assignment means gofmt only runs when this
# target is actually invoked, not on every make call.
# 2>&1 rather than 2>/dev/null: the latter is a POSIX redirect that a Windows
# command prompt cannot parse, and it fails the whole make invocation.
GOFMT_OUT = $(shell cd $(API_DIR) && gofmt -l . 2>&1)

fmt-check:
	$(if $(strip $(GOFMT_OUT)),$(error not gofmt-clean, run: make fmt. files: $(GOFMT_OUT)),echo gofmt clean)

# Composed from the targets above rather than repeating their recipes. As a single
# multi-line recipe this failed on Windows with "The system cannot find the path
# specified" even though the identical commands ran fine as `make vet` and
# `make test`, so depending on the working targets is also the reliable form.
# Order matters: a gofmt failure should stop before vet and test spend their time.
check: fmt-check vet test

test:
	cd $(API_DIR) && $(GO) test ./... -count=1 -timeout 900s

# Per-package coverage, printed inline by the go tool.
#
# Deliberately no post-processing pipeline. A previous version summarised the profile
# with sed/awk/grep, none of which exist on a Windows command prompt, so the target
# worked on one platform and not the other. `-cover` already reports per package, and a
# single total would hide an untested package anyway.
#
# coverage.out is written for `go tool cover -html`, and is gitignored.
test-cover:
	cd $(API_DIR) && $(GO) test ./... -count=1 -timeout 900s -covermode=atomic -cover -coverprofile=coverage.out

# Model-output tests. Skipped unless BUDDI_SMOKE_OLLAMA=1, because they need
# Ollama running and cost about ten seconds per case. This is the tier that
# catches what the unit tier structurally cannot: a fake generator proves the
# validator accepts a correct plan, never that the model produces one.
test-model: ollama
	cd $(API_DIR) && BUDDI_SMOKE_OLLAMA=1 $(GO) test ./internal/application/planner/ \
	  -run TestLive -v -count=1 -timeout 1800s

test-live: export BUDDI_TEST_DATABASE_URI := $(TEST_DATABASE_URI)
test-live: test-db
	cd $(API_DIR) && $(GO) test ./... -count=1 -timeout 900s

test-db: db
	$(COMPOSE) exec -T db psql -U buddi -d buddi -c "SELECT 'CREATE DATABASE buddi_test' WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname='buddi_test')\gexec"