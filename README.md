# gpu-budget-operator

A Kubernetes operator that enforces per-namespace GPU budgets over a
recurring calendar billing period (daily, weekly, or monthly). Namespaces
opt in by creating a `GpuBudget` custom resource specifying a cumulative
GPU-hours and/or dollar budget for that period. The operator continuously
tracks cumulative GPU-hours consumed (by default via GPU *reservation* time,
matching typical GPU-cluster billing — you're charged for what you
reserved, not what you used), and once either budget is exceeded it scales
GPU-consuming workloads down to zero — Deployments, StatefulSets, and
standalone ReplicaSets are scaled to 0 replicas; JobSets and standalone Jobs
are suspended; InferenceServices have their replica bounds zeroed; and
standalone GPU Pods are deleted outright. Enforcement is never lifted
automatically — not when a new period starts, and not if usage would
otherwise read as compliant — only a human clearing the
`gpubudget.io/reset` annotation restores enforced workloads.

## The `GpuBudget` CRD

`GpuBudget` is a **namespaced** custom resource (group `gpubudget.io`,
version `v1alpha1`, short name `gb`). Creating one in a namespace is the
opt-in mechanism — the operator only monitors and enforces namespaces that
have a `GpuBudget` object; everything else is left alone. There's no
restriction on how many `GpuBudget` objects a namespace can have, but in
practice one per namespace is the intended usage since each is reconciled
independently against the same namespace's workloads.

### spec fields

| Field | Type | Default | Description |
|---|---|---|---|
| `period` | string (`Daily`/`Weekly`/`Monthly`) | *(required)* | The calendar-aligned billing cycle the budget resets on. `Daily` starts at 00:00 UTC; `Weekly` starts Monday 00:00 UTC; `Monthly` starts on the 1st at 00:00 UTC. These are fixed calendar boundaries, **not** rolling windows — `Monthly` always resets on the 1st, never "30 days before now." |
| `gpuHoursLimit` | float | *(one of `gpuHoursLimit`/`dollarsLimit` required)* | Max cumulative GPU-hours, summed across all GPU types, allowed within the current period. |
| `dollarsLimit` | float | *(one of `gpuHoursLimit`/`dollarsLimit` required)* | Max cumulative cost in USD allowed within the current period, computed from GPU-hours-by-type and the cluster-wide `GpuBudgetOperatorConfig`'s `spec.gpuRates`. If both `gpuHoursLimit` and `dollarsLimit` are set, **whichever is exceeded first triggers enforcement.** |
| `query` | string | reservation-based default (see below) | Overrides the PromQL used to compute cumulative GPU-hours consumed since the period started, broken out by GPU type. Must return one sample per GPU type, each labeled `gpuType` with a value matching a rate key (`a100`/`h100`/`v100`, case-insensitive). `__NAMESPACE__`, `__RANGE__`, and `__RANGE_HOURS__` are substituted with the namespace, a PromQL range duration, and that same range as a plain hour count, respectively. Override this to switch accounting methodologies (e.g. DCGM utilization-based instead of reservation-based) without any code or CRD change — see `samples/team-b-budget-custom-query.yaml`. |
| `checkInterval` | duration | `15m` | How often cumulative usage is re-evaluated. Defaults to 15m to match typical GPU-cluster billing granularity - checking more often than billing itself updates doesn't surface anything new. |

### status fields

