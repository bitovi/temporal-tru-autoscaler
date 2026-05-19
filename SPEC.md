# Temporal TRU Autoscaler — Project Spec

## Overview

A Kubernetes controller and CRD that watches APS (Actions Per Second) utilization from the Temporal Cloud metrics endpoint and automatically scales TRU (Temporal Resource Units) levels up or down by calling the Temporal Cloud API. Designed to look and feel like a Horizontal Pod Autoscaler.

---

## Background

Temporal Cloud bills capacity in TRUs, which come in tiers. Each tier has an APS ceiling — the maximum Actions Per Second it can handle. If utilization approaches that ceiling, capacity needs to increase. If utilization drops and stays low, capacity (and cost) can be reduced.

Because Temporal Cloud bills for the full hour once a TRU tier change is made, the scale-down cooldown should default to 1 hour to avoid paying for unnecessary tier flips.

---

## What We're Building

| Component | Details |
|---|---|
| Language | Go |
| Framework | kubebuilder + controller-runtime |
| CRD | `TemporalTRUAutoscaler` |
| Metrics source | Temporal Cloud Prometheus endpoint (polled directly by the controller) |
| TRU management | Temporal Cloud API (REST/gRPC) |
| Credentials | Kubernetes `Secret`, referenced from the CRD via `credentialsSecretRef` |
| Packaging | Helm chart |
| Scope | Standalone — no dependency on in-cluster Prometheus or other operators |

---

## CRD: `TemporalTRUAutoscaler`

One resource per Temporal Cloud namespace.

### Spec fields

| Field | Type | Default | Description |
|---|---|---|---|
| `temporalNamespace` | string | required | The Temporal Cloud namespace to manage |
| `credentialsSecretRef.name` | string | required | Name of the `Secret` containing the Temporal Cloud API key |
| `minTRU` | int | required | Minimum TRU level — controller will not scale below this |
| `maxTRU` | int | required | Maximum TRU level — controller will not scale above this |
| `scaleUpThreshold` | int | `70` | APS utilization % of current tier ceiling that triggers scale-up |
| `scaleDownThreshold` | int | `70` | APS utilization % of current tier ceiling below which scale-down is considered |
| `scaleUpCooldown` | duration | `5m` | Minimum time between scale-up actions |
| `scaleDownCooldown` | duration | `1h` | Minimum time between scale-down actions |

### Status fields

| Field | Description |
|---|---|
| `currentTRU` | Current TRU level as reported by Temporal Cloud |
| `lastScaleTime` | Timestamp of the last scale action |
| `lastScaleDirection` | `Up` or `Down` |
| `conditions` | Standard Kubernetes conditions (e.g. `Scaling`, `AtMinimum`, `AtMaximum`) |

### Example resource

```yaml
apiVersion: temporal.bitovi.com/v1alpha1
kind: TemporalTRUAutoscaler
metadata:
  name: my-namespace-autoscaler
spec:
  temporalNamespace: my-temporal-namespace
  credentialsSecretRef:
    name: temporal-cloud-api-key
  minTRU: 5
  maxTRU: 50
  scaleUpThreshold: 70
  scaleDownThreshold: 70
  scaleUpCooldown: 5m
  scaleDownCooldown: 1h
```

---

## Controller Behavior

### Polling loop

The controller reconciles each `TemporalTRUAutoscaler` on a configurable interval. On each reconcile:

1. Fetch current TRU level from the Temporal Cloud API
2. Fetch current APS from the Temporal Cloud Prometheus metrics endpoint
3. Calculate utilization as a percentage of the current tier's APS ceiling
4. Evaluate scale-up and scale-down conditions (see below)
5. If a scale action is taken, call the Temporal Cloud API to update the TRU level
6. Emit a Kubernetes Event describing the action
7. Update the resource status

### Scale-up condition

Scale up by one TRU tier when:
- Utilization > `scaleUpThreshold`%
- Current TRU < `maxTRU`
- Time since last scale action > `scaleUpCooldown`

### Scale-down condition

Scale down by one TRU tier when:
- Utilization has been sustained below `scaleDownThreshold`% for the duration of `scaleDownCooldown`
- Current TRU > `minTRU`

### Cooldown

Cooldowns are asymmetric by design:

| Direction | Default | Rationale |
|---|---|---|
| Scale-up | 5 minutes | React quickly to capacity pressure; Temporal's request throttle escalation is forgiving |
| Scale-down | 1 hour | Temporal Cloud bills for the full hour after a TRU change — scaling down too soon wastes money |

Both are configurable per resource.

### Kubernetes Events

The controller emits events on the `TemporalTRUAutoscaler` resource for:
- Successful scale-up (including old and new TRU level)
- Successful scale-down (including old and new TRU level)
- Scale action blocked by cooldown
- Scale action blocked by min/max bounds
- Errors communicating with the Temporal Cloud API

---

## Credentials

The Temporal Cloud API key is stored in a Kubernetes `Secret` in the same namespace as the controller, and referenced from each `TemporalTRUAutoscaler` via `credentialsSecretRef.name`.

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: temporal-cloud-api-key
type: Opaque
stringData:
  apiKey: <your-temporal-cloud-api-key>
```

---

## Packaging

Delivered as a Helm chart. Install with:

```bash
helm install temporal-tru-autoscaler ./charts/temporal-tru-autoscaler \
  --namespace temporal-autoscaler \
  --create-namespace
```

The chart includes:
- CRD
- Controller `Deployment`
- `ServiceAccount`, `ClusterRole`, `ClusterRoleBinding`

---

## Acceptance Criteria

- [ ] `TemporalTRUAutoscaler` CRD defines `temporalNamespace`, `minTRU`, `maxTRU`, `scaleUpThreshold`, `scaleDownThreshold`, `scaleUpCooldown`, and `scaleDownCooldown`
- [ ] Controller polls the Temporal Cloud Prometheus metrics endpoint for APS utilization
- [ ] Controller scales TRU up when utilization exceeds `scaleUpThreshold`% of the current tier ceiling
- [ ] Controller scales TRU down when utilization has been sustained below `scaleDownThreshold`% for the `scaleDownCooldown` window
- [ ] Scale-up cooldown defaults to 5 minutes and is configurable per resource
- [ ] Scale-down cooldown defaults to 1 hour and is configurable per resource
- [ ] Controller respects `minTRU` and `maxTRU` bounds and never scales outside them
- [ ] Controller emits Kubernetes Events on all scale actions and notable conditions
- [ ] CRD status reflects current TRU level, last scale time, and last scale direction
- [ ] Credentials are read from a Kubernetes `Secret` referenced by the resource spec
- [ ] Packaged as a Helm chart with CRD, Deployment, and RBAC resources
- [ ] One `TemporalTRUAutoscaler` resource per Temporal Cloud namespace

---

## Open Questions

- Exact Temporal Cloud Prometheus metrics endpoint URL and metric names for APS — verify against Temporal Cloud docs before implementation
- Exact Temporal Cloud API endpoint and payload shape for updating TRU level — verify against Temporal Cloud API reference
- Cooldown tracking implementation: store `lastScaleTime` in the resource status (survives controller restarts) vs. in-memory only
