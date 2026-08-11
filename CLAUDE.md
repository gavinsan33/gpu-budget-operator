# gpu-quota-operator

## Overview

An operator that lets namespaces opt in to a GPU quota. A namespace creates a
`GpuQuota` custom resource declaring `spec.gpuLimit` (max concurrently-active
GPUs). The operator watches real GPU utilization for that namespace via
Prometheus/`dcgm-exporter` and, when usage sustains above the limit, scales
down GPU-consuming workloads (`Deployment`, `StatefulSet`, standalone
`ReplicaSet`, `JobSet`, standalone `Job`, `InferenceService`, and standalone
`Pod`) in that namespace until usage falls back under budget.

## Architecture

Single controller: `GpuQuotaReconciler` (`controllers/gpuquota-controller.go`)
reconciles one `GpuQuota` object per pass, scoped to `GpuQuota.Namespace`.
There is no cross-namespace state — each `GpuQuota` is fully independent, so
multiple teams' quotas can't interfere with each other.

Reconcile pipeline per pass:
1. Resolve which Prometheus to query: `spec.prometheusURL` if set, else the
   operator-wide `--default-prometheus-url` flag.
2. Run a PromQL query (`spec.query` override, or `metrics.DefaultQueryTemplate`)
   against that Prometheus to get the current active-GPU count for the
   namespace.
3. Compare against `spec.gpuLimit` and drive a small state machine stored in
   `status.phase`: `Compliant` -> `Violating` -> `Enforced`, and back to
   `Compliant` on recovery.
4. Requeue with `RequeueAfter` tuned to whichever timer is next relevant
   (check interval, remaining grace period, or remaining cooldown) rather
   than polling on a single fixed interval — this is why `Reconcile` computes
   `ctrl.Result{RequeueAfter: ...}` differently in each branch instead of
   using a periodic `SetupWithManager` resync.

### Authenticating to Prometheus (`metrics/prometheus.go`)

The zero-value `metrics.Config` talks plain, unauthenticated HTTP - fine
against a throwaway dev Prometheus, but OpenShift's in-cluster Thanos
Querier/Prometheus sit behind an `oauth-proxy` requiring both a Bearer token
and a trusted TLS certificate. Two things happen inside `metrics.NewClient`
to handle that, wired together via a custom `http.RoundTripper`
(`bearerTokenRoundTripper`) rather than anything built into
`client_golang/api`, which has no auth support of its own:

- **Token**: `Config.TokenFile` is read from disk **on every request**, not
  cached at client-creation time. This matters because Kubernetes rotates
  projected ServiceAccount tokens in place roughly hourly - caching the
  token read at startup would work fine initially and then silently start
  failing with 401s well into the operator's uptime, which would be a nasty
  thing to debug in production.
- **TLS trust**: automatic, not configurable. `buildTransport` always reads
  the fixed path `serviceCACertFile`
  (`/etc/gpu-quota-operator/service-ca/service-ca.crt`) and, if present,
  parses it into an `x509.CertPool` used as `tls.Config.RootCAs` on a clone
  of `http.DefaultTransport` (cloned, not built from scratch, to keep
  proxy/env-var support). If the file is missing (`os.IsNotExist`), it
  silently falls back to the system trust store rather than erroring -
  deliberately, so the operator still works unmodified against a
  non-OpenShift Prometheus with a public-CA or plain-HTTP endpoint. Any
  *other* read error (permissions, corrupt PEM) does fail loudly, since that
  indicates the mount is broken rather than absent. There is intentionally
  no `Config` field, flag, or CR field to override the path or skip
  verification - `serviceCACertFile` is a `var` rather than a `const` purely
  so `metrics/prometheus_test.go` can point it at a temp file per test; it
  is never reassigned outside tests.

The RBAC binding this requires (OpenShift's built-in `cluster-monitoring-view`
ClusterRole) and the CA bundle this needs mounted (via
`service.beta.openshift.io/inject-cabundle: "true"` on a ConfigMap the
service-ca operator populates) live in `manager/monitoring_rolebinding.yaml`
and `manager/service-ca-configmap.yaml` respectively; `manager/deployment.yaml`
mounts that ConfigMap at the exact path `serviceCACertFile` expects. None of
this is required for a non-OpenShift Prometheus that doesn't sit behind an
auth proxy; those two manifests plus `--prometheus-token-file` are the only
OpenShift-specific pieces of the whole operator.

### GPU usage metric

`metrics.DefaultQueryTemplate` assumes `dcgm-exporter` runs with pod-resource
mapping enabled, so `DCGM_FI_DEV_GPU_UTIL` samples carry a `namespace` label.
The default query counts distinct GPU UUIDs reporting `> 0` utilization in
the namespace — i.e. "GPUs actively in use", not raw utilization percentage
or `nvidia.com/gpu` *requests*. This was a deliberate choice: quota against
requests would penalize idle-but-reserved GPUs, and raw utilization percentage
doesn't map cleanly onto "how many GPUs is this namespace using". A `GpuQuota`
can override this entirely via `spec.query`, with the literal string
`__NAMESPACE__` substituted for the target namespace — e.g. to quota on GPU
memory instead of utilization.

