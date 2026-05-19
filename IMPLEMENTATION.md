# Temporal TRU Autoscaler — Implementation Documentation

## Overview

A Kubernetes operator that automatically scales **Temporal Resource Units (TRU)** for a Temporal Cloud namespace based on live **Actions Per Second (APS)** utilization. It is designed to feel like a Kubernetes `HorizontalPodAutoscaler` — declare bounds and thresholds in a custom resource, and the controller handles the rest.

---

## Architecture

```
┌─────────────────────────────────────────────────────────────────────────────┐
│  Kubernetes Cluster                                                          │
│                                                                              │
│  ┌────────────────────────────────────────────────────────────────────────┐  │
│  │  temporal-autoscaler namespace                                         │  │
│  │                                                                        │  │
│  │  ┌──────────────────────────────────┐   ┌──────────────────────────┐  │  │
│  │  │  TemporalTRUAutoscaler (CR)       │   │  Secret                  │  │  │
│  │  │  ─────────────────────────────   │   │  ──────────────────────  │  │  │
│  │  │  temporalNamespace: ns.account   │   │  apiKey: <api-key>       │  │  │
│  │  │  minTRU: 2  /  maxTRU: 12        │   │  accountId: <account>    │  │  │
│  │  │  scaleUpThreshold: 70%           │   └──────────┬───────────────┘  │  │
│  │  │  scaleDownThreshold: 70%         │              │ mounted at        │  │
│  │  │  scaleUpCooldown: 5m             │              │ reconcile time    │  │
│  │  │  scaleDownCooldown: 1h           │              │                   │  │
│  │  │  ─────────────────────────────   │              │                   │  │
│  │  │  status:                         │              │                   │  │
│  │  │    currentTRU: 4                 │◄─────────────┤                   │  │
│  │  │    lastScaleTime: ...            │  status      │                   │  │
│  │  │    lastScaleDirection: Up        │  patch       │                   │  │
│  │  │    conditions: [Ready, ...]      │              │                   │  │
│  │  └────────────────┬─────────────────┘              │                   │  │
│  │                   │ watches                        │                   │  │
│  │                   ▼                                │                   │  │
│  │  ┌─────────────────────────────────────────────────┴──────────────┐  │  │
│  │  │  Controller (Deployment)                                        │  │  │
│  │  │  ─────────────────────────────────────────────────────────────  │  │  │
│  │  │  cmd/main.go  →  controller-runtime Manager                     │  │  │
│  │  │  internal/controller/temporaltruautoscaler_controller.go        │  │  │
│  │  │                                                                  │  │  │
│  │  │  Every 30s per CR:                                               │  │  │
│  │  │    1. Read Secret → API key                                      │  │  │
│  │  │    2. GET  /cloud/namespaces/{ns}  → current TRU                 │  │  │
│  │  │    3. GET  metrics.temporal.io    → current APS                  │  │  │
│  │  │    4. Compute utilization %                                      │  │  │
│  │  │    5. Evaluate scale-up / scale-down                             │  │  │
│  │  │    6. POST /cloud/namespaces/{ns} → set new TRU (if needed)      │  │  │
│  │  │    7. Emit Kubernetes Event                                      │  │  │
│  │  │    8. Patch CR status                                            │  │  │
│  │  └───────────────────────┬────────────────────────────────────────┘  │  │
│  └──────────────────────────┼────────────────────────────────────────────┘  │
└─────────────────────────────┼───────────────────────────────────────────────┘
                              │
              ┌───────────────┴──────────────────────────────┐
              │                                              │
              ▼                                              ▼
 ┌────────────────────────┐               ┌─────────────────────────────────┐
 │  Temporal Cloud         │               │  Temporal Cloud                  │
 │  Cloud Ops API          │               │  OpenMetrics v1 Endpoint         │
 │  ─────────────────────  │               │  ─────────────────────────────   │
 │  saas-api.tmprl.cloud   │               │  metrics.temporal.io             │
 │                         │               │                                  │
 │  GET  /cloud/namespaces │               │  metric: temporal_cloud_v1_      │
 │    /{namespace}         │               │    total_action_count            │
 │  → current TRU value    │               │  → current APS for namespace     │
 │                         │               │                                  │
 │  POST /cloud/namespaces │               │  metric: temporal_cloud_v1_      │
 │    /{namespace}         │               │    action_limit                  │
 │  body: capacity_spec    │               │  → configured APS ceiling        │
 │    .provisioned.value   │               └─────────────────────────────────┘
 │  + resource_version     │
 └────────────────────────┘
```

---

## Reconcile Loop Detail

