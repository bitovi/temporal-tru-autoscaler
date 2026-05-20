package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	temporalv1alpha1 "github.com/bitovi/temporal-tru-autoscaler/api/v1alpha1"
	"github.com/bitovi/temporal-tru-autoscaler/internal/temporal"
)

// ---------------------------------------------------------------------------
// Mock Temporal client
// ---------------------------------------------------------------------------

type mockTemporalClient struct {
	namespaceInfo *temporal.NamespaceInfo
	namespaceErr  error
	currentAPS    float64
	apsErr        error
	setTRUErr     error
	setTRUCalled  bool
	setTRUValue   int
}

func (m *mockTemporalClient) GetNamespaceInfo(_ context.Context, _ string) (*temporal.NamespaceInfo, error) {
	return m.namespaceInfo, m.namespaceErr
}

func (m *mockTemporalClient) GetCurrentAPS(_ context.Context, _ string) (float64, error) {
	return m.currentAPS, m.apsErr
}

func (m *mockTemporalClient) SetTRU(_ context.Context, _ string, newTRU int) error {
	m.setTRUCalled = true
	m.setTRUValue = newTRU
	return m.setTRUErr
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func buildScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := temporalv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("corev1 AddToScheme: %v", err)
	}
	return s
}

func defaultAutoscaler() *temporalv1alpha1.TemporalTRUAutoscaler {
	return &temporalv1alpha1.TemporalTRUAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-autoscaler",
			Namespace: "default",
		},
		Spec: temporalv1alpha1.TemporalTRUAutoscalerSpec{
			TemporalNamespace: "my-ns.acc123",
			CredentialsSecretRef: temporalv1alpha1.SecretReference{
				Name: "temporal-creds",
			},
			MinTRU:             2,
			MaxTRU:             12,
			ScaleUpThreshold:   70,
			ScaleDownThreshold: 30,
			ScaleUpCooldown:    metav1.Duration{Duration: 5 * time.Minute},
			ScaleDownCooldown:  metav1.Duration{Duration: 1 * time.Hour},
		},
	}
}

func defaultSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "temporal-creds",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"apiKey":    []byte("test-api-key"),
			"accountId": []byte("acc123"),
		},
	}
}

func buildReconciler(
	t *testing.T,
	objs []runtime.Object,
	mock *mockTemporalClient,
) *TemporalTRUAutoscalerReconciler {
	t.Helper()
	s := buildScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithRuntimeObjects(objs...).
		WithStatusSubresource(&temporalv1alpha1.TemporalTRUAutoscaler{}).
		Build()

	return &TemporalTRUAutoscalerReconciler{
		Client:              fakeClient,
		Scheme:              s,
		Recorder:            record.NewFakeRecorder(32),
		ControllerNamespace: "default",
		ReconcileInterval:   30 * time.Second,
		NewTemporalClient: func(_, _ string, _, _ []byte) temporal.Interface {
			return mock
		},
	}
}

func reconcileOnce(t *testing.T, r *TemporalTRUAutoscalerReconciler, name, ns string) ctrl.Result {
	t.Helper()
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: ns},
	})
	if err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	return result
}

