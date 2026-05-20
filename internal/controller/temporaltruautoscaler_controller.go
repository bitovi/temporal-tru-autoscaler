// Package controller implements the TemporalTRUAutoscaler controller.
package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	temporalv1alpha1 "github.com/bitovi/temporal-tru-autoscaler/api/v1alpha1"
	"github.com/bitovi/temporal-tru-autoscaler/internal/temporal"
)

// Kubernetes condition type names used on TemporalTRUAutoscaler resources.
const (
	conditionReady     = "Ready"
	conditionScaling   = "Scaling"
	conditionAtMinimum = "AtMinimum"
	conditionAtMaximum = "AtMaximum"
)

// Kubernetes event reason strings.
const (
	reasonScaledUp            = "ScaledUp"
	reasonScaledDown          = "ScaledDown"
	reasonScaleBlockedCooldown = "ScaleBlockedCooldown"
	reasonScaleBlockedBounds  = "ScaleBlockedBounds"
	reasonAPIError            = "TemporalAPIError"
	reasonReconciling         = "Reconciling"
)

// defaultReconcileInterval is how often the controller re-checks each resource
// when no immediate event is pending. Users can override via the --reconcile-interval flag.
const defaultReconcileInterval = 30 * time.Second

// TemporalTRUAutoscalerReconciler reconciles TemporalTRUAutoscaler objects.
//
// +kubebuilder:rbac:groups=temporal.bitovi.com,resources=temporaltruautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=temporal.bitovi.com,resources=temporaltruautoscalers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=temporal.bitovi.com,resources=temporaltruautoscalers/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
type TemporalTRUAutoscalerReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// ReconcileInterval controls how often each resource is re-queued for
	// polling when no status changes are expected.
	ReconcileInterval time.Duration
	// ControllerNamespace is the namespace where the controller is running.
	// Secrets are read from this namespace.
	ControllerNamespace string
	// NewTemporalClient constructs a Temporal Cloud client. Defaults to
	// temporal.NewClient (API key auth) or temporal.NewClientWithMTLS when certs
	// are present; override in tests to inject a mock.
	// tlsCert and tlsKey are PEM-encoded; both must be non-nil to enable mTLS.
	NewTemporalClient func(apiKey, accountID string, tlsCert, tlsKey []byte) temporal.Interface
}

