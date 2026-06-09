## token-control integration test Makefile
## Requires: kind, kubectl, helm, docker on PATH.
##
## Typical workflow:
##   make setup        # create kind cluster, build + load images, install operator
##   make test         # run the Go integration tests
##   make teardown     # delete the kind cluster
##
## To run everything in one shot:
##   make ci

CLUSTER_NAME  ?= tc-integration
KIND_IMAGE    ?= kindest/node:v1.31.0
MANAGER_IMG   ?= token-control:test
FAKE_LLM_IMG  ?= fake-llm:test
HELM_RELEASE  ?= token-control
OPERATOR_NS   ?= token-control-system
REPO_ROOT     := $(shell git rev-parse --show-toplevel)
KUBECONFIG    ?= $(HOME)/.kube/config

.PHONY: all setup cluster build-images load-images install-operator apply-fixtures test teardown ci

all: test

## ci: full lifecycle — setup, test, teardown.
ci: setup test teardown

## setup: create cluster, build/load images, install operator, wait for readiness.
setup: cluster build-images load-images install-operator

## cluster: create a kind cluster (no-op if it already exists).
cluster:
	@kind get clusters 2>/dev/null | grep -q "^$(CLUSTER_NAME)$$" \
		|| kind create cluster --name $(CLUSTER_NAME) --image $(KIND_IMAGE) \
		       --kubeconfig $(KUBECONFIG)
	@kubectl --kubeconfig $(KUBECONFIG) cluster-info --context kind-$(CLUSTER_NAME)

## build-images: build the manager and fake-llm Docker images.
build-images:
	@echo "==> building manager image $(MANAGER_IMG)"
	docker build -t $(MANAGER_IMG) $(REPO_ROOT)
	@echo "==> building fake-llm image $(FAKE_LLM_IMG)"
	docker build -t $(FAKE_LLM_IMG) $(REPO_ROOT)/test/integration/fake_llm

## load-images: load images into the kind cluster.
load-images:
	kind load docker-image $(MANAGER_IMG) --name $(CLUSTER_NAME)
	kind load docker-image $(FAKE_LLM_IMG) --name $(CLUSTER_NAME)

## install-operator: helm install token-control into the cluster.
install-operator:
	kubectl --kubeconfig $(KUBECONFIG) create namespace $(OPERATOR_NS) --dry-run=client -o yaml \
		| kubectl --kubeconfig $(KUBECONFIG) apply -f -
	helm upgrade --install $(HELM_RELEASE) $(REPO_ROOT)/deploy/helm/token-control \
		--namespace $(OPERATOR_NS) \
		--kubeconfig $(KUBECONFIG) \
		--set image.repository=token-control \
		--set image.tag=test \
		--set image.pullPolicy=Never \
		--set controller.leaderElect=false \
		--wait --timeout 120s

## test: run Go integration tests against the current cluster.
test:
	cd $(REPO_ROOT) && \
	  KUBECONFIG=$(KUBECONFIG) \
	  go test -tags integration -v -timeout 5m ./test/integration/...

## teardown: delete the kind cluster.
teardown:
	kind delete cluster --name $(CLUSTER_NAME)

## logs: print manager logs (useful for debugging failures).
logs:
	kubectl --kubeconfig $(KUBECONFIG) -n $(OPERATOR_NS) logs \
		-l app.kubernetes.io/name=token-control --tail=100
