# The operator image is built in-cluster from source by the BuildConfig in
# manager/deploy/build.yaml, not locally with docker - see the `build-image`
# and `deploy` targets below. IMAGE_NAMESPACE/IMAGE_NAME must match
# manager/bootstrap/namespace.yaml and manager/deploy/build.yaml's
# BuildConfig/ImageStream name.
IMAGE_NAMESPACE ?= gpu-quota-operator-system
IMAGE_NAME ?= gpu-quota-operator

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# Setting SHELL to bash allows bash commands to be executed by recipes.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate CRD manifests.
	$(CONTROLLER_GEN) crd paths="./v1alpha1/..." output:crd:artifacts:config=config/crd

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./v1alpha1/..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: ## Run tests.
	go test -coverprofile cover.out $$(go list ./... | grep -v '/v1alpha1$$' | grep -v 'github.com/gsanders/gpu-quota-operator$$')

##@ Build

.PHONY: build
build: fmt vet ## Build manager binary.
	go build -o bin/manager main.go

.PHONY: run
run: fmt vet ## Run from your host (requires cluster access).
	go run ./main.go

##@ Deployment
#
# Split into a one-time, cluster-admin `bootstrap` (CRD, Namespace,
# ClusterRole/ClusterRoleBindings - things `oc apply` will re-attempt on
# every invocation regardless of whether they changed, so they need
# elevated RBAC every single time otherwise) and a routine `deploy` that
# only touches namespace-scoped objects a non-admin edit/admin role in
# gpu-quota-operator-system can apply repeatedly. Run `bootstrap` once (or
# whenever the CRD/RBAC itself changes); run `deploy` as often as you like.

.PHONY: bootstrap
bootstrap: manifests ## One-time cluster-admin setup: install the CRD, Namespace, and cluster-scoped RBAC.
	oc apply -f config/crd/
	oc apply -k manager/bootstrap/

.PHONY: unbootstrap
unbootstrap: ## Remove everything `make bootstrap` installed.
	oc delete -k manager/bootstrap/
	oc delete -f config/crd/

.PHONY: build-image
build-image: ## Trigger (or wait on) an in-cluster build of the operator image via manager/deploy/build.yaml's BuildConfig.
	@if oc get build/$(IMAGE_NAME)-1 -n $(IMAGE_NAMESPACE) >/dev/null 2>&1; then \
		echo "initial build already auto-triggered by the BuildConfig's ConfigChange trigger - waiting on it instead of starting a redundant one"; \
		oc wait --for=condition=Complete build/$(IMAGE_NAME)-1 -n $(IMAGE_NAMESPACE) --timeout=300s; \
	else \
		oc start-build $(IMAGE_NAME) -n $(IMAGE_NAMESPACE) --wait; \
	fi

.PHONY: deploy
deploy: ## Routine redeploy: apply namespace-scoped resources, build the image in-cluster, then roll out. Requires `make bootstrap` to have run at least once.
	oc apply -k manager/deploy/
	$(MAKE) build-image
	oc rollout restart deployment/$(IMAGE_NAME)-controller-manager -n $(IMAGE_NAMESPACE)
	oc rollout status deployment/$(IMAGE_NAME)-controller-manager -n $(IMAGE_NAMESPACE) --timeout=120s

.PHONY: undeploy
undeploy: ## Undeploy the namespace-scoped resources. Leaves the CRD/Namespace/RBAC from `make bootstrap` in place - use `make unbootstrap` for those.
	oc delete -k manager/deploy/

##@ Build Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen

## Tool Versions
CONTROLLER_TOOLS_VERSION ?= v0.19.0

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)
