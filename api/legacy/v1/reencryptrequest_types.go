package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ReencryptRequestPhase tracks the lifecycle of a master-key reencryption batch.
type ReencryptRequestPhase string

const (
	ReencryptPhasePending   ReencryptRequestPhase = "Pending"
	ReencryptPhaseRunning   ReencryptRequestPhase = "Running"
	ReencryptPhaseCompleted ReencryptRequestPhase = "Completed"
	ReencryptPhaseFailed    ReencryptRequestPhase = "Failed"
)

// ReencryptRequestSpec selects volumes to reencrypt and constrains the rollout.
type ReencryptRequestSpec struct {
	// Selector picks the set of LogicalVolume objects to reencrypt. Nil selector
	// is rejected to avoid accidental cluster-wide rotations.
	// +kubebuilder:validation:Required
	Selector *metav1.LabelSelector `json:"selector,omitempty"`
	// Reason is an operator-supplied audit string (annual-key-rotation, suspected-compromise...).
	// +optional
	Reason string `json:"reason,omitempty"`
	// NewCipher requests a cipher upgrade (crypto-agility). Empty keeps the current cipher.
	// +optional
	NewCipher string `json:"newCipher,omitempty"`
	// MaxConcurrentPerNode caps the number of simultaneous reencrypts on a single node.
	// Zero defaults to 1 at the controller.
	// +optional
	MaxConcurrentPerNode int32 `json:"maxConcurrentPerNode,omitempty"`
	// ThroughputLimit caps cryptsetup's reencrypt throughput (for example "50MiB/s").
	// Empty disables the throttle.
	// +optional
	ThroughputLimit string `json:"throughputLimit,omitempty"`
}

// ReencryptVolumeStatus reports per-volume progress within a ReencryptRequest.
type ReencryptVolumeStatus struct {
	// VolumeID identifies the LogicalVolume.
	VolumeID string `json:"volumeID"`
	// Phase mirrors the per-volume lifecycle.
	// +optional
	Phase ReencryptRequestPhase `json:"phase,omitempty"`
	// Message holds a short human-readable status.
	// +optional
	Message string `json:"message,omitempty"`
	// StartedAt records when this volume's reencrypt began.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// CompletedAt records when this volume's reencrypt finished.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// ReencryptRequestStatus reports the aggregate progress of the rotation.
type ReencryptRequestStatus struct {
	// Phase is the overall lifecycle phase.
	// +optional
	Phase ReencryptRequestPhase `json:"phase,omitempty"`
	// Total is the count of volumes selected for reencryption.
	// +optional
	Total int32 `json:"total,omitempty"`
	// Completed counts volumes that finished successfully.
	// +optional
	Completed int32 `json:"completed,omitempty"`
	// Failed counts volumes that failed permanently.
	// +optional
	Failed int32 `json:"failed,omitempty"`
	// PerVolume reports per-volume progress.
	// +optional
	PerVolume []ReencryptVolumeStatus `json:"perVolume,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Total",type=integer,JSONPath=`.status.total`
// +kubebuilder:printcolumn:name="Completed",type=integer,JSONPath=`.status.completed`
// +kubebuilder:printcolumn:name="Failed",type=integer,JSONPath=`.status.failed`
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// ReencryptRequest drives master-key rotation as an auditable, throttled operation.
type ReencryptRequest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ReencryptRequestSpec   `json:"spec,omitempty"`
	Status ReencryptRequestStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ReencryptRequestList contains a list of ReencryptRequest.
type ReencryptRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ReencryptRequest `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ReencryptRequest{}, &ReencryptRequestList{})
}
