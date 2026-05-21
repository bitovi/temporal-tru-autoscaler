# Temporal TRU Autoscaler

A Kubernetes operator that automatically scales [Temporal Resource Units (TRU)](https://docs.temporal.io/cloud/capacity-modes) for your Temporal Cloud namespace based on live Actions Per Second (APS) utilization.

Works with any language — Node.js, Python, Java, Go, or anything else using the Temporal SDK. The operator talks directly to Temporal Cloud; your application code is never touched.

## How it works

The operator watches your Temporal Cloud namespace's APS utilization and adjusts TRU capacity up or down to keep utilization within your configured thresholds. It respects Temporal Cloud's billing model by defaulting to a 1-hour cooldown before scaling down (Temporal Cloud bills for the full hour after any TRU change).

## Prerequisites

- Kubernetes cluster (any — local or cloud)
- [Helm 3](https://helm.sh/docs/intro/install/)
- A Temporal Cloud namespace in [**provisioned capacity mode**](https://docs.temporal.io/cloud/capacity-modes) (not on-demand)
- A Temporal Cloud API key (Cloud UI → Settings → API Keys)

## Installation

### 1. Add the Helm repository

```bash
helm repo add bitovi https://bitovi.github.io/temporal-tru-autoscaler
helm repo update
```

### 2. Install the operator

```bash
helm install temporal-tru-autoscaler bitovi/temporal-tru-autoscaler \
  --namespace temporal-autoscaler \
  --create-namespace
```

### 3. Create the credentials Secret

The API key must belong to a Temporal Cloud service account with **two roles**:
- **Account-level:** Metrics Read-Only (to query the APS metrics endpoint)
- **Namespace-level:** Namespace Admin on the namespace being managed (to update provisioned TRU)

Create the service account and key in the Temporal Cloud UI under **Settings → Service Accounts**, then:

```bash
kubectl create secret generic temporal-cloud-api-key \
  --namespace temporal-autoscaler \
  --from-literal=apiKey=<your-temporal-cloud-api-key> \
  --from-literal=accountId=<your-account-id>
```

### 4. Create a TemporalTRUAutoscaler resource

Create one resource per Temporal Cloud namespace you want to manage:

```yaml
apiVersion: temporal.bitovi.com/v1alpha1
kind: TemporalTRUAutoscaler
metadata:
  name: my-app-autoscaler
  namespace: temporal-autoscaler
spec:
  temporalNamespace: my-app.abc123      # your Temporal Cloud namespace
  credentialsSecretRef:
    name: temporal-cloud-api-key
  minTRU: 2                             # never scale below this
  maxTRU: 12                            # never scale above this
  scaleUpThreshold: 70                  # scale up when APS > 70% of ceiling
  scaleDownThreshold: 30                # scale down when APS < 30% of ceiling
  scaleUpCooldown: 5m
  scaleDownCooldown: 1h
```

```bash
kubectl apply -f my-app-autoscaler.yaml
```

## Configuration reference

### Spec fields

| Field | Type | Default | Description |
|---|---|---|---|
| `temporalNamespace` | string | required | Temporal Cloud namespace to manage. Format: `name.accountId` |
| `credentialsSecretRef.name` | string | required | Name of the Secret containing `apiKey` (and optionally `accountId`) |
| `minTRU` | int | required | Minimum TRU — the operator will never scale below this |
| `maxTRU` | int | required | Maximum TRU — the operator will never scale above this |
| `scaleUpThreshold` | int | `70` | APS utilization % that triggers a scale-up |
| `scaleDownThreshold` | int | `70` | APS utilization % below which scale-down is considered |
| `scaleUpCooldown` | duration | `5m` | Minimum time between scale-up actions |
| `scaleDownCooldown` | duration | `1h` | Minimum time between scale-down actions |

Valid TRU values are: **2, 3, 4, 6, 8, 10, 12** (Temporal Cloud constraint). The operator always rounds to the nearest valid increment.

### Helm values

| Value | Default | Description |
|---|---|---|
| `replicaCount` | `1` | Controller replicas (set `leaderElection: true` if > 1) |
| `controller.reconcileInterval` | `30s` | How often each resource is polled |
| `controller.leaderElection` | `false` | Required when `replicaCount` > 1 |
| `metrics.enabled` | `true` | Expose controller Prometheus metrics |
| `metrics.serviceMonitor.enabled` | `false` | Create a Prometheus ServiceMonitor |

## Observing the operator

```bash
# Watch status
kubectl get ttautoscaler -n temporal-autoscaler -w

# See scale events
kubectl get events -n temporal-autoscaler \
  --field-selector involvedObject.kind=TemporalTRUAutoscaler

# Inspect full status and conditions
kubectl describe ttautoscaler my-app-autoscaler -n temporal-autoscaler

# Tail controller logs
kubectl logs -n temporal-autoscaler \
  -l app.kubernetes.io/name=temporal-tru-autoscaler \
  --follow
```

## Status fields

| Field | Description |
|---|---|
| `currentTRU` | TRU level currently set on the Temporal Cloud namespace |
| `lastScaleTime` | Timestamp of the most recent scale action |
| `lastScaleDirection` | `Up` or `Down` |
| `conditions` | `Ready`, `Scaling`, `AtMinimum`, `AtMaximum` |

## Uninstall

```bash
helm uninstall temporal-tru-autoscaler -n temporal-autoscaler
kubectl delete namespace temporal-autoscaler
```

## Contributing

See [IMPLEMENTATION.md](./IMPLEMENTATION.md) for architecture details, project structure, and local development instructions.

## License

Apache 2.0
