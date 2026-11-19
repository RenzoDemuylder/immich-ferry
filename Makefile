VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMMICH_VERSION ?= v3.4.1

.PHONY: build test vet fmt-check lint clean \
	image image-multiarch chart-lint \
	e2e-up e2e-down e2e e2e-kind demo

## --- core (owner: W0) ---
build:
	go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/immich-ferry ./cmd/immich-ferry

test:
	go test -race -count=1 ./...

vet:
	go vet ./...

fmt-check:
	@test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }

lint: fmt-check vet

clean:
	rm -rf bin
## --- end core ---

## --- image + chart (owner: W1c) ---
IMAGE ?= immich-ferry
# The Helm chart lives in github.com/RenzoDemuylder/helm-charts (charts/immich-ferry); by default a checkout next to this repo.
HELM_CHARTS ?= ../helm-charts
CHART ?= $(HELM_CHARTS)/charts/immich-ferry
CHART_APP_VERSION ?= $(shell sed -n 's/^appVersion:[[:space:]]*"\{0,1\}\([^"[:space:]]*\).*/\1/p' $(CHART)/Chart.yaml 2>/dev/null)

image:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) -t $(IMAGE):dev -t $(IMAGE):latest $(if $(CHART_APP_VERSION),-t $(IMAGE):$(CHART_APP_VERSION)) .

# Needs a buildx builder with the docker-container driver (or the containerd image store): docker buildx create --use
image-multiarch:
	docker buildx build --platform linux/amd64,linux/arm64 --build-arg VERSION=$(VERSION) \
		-t $(IMAGE):$(VERSION) $(if $(PUSH),--push,--output type=cacheonly) .

# Static checks of the chart in the helm-charts checkout (lint, helm-unittest, kubeconform, policies run there: make test)
chart-lint:
	@test -f $(CHART)/Chart.yaml || { echo "chart not found at $(CHART): clone github.com/RenzoDemuylder/helm-charts next to this repo or set HELM_CHARTS"; exit 1; }
	helm dependency build $(CHART) >/dev/null
	@for f in $(CHART)/ci/*-values.yaml; do echo ">> helm lint $$f"; helm lint --strict $(CHART) -f $$f || exit 1; done
## --- end image + chart ---

## --- e2e (owner: W1b/W3) ---
E2E_COMPOSE = docker compose -f test/e2e/compose.yaml
E2E_SERVICES = immich-a db-a redis-a immich-b db-b redis-b

e2e-up:
	-$(E2E_COMPOSE) pull --ignore-buildable -q
	$(E2E_COMPOSE) up -d --wait --wait-timeout 600 $(E2E_SERVICES)
	go run ./test/e2e/cmd/bootstrap

e2e-down:
	$(E2E_COMPOSE) --profile daemon down -v --remove-orphans

e2e:
	@$(MAKE) e2e-up || { $(MAKE) e2e-down; exit 1; }; \
	set -a; . ./.e2e/env; set +a; \
	go test -tags e2e -count=1 -timeout 45m ./test/e2e/...; rc=$$?; \
	if [ "$(E2E_KEEP)" != "1" ]; then $(MAKE) e2e-down; fi; \
	exit $$rc

e2e-kind:
	@$(MAKE) e2e-up || { $(MAKE) e2e-down; exit 1; }; \
	set -a; . ./.e2e/env; set +a; \
	go test -tags e2e_kind -count=1 -timeout 30m -run Kind ./test/e2e/...; rc=$$?; \
	if [ "$(E2E_KEEP)" != "1" ]; then $(MAKE) e2e-down; fi; \
	exit $$rc

# Starts the two Immich instances, creates an album with a few photos on A and an empty one on B, syncs once.
demo: e2e-up
	@set -a; . ./.e2e/env; set +a; \
	ids=$$(go run ./test/e2e/cmd/demo) && eval "$$ids" && export SYNC_A_ALBUM_ID SYNC_B_ALBUM_ID && \
	IMMICH_A_API_KEY=$$E2E_A_KEY IMMICH_B_API_KEY=$$E2E_B_KEY SYNC_PAIR_NAME=demo \
	SYNC_STATE_DIR=.e2e/demo-state LOG_FORMAT=text go run ./cmd/immich-ferry once && \
	echo "" && echo "Immich A: $$IMMICH_A_URL" && echo "Immich B: $$IMMICH_B_URL" && \
	echo "Login (both): $$E2E_ADMIN_EMAIL / $$E2E_ADMIN_PASSWORD" && \
	echo "Album 'immich-ferry demo' now exists on both. Stop with: make e2e-down"
## --- end e2e ---