| Field | Description |
|---|---|
| `currentPeriodStart` | When the current billing period began. |
| `gpuHoursUsed` | Cumulative GPU-hours consumed so far in the current period, summed across all GPU types. |
| `gpuHoursByType` | `gpuHoursUsed` broken down per GPU type. |
| `dollarsUsed` | Cumulative cost in USD so far in the current period. |
| `phase` | One of `Compliant`, `Enforced`, or `Unknown` (set when the cluster-wide `GpuBudgetOperatorConfig` doesn't exist yet, a Prometheus query fails, or `dollarsLimit` is set but usage includes a GPU type with no configured rate). |
| `lastCheckedTime` | When usage was last queried from Prometheus. |
| `lastEnforcementTime` | When enforcement most recently acted against this namespace. |
| `enforcedResources` | List of workloads currently scaled down/suspended by the operator (kind, name, action taken, timestamp), used to drive a manual restore. |
| `conditions` | Standard `metav1.Condition` list; a `Ready` condition reports whether the last Prometheus query (and, if applicable, GPU-type pricing) succeeded. |

### Example

```yaml
apiVersion: gpubudget.io/v1alpha1
kind: GpuBudget
metadata:
  name: gavin-test-budget
  namespace: gavin-test
spec:
  period: Monthly       # resets on the 1st of the month, 00:00 UTC
  gpuHoursLimit: 500    # at most 500 cumulative GPU-hours this month
  checkInterval: 15m
status:
  currentPeriodStart: "2026-08-01T00:00:00Z"
  gpuHoursUsed: 612.4
  gpuHoursByType:
  - gpuType: A100
    gpuHours: 612.4
  phase: Enforced
  lastEnforcementTime: "2026-08-11T09:15:00Z"
  enforcedResources:
  - apiVersion: apps/v1
    kind: Deployment
    name: inference-worker
    action: ScaledToZero
    enforcedAt: "2026-08-11T09:15:00Z"
```

See `samples/` for more examples, including a `dollarsLimit` + custom-query
example.

### Restoring enforced workloads

Enforcement is **never** lifted automatically — not when usage would
otherwise read as compliant, and not when a new billing period starts
(cumulative usage never decreases within a period, and a new period alone
doesn't mean the underlying cost problem was addressed). The only way to
restore enforced workloads is to set the `gpubudget.io/reset`
annotation to `"true"` on the `GpuBudget`:

```
oc annotate gpubudget gavin-test-budget -n gavin-test gpubudget.io/reset=true
```

The operator restores everything in `status.enforcedResources` to its
original state, clears enforcement status, and removes the annotation
itself. If usage is still over budget, it re-enforces on the very same
reconcile — resetting doesn't override the budget, it just gives you a
clean slate to try again (e.g. after raising the limit).

## The `GpuBudgetOperatorConfig` CRD

`GpuBudgetOperatorConfig` is a **cluster-scoped singleton** (group
`gpubudget.io`, version `v1alpha1`, short name `gboc`) holding the two
settings every `GpuBudget` in the cluster shares: the Prometheus endpoint
and the `$/GPU-hour` rate table. The operator only ever reconciles the
object **named `cluster`** — create exactly one, cluster-wide, before
creating any `GpuBudget`:

```yaml
apiVersion: gpubudget.io/v1alpha1
kind: GpuBudgetOperatorConfig
metadata:
  name: cluster
spec:
  prometheusURL: https://thanos-querier.openshift-monitoring.svc:9091
  gpuRates:
  - family: A100
    dollarsPerGPUHour: 1.70
  - family: H100
    dollarsPerGPUHour: 1.10
```

Every `GpuBudget` reconcile reads this object fresh — editing
`spec.prometheusURL` or adding/changing a `spec.gpuRates` entry takes
effect on the very next reconcile, with no operator restart. Until this
object exists, every `GpuBudget` reports `status.phase: Unknown` with
reason `OperatorConfigMissing`. See `samples/gpubudgetoperatorconfig.yaml`.

## Requirements

- Go 1.26+
- An OpenShift cluster — `manager/deploy/build.yaml` builds the operator
  image in-cluster via OpenShift's Build API (`BuildConfig`/`ImageStream`),
  so no local Docker install or external registry is needed, but this
  specific build mechanism won't work on vanilla Kubernetes (swap it for a
  plain `docker build`/`docker push` + an `images:` kustomize override if
  you need that).
- A Prometheus (or Thanos) instance in-cluster. The default accounting query
  assumes kube-state-metrics exposes `kube_pod_resource_request` and
  `kube_node_labels` (with the node's GPU product allow-listed under
  `nvidia.com/gpu.product`), scraped through an ACM-hub-style federation
  layer that relabels each metric's own namespace label to
  `exported_namespace` — **verify this matches your cluster** and override
  `spec.query` if not (e.g. to use DCGM metrics instead, which this
  operator already assumes are present for its own `dcgm-exporter`-fed
  monitoring elsewhere, or to use a plain `namespace` label if you're not
  behind that kind of federation).
- `oc`, `kustomize`
- If targeting OpenShift's built-in monitoring (the `manager/deploy`
  default): see "Prometheus authentication" below for the RBAC/token/TLS
  pieces required.
