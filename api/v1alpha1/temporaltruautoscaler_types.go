package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SecretReference holds a reference to a Kubernetes Secret by name.
type SecretReference struct {
	// Name is the name of the Secret in the same namespace as the controller.
	// +kubebuilder:validation:Required
	Name string `json:"name"`
}

// TemporalTRUAutoscalerSpec defines the desired state of TemporalTRUAutoscaler.
type TemporalTRUAutoscalerSpec struct {
	// TemporalNamespace is the Temporal Cloud namespace to manage.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	TemporalNamespace string `json:"temporalNamespace"`

	// CredentialsSecretRef references the Kubernetes Secret containing the
	// Temporal Cloud API key (key: "apiKey").
	// +kubebuilder:validation:Required
	CredentialsSecretRef SecretReference `json:"credentialsSecretRef"`

	// MinTRU is the minimum TRU level; the controller will never scale below this.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	MinTRU int `json:"minTRU"`

	// MaxTRU is the maximum TRU level; the controller will never scale above this.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	MaxTRU int `json:"maxTRU"`

	// ScaleUpThreshold is the APS utilization percentage of the current tier ceiling
	// that triggers a scale-up action.
	// +kubebuilder:default=70
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	ScaleUpThreshold int `json:"scaleUpThreshold,omitempty"`

	// ScaleDownThreshold is the APS utilization percentage below which scale-down
	// is considered (must be sustained for the full scaleDownCooldown window).
	// +kubebuilder:default=70
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	ScaleDownThreshold int `json:"scaleDownThreshold,omitempty"`

	// ScaleUpCooldown is the minimum duration between successive scale-up actions.
	// Defaults to 5 minutes.
	// +kubebuilder:default="5m"
	ScaleUpCooldown metav1.Duration `json:"scaleUpCooldown,omitempty"`

	// ScaleDownCooldown is the minimum duration between successive scale-down actions.
	// Defaults to 1 hour because Temporal Cloud bills for the full hour after a TRU change.
	// +kubebuilder:default="1h"
	ScaleDownCooldown metav1.Duration `json:"scaleDownCooldown,omitempty"`
}

// ScaleDirection indicates whether the last scale action was up or down.
// +kubebuilder:validation:Enum=Up;Down
type ScaleDirection string

const (
	ScaleDirectionUp   ScaleDirection = "Up"
	ScaleDirectionDown ScaleDirection = "Down"
)

// TemporalTRUAutoscalerStatus defines the observed state of TemporalTRUAutoscaler.
type TemporalTRUAutoscalerStatus struct {
	// CurrentTRU is the TRU level currently reported by the Temporal Cloud API.
	// +optional
	CurrentTRU int `json:"currentTRU,omitempty"`

	// LastScaleTime is the timestamp of the most recent scale action.
	// +optional
	LastScaleTime *metav1.Time `json:"lastScaleTime,omitempty"`

	// LastScaleDirection indicates whether the most recent scale action was Up or Down.
	// +optional
	LastScaleDirection *ScaleDirection `json:"lastScaleDirection,omitempty"`

	// Conditions contains the current conditions of the autoscaler.
	// Known condition types: Scaling, AtMinimum, AtMaximum, Ready.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=ttautoscaler
// +kubebuilder:printcolumn:name="Namespace",type="string",JSONPath=".spec.temporalNamespace"
// +kubebuilder:printcolumn:name="MinTRU",type="integer",JSONPath=".spec.minTRU"
// +kubebuilder:printcolumn:name="MaxTRU",type="integer",JSONPath=".spec.maxTRU"
// +kubebuilder:printcolumn:name="CurrentTRU",type="integer",JSONPath=".status.currentTRU"
// +kubebuilder:printcolumn:name="LastScale",type="string",JSONPath=".status.lastScaleTime"
// +kubebuilder:printcolumn:name="Direction",type="string",JSONPath=".status.lastScaleDirection"

// TemporalTRUAutoscaler is the Schema for the temporaltruautoscalers API.
// One resource manages the TRU level for a single Temporal Cloud namespace.
type TemporalTRUAutoscaler struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TemporalTRUAutoscalerSpec   `json:"spec,omitempty"`
	Status TemporalTRUAutoscalerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// TemporalTRUAutoscalerList contains a list of TemporalTRUAutoscaler.
type TemporalTRUAutoscalerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TemporalTRUAutoscaler `json:"items"`
}

func init() {
	SchemeBuilder.Register(&TemporalTRUAutoscaler{}, &TemporalTRUAutoscalerList{})
}
