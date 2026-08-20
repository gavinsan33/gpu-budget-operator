# The operator image is built in-cluster from source by the BuildConfig in
# manager/deploy/build.yaml, not locally with docker - see the `build-image`
# and `deploy` targets below. IMAGE_NAMESPACE/IMAGE_NAME must match
# manager/bootstrap/namespace.yaml and manager/deploy/build.yaml's
# BuildConfig/ImageStream name.
IMAGE_NAMESPACE ?= gpu-budget-operator-system
IMAGE_NAME ?= gpu-budget-operator

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
	# allowDangerousTypes=true: GpuBudgetSpec/Status use float64 for GPU-hours/dollar
	# amounts (fractional GPU-hours and cents matter here); controller-gen otherwise
	# refuses float fields since JSON-number precision varies across client languages.
	$(CONTROLLER_GEN) crd:allowDangerousTypes=true paths="./v1alpha1/..." output:crd:artifacts:config=config/crd

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
	go test -coverprofile cover.out $$(go list ./... | grep -v '/v1alpha1$$' | grep -v 'github.com/gsanders/gpu-budget-operator$$')

##@ Build

.PHONY: build
build: fmt vet ## Build manager binary.
	go build -o bin/manager main.go

.PHONY: run
run: fmt vet ## Run from your host (requires cluster access).
	go run ./main.go

##@ Local kind cluster
#
# Not real OpenShift (no Routes/SCCs/Build API) - a kind cluster with real
# kube-state-metrics + Prometheus + a mock DCGM exporter, for exercising the
# reconcile/enforce loop without a live OpenShift cluster or GPUs. Lives in
# the sibling mock-openshift-cluster repo since aibom-webhook-service (and
# any future OpenShift+Prometheus+GPU project here) needs the same thing.
#
# The operator runs as a REAL in-cluster Deployment here, same as on
# OpenShift - just built/loaded differently, since kind has neither
# OpenShift's Build API (manager/deploy/build.yaml) nor its internal
# registry (manager/deploy/deployment.yaml's image field). `kind-image`
# builds locally and loads the result directly into the kind node instead
# of pushing anywhere. manager/deploy/deployment.kind.yaml is the
# corresponding stand-in for deployment.yaml: local image tag, no
# service-ca mount (nothing here populates it, and the operator already
# falls back to the system trust store when that file is simply absent),
# and no --leader-elect (single replica for local testing; there's also no
# RBAC anywhere in this repo for the coordination.k8s.io Lease that flag
# needs - a real gap even on OpenShift, not specific to kind, tracked
# separately). samples/gpubudgetoperatorconfig.kind.yaml is the
# corresponding stand-in for samples/gpubudgetoperatorconfig.yaml, pointing
# spec.prometheusURL at this cluster's own Prometheus Service directly.
#
# Cluster lifecycle (create/delete the kind cluster, KSM/Prometheus/mock
# DCGM, fake GPU node capacity, CRD install) is handled directly in the
# sibling mock-openshift-cluster repo, not here - these targets assume that
# cluster is already up.
#
# kind's kube-scheduler binds its metrics port to 127.0.0.1 by default,
# unreachable from any in-cluster Prometheus - mock-openshift-cluster's
# kind-config.yaml patches this. If you're pointed at a different kind
# cluster, kube_pod_resource_request/_limit (what the default GPU-hours
# query joins against) will silently read as empty.

KIND_CLUSTER_NAME ?= mock-openshift
KIND_IMAGE ?= localhost/gpu-budget-operator:dev
KIND_IMAGE_TAR ?= /tmp/gpu-budget-operator-kind.tar

.PHONY: kind-image
kind-image: ## Build the operator image locally and load it into the kind node - no registry, no OpenShift Build API required.
	podman build -t $(KIND_IMAGE) .
	podman save $(KIND_IMAGE) -o $(KIND_IMAGE_TAR)
	KIND_EXPERIMENTAL_PROVIDER=podman kind load image-archive $(KIND_IMAGE_TAR) --name $(KIND_CLUSTER_NAME)
	rm -f $(KIND_IMAGE_TAR)