### Grace period vs. cooldown period — don't confuse these

- `spec.gracePeriod`: how long usage must stay *continuously* over
  `gpuLimit` before enforcement fires. Absorbs short bursts (e.g. a batch job
  briefly spiking GPU count) without punishing them. Tracked via
  `status.firstViolationTime`, which is cleared the moment usage drops back
  under the limit — the streak does not accumulate across separate
  violations.
- `spec.cooldownPeriod`: once enforcement *has* fired, the minimum time
  before it fires again against the same namespace. This exists because
  after scaling everything to zero, usage will read as compliant on the very
  next check (nothing is running to use GPUs) — without a cooldown separate
  from the grace period, a namespace could get re-enforced (and any
  in-progress restore fought) on every reconcile. `status.lastEnforcementTime`
  tracks this independently of `firstViolationTime`.

### Enforcement and restore (`enforce/enforce.go`)

Seven workload kinds, four distinct "scale to zero" primitives (ReplicaSet
and StatefulSet reuse Deployment's; standalone Job reuses JobSet's), because
none of them share a common scaling API:

- **Deployment** (typed `appsv1`, vendored): `spec.replicas` set to 0.
  Original value saved in annotation `gpuquota.example.com/original-replicas`
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
  `gpuquota.example.com/original-replica-spec` — a `nil` original value
  means the field was unset (not "was 0"), and restore removes the field
  entirely in that case rather than writing back a literal 0. Zeroing both
  `minReplicas` and `maxReplicas` (not just `minReplicas`) matters because
  some KServe autoscalers will scale back up from an idle 0-replica state if
  only the minimum is floored while max stays positive.
- **Job** (typed `batchv1`, vendored), but only ones with **no
  `ownerReferences`**: same `spec.suspend` mechanism as JobSet, reusing the
  `gpuquota.example.com/original-suspend` annotation. A JobSet's or
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
workloads in an over-quota namespace are left alone.

Restore is driven entirely by `status.enforcedResources` — the reconciler
doesn't re-derive "what did I touch" by re-scanning the namespace, it replays
exactly the list it recorded at enforcement time. `enforce.mergeEnforced`
dedupes by `kind+name` so repeated enforcement passes (e.g. a new GPU
workload created after the initial enforcement) append rather than duplicate.

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
  `./bin/controller-gen`, pinned version in the Makefile).
- `make fmt` / `make vet` / `make test`
- `make build` / `make run`
- `make install` / `make uninstall` — CRD only
- `make build-image` — trigger (or wait on) an in-cluster image build via
  manager/build.yaml's BuildConfig; run standalone to rebuild without a full
  redeploy
- `make deploy` / `make undeploy` — apply the `manager/` kustomize base,
  build the image in-cluster, then `oc rollout restart` (a plain
  `apps/v1` Deployment has no ImageChange trigger, so a manual restart is
  required to actually pick up a freshly built image)

## Code Structure

- `v1alpha1/` — `GpuQuota` CRD Go types, groupversion registration, generated
  deepcopy.
- `controllers/gpuquota-controller.go` — the reconcile state machine.
- `metrics/prometheus.go` — Prometheus HTTP API client + default PromQL.
- `enforce/enforce.go` — scale-down/suspend/restore logic per workload kind.
- `manager/` — kustomize base for the operator's own namespace/SA/RBAC/Deployment
  (flat layout, not kubebuilder's split `config/{manager,rbac,default}` —
  matches the convention used elsewhere in this environment).
  `manager/build.yaml` holds the `ImageStream`/`BuildConfig` that build the
  operator image in-cluster from git source (OpenShift's Build API) rather
  than via local `docker build`/`docker push` to an external registry -
  `manager/deployment.yaml`'s image field points directly at the resulting
  ImageStreamTag via the internal registry service DNS
  (`image-registry.openshift-image-registry.svc:5000/...`).
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
- **Blocking new workloads while `Enforced`**: enforcement is purely
  reactive. Nothing stops a namespace from immediately recreating a new bare
  Pod/Job the instant an old one is acted on, which would just cause
  enforce/recreate churn every reconcile. Closing this needs a
  `ValidatingAdmissionPolicy` or admission webhook rejecting new
  GPU-requesting objects while `status.phase == Enforced`, which is a
  materially bigger change (webhook infra, cert management) than adding
  another workload kind.

## Prerequisites

- Go 1.26+
- `oc`, `kustomize`
- An OpenShift cluster (the Build API used by `manager/build.yaml` is
  OpenShift-specific, not vanilla Kubernetes)
- A cluster with `dcgm-exporter` + Prometheus already scraping GPU metrics
  with pod/namespace labels attached