- If any `GpuBudget` sets `spec.dollarsLimit`: real `$/GPU-hour` rates set in
  the cluster-wide `GpuBudgetOperatorConfig`'s `spec.gpuRates` (one entry per
  GPU family, e.g. `{family: A100, dollarsPerGPUHour: 1.70}`) for every GPU
  family namespaces actually use — see "Install" below.
- Optional: [JobSet](https://github.com/kubernetes-sigs/jobset) and
  [KServe](https://github.com/kserve/kserve) CRDs installed, if you want
  those workload types enforced (Deployment/StatefulSet/ReplicaSet/Job/Pod
  enforcement always works; the operator skips JobSet/InferenceService
  enforcement if their CRDs aren't installed).

## How it works

1. A namespace opts in by creating a `GpuBudget` resource in that namespace
   (see `samples/`), setting `spec.period` and at least one of
   `spec.gpuHoursLimit`/`spec.dollarsLimit`.
2. Each reconcile, the operator queries Prometheus for cumulative GPU-hours
   consumed by the namespace since the period started, broken out by GPU
   type, then (if `spec.dollarsLimit` is set) prices each type via the
   operator's configured rates.
3. The moment either budget is exceeded — no grace period, since a
   cumulative counter can't "spike" the way an instantaneous gauge can —
   the operator enforces: Deployments, StatefulSets, and standalone
   ReplicaSets (not owned by a Deployment) requesting `nvidia.com/gpu` are
   scaled to 0 replicas; JobSets and standalone Jobs (not owned by a JobSet
   or CronJob) are suspended (`spec.suspend: true`); InferenceServices have
   `minReplicas`/`maxReplicas` zeroed on every component
   (predictor/transformer/explainer); and standalone GPU Pods with no owner
   reference are deleted outright (they have no scale/suspend primitive).
   Original values are recorded in annotations for a later manual restore.
4. Once enforced, the operator keeps re-applying enforcement on every
   reconcile (catching any newly created GPU workloads too) regardless of
   what usage now reads — enforcement is never lifted by usage dropping or
   a new period starting. Only the `gpubudget.io/reset` annotation
   restores things (see above).

### Enforcement actions by workload kind

Each workload kind has a different native "scale to zero" primitive, so the
operator uses a different mechanism per kind rather than one generic action:

| Workload kind | "Kill" mechanism | Reversible? | Restore annotation |
|---|---|---|---|
| `Deployment` (`apps/v1`) | `spec.replicas` set to `0` | Yes — original replica count is saved and restored | `gpubudget.io/original-replicas` |
| `StatefulSet` (`apps/v1`) | `spec.replicas` set to `0` | Yes — original replica count is saved and restored | `gpubudget.io/original-replicas` |
| Standalone `ReplicaSet` (`apps/v1`, no owner reference) | `spec.replicas` set to `0` | Yes — original replica count is saved and restored | `gpubudget.io/original-replicas` |
| `JobSet` (`jobset.x-k8s.io/v1alpha2`) | `spec.suspend` set to `true` | Yes — native suspend/resume | `gpubudget.io/original-suspend` |
| Standalone `Job` (`batch/v1`, no owner reference) | `spec.suspend` set to `true` | Yes — native suspend/resume | `gpubudget.io/original-suspend` |
| `InferenceService` (`serving.kserve.io/v1beta1`) | `minReplicas`/`maxReplicas` set to `0` on every present component (`predictor`, `transformer`, `explainer`) | Yes — original per-component min/max (or "was unset") saved and restored | `gpubudget.io/original-replica-spec` |
| Standalone `Pod` (`v1`, no owner reference) | Deleted | **No** — a bare Pod has no scale/suspend primitive, so this is a hard delete | none (nothing to restore) |

Only workloads that actually request `nvidia.com/gpu` (in any container, at
any nesting depth) are touched — non-GPU workloads in an over-budget
namespace are left running. "Owned" resources are always deferred to their
owner: ReplicaSets owned by a Deployment, Jobs owned by a JobSet or CronJob,
and Pods owned by anything (a ReplicaSet, a Job, a KServe component, etc.),
are left alone — only the owning resource is enforced, since acting on the
child directly would either be redundant or get immediately
undone/recreated by its controller. Only truly standalone
ReplicaSets/Jobs/Pods (no `ownerReferences` at all — e.g. created via
`oc create replicaset`/`oc create job`/`oc run` or a bare manifest) are
acted on directly, because that's the only case where nothing else is
enforcing them. As noted above, **none of this is ever undone
automatically** — see "Restoring enforced workloads." See `CLAUDE.md` for
why each mechanism was chosen over the alternatives (e.g. why both
`minReplicas` and `maxReplicas` are zeroed for InferenceServices, not just
`minReplicas`).

See `CLAUDE.md` for the full architecture and non-obvious implementation
details.

## Prometheus authentication

Every `GpuBudget` in the cluster is evaluated against one single,
cluster-wide Prometheus, set via the singleton `GpuBudgetOperatorConfig`'s
`spec.prometheusURL` - there's no per-namespace override. The default
`manager/` manifests target OpenShift's
built-in monitoring stack (Thanos Querier over HTTPS), which requires both a
Bearer token and a trusted TLS certificate - the operator does not get this
"for free" just by running in-cluster. Three pieces make it work together:

1. **RBAC**: `manager/bootstrap/monitoring_rolebinding.yaml` binds the
   operator's ServiceAccount to OpenShift's built-in `cluster-monitoring-view`
   ClusterRole, so its token is actually authorized to read metrics.
2. **Token**: automatic, with no flag or config to override it. Kubernetes
   auto-mounts a Bearer token into every pod at
   `/var/run/secrets/kubernetes.io/serviceaccount/token`; the operator
   always looks for it there and attaches it as `Authorization: Bearer
   <token>` if present, falling back to an unauthenticated request if not
   (e.g. running locally outside a pod, or against a dev Prometheus that
   doesn't require auth).
3. **TLS trust**: also automatic, with no flag or config to override it.
   `manager/deploy/service-ca-configmap.yaml` creates a ConfigMap annotated
   for OpenShift's service-ca operator to inject the cluster's
   service-serving CA into; `manager/deploy/deployment.yaml` mounts it at
   `/etc/gpu-budget-operator/service-ca/service-ca.crt`, and the operator
   always looks for a CA bundle at that fixed path, trusting it if present
   and falling back to the system trust store if not. There's no way to
   disable TLS verification - if you need that for local testing against a
   self-signed dev Prometheus, do it outside the operator (e.g. terminate
   TLS at a local proxy instead).

Not on OpenShift, or using a different Prometheus? Edit `spec.prometheusURL`
on the `GpuBudgetOperatorConfig` to your endpoint, and drop or adjust
`manager/bootstrap/monitoring_rolebinding.yaml`/
`manager/deploy/service-ca-configmap.yaml` to match how that Prometheus
authenticates (or doesn't). If it isn't behind OpenShift's service-ca and
uses a certificate from a public CA (or plain HTTP) and doesn't require
auth, no changes are needed - the fallback to the system trust store and to
an unauthenticated request both happen automatically.

## Install

1. `make manifests` — regenerate the CRDs from the Go types.
2. `make test` — run unit tests.
3. `make bootstrap` — **cluster-admin, one-time**: installs the `GpuBudget`/
   `GpuBudgetOperatorConfig` CRDs plus the Namespace and cluster-scoped RBAC
   (`manager/bootstrap/`). Only needs re-running if the CRDs or that RBAC
   change.
4. `make deploy` — **routine, no cluster-admin needed**: applies the
   namespace-scoped resources in `manager/deploy/` (ServiceAccount,
   ConfigMap, BuildConfig/ImageStream, Deployment), builds the operator
   image in-cluster from git source, and rolls out the result. Safe to run
   repeatedly — see `make build-image` if you just want to rebuild without a
   full redeploy.
5. Apply a `GpuBudgetOperatorConfig` named `cluster` pointing
   `spec.prometheusURL` at your (single, cluster-wide) Prometheus — see
   "Prometheus authentication" above for what else needs to match. If any
   namespace will use `spec.dollarsLimit`, also set real `$/GPU-hour` rates
   in `spec.gpuRates` — start from `samples/gpubudgetoperatorconfig.yaml`
   and edit the placeholder values, then `oc apply -f`. Rates are matched
   against `gpuType` by family prefix (e.g. `A100` matches a `gpuType` of
   `A100-SXM4-80GB`), and an unset family is treated as "not configured,"
   not "free" — a namespace using an unpriced GPU family with
   `dollarsLimit` set will report `status.phase: Unknown` rather than
   silently undercounting cost. Every `GpuBudget` reports `status.phase:
   Unknown` (reason `OperatorConfigMissing`) until this object exists.
6. Apply a `GpuBudget` in any namespace you want monitored, e.g.
   `oc apply -f samples/gavin-test-budget.yaml`.

## Uninstall

```
make undeploy      # namespace-scoped resources only
make unbootstrap   # CRD, Namespace, and cluster-scoped RBAC (cluster-admin)
```

## Install via OLM (alternative)

Instead of `make bootstrap`/`make deploy`, the operator can be installed
through the Operator Lifecycle Manager as a `Subscription` against a
`CatalogSource` containing this operator's bundle (`bundle/`,
`bundle.Dockerfile`). This mirrors `manager/deploy/deployment.yaml`'s
Deployment and `manager/bootstrap/role.yaml`'s ClusterRole/ClusterRoleBinding
inside a `ClusterServiceVersion` — OLM applies both for you as part of the
Subscription, same as `make deploy`/`make bootstrap` do manually.

Two things `make bootstrap` normally covers are **not** part of the OLM
bundle, since OLM's install strategy has no mechanism for either, and must
still be applied once, out of band, before subscribing:
- `manager/deploy/service-ca-configmap.yaml` — the ConfigMap OpenShift's
  service-ca operator injects the cluster's serving CA bundle into.
- `manager/bootstrap/monitoring_rolebinding.yaml` — the ClusterRoleBinding
  to OpenShift's pre-existing `cluster-monitoring-view` ClusterRole (OLM's
  `clusterPermissions` can only grant rules the CSV defines itself, not bind
  to an existing external ClusterRole by name).

```
oc apply -f manager/deploy/service-ca-configmap.yaml
oc apply -f manager/bootstrap/monitoring_rolebinding.yaml
make bundle-validate   # validate bundle/ with operator-sdk before building
make bundle-build      # build the bundle image from bundle.Dockerfile
make bundle-push       # push it - required before the catalog can reference it
```

A bundle image alone isn't something OperatorHub/the OLM console can browse
— it needs to be listed in a **catalog** (a small File-Based Catalog image,
`catalog/`, `catalog.Dockerfile`) registered as a cluster `CatalogSource`.
This is what actually makes the operator show up as a tile in OperatorHub:

```
make catalog-render    # resolve BUNDLE_IMG (must be pushed already) into catalog/gpu-budget-operator/catalog.yaml
make catalog-validate  # validate the rendered catalog with opm - no registry access needed
make catalog-build     # build the catalog image from catalog.Dockerfile
make catalog-push      # push it
make catalog-deploy    # apply the CatalogSource + OperatorGroup + Subscription in manager/olm/
```

`make catalog-deploy` creates the `Subscription` that actually triggers
installation — once the `CatalogSource` shows `READY` (`oc get catalogsource
-n openshift-marketplace gpu-budget-operator-catalog`), the operator appears
under **Operators → OperatorHub** in the console (search "GPU Budget"), and
`oc get csv -n gpu-budget-operator-system` should show
`gpu-budget-operator.v0.1.0` reach `Succeeded`.

Once installed, create a `GpuBudgetOperatorConfig` named `cluster` — the
console's **Installed Operators → GPU Budget Operator → Provided APIs**
tab renders a real form for it (from the CSV's `specDescriptors`), or apply
`samples/gpubudgetoperatorconfig.yaml` directly. Every `GpuBudget` reports
`status.phase: Unknown` until this exists — see "The `GpuBudgetOperatorConfig`
CRD" above.

If `BUNDLE_IMG`/`CATALOG_IMG` (both default to the in-cluster OpenShift
registry, matching `manager/deploy/deployment.yaml`'s image) need to point
somewhere else — an external registry, a different namespace — override them
on the `make` command line, e.g. `make bundle-build BUNDLE_IMG=quay.io/you/gpu-budget-operator-bundle:v0.1.0`.
See `Makefile`'s `##@ OLM Bundle`/`##@ OLM Catalog` sections and `CLAUDE.md`
for why the CSV is hand-maintained rather than generated, and why
`catalog.yaml` is generated-but-gitignored rather than committed like
`bundle/manifests`'s CRD copy.
