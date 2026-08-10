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
   and InferenceServices have `minReplicas`/`maxReplicas` zeroed on every
   component (predictor/transformer/explainer). Original values are recorded
   in annotations so they can be restored.
4. `spec.cooldownPeriod` limits how often enforcement re-runs against the
   same namespace, to avoid thrashing workloads that are already scaled down.
5. Once usage drops back under the limit, if `spec.autoRestore` is true
   (default), previously enforced workloads are restored to their original
   replica counts / suspend state.

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