func getAutoscaler(t *testing.T, r *TemporalTRUAutoscalerReconciler, name, ns string) *temporalv1alpha1.TemporalTRUAutoscaler {
	t.Helper()
	a := &temporalv1alpha1.TemporalTRUAutoscaler{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, a); err != nil {
		t.Fatalf("Get autoscaler: %v", err)
	}
	return a
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestReconcile_ScaleUp(t *testing.T) {
	// currentTRU=4, currentAPS=1800 → utilization = 1800/(4*500)*100 = 90% > 70% threshold
	// Expected: scale up to NextValidTRU(4) = 6
	mock := &mockTemporalClient{
		namespaceInfo: &temporal.NamespaceInfo{CurrentTRU: 4, APSCeiling: 2000},
		currentAPS:    1800,
	}
	a := defaultAutoscaler()
	r := buildReconciler(t, []runtime.Object{a, defaultSecret()}, mock)

	reconcileOnce(t, r, a.Name, a.Namespace)

	if !mock.setTRUCalled {
		t.Fatal("expected SetTRU to be called for scale-up")
	}
	if mock.setTRUValue != 6 {
		t.Errorf("SetTRU called with %d, want 6 (NextValidTRU(4))", mock.setTRUValue)
	}

	updated := getAutoscaler(t, r, a.Name, a.Namespace)
	if updated.Status.CurrentTRU != 6 {
		t.Errorf("status.currentTRU = %d, want 6", updated.Status.CurrentTRU)
	}
	if updated.Status.LastScaleDirection == nil || *updated.Status.LastScaleDirection != temporalv1alpha1.ScaleDirectionUp {
		t.Errorf("expected lastScaleDirection=Up")
	}
}

func TestReconcile_ScaleDown(t *testing.T) {
	// currentTRU=6, currentAPS=300 → utilization = 300/(6*500)*100 = 10% < 30% threshold
	// Last scale was 2 hours ago → cooldown (1h) elapsed
	// Expected: scale down to PrevValidTRU(6) = 4
	twoHoursAgo := metav1.NewTime(time.Now().Add(-2 * time.Hour))
	dirUp := temporalv1alpha1.ScaleDirectionUp
	a := defaultAutoscaler()
	a.Status.LastScaleTime = &twoHoursAgo
	a.Status.LastScaleDirection = &dirUp

	mock := &mockTemporalClient{
		namespaceInfo: &temporal.NamespaceInfo{CurrentTRU: 6, APSCeiling: 3000},
		currentAPS:    300,
	}
	r := buildReconciler(t, []runtime.Object{a, defaultSecret()}, mock)

	reconcileOnce(t, r, a.Name, a.Namespace)

	if !mock.setTRUCalled {
		t.Fatal("expected SetTRU to be called for scale-down")
	}
	if mock.setTRUValue != 4 {
		t.Errorf("SetTRU called with %d, want 4 (PrevValidTRU(6))", mock.setTRUValue)
	}

	updated := getAutoscaler(t, r, a.Name, a.Namespace)
	if updated.Status.CurrentTRU != 4 {
		t.Errorf("status.currentTRU = %d, want 4", updated.Status.CurrentTRU)
	}
	if updated.Status.LastScaleDirection == nil || *updated.Status.LastScaleDirection != temporalv1alpha1.ScaleDirectionDown {
		t.Errorf("expected lastScaleDirection=Down")
	}
}

func TestReconcile_ScaleUp_CooldownBlocked(t *testing.T) {
	// Last scale was 1 minute ago; scaleUpCooldown is 5 minutes → blocked.
	oneMinuteAgo := metav1.NewTime(time.Now().Add(-1 * time.Minute))
	dirDown := temporalv1alpha1.ScaleDirectionDown
	a := defaultAutoscaler()
	a.Status.LastScaleTime = &oneMinuteAgo
	a.Status.LastScaleDirection = &dirDown

	mock := &mockTemporalClient{
		namespaceInfo: &temporal.NamespaceInfo{CurrentTRU: 4, APSCeiling: 2000},
		currentAPS:    1800, // 90% utilization → would trigger scale-up
	}
	r := buildReconciler(t, []runtime.Object{a, defaultSecret()}, mock)

	reconcileOnce(t, r, a.Name, a.Namespace)

	if mock.setTRUCalled {
		t.Errorf("expected SetTRU NOT to be called (cooldown active), but it was called with %d", mock.setTRUValue)
	}
}

func TestReconcile_ScaleDown_CooldownBlocked(t *testing.T) {
	// Last scale was 30 minutes ago; scaleDownCooldown is 1 hour → blocked.
	thirtyMinutesAgo := metav1.NewTime(time.Now().Add(-30 * time.Minute))
	dirUp := temporalv1alpha1.ScaleDirectionUp
	a := defaultAutoscaler()
	a.Status.LastScaleTime = &thirtyMinutesAgo
	a.Status.LastScaleDirection = &dirUp

	mock := &mockTemporalClient{
		namespaceInfo: &temporal.NamespaceInfo{CurrentTRU: 6, APSCeiling: 3000},
		currentAPS:    300, // 10% utilization → would trigger scale-down
	}
	r := buildReconciler(t, []runtime.Object{a, defaultSecret()}, mock)

	reconcileOnce(t, r, a.Name, a.Namespace)

	if mock.setTRUCalled {
		t.Errorf("expected SetTRU NOT to be called (cooldown active), but it was called with %d", mock.setTRUValue)
	}
}

func TestReconcile_ScaleUp_AtMaxTRU(t *testing.T) {
	a := defaultAutoscaler()
	// currentTRU already equals maxTRU.
	mock := &mockTemporalClient{
		namespaceInfo: &temporal.NamespaceInfo{CurrentTRU: 12, APSCeiling: 6000},
		currentAPS:    5500, // 91% → would scale up if not at max
	}
	r := buildReconciler(t, []runtime.Object{a, defaultSecret()}, mock)

	reconcileOnce(t, r, a.Name, a.Namespace)

	if mock.setTRUCalled {
		t.Errorf("expected SetTRU NOT to be called (at maxTRU=12)")
	}

	updated := getAutoscaler(t, r, a.Name, a.Namespace)
	atMax := false
	for _, c := range updated.Status.Conditions {
		if c.Type == conditionAtMaximum && c.Status == metav1.ConditionTrue {
			atMax = true
		}
	}
	if !atMax {
		t.Errorf("expected AtMaximum condition to be True")
	}
}

func TestReconcile_ScaleDown_AtMinTRU(t *testing.T) {
	twoHoursAgo := metav1.NewTime(time.Now().Add(-2 * time.Hour))
	dirUp := temporalv1alpha1.ScaleDirectionUp
	a := defaultAutoscaler()
	a.Status.LastScaleTime = &twoHoursAgo
	a.Status.LastScaleDirection = &dirUp

	// currentTRU already equals minTRU.
	mock := &mockTemporalClient{
		namespaceInfo: &temporal.NamespaceInfo{CurrentTRU: 2, APSCeiling: 1000},
		currentAPS:    50, // 5% → would scale down if not at min
	}
	r := buildReconciler(t, []runtime.Object{a, defaultSecret()}, mock)

	reconcileOnce(t, r, a.Name, a.Namespace)

	if mock.setTRUCalled {
		t.Errorf("expected SetTRU NOT to be called (at minTRU=2)")
	}

	updated := getAutoscaler(t, r, a.Name, a.Namespace)
	atMin := false
	for _, c := range updated.Status.Conditions {
		if c.Type == conditionAtMinimum && c.Status == metav1.ConditionTrue {
			atMin = true
		}
	}
	if !atMin {
		t.Errorf("expected AtMinimum condition to be True")
	}
}

func TestReconcile_UtilizationInRange_NoScale(t *testing.T) {
	// 50% utilization: above scaleDownThreshold(30%) and below scaleUpThreshold(70%) → no action.
	mock := &mockTemporalClient{
		namespaceInfo: &temporal.NamespaceInfo{CurrentTRU: 4, APSCeiling: 2000},
		currentAPS:    1000, // 50%
	}
	a := defaultAutoscaler()
	r := buildReconciler(t, []runtime.Object{a, defaultSecret()}, mock)

	reconcileOnce(t, r, a.Name, a.Namespace)

	if mock.setTRUCalled {
		t.Errorf("expected no scale action at 50%% utilization")
	}
}

func TestReconcile_GetNamespaceInfo_Error(t *testing.T) {
	mock := &mockTemporalClient{
		namespaceErr: errors.New("temporal API unavailable"),
	}
	a := defaultAutoscaler()
	r := buildReconciler(t, []runtime.Object{a, defaultSecret()}, mock)

	// Reconcile should not return an error itself — it emits a warning event and re-queues.
	result := reconcileOnce(t, r, a.Name, a.Namespace)
	if result.RequeueAfter == 0 {
		t.Errorf("expected non-zero RequeueAfter when API fails")
	}

	updated := getAutoscaler(t, r, a.Name, a.Namespace)
	ready := true
	for _, c := range updated.Status.Conditions {
		if c.Type == conditionReady && c.Status == metav1.ConditionFalse {
			ready = false
		}
	}
	if ready {
		t.Errorf("expected Ready condition to be False after API error")
	}
}

func TestReconcile_GetCurrentAPS_Error(t *testing.T) {
	mock := &mockTemporalClient{
		namespaceInfo: &temporal.NamespaceInfo{CurrentTRU: 4, APSCeiling: 2000},
		apsErr:        errors.New("metrics endpoint unreachable"),
	}
	a := defaultAutoscaler()
	r := buildReconciler(t, []runtime.Object{a, defaultSecret()}, mock)

	result := reconcileOnce(t, r, a.Name, a.Namespace)
	if result.RequeueAfter == 0 {
		t.Errorf("expected non-zero RequeueAfter when metrics fail")
	}
	if mock.setTRUCalled {
		t.Errorf("expected no SetTRU call when metrics are unavailable")
	}
}

func TestReconcile_SetTRU_Error(t *testing.T) {
	// Scale-up conditions are met but SetTRU fails → status should reflect the error.
	mock := &mockTemporalClient{
		namespaceInfo: &temporal.NamespaceInfo{CurrentTRU: 4, APSCeiling: 2000},
		currentAPS:    1800,
		setTRUErr:     errors.New("API conflict"),
	}
	a := defaultAutoscaler()
	r := buildReconciler(t, []runtime.Object{a, defaultSecret()}, mock)

	reconcileOnce(t, r, a.Name, a.Namespace)

	if !mock.setTRUCalled {
		t.Fatal("expected SetTRU to be called")
	}

	// Status TRU should not have been updated when the API call failed.
	updated := getAutoscaler(t, r, a.Name, a.Namespace)
	if updated.Status.CurrentTRU == 6 {
		t.Errorf("status.currentTRU should not be updated after a failed SetTRU")
	}
}

func TestReconcile_MissingSecret(t *testing.T) {
	// Secret is not present in the fake client → credentials error.
	a := defaultAutoscaler()
	mock := &mockTemporalClient{}
	r := buildReconciler(t, []runtime.Object{a /* no secret */}, mock)

	result := reconcileOnce(t, r, a.Name, a.Namespace)
	if result.RequeueAfter == 0 {
		t.Errorf("expected requeue after missing secret")
	}
	if mock.setTRUCalled {
		t.Errorf("expected no API calls when credentials are missing")
	}
}

func TestReconcile_ResourceNotFound(t *testing.T) {
	// CR doesn't exist — reconcile should return a zero result without error.
	s := buildScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(s).Build()
	r := &TemporalTRUAutoscalerReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(4),
	}
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "gone", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("expected no error for missing resource, got: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("expected zero RequeueAfter for deleted resource")
	}
}