.PHONY: kind-deploy
kind-deploy: manifests kind-image ## Build+load the image, then deploy (or redeploy) the operator as a real in-cluster Deployment, including the CRD. Requires the kind cluster to already be up.
	oc apply -f config/crd/
	oc apply -f manager/bootstrap/namespace.yaml
	oc apply -f manager/bootstrap/role.yaml
	oc apply -f manager/bootstrap/role_binding.yaml
	oc apply -f manager/deploy/service_account.yaml
	oc apply -f manager/deploy/deployment.kind.yaml
	oc apply -f samples/gpubudgetoperatorconfig.kind.yaml
	oc -n gpu-budget-operator-system rollout restart deployment/gpu-budget-operator-controller-manager
	oc -n gpu-budget-operator-system rollout status deployment/gpu-budget-operator-controller-manager --timeout=120s

.PHONY: kind-undeploy
kind-undeploy: ## Remove the in-cluster operator Deployment. Leaves the kind cluster, its RBAC/namespace, and the CRD in place.
	oc delete -f manager/deploy/deployment.kind.yaml --ignore-not-found

##@ Deployment
#
# Split into a one-time, cluster-admin `bootstrap` (CRD, Namespace,
# ClusterRole/ClusterRoleBindings - things `oc apply` will re-attempt on
# every invocation regardless of whether they changed, so they need
# elevated RBAC every single time otherwise) and a routine `deploy` that
# only touches namespace-scoped objects a non-admin edit/admin role in
# gpu-budget-operator-system can apply repeatedly. Run `bootstrap` once (or
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

##@ OLM Bundle
#
# bundle/manifests/gpu-budget-operator.clusterserviceversion.yaml is
# hand-maintained, not kustomize/operator-sdk-generated - this repo's
# manager/{bootstrap,deploy} split doesn't match the config/{crd,rbac,manager}
# layout operator-sdk's own `generate kustomize manifests`/`generate bundle`
# commands expect, so the CSV's Deployment/RBAC are kept in sync with
# manager/deploy/deployment.yaml and manager/bootstrap/role.yaml by hand
# instead. The CRD copy in bundle/manifests IS generated - `bundle-manifests`
# just syncs it from config/crd after `make manifests` runs.
#
# The CSV intentionally omits two cluster-admin prerequisites that OLM's
# install strategy has no mechanism for: the service-ca ConfigMap
# (manager/deploy/service-ca-configmap.yaml) and the ClusterRoleBinding to
# OpenShift's pre-existing cluster-monitoring-view ClusterRole
# (manager/bootstrap/monitoring_rolebinding.yaml) - see the CSV's own
# description field for why. Apply both once before subscribing.

BUNDLE_IMG ?= image-registry.openshift-image-registry.svc:5000/gpu-budget-operator-system/gpu-budget-operator-bundle:latest

.PHONY: bundle-manifests
bundle-manifests: manifests ## Sync the generated CRD into bundle/manifests (the CSV itself is hand-maintained - see above).
	cp config/crd/gpubudget.io_gpubudgets.yaml bundle/manifests/gpubudget.io_gpubudgets.yaml

.PHONY: bundle-validate
bundle-validate: bundle-manifests operator-sdk ## Validate the OLM bundle (CSV + CRD + annotations) with operator-sdk.
	$(OPERATOR_SDK) bundle validate ./bundle

.PHONY: bundle-build
bundle-build: bundle-manifests ## Build the bundle image from bundle.Dockerfile.
	podman build -f bundle.Dockerfile -t $(BUNDLE_IMG) .

##@ Build Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
OPERATOR_SDK ?= $(LOCALBIN)/operator-sdk

## Tool Versions
CONTROLLER_TOOLS_VERSION ?= v0.19.0
OPERATOR_SDK_VERSION ?= v1.42.3

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)

.PHONY: operator-sdk
operator-sdk: $(OPERATOR_SDK) ## Download operator-sdk locally if necessary (only used for `make bundle-validate`).
$(OPERATOR_SDK): $(LOCALBIN)
	# `go install` on operator-sdk drags in a cgo dependency on libgpgme
	# (via containers/image, transitively pulled in for image-signature
	# verification code this project never calls) that isn't installed on
	# most dev machines by default - operator-sdk's own docs install the
	# prebuilt release binary for exactly this reason, so this does the same
	# instead of `go install`.
	curl -sSLo $(OPERATOR_SDK) https://github.com/operator-framework/operator-sdk/releases/download/$(OPERATOR_SDK_VERSION)/operator-sdk_$(shell go env GOOS)_$(shell go env GOARCH)
	chmod +x $(OPERATOR_SDK)
