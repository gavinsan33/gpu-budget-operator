# gpu-quota-operator

A Kubernetes operator that enforces per-namespace GPU quotas. Namespaces opt
in by creating a `GpuQuota` custom resource specifying how many GPUs they're
allowed to use concurrently. The operator continuously evaluates real GPU
utilization for the namespace via Prometheus (fed by `dcgm-exporter`), and
when usage exceeds the configured limit it scales GPU-consuming workloads
down to zero — Deployments are scaled to 0 replicas, JobSets are suspended,
and InferenceServices have their replica bounds zeroed. Once usage drops back
under quota, enforced workloads are automatically restored (unless
`autoRestore: false`).

## The `GpuQuota` CRD

`GpuQuota` is a **namespaced** custom resource (group `gpuquota.example.com`,
version `v1alpha1`, short name `gq`). Creating one in a namespace is the
opt-in mechanism — the operator only monitors and enforces namespaces that
have a `GpuQuota` object; everything else is left alone. There's no
restriction on how many `GpuQuota` objects a namespace can have, but in
practice one per namespace is the intended usage since each is reconciled
independently against the same namespace's workloads.

### spec fields

| Field | Type | Default | Description |
|---|---|---|---|
| `gpuLimit` | int32 | *(required)* | Max number of GPUs allowed to be concurrently active (reporting non-zero utilization) in this namespace. This is a count of GPUs in use, not a raw utilization percentage and not `nvidia.com/gpu` *requests* — a namespace can request more GPUs than its limit as long as it isn't actively running on more than `gpuLimit` at once. |
| `prometheusURL` | string | operator's `--default-prometheus-url` flag | Overrides which Prometheus/Thanos endpoint is queried for this namespace's usage. Use this when different teams' metrics live in different Prometheus instances. |
| `query` | string | `metrics.DefaultQueryTemplate` (counts distinct GPU UUIDs with `DCGM_FI_DEV_GPU_UTIL > 0`) | Overrides the PromQL used to compute current usage. The literal string `__NAMESPACE__` is substituted with the namespace name before the query runs. The query must return a single vector/scalar sample. Use this to quota on something other than active-GPU-count, e.g. GPU memory (`DCGM_FI_DEV_FB_USED`). |
| `checkInterval` | duration | `1m` | How often usage is re-evaluated while the namespace is compliant. |
| `gracePeriod` | duration | `2m` | How long usage must stay *continuously* over `gpuLimit` before enforcement fires. Absorbs short bursts (e.g. a batch job briefly spiking GPU count) without punishing them. The streak resets to zero the moment usage dips back under the limit, even for one check. |
| `cooldownPeriod` | duration | `5m` | Minimum time between successive enforcement passes against the same namespace. Prevents re-enforcing (and fighting an in-progress restore) on every reconcile once workloads are already scaled down. |
| `autoRestore` | bool | `true` | Whether workloads this controller scaled down/suspended are automatically restored to their original state once usage falls back under `gpuLimit`. Set to `false` if you'd rather have a human review and manually restore capacity after an enforcement event. |

### status fields

| Field | Description |
|---|---|
| `currentUsage` | Most recently observed active-GPU count for the namespace. |
| `phase` | One of `Compliant`, `Violating`, `Enforced`, or `Unknown` (set when a Prometheus query fails). |
| `firstViolationTime` | When the current over-quota streak began; cleared once compliant again. |
| `lastCheckedTime` | When usage was last queried from Prometheus. |
| `lastEnforcementTime` | When enforcement last ran against this namespace; drives the cooldown window. |
| `enforcedResources` | List of workloads currently scaled down/suspended by the operator (kind, name, action taken, timestamp), used to drive restoration. Cleared once everything is restored. |
| `conditions` | Standard `metav1.Condition` list; a `Ready` condition reports whether the last Prometheus query succeeded. |

### Example

```yaml
apiVersion: gpuquota.example.com/v1alpha1
kind: GpuQuota
metadata:
  name: team-a-quota
  namespace: team-a
spec:
  gpuLimit: 4          # at most 4 GPUs active at once in this namespace
  checkInterval: 1m    # re-check usage every minute while compliant
  gracePeriod: 2m       # must be over quota for 2 continuous minutes before enforcing
  cooldownPeriod: 5m    # wait at least 5 minutes between enforcement passes
  autoRestore: true     # scale enforced workloads back up once usage recovers
status:
  currentUsage: 6
  phase: Enforced
  firstViolationTime: "2026-08-10T14:00:00Z"
  lastEnforcementTime: "2026-08-10T14:02:00Z"
  enforcedResources:
  - apiVersion: apps/v1
    kind: Deployment
    name: inference-worker
    action: ScaledToZero
    enforcedAt: "2026-08-10T14:02:00Z"
```

