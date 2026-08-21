# gpu-budget-operator

## Overview

An operator that lets namespaces opt in to a cumulative GPU budget over a
recurring calendar billing period. A namespace creates a `GpuBudget` custom
resource declaring `spec.period` (`Daily`/`Weekly`/`Monthly`) and at least
one of `spec.gpuHoursLimit`/`spec.dollarsLimit`. The operator tracks
cumulative GPU-hours (and, if priced, dollars) consumed since the period
started via Prometheus and, once either budget is exceeded, scales down
GPU-consuming workloads (`Deployment`, `StatefulSet`, standalone
`ReplicaSet`, `JobSet`, standalone `Job`, `InferenceService`, and standalone
`Pod`) in that namespace. Enforcement is **never lifted automatically** -
only a human clearing the `gpubudget.io/reset` annotation restores
enforced workloads (see below).

## Architecture

Single controller: `GpuBudgetReconciler` (`controllers/gpubudget-controller.go`)
reconciles one `GpuBudget` object per pass, scoped to `GpuBudget.Namespace`.
There is no cross-namespace state — each `GpuBudget` is fully independent, so
multiple teams' budgets can't interfere with each other.

Reconcile pipeline per pass:
1. If `gpubudget.io/reset` is set to `"true"`, restore any workloads
   in `status.enforcedResources`, clear enforcement state, and remove the
   annotation (`handleManualReset`) - before anything else, so a reset takes
   effect even if usage is still over budget (in which case the same pass
   re-enforces below rather than leaving the namespace uncapped).
2. Compute `periodStart(spec.period, now)` (`controllers/period.go`) -
   calendar-aligned in UTC, never a rolling window.
3. Fetch the singleton `GpuBudgetOperatorConfig` (named
   `gpubudgetv1alpha1.SingletonConfigName`, i.e. `"cluster"`) - if it
   doesn't exist yet, the reconcile fails with `status.phase: Unknown`,
   reason `OperatorConfigMissing`, before ever touching Prometheus. See
   "GpuBudgetOperatorConfig" below.
4. Query the single, cluster-wide Prometheus set via that config's
   `spec.prometheusURL` - there is no per-namespace override of *which*
   Prometheus is queried (only the PromQL run against it is overridable,
   via `spec.query`). One Prometheus for the whole cluster keeps budgets
   comparable across namespaces.
5. Run a PromQL query (`spec.query` override, or
   `metrics.DefaultGPUHoursQueryTemplate`) for cumulative GPU-hours consumed
   since `periodStart`, broken out by GPU type (`gpuType` label per sample).
6. If `spec.dollarsLimit` is set, price each type via `GPURates.RateFor`
   using the config's `spec.gpuRates` - an unpriced type present in usage
   fails the whole reconcile with `status.phase: Unknown` rather than
   silently undercounting cost (`computeUsage` in the controller).
7. Compare `gpuHoursUsed`/`dollarsUsed` against `spec.gpuHoursLimit`/
   `spec.dollarsLimit` - **whichever limit is exceeded first** triggers
   enforcement, no grace period (unlike the old instantaneous-threshold
   design this replaced, a monotonically-increasing cumulative counter
   can't "spike" and settle back down on its own, so there's no burst to
   absorb).
8. If over budget, **or already `Enforced`**, call `EnforceNamespace` again
   this pass (catching newly created GPU workloads too) and stay/become
   `Enforced`. Otherwise `Compliant`. There is no code path that transitions
   `Enforced` -> `Compliant` on its own - only `handleManualReset` (step 1)
   does that.
9. Requeue after `spec.checkInterval` (default `15m`, matching typical GPU-cluster billing granularity) - no per-branch
   `RequeueAfter` tuning is needed anymore, since there's no grace/cooldown
   timer to wake up early for.

### Why enforcement only ever escalates, never auto-resolves

This is the single biggest behavioral difference from a typical
instantaneous-threshold budget controller, and it's a deliberate design
choice (confirmed with the user), not an oversight:

- Cumulative GPU-hours/dollars **only increase** within a period - there's
  no "usage dropped back under the limit" event to key an automatic restore
  off of, the way the old `gpuLimit`-based design could.
- A new period starting doesn't mean the underlying cost overrun was
  addressed - it just means the counter reset. Auto-restoring on period
  rollover would silently let a namespace that blew its budget every single
  month keep doing so forever with no human ever noticing.