```
Reconcile triggered (every 30s, or on CR change)
│
├─ Read credentials Secret
│    apiKey, accountId
│
├─ GET saas-api.tmprl.cloud/cloud/namespaces/{ns}
│    └─ Extract: spec.capacity_spec.provisioned.value → currentTRU
│                resource_version  (saved for update)
│
├─ GET metrics.temporal.io/prometheus/metrics?namespace={ns}
│    └─ Extract: temporal_cloud_v1_total_action_count → currentAPS
│
├─ utilization = currentAPS / (currentTRU × 500) × 100
│
├─ utilization > scaleUpThreshold?
│    ├─ currentTRU >= maxTRU?        → Event: ScaleBlockedBounds
│    ├─ within scaleUpCooldown?      → Event: ScaleBlockedCooldown
│    └─ else → newTRU = NextValidTRU(currentTRU)
│              POST /cloud/namespaces/{ns}
│              → Event: ScaledUp
│
├─ utilization < scaleDownThreshold?
│    ├─ currentTRU <= minTRU?        → Event: ScaleBlockedBounds
│    ├─ within scaleDownCooldown?    → Event: ScaleBlockedCooldown
│    └─ else → newTRU = PrevValidTRU(currentTRU)
│              POST /cloud/namespaces/{ns}
│              → Event: ScaledDown
│
└─ Patch CR status (currentTRU, lastScaleTime, lastScaleDirection, conditions)
```

---

## Project Structure

```
.
├── api/v1alpha1/
│   ├── groupversion_info.go              # scheme registration
│   ├── temporaltruautoscaler_types.go    # CRD Go types (Spec, Status, conditions)
│   └── zz_generated.deepcopy.go         # DeepCopy implementations
│
├── cmd/
│   └── main.go                          # controller-runtime Manager entrypoint
│
├── internal/
│   ├── controller/
│   │   └── temporaltruautoscaler_controller.go  # reconcile loop
│   └── temporal/
│       └── client.go                    # Temporal Cloud REST + metrics client
│
├── config/
│   ├── crd/
│   │   └── temporal.bitovi.com_temporaltruautoscalers.yaml
│   ├── rbac/
│   │   ├── service_account.yaml
│   │   ├── cluster_role.yaml
│   │   └── cluster_role_binding.yaml
│   └── samples/
│       └── temporal_v1alpha1_temporaltruautoscaler.yaml
│
├── charts/temporal-tru-autoscaler/
│   ├── Chart.yaml
│   ├── values.yaml
│   └── templates/
│       ├── _helpers.tpl
│       ├── crd.yaml
│       ├── deployment.yaml
│       ├── serviceaccount.yaml
│       ├── clusterrole.yaml
│       ├── clusterrolebinding.yaml
│       ├── service.yaml
│       └── servicemonitor.yaml          # optional, for prometheus-operator
│
├── Dockerfile
├── Makefile
└── SPEC.md
```

---

## CRD Reference

**API group:** `temporal.bitovi.com/v1alpha1`  
**Kind:** `TemporalTRUAutoscaler`  
**Short name:** `ttautoscaler`  
**Scope:** Namespaced (one CR per Temporal Cloud namespace)

### Spec

| Field | Type | Default | Description |
|---|---|---|---|
| `temporalNamespace` | string | required | Temporal Cloud namespace. Format: `name.accountId` |
| `credentialsSecretRef.name` | string | required | Name of the Secret containing `apiKey` (and optionally `accountId`) |
| `minTRU` | int | required | Lower bound — controller never scales below this |
| `maxTRU` | int | required | Upper bound — controller never scales above this |
| `scaleUpThreshold` | int | `70` | APS utilization % of current tier ceiling that triggers scale-up |
| `scaleDownThreshold` | int | `70` | APS utilization % below which scale-down is considered |
| `scaleUpCooldown` | duration | `5m` | Minimum time between scale-up actions |
| `scaleDownCooldown` | duration | `1h` | Minimum time between scale-down actions |

### Status

| Field | Description |
|---|---|
| `currentTRU` | TRU level currently reported by Temporal Cloud |
| `lastScaleTime` | Timestamp of the most recent scale action |
| `lastScaleDirection` | `Up` or `Down` |
| `conditions` | Standard Kubernetes conditions: `Ready`, `Scaling`, `AtMinimum`, `AtMaximum` |

### kubectl columns

```
NAME                       NAMESPACE                  MINTRU   MAXTRU   CURRENTTRU   LASTSCALE   DIRECTION
my-namespace-autoscaler    my-temporal-namespace...   2        12       4            10m ago     Up
```

---

## Scaling Logic

### TRU increments

Temporal Cloud only accepts TRU values in specific increments: **2, 3, 4, 6, 8, 10, 12**.