func TestReconcile_StatusCurrentTRUUpdated(t *testing.T) {
	// No scaling action, but status.currentTRU should still be set from the API.
	mock := &mockTemporalClient{
		namespaceInfo: &temporal.NamespaceInfo{CurrentTRU: 6, APSCeiling: 3000},
		currentAPS:    1500, // 50% — no scale action
	}
	a := defaultAutoscaler()
	r := buildReconciler(t, []runtime.Object{a, defaultSecret()}, mock)

	reconcileOnce(t, r, a.Name, a.Namespace)

	updated := getAutoscaler(t, r, a.Name, a.Namespace)
	if updated.Status.CurrentTRU != 6 {
		t.Errorf("status.currentTRU = %d, want 6", updated.Status.CurrentTRU)
	}
}

func TestReconcile_ReadyConditionSetOnSuccess(t *testing.T) {
	mock := &mockTemporalClient{
		namespaceInfo: &temporal.NamespaceInfo{CurrentTRU: 4, APSCeiling: 2000},
		currentAPS:    1000, // 50% — no scale action
	}
	a := defaultAutoscaler()
	r := buildReconciler(t, []runtime.Object{a, defaultSecret()}, mock)

	reconcileOnce(t, r, a.Name, a.Namespace)

	updated := getAutoscaler(t, r, a.Name, a.Namespace)
	ready := false
	for _, c := range updated.Status.Conditions {
		if c.Type == conditionReady && c.Status == metav1.ConditionTrue {
			ready = true
		}
	}
	if !ready {
		t.Errorf("expected Ready condition to be True after successful reconcile")
	}
}
