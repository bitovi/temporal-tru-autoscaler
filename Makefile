# Makefile for temporal-tru-autoscaler

# Image settings — override IMAGE_REPO and VERSION at build time.
IMAGE_REPO ?= ghcr.io/bitovi/temporal-tru-autoscaler
VERSION    ?= 0.1.0
IMG        ?= $(IMAGE_REPO):$(VERSION)

# Go settings
GOFLAGS ?=
GOBIN   ?= $(shell go env GOPATH)/bin

# Kubebuilder / controller-gen tool path (installed locally when needed)
CONTROLLER_GEN ?= $(GOBIN)/controller-gen

.PHONY: all
all: build

## -----------------------------------------------------------
## Build
## -----------------------------------------------------------

.PHONY: build
build: ## Build the manager binary
	go build $(GOFLAGS) -o bin/manager ./cmd/main.go

.PHONY: run
run: ## Run the controller locally against the current kube context
	go run ./cmd/main.go

.PHONY: fmt
fmt: ## Run go fmt
	go fmt ./...

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: lint
lint: ## Run golangci-lint (must be installed separately)
	golangci-lint run ./...

.PHONY: test
test: ## Run unit tests
	go test ./... -v

## -----------------------------------------------------------
## Docker
## -----------------------------------------------------------

.PHONY: docker-build
docker-build: ## Build the controller Docker image
	docker build -t $(IMG) .

.PHONY: docker-push
docker-push: ## Push the controller Docker image
	docker push $(IMG)

## -----------------------------------------------------------
## CRD / manifests (requires controller-gen)
## -----------------------------------------------------------

.PHONY: controller-gen
controller-gen: ## Download controller-gen if needed
	@which controller-gen >/dev/null 2>&1 || \
	  go install sigs.k8s.io/controller-tools/cmd/controller-gen@latest

.PHONY: generate
generate: controller-gen ## Generate DeepCopy methods
	$(CONTROLLER_GEN) object:headerFile="" paths="./..."

.PHONY: manifests
manifests: controller-gen ## Generate CRD manifests
	$(CONTROLLER_GEN) crd paths="./..." output:crd:artifacts:config=config/crd

## -----------------------------------------------------------
## Helm
## -----------------------------------------------------------

.PHONY: helm-lint
helm-lint: ## Lint the Helm chart
	helm lint charts/temporal-tru-autoscaler

.PHONY: helm-template
helm-template: ## Render the Helm chart templates
	helm template temporal-tru-autoscaler charts/temporal-tru-autoscaler \
	  --namespace temporal-autoscaler

.PHONY: helm-install
helm-install: ## Install the Helm chart to the current kube context
	helm upgrade --install temporal-tru-autoscaler charts/temporal-tru-autoscaler \
	  --namespace temporal-autoscaler \
	  --create-namespace

.PHONY: helm-uninstall
helm-uninstall: ## Uninstall the Helm chart
	helm uninstall temporal-tru-autoscaler --namespace temporal-autoscaler

## -----------------------------------------------------------
## Deploy (kubectl / raw manifests)
## -----------------------------------------------------------

.PHONY: install
install: ## Install the CRD into the cluster
	kubectl apply -f config/crd/temporal.bitovi.com_temporaltruautoscalers.yaml

.PHONY: uninstall
uninstall: ## Remove the CRD from the cluster
	kubectl delete -f config/crd/temporal.bitovi.com_temporaltruautoscalers.yaml --ignore-not-found

.PHONY: deploy
deploy: ## Deploy the controller with the raw RBAC manifests
	kubectl apply -f config/rbac/
	kubectl apply -f config/crd/

.PHONY: undeploy
undeploy: ## Remove the controller deployment
	kubectl delete -f config/rbac/ --ignore-not-found
	kubectl delete -f config/crd/ --ignore-not-found

## -----------------------------------------------------------
## Help
## -----------------------------------------------------------

.PHONY: help
help: ## Display this help message
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n\nTargets:\n"} /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