- So the only trigger is explicit: `gpubudget.io/reset=true`. This
  also means an admin can use it as an "unlock and see what happens" tool
  after raising a limit - if usage is still over budget post-restore, the
  reconciler re-enforces in the same pass (see pipeline step 1 above),
  rather than requiring a second manual step.

### GpuBudgetOperatorConfig (`v1alpha1/gpubudgetoperatorconfig_types.go`)

`spec.prometheusURL` and `spec.gpuRates` used to be `--prometheus-url`/
`--gpu-rate=<family>=<usd>` flags, read once into `GpuBudgetReconciler`
fields at `main.go` startup. They're a `GpuBudgetOperatorConfig` CR instead
now, fetched fresh via `r.Get` at the top of every `Reconcile` call (right
after the reset-annotation check, before anything Prometheus-related) -
this was a deliberate architecture change, not a refactor for its own sake:

- **Why**: this operator is meant to be installable via OLM (see the OLM
  bundle sections below), and OLM's install wizard has no mechanism for
  prompting for custom config at install time - the only way to change a
  value baked into the CSV's Deployment spec (like a flag) is to edit the
  CSV itself and rebuild/repush the bundle and catalog images. A plain CRD,
  by contrast, gets a real auto-generated form in the OpenShift console
  (from `specDescriptors` - see the CSV) and can be created/edited by any
  cluster-admin with no image rebuild at all, matching how "day 2 config"
  is conventionally handled by operators that need cluster-specific
  settings after install (see `GpuBudget`'s own `specDescriptors` for the
  same pattern, already in place before this CRD existed).
- **Singleton, not namespaced**: named `gpubudgetv1alpha1.SingletonConfigName`
  (`"cluster"`) by convention - mirroring OpenShift's own
  `config.openshift.io` singletons (`Infrastructure`, `Network`, etc.). The
  reconciler always looks up that exact name; there's no mechanism to pick
  among several `GpuBudgetOperatorConfig` objects, since these are
  cluster-wide settings (matching `GpuBudget`'s own "one Prometheus for the
  whole cluster" design), not something more than one could reasonably
  exist for. `+kubebuilder:resource:scope=Cluster` enforces this can't be
  namespaced at all, even if scope alone can't enforce the name.
- **Missing config fails loudly, not silently**: if `r.Get` returns
  not-found, `Reconcile` returns `markFailed(..., "OperatorConfigMissing",
  ...)` before ever calling `prometheusClient` - a `GpuBudget` created
  before any `GpuBudgetOperatorConfig` exists reports `status.phase:
  Unknown` with a message telling the user exactly what to create, rather
  than some deeper Prometheus-dial error implying a network/auth problem.
- **Live reconfiguration, not just install-time**: `prometheusClient`
  caches by URL (`promClientURL` alongside `promClient`) and rebuilds only
  when the fetched URL differs from the cached one - unlike the old
  build-once-at-startup client, `spec.prometheusURL` can change at any
  time. `SetupWithManager` also `Watches` `GpuBudgetOperatorConfig` and
  enqueues every existing `GpuBudget` on any change
  (`enqueueAllGpuBudgets`), so an edited Prometheus URL or a newly added
  GPU rate takes effect on the next reconcile rather than waiting up to
  each `GpuBudget`'s own `spec.checkInterval` to notice.
- **RBAC**: cluster-scoped `get;list;watch` only (no `create`/`update`/
  `delete` - the operator only ever reads this, a human manages it),
  granted the same way as every other permission this operator needs: a
  `ClusterRole`/`ClusterRoleBinding` bound to its ServiceAccount cluster-wide
  (`manager/bootstrap/role.yaml`), since the reconciler already runs with
  cluster-wide `GpuBudget` watch/enforce permissions regardless.

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
  `serviceCACertFile` (`/etc/gpu-budget-operator/service-ca/service-ca.crt`)
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
ClusterRole) and the ConfigMap the CA bundle needs mounted (annotated
`service.beta.openshift.io/inject-cabundle: "true"` for the service-ca
operator to populate) are both self-provisioned by the operator itself at
startup - see `controllers.EnsurePrerequisites` and its own "OLM bundle"
architecture note below for why, rather than living as static manifests a
human applies. `manager/deploy/deployment.yaml` mounts that ConfigMap (as
`optional: true`, so the pod starts fine before that first-boot
self-provisioning completes) at the exact path `serviceCACertFile` expects.
None of this is required for a non-OpenShift Prometheus that doesn't sit
behind an auth proxy - both self-provisioned objects just end up inert.

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
`samples/team-b-budget-custom-query.yaml` for a worked example query). Any
override must still return one vector sample per GPU type, labeled
`gpuType`, with a value matching a rate key (`a100`/`h100`/`v100`,
case-insensitive) - see `metrics.GPUHoursByType`.