// Reconcile implements the main reconciliation loop for TemporalTRUAutoscaler.
func (r *TemporalTRUAutoscalerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// 1. Fetch the TemporalTRUAutoscaler resource.
	autoscaler := &temporalv1alpha1.TemporalTRUAutoscaler{}
	if err := r.Get(ctx, req.NamespacedName, autoscaler); err != nil {
		if apierrors.IsNotFound(err) {
			// Resource was deleted; nothing to do.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Work on a copy to avoid mutating the cache.
	original := autoscaler.DeepCopy()

	// 2. Read the API key from the referenced Secret.
	apiKey, accountID, err := r.readCredentials(ctx, autoscaler)
	if err != nil {
		logger.Error(err, "failed to read credentials secret",
			"secret", autoscaler.Spec.CredentialsSecretRef.Name)
		r.Recorder.Eventf(autoscaler, corev1.EventTypeWarning, reasonAPIError,
			"Failed to read credentials secret %q: %v", autoscaler.Spec.CredentialsSecretRef.Name, err)
		setCondition(&autoscaler.Status, conditionReady, metav1.ConditionFalse,
			"CredentialsError", fmt.Sprintf("cannot read secret: %v", err))
		_ = r.patchStatus(ctx, autoscaler, original)
		return ctrl.Result{RequeueAfter: r.reconcileInterval()}, nil
	}

	// Read optional observability mTLS creds (for accounts with CA cert configured).
	tlsCert, tlsKey, err := r.readObservabilityCreds(ctx, autoscaler)
	if err != nil {
		logger.Error(err, "failed to read observability secret",
			"secret", autoscaler.Spec.ObservabilitySecretRef.Name)
		r.Recorder.Eventf(autoscaler, corev1.EventTypeWarning, reasonAPIError,
			"Failed to read observability secret %q: %v", autoscaler.Spec.ObservabilitySecretRef.Name, err)
		setCondition(&autoscaler.Status, conditionReady, metav1.ConditionFalse,
			"CredentialsError", fmt.Sprintf("cannot read observability secret: %v", err))
		_ = r.patchStatus(ctx, autoscaler, original)
		return ctrl.Result{RequeueAfter: r.reconcileInterval()}, nil
	}

	temporalClient := r.newTemporalClient(apiKey, accountID, tlsCert, tlsKey)

	// 3. Fetch current TRU level from the Temporal Cloud API.
	nsInfo, err := temporalClient.GetNamespaceInfo(ctx, autoscaler.Spec.TemporalNamespace)
	if err != nil {
		logger.Error(err, "failed to fetch namespace info from Temporal Cloud",
			"namespace", autoscaler.Spec.TemporalNamespace)
		r.Recorder.Eventf(autoscaler, corev1.EventTypeWarning, reasonAPIError,
			"Failed to fetch namespace info for %q: %v", autoscaler.Spec.TemporalNamespace, err)
		setCondition(&autoscaler.Status, conditionReady, metav1.ConditionFalse,
			"APIError", fmt.Sprintf("cannot fetch namespace info: %v", err))
		_ = r.patchStatus(ctx, autoscaler, original)
		return ctrl.Result{RequeueAfter: r.reconcileInterval()}, nil
	}

	// 4. Fetch current APS from the Temporal Cloud Prometheus metrics endpoint.
	currentAPS, err := temporalClient.GetCurrentAPS(ctx, autoscaler.Spec.TemporalNamespace)
	if err != nil {
		logger.Error(err, "failed to fetch APS metrics from Temporal Cloud",
			"namespace", autoscaler.Spec.TemporalNamespace)
		r.Recorder.Eventf(autoscaler, corev1.EventTypeWarning, reasonAPIError,
			"Failed to fetch APS metrics for %q: %v", autoscaler.Spec.TemporalNamespace, err)
		setCondition(&autoscaler.Status, conditionReady, metav1.ConditionFalse,
			"MetricsError", fmt.Sprintf("cannot fetch APS metrics: %v", err))
		_ = r.patchStatus(ctx, autoscaler, original)
		return ctrl.Result{RequeueAfter: r.reconcileInterval()}, nil
	}

	// 5. Calculate utilization percentage.
	currentTRU := nsInfo.CurrentTRU
	apsCeiling := nsInfo.APSCeiling
	var utilization float64
	if apsCeiling > 0 {
		utilization = (currentAPS / apsCeiling) * 100
	}

	logger.Info("reconcile metrics",
		"namespace", autoscaler.Spec.TemporalNamespace,
		"currentTRU", currentTRU,
		"currentAPS", currentAPS,
		"apsCeiling", apsCeiling,
		"utilizationPct", fmt.Sprintf("%.1f", utilization),
	)

	// Update currentTRU in status.
	autoscaler.Status.CurrentTRU = currentTRU

	// 6. Evaluate scaling decisions.
	scaledUp, scaledDown := false, false
	var scaleErr error

	now := time.Now()
	spec := autoscaler.Spec

	switch {
	case utilization > float64(spec.ScaleUpThreshold):
		// --- Scale-up evaluation ---
		if currentTRU >= spec.MaxTRU {
			logger.Info("scale-up blocked: already at maxTRU",
				"currentTRU", currentTRU, "maxTRU", spec.MaxTRU)
			r.Recorder.Eventf(autoscaler, corev1.EventTypeNormal, reasonScaleBlockedBounds,
				"Scale-up blocked: currentTRU %d is already at maxTRU %d (utilization %.1f%%)",
				currentTRU, spec.MaxTRU, utilization)
			setCondition(&autoscaler.Status, conditionAtMaximum, metav1.ConditionTrue,
				"AtMaximum", fmt.Sprintf("TRU at maximum (%d)", spec.MaxTRU))
		} else if sinceCooldown := r.timeSinceLastScale(autoscaler, ScaleUp); sinceCooldown < spec.ScaleUpCooldown.Duration {
			remaining := spec.ScaleUpCooldown.Duration - sinceCooldown
			logger.Info("scale-up blocked: cooldown active",
				"remaining", remaining.Round(time.Second))
			r.Recorder.Eventf(autoscaler, corev1.EventTypeNormal, reasonScaleBlockedCooldown,
				"Scale-up blocked by cooldown (%.0fs remaining, utilization %.1f%%)",
				remaining.Seconds(), utilization)
		} else {
			newTRU := temporal.NextValidTRU(currentTRU)
			logger.Info("scaling up", "from", currentTRU, "to", newTRU)
			scaleErr = temporalClient.SetTRU(ctx, spec.TemporalNamespace, newTRU)
			if scaleErr != nil {
				logger.Error(scaleErr, "failed to scale up TRU")
				r.Recorder.Eventf(autoscaler, corev1.EventTypeWarning, reasonAPIError,
					"Failed to scale up TRU from %d to %d: %v", currentTRU, newTRU, scaleErr)
			} else {
				scaledUp = true
				r.Recorder.Eventf(autoscaler, corev1.EventTypeNormal, reasonScaledUp,
					"Scaled up TRU from %d to %d (utilization was %.1f%%, threshold %d%%)",
					currentTRU, newTRU, utilization, spec.ScaleUpThreshold)
				autoscaler.Status.CurrentTRU = newTRU
				t := metav1.NewTime(now)
				autoscaler.Status.LastScaleTime = &t
				dir := temporalv1alpha1.ScaleDirectionUp
				autoscaler.Status.LastScaleDirection = &dir
			}
		}

	case utilization < float64(spec.ScaleDownThreshold):
		// --- Scale-down evaluation ---
		// Per spec: scale down only when utilization has been SUSTAINED below
		// the threshold for the full scaleDownCooldown window. We implement this
		// by treating scaleDownCooldown as the time that must elapse since the
		// last scale action (in any direction) before a scale-down is attempted.
		// This satisfies the spec's intent: if a scale-up occurred recently, the
		// cooldown window resets and we wait for a full period of low utilization.
		if currentTRU <= spec.MinTRU {
			logger.Info("scale-down blocked: already at minTRU",
				"currentTRU", currentTRU, "minTRU", spec.MinTRU)
			r.Recorder.Eventf(autoscaler, corev1.EventTypeNormal, reasonScaleBlockedBounds,
				"Scale-down blocked: currentTRU %d is already at minTRU %d (utilization %.1f%%)",
				currentTRU, spec.MinTRU, utilization)
			setCondition(&autoscaler.Status, conditionAtMinimum, metav1.ConditionTrue,
				"AtMinimum", fmt.Sprintf("TRU at minimum (%d)", spec.MinTRU))
		} else if sinceCooldown := r.timeSinceLastScale(autoscaler, ScaleDown); sinceCooldown < spec.ScaleDownCooldown.Duration {
			remaining := spec.ScaleDownCooldown.Duration - sinceCooldown
			logger.Info("scale-down blocked: cooldown active",
				"remaining", remaining.Round(time.Second))
			r.Recorder.Eventf(autoscaler, corev1.EventTypeNormal, reasonScaleBlockedCooldown,
				"Scale-down blocked by cooldown (%.0fs remaining, utilization %.1f%%)",
				remaining.Seconds(), utilization)
		} else {
			newTRU := temporal.PrevValidTRU(currentTRU)
			logger.Info("scaling down", "from", currentTRU, "to", newTRU)
			scaleErr = temporalClient.SetTRU(ctx, spec.TemporalNamespace, newTRU)
			if scaleErr != nil {
				logger.Error(scaleErr, "failed to scale down TRU")
				r.Recorder.Eventf(autoscaler, corev1.EventTypeWarning, reasonAPIError,
					"Failed to scale down TRU from %d to %d: %v", currentTRU, newTRU, scaleErr)
			} else {
				scaledDown = true
				r.Recorder.Eventf(autoscaler, corev1.EventTypeNormal, reasonScaledDown,
					"Scaled down TRU from %d to %d (utilization was %.1f%%, threshold %d%%)",
					currentTRU, newTRU, utilization, spec.ScaleDownThreshold)
				autoscaler.Status.CurrentTRU = newTRU
				t := metav1.NewTime(now)
				autoscaler.Status.LastScaleTime = &t
				dir := temporalv1alpha1.ScaleDirectionDown
				autoscaler.Status.LastScaleDirection = &dir
			}
		}
	}

	// 7. Update boundary conditions when no scale action changed them above.
	if !scaledUp && !scaledDown {
		if currentTRU >= spec.MaxTRU {
			setCondition(&autoscaler.Status, conditionAtMaximum, metav1.ConditionTrue,
				"AtMaximum", fmt.Sprintf("TRU at maximum (%d)", spec.MaxTRU))
		} else {
			setCondition(&autoscaler.Status, conditionAtMaximum, metav1.ConditionFalse,
				"BelowMaximum", "TRU is below maximum")
		}
		if currentTRU <= spec.MinTRU {
			setCondition(&autoscaler.Status, conditionAtMinimum, metav1.ConditionTrue,
				"AtMinimum", fmt.Sprintf("TRU at minimum (%d)", spec.MinTRU))
		} else {
			setCondition(&autoscaler.Status, conditionAtMinimum, metav1.ConditionFalse,
				"AboveMinimum", "TRU is above minimum")
		}
	}

	// Mark as scaling when an action was taken, clear once stable.
	if scaledUp || scaledDown {
		setCondition(&autoscaler.Status, conditionScaling, metav1.ConditionTrue,
			"ScaleInProgress", "A TRU scale action was performed")
	} else {
		setCondition(&autoscaler.Status, conditionScaling, metav1.ConditionFalse,
			"Stable", "No scale action required")
	}

	// Mark ready if no API error occurred.
	if scaleErr == nil {
		setCondition(&autoscaler.Status, conditionReady, metav1.ConditionTrue,
			"Reconciled", "Controller reconciled successfully")
	}

	// 8. Persist status changes.
	if err := r.patchStatus(ctx, autoscaler, original); err != nil {
		logger.Error(err, "failed to patch status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: r.reconcileInterval()}, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// ScaleDirection is used internally to select the appropriate cooldown.
type ScaleDirection int

const (
	ScaleUp   ScaleDirection = iota
	ScaleDown ScaleDirection = iota
)

// timeSinceLastScale returns how long it has been since the last scale action
// in the given direction. When no prior scale action exists, it returns a very
// large duration (effectively "never scaled") so the cooldown check always passes.
func (r *TemporalTRUAutoscalerReconciler) timeSinceLastScale(
	a *temporalv1alpha1.TemporalTRUAutoscaler,
	direction ScaleDirection,
) time.Duration {
	if a.Status.LastScaleTime == nil {
		return 24 * 365 * time.Hour // effectively infinite — never scaled
	}
	// The cooldown applies regardless of direction: any recent scale resets the
	// window. This prevents rapid oscillation.
	return time.Since(a.Status.LastScaleTime.Time)
}

// readCredentials retrieves the Temporal Cloud API key from the referenced Secret.
// Returns (apiKey, accountID, error).
// accountID is stored in the secret under the optional "accountId" key.
func (r *TemporalTRUAutoscalerReconciler) readCredentials(
	ctx context.Context,
	autoscaler *temporalv1alpha1.TemporalTRUAutoscaler,
) (string, string, error) {
	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{
		Namespace: r.ControllerNamespace,
		Name:      autoscaler.Spec.CredentialsSecretRef.Name,
	}
	if err := r.Get(ctx, secretKey, secret); err != nil {
		return "", "", fmt.Errorf("get secret %s/%s: %w", secretKey.Namespace, secretKey.Name, err)
	}

	apiKey, ok := secret.Data["apiKey"]
	if !ok {
		return "", "", fmt.Errorf("secret %s/%s is missing required key 'apiKey'",
			secretKey.Namespace, secretKey.Name)
	}

	// accountId is optional; it may be embedded in the namespace name (ns.accountId format)
	// or provided explicitly.
	accountID := string(secret.Data["accountId"])
	if accountID == "" {
		// Try to extract from the namespace name (e.g. "my-ns.a1b2c3").
		parts := strings.SplitN(autoscaler.Spec.TemporalNamespace, ".", 2)
		if len(parts) == 2 {
			accountID = parts[1]
		}
	}

	return strings.TrimSpace(string(apiKey)), accountID, nil
}

// readObservabilityCreds reads the optional mTLS client cert and key for the metrics
// endpoint. Returns nil slices (no error) when ObservabilitySecretRef is not set.
func (r *TemporalTRUAutoscalerReconciler) readObservabilityCreds(
	ctx context.Context,
	autoscaler *temporalv1alpha1.TemporalTRUAutoscaler,
) (certPEM, keyPEM []byte, err error) {
	if autoscaler.Spec.ObservabilitySecretRef == nil {
		return nil, nil, nil
	}
	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{
		Namespace: r.ControllerNamespace,
		Name:      autoscaler.Spec.ObservabilitySecretRef.Name,
	}
	if err := r.Get(ctx, secretKey, secret); err != nil {
		return nil, nil, fmt.Errorf("get observability secret %s/%s: %w", secretKey.Namespace, secretKey.Name, err)
	}
	certPEM = secret.Data["clientCert"]
	keyPEM = secret.Data["clientKey"]
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		return nil, nil, fmt.Errorf("observability secret %s/%s must contain 'clientCert' and 'clientKey'",
			secretKey.Namespace, secretKey.Name)
	}
	return certPEM, keyPEM, nil
}

// patchStatus applies a status-only patch to avoid overwriting the spec.
func (r *TemporalTRUAutoscalerReconciler) patchStatus(
	ctx context.Context,
	autoscaler, original *temporalv1alpha1.TemporalTRUAutoscaler,
) error {
	return r.Status().Patch(ctx, autoscaler, client.MergeFrom(original))
}

// newTemporalClient invokes the configurable factory. When tlsCert and tlsKey are
// both non-nil it uses mTLS for the metrics endpoint; otherwise API key auth.
func (r *TemporalTRUAutoscalerReconciler) newTemporalClient(apiKey, accountID string, tlsCert, tlsKey []byte) temporal.Interface {
	if r.NewTemporalClient != nil {
		return r.NewTemporalClient(apiKey, accountID, tlsCert, tlsKey)
	}
	if len(tlsCert) > 0 && len(tlsKey) > 0 {
		c, err := temporal.NewClientWithMTLS(apiKey, accountID, tlsCert, tlsKey)
		if err == nil {
			return c
		}
		// Fall through to plain API key client if cert parse fails.
	}
	return temporal.NewClient(apiKey, accountID)
}

// reconcileInterval returns the configured interval, falling back to the default.
func (r *TemporalTRUAutoscalerReconciler) reconcileInterval() time.Duration {
	if r.ReconcileInterval > 0 {
		return r.ReconcileInterval
	}
	return defaultReconcileInterval
}

// setCondition sets or updates a condition on the autoscaler status.
func setCondition(
	status *temporalv1alpha1.TemporalTRUAutoscalerStatus,
	condType string,
	condStatus metav1.ConditionStatus,
	reason, message string,
) {
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             condStatus,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	})
}

// SetupWithManager registers the controller with the manager.
func (r *TemporalTRUAutoscalerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&temporalv1alpha1.TemporalTRUAutoscaler{}).
		Complete(r)
}
