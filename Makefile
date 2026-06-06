# token-control Makefile
#
# This project intentionally requires no host Go or Helm toolchain: all Go tooling runs in
# the official golang image via hack/with-go.sh, and all Helm commands run in the alpine/helm
# image via hack/with-helm.sh. Only Docker is required on the host.

# Image URL to use for building/pushing the manager image.
IMG ?= ghcr.io/token-control/token-control:latest
# Version of controller-gen used to generate CRDs and deepcopy code.
CONTROLLER_GEN_VERSION ?= v0.16.5
# controller-gen CRD output and the Helm release defaults used by helm targets.
CHART_DIR ?= deploy/helm/token-control
CRD_DIR ?= $(CHART_DIR)/crds
RELEASE ?= token-control
NAMESPACE ?= token-control-system

WITH_GO := ./hack/with-go.sh
WITH_HELM := ./hack/with-helm.sh
CONTROLLER_GEN := go run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

.DEFAULT_GOAL := help

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Development

.PHONY: generate
generate: ## Generate DeepCopy methods (zz_generated.deepcopy.go).
	$(WITH_GO) $(CONTROLLER_GEN) object paths=./api/...

.PHONY: manifests
manifests: ## Generate CRDs into the Helm chart's crds/ directory.
	$(WITH_GO) $(CONTROLLER_GEN) crd paths=./api/... output:crd:dir=$(CRD_DIR)

.PHONY: fmt
fmt: ## Run go fmt.
	$(WITH_GO) go fmt ./...

.PHONY: vet
vet: ## Run go vet.
	$(WITH_GO) go vet ./...

.PHONY: test
test: ## Run unit tests.
	$(WITH_GO) go test ./... -coverprofile cover.out

.PHONY: tidy
tidy: ## Tidy go.mod/go.sum.
	$(WITH_GO) go mod tidy

##@ Build

.PHONY: build
build: ## Build the manager binary into bin/manager.
	$(WITH_GO) go build -o bin/manager ./cmd/main.go

.PHONY: docker-build
docker-build: ## Build the manager container image ($(IMG)).
	docker build -t $(IMG) .

.PHONY: docker-push
docker-push: ## Push the manager container image ($(IMG)).
	docker push $(IMG)

##@ Helm

.PHONY: helm-lint
helm-lint: ## Lint the Helm chart.
	$(WITH_HELM) lint $(CHART_DIR)

.PHONY: helm-template
helm-template: ## Render the Helm chart to stdout.
	$(WITH_HELM) template $(RELEASE) $(CHART_DIR) --namespace $(NAMESPACE)

.PHONY: helm-package
helm-package: ## Package the Helm chart into a .tgz.
	$(WITH_HELM) package $(CHART_DIR) --destination dist

##@ Deploy (requires a configured kubeconfig on the host)

.PHONY: install
install: ## Install/upgrade the chart into the cluster (helm must reach your kube API).
	$(WITH_HELM) upgrade --install $(RELEASE) $(CHART_DIR) \
		--namespace $(NAMESPACE) --create-namespace --set image.repository=$(IMG)

.PHONY: uninstall
uninstall: ## Uninstall the chart from the cluster.
	$(WITH_HELM) uninstall $(RELEASE) --namespace $(NAMESPACE)