The GPU-hours-per-type computation itself is `sum_over_time(X[__RANGE__:5m])
* (5.0/60)`, NOT `avg_over_time(X[__RANGE__:5m]) * __RANGE_HOURS__` (an
earlier version used the latter). Averaging the reservation over the whole
period and multiplying by the period's full nominal length silently assumes
the series existed for the entire period - it doesn't whenever a workload
(or Prometheus itself) started mid-period, so a pod that only reserved a
GPU for the last hour of a 300-hour month got billed ~300 GPU-hours instead
of ~1. `sum_over_time * step-hours` instead accumulates only the time
actually covered by samples. The `:5m` step here is this query's own
subquery resampling interval, fixed by us and evaluated the same way
regardless of the target Prometheus's native scrape/recording interval -
so this is no less resolution-independent than the range-average form was;
it never needed to know the target's own recording-rule interval, either.

### Enforcement and restore (`enforce/enforce.go`)

Seven workload kinds, four distinct "scale to zero" primitives (ReplicaSet
and StatefulSet reuse Deployment's; standalone Job reuses JobSet's), because
none of them share a common scaling API:

- **Deployment** (typed `appsv1`, vendored): `spec.replicas` set to 0.
  Original value saved in annotation `gpubudget.io/original-replicas`
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
  `gpubudget.io/original-replica-spec` — a `nil` original value
  means the field was unset (not "was 0"), and restore removes the field
  entirely in that case rather than writing back a literal 0. Zeroing both
  `minReplicas` and `maxReplicas` (not just `minReplicas`) matters because
  some KServe autoscalers will scale back up from an idle 0-replica state if
  only the minimum is floored while max stays positive.
- **Job** (typed `batchv1`, vendored), but only ones with **no
  `ownerReferences`**: same `spec.suspend` mechanism as JobSet, reusing the
  `gpubudget.io/original-suspend` annotation. A JobSet's or
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

