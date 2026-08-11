# gpu-quota-operator

## Overview

An operator that lets namespaces opt in to a cumulative GPU budget over a
recurring calendar billing period. A namespace creates a `GpuQuota` custom
resource declaring `spec.period` (`Daily`/`Weekly`/`Monthly`) and at least
one of `spec.gpuHoursLimit`/`spec.dollarsLimit`. The operator tracks
cumulative GPU-hours (and, if priced, dollars) consumed since the period
started via Prometheus and, once either budget is exceeded, scales down
GPU-consuming workloads (`Deployment`, `StatefulSet`, standalone
`ReplicaSet`, `JobSet`, standalone `Job`, `InferenceService`, and standalone
`Pod`) in that namespace. Enforcement is **never lifted automatically** -
only a human clearing the `gpuquota.io/reset` annotation restores
enforced workloads (see below).

## Architecture

Single controller: `GpuQuotaReconciler` (`controllers/gpuquota-controller.go`)
reconciles one `GpuQuota` object per pass, scoped to `GpuQuota.Namespace`.
There is no cross-namespace state — each `GpuQuota` is fully independent, so
multiple teams' quotas can't interfere with each other.

Reconcile pipeline per pass:
1. If `gpuquota.io/reset` is set to `"true"`, restore any workloads
   in `status.enforcedResources`, clear enforcement state, and remove the
   annotation (`handleManualReset`) - before anything else, so a reset takes
   effect even if usage is still over budget (in which case the same pass
   re-enforces below rather than leaving the namespace uncapped).
2. Compute `periodStart(spec.period, now)` (`controllers/period.go`) -
   calendar-aligned in UTC, never a rolling window.
3. Query the single, cluster-wide Prometheus set via `--prometheus-url` -
   there is no per-namespace override of *which* Prometheus is queried
   (only the PromQL run against it is overridable, via `spec.query`). One
   Prometheus for the whole cluster keeps budgets comparable across
   namespaces.
4. Run a PromQL query (`spec.query` override, or
   `metrics.DefaultGPUHoursQueryTemplate`) for cumulative GPU-hours consumed
   since `periodStart`, broken out by GPU type (`gpuType` label per sample).
5. If `spec.dollarsLimit` is set, price each type via `GPURates.RateFor` -
   an unpriced type present in usage fails the whole reconcile with
   `status.phase: Unknown` rather than silently undercounting cost
   (`computeUsage` in the controller).
6. Compare `gpuHoursUsed`/`dollarsUsed` against `spec.gpuHoursLimit`/
   `spec.dollarsLimit` - **whichever limit is exceeded first** triggers
   enforcement, no grace period (unlike the old instantaneous-threshold
   design this replaced, a monotonically-increasing cumulative counter
   can't "spike" and settle back down on its own, so there's no burst to
   absorb).
7. If over budget, **or already `Enforced`**, call `EnforceNamespace` again
   this pass (catching newly created GPU workloads too) and stay/become
   `Enforced`. Otherwise `Compliant`. There is no code path that transitions
   `Enforced` -> `Compliant` on its own - only `handleManualReset` (step 1)
   does that.
8. Requeue after `spec.checkInterval` (default `5m`) - no per-branch
   `RequeueAfter` tuning is needed anymore, since there's no grace/cooldown
   timer to wake up early for.

### Why enforcement only ever escalates, never auto-resolves

This is the single biggest behavioral difference from a typical
instantaneous-threshold quota controller, and it's a deliberate design
choice (confirmed with the user), not an oversight:

- Cumulative GPU-hours/dollars **only increase** within a period - there's
  no "usage dropped back under the limit" event to key an automatic restore
  off of, the way the old `gpuLimit`-based design could.
- A new period starting doesn't mean the underlying cost overrun was
  addressed - it just means the counter reset. Auto-restoring on period
  rollover would silently let a namespace that blew its budget every single
  month keep doing so forever with no human ever noticing.