The controller uses `NextValidTRU` / `PrevValidTRU` helpers to always land on a valid value — e.g. scaling up from 4 jumps to 6, not 5.

### APS ceiling per TRU

Each TRU supports **500 APS**. A namespace provisioned at 4 TRU has a ceiling of 2,000 APS.

```
utilization = currentAPS / (currentTRU × 500) × 100
```

### Cooldown rationale

| Direction | Default | Why |
|---|---|---|
| Scale-up | 5m | React quickly to load spikes |
| Scale-down | 1h | Temporal Cloud bills for the full hour after any TRU change — scaling down too soon wastes money |

Cooldown is measured from `status.lastScaleTime`, which persists across controller restarts.

---

## Temporal Cloud API Facts

These were verified against the live Temporal Cloud documentation and proto definitions on 2026-05-19.

| Item | Value |
|---|---|
| APS per TRU | **500** |
| Valid TRU values | **2, 3, 4, 6, 8, 10, 12** |
| Metrics endpoint | `https://metrics.temporal.io/prometheus/metrics` (v1 OpenMetrics) |
| APS metric | `temporal_cloud_v1_total_action_count` |
| APS limit metric | `temporal_cloud_v1_action_limit` |
| Read namespace | `GET https://saas-api.tmprl.cloud/cloud/namespaces/{namespace}` |
| Update TRU | `POST https://saas-api.tmprl.cloud/cloud/namespaces/{namespace}` |
| Update payload | `spec.capacity_spec.provisioned.value` (float TRU) + `resource_version` |

> **Note:** The v0 PromQL metrics endpoint (`saas-api.tmprl.cloud/prometheus/metrics`) was deprecated 2026-04-02 and will be disabled 2026-10-05. This implementation uses the v1 endpoint.

---

## Installation

### 1. Install the Helm chart

```bash
helm install temporal-tru-autoscaler ./charts/temporal-tru-autoscaler \
  --namespace temporal-autoscaler \
  --create-namespace
```

### 2. Create the credentials Secret

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: temporal-cloud-api-key
  namespace: temporal-autoscaler
type: Opaque
stringData:
  apiKey: <your-temporal-cloud-api-key>
  accountId: <your-account-id>  # optional if embedded in temporalNamespace
```

### 3. Apply a TemporalTRUAutoscaler resource

```yaml
apiVersion: temporal.bitovi.com/v1alpha1
kind: TemporalTRUAutoscaler
metadata:
  name: my-namespace-autoscaler
  namespace: temporal-autoscaler
spec:
  temporalNamespace: my-temporal-namespace.a1b2c3
  credentialsSecretRef:
    name: temporal-cloud-api-key
  minTRU: 2
  maxTRU: 12
  scaleUpThreshold: 70
  scaleDownThreshold: 70
  scaleUpCooldown: 5m
  scaleDownCooldown: 1h
```

### 4. Watch it work

```bash
kubectl get ttautoscaler -n temporal-autoscaler -w
kubectl describe ttautoscaler my-namespace-autoscaler -n temporal-autoscaler
kubectl get events -n temporal-autoscaler --field-selector involvedObject.kind=TemporalTRUAutoscaler
```

---

## Helm Values Reference

| Value | Default | Description |
|---|---|---|
| `replicaCount` | `1` | Controller replicas (set `leaderElection: true` if > 1) |
| `image.repository` | `ghcr.io/bitovi/temporal-tru-autoscaler` | Container image |
| `image.tag` | chart `appVersion` | Image tag |
| `controller.reconcileInterval` | `30s` | How often each CR is polled |
| `controller.leaderElection` | `false` | Enable for multi-replica HA |
| `controller.metricsBindAddress` | `:8080` | Controller Prometheus metrics |
| `controller.healthProbeBindAddress` | `:8081` | Health/readiness probes |
| `metrics.enabled` | `true` | Create a Service for scraping controller metrics |
| `metrics.serviceMonitor.enabled` | `false` | Create a Prometheus ServiceMonitor |

---

## RBAC

The controller requires the following cluster-level permissions:

| Resource | Verbs |
|---|---|
| `temporaltruautoscalers` | get, list, watch, create, update, patch, delete |
| `temporaltruautoscalers/status` | get, update, patch |
| `temporaltruautoscalers/finalizers` | update |
| `secrets` | get, list, watch |
| `events` | create, patch |

---

## Building from Source

```bash
# Build the binary
make build

# Build the Docker image
make docker-build IMG=ghcr.io/bitovi/temporal-tru-autoscaler:latest

# Push the image
make docker-push IMG=ghcr.io/bitovi/temporal-tru-autoscaler:latest
```