**Fixed bug: orphaned enforcement records.** Because restore trusts
`status.enforcedResources` as its *only* source of truth, and a resource is
only ever added to that list on the one reconcile where it's *newly*
enforced (later reconciles correctly re-affirm zero via the resource's own
`gpubudget.io/original-*` annotation without re-adding a tracking entry —
see `enforceInferenceServices`'s `newlyCaptured`/`alreadyCaptured` comment),
there used to be a narrow window where that one tracking write could be
lost while the live enforcement action (zeroing + annotating the workload)
had already happened. Two distinct ways this occurred, both observed live
against the mock cluster in `../mock-openshift-cluster`:

1. `EnforceNamespace` processes resource kinds in a fixed order (Deployments
   → StatefulSets → ReplicaSets → JobSets → Jobs → InferenceServices →
   Pods) and used to bail out on the very first kind's error, discarding
   the `[]EnforcedResource` it had already accumulated for every
   *earlier*-in-order kind that succeeded in the same call. E.g., a
   transient conflict enforcing an InferenceService (common, since KServe's
   own controller writes to the same object's status concurrently) would
   drop the fact that a StatefulSet and ReplicaSet processed just before it
   in that same pass had already been zeroed and annotated.
2. Even when `EnforceNamespace` returned cleanly, `Reconcile` persisted the
   merged `EnforcedResources` via a single unretried
   `r.Status().Update(ctx, &gb)` at the very end. A resourceVersion
   conflict there (e.g. a user concurrently `kubectl annotate`/`patch`-ing
   the same `GpuBudget`) discarded the entire reconcile's status
   changes — including any freshly-captured `EnforcedResources` entries —
   even though the underlying workloads were already live-enforced.

Once either happened, the resource was orphaned permanently: nothing
re-derives `status.enforcedResources` from the live
`gpubudget.io/original-*` annotations, so `gpubudget.io/reset` would report
success (nothing in its list to restore) while the workload stayed scaled
to zero forever, with no error ever surfaced again.

Fixed by: `EnforceNamespace` now runs every resource kind regardless of an
earlier kind's failure and always returns everything it acted on so far
(joining any per-kind errors via `errors.Join` rather than returning on the
first one); `Reconcile` merges that result into
`gb.Status.EnforcedResources` *before* checking whether enforcement
returned an error, instead of discarding the merge on error; and the final
status write goes through `persistStatus`, which retries on conflict by
re-fetching the latest object and reapplying the same (already fully
computed, conflict-independent) desired status rather than giving up after
one attempt. See `TestEnforceNamespace_ContinuesPastOneKindsFailure`,
`TestReconcile_PartialEnforcementFailurePersistsSuccessfulEntries`, and
`TestReconcile_StatusConflictIsRetriedNotDropped` for regression coverage
of both failure modes.

**Follow-up fix: `RestoreNamespace` head-of-line blocking.** The same
"stop at the first failure" mistake existed on the restore side:
`RestoreNamespace` iterated `status.enforcedResources` in order and
returned immediately on the first entry's error, so anything listed
*after* a persistently-failing entry (not just a transiently-failing one)
never got restored on that attempt - or on any later retry, since the
caller always replays the same list in the same order. It now runs every
entry regardless of an earlier one's failure and joins any errors via
`errors.Join`, so restoring nine-out-of-ten resources' worth of a
namespace no longer depends on that tenth one's problem going away first.
See `TestRestoreNamespace_ContinuesPastOneEntrysFailure` and
`TestRestoreNamespace_RetryAfterPartialFailureFinishesTheRest`.

**Follow-up fix: sticky-enforcement signal.** `Reconcile`'s "is this
namespace already enforced" check used to be `gb.Status.Phase ==
PhaseEnforced`. `markFailed` (used for `PrometheusClientError`/
`MetricsQueryFailed`/`UnpricedGPUType`) sets `Phase = PhaseUnknown`
without touching `EnforcedResources` at all - and returns *before*
`Reconcile` ever reaches the enforcement decision that reconcile. So an
already-enforced namespace hitting one of those transient failures (e.g.
a workload using a not-yet-priced GPU type shows up alongside existing
usage) would flip to `Unknown`, and if the failure cleared on a later
reconcile with usage happening to read as back under budget, it would
fall straight through to the `else` branch and become `Compliant` -
never restoring the still-zeroed workload, since only a real
`gpubudget.io/reset` does that, and clearing nothing from
`EnforcedResources` either (the `else` branch never touches it), leaving
status internally inconsistent (`Compliant` with a stale non-empty
`EnforcedResources`). The check is now `len(gb.Status.EnforcedResources)
> 0`, which `markFailed` never perturbs - true ground truth for "there's
enforcement in effect that hasn't gone through reset yet." See
`TestReconcile_UnpricedGPUTypeWhileEnforcedStaysStickyUntilReset`.

### Optional CRDs

JobSet and InferenceService are optional dependencies — if either CRD isn't
installed in the cluster, `enforceJobSets`/`enforceInferenceServices` detect
the `NoKindMatch`/not-found error from the list call and treat it as "nothing
to enforce" rather than failing the whole reconcile. None of the other five
kinds need this fallback, since `apps/v1`/`batch/v1`/core `v1` are always
present on any Kubernetes cluster.

### OLM bundle (`bundle/`, `bundle.Dockerfile`)

`bundle/manifests/gpu-budget-operator.clusterserviceversion.yaml` is
**hand-maintained, not generated** - operator-sdk's usual `generate
kustomize manifests`/`generate bundle` commands expect a kubebuilder-style
`config/{crd,rbac,manager,manifests}` split, which this repo deliberately
doesn't use (see `manager/` below for why). Instead, the CSV's
`install.spec.deployments` and `install.spec.clusterPermissions` are kept in
sync by hand with `manager/deploy/deployment.yaml` and
`manager/bootstrap/role.yaml` whenever either changes. Only the CRD copy in
`bundle/manifests` is generated - `make bundle-manifests` (a dependency of
`make bundle-validate`/`make bundle-build`) just copies both `make
manifests`-generated CRDs (`gpubudget.io_gpubudgets.yaml` and
`gpubudget.io_gpubudgetoperatorconfigs.yaml`) over it.

The CSV's Deployment spec deliberately carries no `--prometheus-url`/
`--gpu-rate` args (main.go doesn't accept them - see the
`GpuBudgetOperatorConfig` architecture section above) - `clusterPermissions`
instead grants `get;list;watch` on `gpubudgetoperatorconfigs`, and
`customresourcedefinitions.owned` lists both CRDs so the console renders a
form for `GpuBudgetOperatorConfig` too (from its own `specDescriptors`),
the same way it already does for `GpuBudget`.

The permissions in `manager/bootstrap/role.yaml` are namespace-scoped
resource kinds (Deployments, Jobs, Pods, etc.) but bound cluster-wide via a
`ClusterRoleBinding`, since a single operator instance reconciles `GpuBudget`
objects across every namespace (`main.go`'s manager has no `Namespace`
restriction) - this maps directly onto the CSV's `clusterPermissions` (not
`permissions`, which OLM scopes to one namespace) and `installModes:
AllNamespaces: true`.

Two things OLM's install strategy has no mechanism to create itself are
**not** representable in the CSV, and used to require a human to `oc
apply` a manifest before subscribing - the operator now self-provisions
both instead (`controllers.EnsurePrerequisites`, called once at startup):
- The `gpu-budget-operator-service-ca` ConfigMap - OLM's install strategy
  only knows how to create Deployments/(Cluster)Roles/(Cluster)RoleBindings/
  ServiceAccounts, not arbitrary ConfigMaps.
- The `gpu-budget-operator-monitoring-view` ClusterRoleBinding - OLM's
  `clusterPermissions` only lets a CSV grant rules it defines itself as a
  new ClusterRole; it has no way to bind the operator's ServiceAccount to a
  pre-existing external ClusterRole like OpenShift's built-in
  `cluster-monitoring-view` by name.

`EnsurePrerequisites` only ever calls `Create` (treating `AlreadyExists` as
success - no `Get`/`Update` anywhere), so only `create` is granted for
either object; there was an earlier draft of this that also granted
`get`/`update` "for idempotency" before anything actually called them -
removed once audited, since a permission a real request never exercises is
pure attack surface with no offsetting benefit. `create` can't be
restricted by `resourceNames` (the object doesn't exist yet when the check
runs), so both rules are otherwise as tight as Kubernetes' RBAC model
allows:
- `clusterrolebindings: [create]` is unavoidably unscoped, but in practice
  constrained by the `bind` rule below: creating a ClusterRoleBinding to
  any ClusterRole *other* than `cluster-monitoring-view` would fail
  Kubernetes' own RBAC escalation check anyway, since this ServiceAccount
  doesn't already possess that other role's permissions.
- `clusterroles: [bind]` is pinned via `resourceNames:
  [cluster-monitoring-view]` - required at all because Kubernetes' RBAC
  escalation check otherwise refuses to let a ServiceAccount create a
  ClusterRoleBinding to a ClusterRole whose permissions it doesn't already
  have itself.
- `configmaps: [create]` is **namespace-scoped**, not part of
  `clusterPermissions` at all: `manager/deploy/configmap_role.yaml` (a
  `Role`+`RoleBinding` in `gpu-budget-operator-system` only, applied via
  `make deploy`) for the manual path, and the CSV's `install.spec.permissions`
  (which OLM binds only within whatever namespace the Subscription installs
  into) for the OLM path. `EnsurePrerequisites` only ever creates this
  ConfigMap in the operator's own namespace - granting it via the
  cluster-scoped `ClusterRole` in `manager/bootstrap/role.yaml` instead
  would let the ServiceAccount create a ConfigMap in *any* namespace on the
  cluster, which is broader than what's actually used.

The Deployment's ConfigMap volume is `optional: true` specifically to break
the chicken-and-egg problem this creates: the pod has to actually start
(and run `EnsurePrerequisites`) before the ConfigMap it mounts can exist,
so a non-optional mount would leave every fresh install stuck in
`ContainerCreating` forever. `main.go` also does a bounded (60s) poll after
creating the ConfigMap for the CA file to actually land on disk before
starting the manager - `metrics.WaitForServiceCA` - since
`GpuBudgetReconciler.prometheusClient` caches its underlying `http.Client`
per URL and would otherwise never notice the CA becoming available shortly
after the first reconcile already built one against the system trust store
without it; see that function's own comment for the residual risk (a
timeout still falls back to the system trust store rather than blocking
startup indefinitely).

The CSV's container image is a fixed reference to the in-cluster OpenShift
registry image built by `manager/deploy/build.yaml`'s BuildConfig
(`image-registry.openshift-image-registry.svc:5000/gpu-budget-operator-system/gpu-budget-operator:latest`),
matching `manager/deploy/deployment.yaml` exactly - unlike that Deployment,
though, nothing re-triggers a rollout when the underlying ImageStreamTag is
rebuilt, since OLM (not `make deploy`) owns this Deployment once installed
via a Subscription; bumping the image requires a new CSV version (a
`replaces`/`skipRange` upgrade), not just a new build.

### OLM catalog (`catalog/`, `catalog.Dockerfile`, `manager/olm/`)

A bundle image (above) is one operator's install manifests, but OLM's
console/OperatorHub doesn't browse bundle images directly - it reads a
**catalog**: a small File-Based Catalog (FBC), the modern replacement for
the deprecated sqlite-index format, describing which package/channel/bundle
combinations exist. `catalog/gpu-budget-operator/basic-template.yaml` is the
hand-maintained input (an `olm.template.basic` document: one `olm.package`,
one `olm.channel` with a single `gpu-budget-operator.v0.1.0` entry, one
`olm.bundle` pointing at `BUNDLE_IMG_PLACEHOLDER`); `make catalog-render`
substitutes the real `BUNDLE_IMG` and runs `opm alpha render-template basic`
to produce `catalog/gpu-budget-operator/catalog.yaml` - **not** committed
(gitignored), unlike `bundle/manifests`'s CRD copy, because rendering
requires `opm` to actually resolve and inline `BUNDLE_IMG`'s manifests, so a
committed copy would either need a real registry push at commit time or go
stale/wrong (a blank `image:` field) the moment anyone re-ran it locally
without one - there's no version of this file that's simultaneously
accurate and independent of a live registry, the way `config/crd`'s output
is.

`catalog.Dockerfile` was generated once via `opm generate dockerfile
catalog --base-image quay.io/operator-framework/opm:v1.73.0` (pin matching
the Makefile's `OPM_VERSION`) and checked in as-is - it's boilerplate
`opm serve` wiring, not something this project's own logic touches, so
there's nothing to hand-maintain the way the CSV is.

`manager/olm/` holds the three objects that make a rendered-and-pushed
catalog actually installable: `catalogsource.yaml` (registers the catalog
image in `openshift-marketplace`, the namespace every other `CatalogSource`
on an OpenShift cluster - including Red Hat's own - lives in),
`operatorgroup.yaml` (no `spec.targetNamespaces`, matching the CSV's
`AllNamespaces` install mode), and `subscription.yaml` (what actually
triggers OLM to create the InstallPlan). These are deliberately **not**
folded into `manager/bootstrap/` or `manager/deploy/` - they're a third,
mutually-exclusive install path (`make catalog-deploy`), not something a
`make bootstrap`/`make deploy` install also needs.

## Development Commands

- `make manifests` / `make generate` — regenerate `config/crd/*.yaml` and
  `v1alpha1/zz_generated.deepcopy.go` via `controller-gen` (auto-installed to
  `./bin/controller-gen`, pinned version in the Makefile). `manifests` passes
  `crd:allowDangerousTypes=true`, since `GpuBudgetSpec`/`GpuBudgetStatus` use
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

- `v1alpha1/` — `GpuBudget` CRD Go types (period/budget spec, cumulative-usage
  status, `ResetAnnotation` constant) and `GpuBudgetOperatorConfig` CRD Go
  types (`SingletonConfigName` constant), groupversion registration,
  generated deepcopy.
- `controllers/gpubudget-controller.go` — the reconcile pipeline: query usage,
  price it, compare to budget, enforce/hold/restore.
- `controllers/period.go` — `periodStart`: calendar-aligned (UTC) period
  boundary calculation for Daily/Weekly/Monthly.
- `controllers/rates.go` — `GPURates`: a `map[string]float64` from GPU
  family (e.g. `"A100"`) to its `$/GPU-hour` rate, built fresh each
  reconcile via `ratesFromSpec` from the singleton `GpuBudgetOperatorConfig`'s
  `spec.gpuRates` (see the `GpuBudgetOperatorConfig` architecture section
  above for why this moved off command-line flags). Adding a rate for a new
  GPU family, or changing an existing one, is purely a CR edit - never a
  code change or rebuild. `RateFor` matches by family
  *prefix* (longest match wins), not exact equality: the default query's
  `gpuType` is typically a full SKU like `"A100-SXM4-80GB"` (from a node's
  product label), not the bare family name a rate is configured under - an
  exact-match `RateFor` would leave every such SKU permanently unpriced
  regardless of the family rate being set (a real bug found running
  against a mock cluster; see `TestGPURates_RateFor_MatchesFullSKUsByFamilyPrefix`
  and `TestReconcile_PricesFullSKUGpuTypeNotJustBareFamilyName`). A zero/
  absent rate is treated as "unconfigured," not "free."
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
- `config/crd/` — generated CRD manifests only.
- `samples/` — example `GpuBudget` and `GpuBudgetOperatorConfig` CRs.

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
  or scheduling cluster-wide rather than free up budget.
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