- So the only trigger is explicit: `gpuquota.io/reset=true`. This
  also means an admin can use it as an "unlock and see what happens" tool
  after raising a limit - if usage is still over budget post-restore, the
  reconciler re-enforces in the same pass (see pipeline step 1 above),
  rather than requiring a second manual step.

### Authenticating to Prometheus (`metrics/prometheus.go`)

`metrics.Config` has exactly one field, `Address` - auth and TLS trust are
never per-client configuration, they're always-on package behavior, wired
together via a custom `http.RoundTripper` (`bearerTokenRoundTripper`)
rather than anything built into `client_golang/api`, which has no auth
support of its own. Both follow the identical shape: read a fixed,
well-known path; use it if present; silently degrade (not error) if it's
genuinely absent; hard-error only if it's present but broken:

- **Token**: `bearerTokenRoundTripper.RoundTrip` reads
  `serviceAccountTokenFile` (`/var/run/secrets/kubernetes.io/serviceaccount/token`,
  the path Kubernetes auto-mounts a token into every pod at) from disk **on
  every request**, not cached at client-creation time. This matters because
  Kubernetes rotates projected ServiceAccount tokens in place roughly
  hourly - caching the token read at startup would work fine initially and
  then silently start failing with 401s well into the operator's uptime,
  which would be a nasty thing to debug in production. If the file is
  missing (`os.IsNotExist` - e.g. running locally outside a pod, or the
  target Prometheus doesn't require auth), the request goes out with no
  `Authorization` header instead of failing.
- **TLS trust**: `buildTransport` always reads the fixed path
  `serviceCACertFile` (`/etc/gpu-quota-operator/service-ca/service-ca.crt`)
  and, if present, parses it into an `x509.CertPool` used as
  `tls.Config.RootCAs` on a clone of `http.DefaultTransport` (cloned, not
  built from scratch, to keep proxy/env-var support). If the file is
  missing, it silently falls back to the system trust store - so the
  operator still works unmodified against a non-OpenShift Prometheus with a
  public-CA or plain-HTTP endpoint.

In both cases, any *other* read error (permissions, corrupt PEM/JWT) does
fail loudly, since that indicates the mount is broken rather than absent.
There is intentionally no `Config` field, flag, or CR field to override
either path or skip verification/auth - `serviceCACertFile` and
`serviceAccountTokenFile` are `var`s rather than `const`s purely so
`metrics/prometheus_test.go` can point them at temp files per test; neither
is ever reassigned outside tests.

The RBAC binding this requires (OpenShift's built-in `cluster-monitoring-view`
ClusterRole) lives in `manager/bootstrap/monitoring_rolebinding.yaml`
(cluster-scoped, applied by `make bootstrap`); the CA bundle this needs
mounted (via `service.beta.openshift.io/inject-cabundle: "true"` on a
ConfigMap the service-ca operator populates) lives in
`manager/deploy/service-ca-configmap.yaml` (namespace-scoped, applied by
`make deploy`), and `manager/deploy/deployment.yaml` mounts that ConfigMap
at the exact path `serviceCACertFile` expects. None of this is required for
a non-OpenShift Prometheus that doesn't sit behind an auth proxy; those two
manifests are the only OpenShift-specific pieces of the whole operator.

### GPU-hours accounting (`metrics.DefaultGPUHoursQueryTemplate`)

Defaults to **reservation-based** accounting (`kube_pod_resource_request`
joined to node GPU-type labels via `kube_node_labels`), not utilization -
this matches how GPU clusters are typically actually billed (Service
Units/GPU-hours charged for what was *reserved*, regardless of whether it
was fully utilized). This was a deliberate reversal from an earlier
utilization-based design (`DCGM_FI_DEV_GPU_UTIL > 0`) once it became clear
utilization-based accounting would systematically undercount real billed
cost for idle-but-reserved GPUs.

The default query's exact join (`kube_pod_resource_request` × on `node` ×
`kube_node_labels{label_nvidia_com_gpu_product}`) is a **best-effort
starting point, not a guarantee** - it assumes kube-state-metrics is
configured to expose that specific node label (commonly set by NVIDIA's GPU
Operator / node feature discovery), which is cluster-specific configuration
this operator has no way to verify. It also filters on `exported_namespace`
rather than a plain `namespace` label, since the target Prometheus sits
behind an ACM-hub-style federation layer that relabels every metric's own
namespace/pod labels to `exported_namespace`/`exported_pod` (reserving
plain `namespace` for hub-side routing metadata) - if your Prometheus isn't
behind that kind of federation, override `spec.query` with a plain
`namespace` label instead. `spec.query` exists specifically so any of this
is swappable per-namespace without any code or CRD change - e.g. to switch
to DCGM utilization-based accounting instead (see
`samples/team-b-quota-custom-query.yaml` for a worked example query). Any
override must still return one vector sample per GPU type, labeled
`gpuType`, with a value matching a rate key (`a100`/`h100`/`v100`,
case-insensitive) - see `metrics.GPUHoursByType`.