See `samples/` for more examples, including a per-namespace Prometheus/query
override.

## Requirements

- Go 1.26+
- A Prometheus (or Thanos) instance in-cluster that scrapes `dcgm-exporter`
  with pod-resource mapping enabled, so `DCGM_FI_DEV_GPU_UTIL` samples carry
  `namespace`/`pod` labels.
- `kubectl`, `kustomize`
- Optional: [JobSet](https://github.com/kubernetes-sigs/jobset) and
  [KServe](https://github.com/kserve/kserve) CRDs installed, if you want
  those workload types enforced (Deployments always work; the operator skips
  JobSet/InferenceService enforcement if their CRDs aren't installed).

## How it works

1. A namespace opts in by creating a `GpuQuota` resource in that namespace
   (see `samples/`), setting `spec.gpuLimit` to the max number of GPUs it may
   use concurrently.
2. Each reconcile, the operator queries Prometheus for the number of GPUs
   currently reporting non-zero utilization in that namespace.
3. If usage stays over `spec.gpuLimit` for longer than `spec.gracePeriod`,
   the operator enforces the quota: Deployments requesting `nvidia.com/gpu`
   are scaled to 0 replicas, JobSets are suspended (`spec.suspend: true`),
   InferenceServices have `minReplicas`/`maxReplicas` zeroed on every
   component (predictor/transformer/explainer), and standalone GPU Pods with
   no owner reference are deleted outright (they have no scale/suspend
   primitive). Original values are recorded in annotations so everything
   except deleted Pods can be restored.
4. `spec.cooldownPeriod` limits how often enforcement re-runs against the
   same namespace, to avoid thrashing workloads that are already scaled down.
5. Once usage drops back under the limit, if `spec.autoRestore` is true
   (default), previously enforced workloads are restored to their original
   replica counts / suspend state.

### Enforcement actions by workload kind

Each workload kind has a different native "scale to zero" primitive, so the
operator uses a different mechanism per kind rather than one generic action:

| Workload kind | "Kill" mechanism | Reversible? | Restore annotation |
|---|---|---|---|
| `Deployment` (`apps/v1`) | `spec.replicas` set to `0` | Yes — original replica count is saved and restored | `gpuquota.example.com/original-replicas` |
| `JobSet` (`jobset.x-k8s.io/v1alpha2`) | `spec.suspend` set to `true` | Yes — native suspend/resume, same as `batch/v1.Job` | `gpuquota.example.com/original-suspend` |
| `InferenceService` (`serving.kserve.io/v1beta1`) | `minReplicas`/`maxReplicas` set to `0` on every present component (`predictor`, `transformer`, `explainer`) | Yes — original per-component min/max (or "was unset") saved and restored | `gpuquota.example.com/original-replica-spec` |
| Standalone `Pod` (`v1`, no owner reference) | Deleted | **No** — a bare Pod has no scale/suspend primitive, so this is a hard delete | none (nothing to restore) |

Only workloads that actually request `nvidia.com/gpu` (in any container, at
any nesting depth) are touched — non-GPU workloads in an over-quota namespace
are left running. Pods owned by a Deployment/ReplicaSet/JobSet/anything else
are left alone too — only the owning resource is enforced, since deleting an
owned Pod directly would just get it recreated by its controller. Only truly
standalone Pods (no `ownerReferences` at all — e.g. created via `kubectl run`
or a bare manifest) are deleted directly, because that's the only case where
nothing else is enforcing them. Every other action above is undone
automatically once the namespace's usage drops back under `gpuLimit`, unless
`spec.autoRestore: false` — Pod deletion is the one exception, since a
deleted Pod can't be un-deleted. See `CLAUDE.md` for why each mechanism was
chosen over the alternatives (e.g. why both `minReplicas` and `maxReplicas`
are zeroed for InferenceServices, not just `minReplicas`).

See `CLAUDE.md` for the full architecture and non-obvious implementation
details.

## Install

1. Point the operator at your Prometheus by editing
   `manager/deployment.yaml`'s `--default-prometheus-url` flag (or override
   per-namespace via `spec.prometheusURL` on individual `GpuQuota`s).
2. `make manifests` — regenerate the CRD from the Go types.
3. `make test` — run unit tests.
4. `make docker-build IMAGE_NAME=... QUAY_USER=...` and
   `make docker-push IMAGE_NAME=... QUAY_USER=...` — build and push the
   operator image.
5. `make install` — install the `GpuQuota` CRD.
6. `make deploy IMG=<your image>` — deploy the operator.
7. Apply a `GpuQuota` in any namespace you want monitored, e.g.
   `kubectl apply -f samples/team-a-quota.yaml`.

## Uninstall

```
make undeploy
make uninstall
```