The GPU-hours-per-type computation itself (`avg_over_time(...) *
__RANGE_HOURS__` in `BuildGPUHoursQuery`) is deliberately
resolution-independent: it doesn't assume any particular Prometheus
scrape/recording interval, unlike a `sum_over_time(...) * step/3600`
formulation (which is what a typical showback/billing dashboard uses, since
it usually already knows its own recording-rule interval) would require.

### Enforcement and restore (`enforce/enforce.go`)

Seven workload kinds, four distinct "scale to zero" primitives (ReplicaSet
and StatefulSet reuse Deployment's; standalone Job reuses JobSet's), because
none of them share a common scaling API:

- **Deployment** (typed `appsv1`, vendored): `spec.replicas` set to 0.
  Original value saved in annotation `gpuquota.io/original-replicas`
  before zeroing, since a Deployment default-scaled at creation time to
  something other than 1 needs to restore to that value, not to 1.
- **StatefulSet** (typed `appsv1`, vendored): identical mechanism to
  Deployment (`spec.replicas` / same annotation). No owner-reference check
  is needed here, unlike ReplicaSet/Job below - there's no vanilla
  higher-level controller that owns a StatefulSet.
- **ReplicaSet** (typed `appsv1`, vendored), but only ones with **no
  `ownerReferences`**: same `spec.replicas` / same annotation as Deployment.
  A Deployment-owned ReplicaSet is deliberately skipped here - scaling it
  directly would just get overwritten the next time the Deployment
  controller reconciles it back to the Deployment's desired replica count,
  so the Deployment itself is the correct (and only) thing to act on for
  that case. Only a *standalone* ReplicaSet (created directly, not as a
  Deployment's child) needs this separate code path.
- **JobSet** (`jobset.x-k8s.io/v1alpha2`, **not vendored** — accessed via
  `unstructured.Unstructured` like Kubeflow Notebooks in prior operators in
  this environment): `spec.suspend` set to `true`. JobSet natively supports
  suspend/resume (mirroring `batch/v1.Job`), so this is the one case where
  "scale to zero" is a first-class, reversible API operation rather than an
  approximation.
- **InferenceService** (`serving.kserve.io/v1beta1`, not vendored, also
  unstructured): `minReplicas`/`maxReplicas` set to `0` on every component
  present (`predictor`, `transformer`, `explainer`). Original per-component
  values are marshaled to JSON and stored in annotation
  `gpuquota.io/original-replica-spec` — a `nil` original value
  means the field was unset (not "was 0"), and restore removes the field
  entirely in that case rather than writing back a literal 0. Zeroing both
  `minReplicas` and `maxReplicas` (not just `minReplicas`) matters because
  some KServe autoscalers will scale back up from an idle 0-replica state if
  only the minimum is floored while max stays positive.
- **Job** (typed `batchv1`, vendored), but only ones with **no
  `ownerReferences`**: same `spec.suspend` mechanism as JobSet, reusing the
  `gpuquota.io/original-suspend` annotation. A JobSet's or
  CronJob's child Job is skipped - suspending the JobSet already covers its
  Jobs, and a CronJob's Job is a single scheduled run that isn't yet handled
  (see "Known gaps" below). Only a *standalone* Job (created directly, e.g.
  via `oc create job` or a one-off training script) needs this path.
- **Pod** (typed `corev1`, vendored), but only Pods with **no
  `ownerReferences` at all**: deleted outright via `client.Delete`. A bare
  Pod has no scale-to-zero or suspend field, so deletion is the only lever
  available - and it's the one enforcement action in this operator that is
  NOT reversible. `RestoreNamespace` explicitly no-ops on `Kind: "Pod"`
  entries rather than attempting anything, since there's nothing to restore.
  Pods *with* an owner (a ReplicaSet, a JobSet's Job, a KServe component's
  ReplicaSet, or anything else) are deliberately left alone here: either the
  owner is one of the kinds above and its own enforcement path is the
  correct action, or the owner is a kind this operator doesn't manage, in
  which case deleting the Pod would just cause its controller to immediately
  recreate it - pure churn with no effect on GPU usage.

The same "only act on the unowned/top-level resource" rule applies twice in
this list (ReplicaSet vs. Deployment, Pod vs. everything) precisely so that
enforcement composes correctly through ownership chains instead of every
level of a chain independently thrashing the same workload.

All seven enforcement paths only touch workloads that actually request
`nvidia.com/gpu` (`podTemplateRequestsGPU` for the typed kinds - Deployment,
StatefulSet, ReplicaSet, Job, Pod; a generic recursive `scanForGPURequest`
walk for the unstructured JobSet/InferenceService trees, since GPU requests
live at different nesting depths across API versions/components). Non-GPU
workloads in an over-budget namespace are left alone.

`enforce.go` itself is unaware of budgets/periods entirely - it only knows
"enforce this namespace" / "restore these previously-enforced resources."
All of the period/budget/reset logic lives in the controller; `enforce.go`
is called identically regardless of *why* the controller decided to call it.

Restore is driven entirely by `status.enforcedResources` — the reconciler
doesn't re-derive "what did I touch" by re-scanning the namespace, it replays
exactly the list it recorded at enforcement time. `enforce.mergeEnforced`
dedupes by `kind+name` so repeated enforcement passes (e.g. a new GPU
workload created after the initial enforcement, or the same namespace being
swept again on a later reconcile while still `Enforced`) append rather than
duplicate.

### Optional CRDs

JobSet and InferenceService are optional dependencies — if either CRD isn't
installed in the cluster, `enforceJobSets`/`enforceInferenceServices` detect
the `NoKindMatch`/not-found error from the list call and treat it as "nothing
to enforce" rather than failing the whole reconcile. None of the other five
kinds need this fallback, since `apps/v1`/`batch/v1`/core `v1` are always
present on any Kubernetes cluster.

## Development Commands

- `make manifests` / `make generate` — regenerate `config/crd/*.yaml` and
  `v1alpha1/zz_generated.deepcopy.go` via `controller-gen` (auto-installed to
  `./bin/controller-gen`, pinned version in the Makefile). `manifests` passes
  `crd:allowDangerousTypes=true`, since `GpuQuotaSpec`/`GpuQuotaStatus` use
  `float64` for GPU-hours/dollar amounts and controller-gen otherwise refuses
  float fields (JSON-number precision varies across client languages).
- `make fmt` / `make vet` / `make test`
- `make build` / `make run`
- `make bootstrap` / `make unbootstrap` — **cluster-admin, one-time**: CRD +
  `manager/bootstrap/` (Namespace, ClusterRole, ClusterRoleBindings). `oc
  apply`/`oc delete` re-attempt every object on every invocation regardless
  of whether it changed, so these need elevated RBAC every single time -
  splitting them out means routine `deploy` never needs that RBAC.
- `make build-image` — trigger (or wait on) an in-cluster image build via
  `manager/deploy/build.yaml`'s BuildConfig; run standalone to rebuild
  without a full redeploy
- `make deploy` / `make undeploy` — **routine, no cluster-admin needed**:
  apply `manager/deploy/` (namespace-scoped only), build the image
  in-cluster, then `oc rollout restart` (a plain `apps/v1` Deployment has no
  ImageChange trigger, so a manual restart is required to actually pick up
  a freshly built image)

## Code Structure

- `v1alpha1/` — `GpuQuota` CRD Go types (period/budget spec, cumulative-usage
  status, `ResetAnnotation` constant), groupversion registration, generated
  deepcopy.
- `controllers/gpuquota-controller.go` — the reconcile pipeline: query usage,
  price it, compare to budget, enforce/hold/restore.
- `controllers/period.go` — `periodStart`: calendar-aligned (UTC) period
  boundary calculation for Daily/Weekly/Monthly.
- `controllers/rates.go` — `GPURates`: the operator-wide `$/GPU-hour` table
  (`RateFor` treats a zero rate as "unconfigured," not "free").
- `metrics/prometheus.go` — Prometheus HTTP API client, GPU-hours-by-type
  query building/parsing, default PromQL.
- `enforce/enforce.go` — scale-down/suspend/restore logic per workload kind;
  budget-agnostic (see above).
- `manager/` — two separate kustomize bases (flat layout within each, not
  kubebuilder's split `config/{manager,rbac,default}` — matches the
  convention used elsewhere in this environment), split by required
  privilege rather than by resource type:
  - `manager/bootstrap/` — cluster-scoped, admin-only: Namespace,
    ClusterRole, ClusterRoleBindings. Applied once via `make bootstrap`.
  - `manager/deploy/` — namespace-scoped, routine: ServiceAccount,
    service-ca ConfigMap, Deployment, and `build.yaml` (the
    `ImageStream`/`BuildConfig` that build the operator image in-cluster
    from git source via OpenShift's Build API, rather than local
    `docker build`/`docker push` to an external registry -
    `deployment.yaml`'s image field points directly at the resulting
    ImageStreamTag via the internal registry service DNS
    `image-registry.openshift-image-registry.svc:5000/...`). Applied
    repeatedly via `make deploy`.
- `config/crd/` — generated CRD manifest only.
- `samples/` — example `GpuQuota` CRs.

## Known gaps (not yet implemented)

- **CronJob**: nothing currently suspends a CronJob's *future* scheduled
  runs. Enforcement can suspend/leave alone an already-running child Job,
  but the next scheduled tick will spin up a new one regardless. Would need
  `spec.suspend` on the CronJob itself, mirroring the Job/JobSet mechanism.
- **Kubeflow Training Operator CRDs** (`PyTorchJob`, `TFJob`, `MPIJob`, etc.,
  group `kubeflow.org`): not enforced. Each has a `spec.runPolicy.suspend`
  field, so they'd fit the same unstructured-suspend pattern as JobSet.
- **DaemonSet**: intentionally never enforced. DaemonSets have no replica
  concept and are almost always infra (the NVIDIA device plugin,
  `dcgm-exporter` itself) - scaling/deleting one would break GPU visibility
  or scheduling cluster-wide rather than free up quota.
- **Default query's node-label assumption**: `DefaultGPUHoursQueryTemplate`
  assumes a specific kube-state-metrics node-label allowlist config that
  varies per cluster (see "GPU-hours accounting" above) - it's a
  documented starting point, not something guaranteed to work unmodified
  everywhere.

Note: "blocking new workloads while Enforced" - previously a listed gap in
the old instantaneous-threshold design - is no longer one: since enforcement
now persists and re-sweeps the namespace every reconcile once `Enforced`
(rather than only reacting to a live grace-period violation), any new
GPU-requesting workload created while already enforced gets caught on the
very next reconcile.

## Prerequisites

- Go 1.26+
- `oc`, `kustomize`
- An OpenShift cluster (the Build API used by `manager/deploy/build.yaml`
  is OpenShift-specific, not vanilla Kubernetes)
- A Prometheus/Thanos instance with kube-state-metrics exposing
  `kube_pod_resource_request` and `kube_node_labels` (see "GPU-hours
  accounting" above) - or a `spec.query` override targeting whatever metrics
  your cluster actually has, e.g. this operator's own assumed `dcgm-exporter`
  deployment.
