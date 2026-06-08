package v1

import (
	"google.golang.org/grpc/codes"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// LogicalVolumeSpec defines the desired state of LogicalVolume
type LogicalVolumeSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	Name                string            `json:"name"`
	NodeName            string            `json:"nodeName"`
	Size                resource.Quantity `json:"size"`
	DeviceClass         string            `json:"deviceClass,omitempty"`
	LvcreateOptionClass string            `json:"lvcreateOptionClass,omitempty"`

	// 'source' specifies the logicalvolume name of the source; if present.
	// This field is populated only when LogicalVolume has a source.
	//+kubebuilder:validation:Optional
	Source string `json:"source,omitempty"`

	//'accessType' specifies how the user intends to consume the snapshot logical volume.
	// Set to "ro" when creating a snapshot and to "rw" when restoring a snapshot or creating a clone.
	// This field is populated only when LogicalVolume has a source.
	//+kubebuilder:validation:Optional
	AccessType string `json:"accessType,omitempty"`

	// Encryption configures transparent data encryption (LUKS2) for this volume.
	// When nil or Enabled=false, the legacy unencrypted code path is used.
	// +optional
	Encryption *EncryptionSpec `json:"encryption,omitempty"`
}

// EncryptionSpec is set by the controller at provisioning time.
type EncryptionSpec struct {
	// Enabled gates encryption for this volume. Absence is equivalent to Enabled=false.
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// Provider names the KeyProvider used to wrap the DEK (vault, aws-kms, gcp-kms, azure-kv, pkcs11).
	// +optional
	Provider string `json:"provider,omitempty"`
	// KeyRef is the provider-specific KEK identifier.
	// +optional
	KeyRef string `json:"keyRef,omitempty"`
	// Cipher is the LUKS2 cipher (default aes-xts-plain64).
	// +optional
	Cipher string `json:"cipher,omitempty"`
	// KeySize is the LUKS2 key size in bits (default 512).
	// +optional
	KeySize int32 `json:"keySize,omitempty"`
}

// EncryptionState describes the lifecycle of the encrypted device on this node.
type EncryptionState string

const (
	EncryptionPending      EncryptionState = "Pending"
	EncryptionFormatted    EncryptionState = "Formatted"
	EncryptionOpened       EncryptionState = "Opened"
	EncryptionReencrypting EncryptionState = "Reencrypting"
	EncryptionError        EncryptionState = "Error"
)

// EncryptionStatus reports the observed state of the LUKS device and the
// EncryptionKey object that currently unlocks it.
type EncryptionStatus struct {
	// State is the current encryption lifecycle state.
	// +optional
	State EncryptionState `json:"state,omitempty"`
	// HeaderUUID is the LUKS2 header UUID, identifies the on-disk header.
	// +optional
	HeaderUUID string `json:"headerUUID,omitempty"`
	// ActiveKeyID is the EncryptionKey object name that currently unlocks this volume.
	// +optional
	ActiveKeyID string `json:"activeKeyID,omitempty"`
	// Keyslot is the LUKS2 keyslot occupied by the active passphrase.
	// +optional
	Keyslot int32 `json:"keyslot,omitempty"`
	// MasterKeyEpoch is bumped only by a completed reencrypt.
	// +optional
	MasterKeyEpoch int32 `json:"masterKeyEpoch,omitempty"`
}

// LogicalVolumeStatus defines the observed state of LogicalVolume
type LogicalVolumeStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
	VolumeID    string             `json:"volumeID,omitempty"`
	Code        codes.Code         `json:"code,omitempty"`
	Message     string             `json:"message,omitempty"`
	CurrentSize *resource.Quantity `json:"currentSize,omitempty"`

	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// +optional
	Snapshot *SnapshotStatus `json:"snapshot,omitempty"`

	// Encryption reports the observed encryption state of this volume.
	// +optional
	Encryption *EncryptionStatus `json:"encryption,omitempty"`
}

// SnapshotStatus defines the observed state of a backup or restore operation.
type SnapshotStatus struct {
	// Operation indicates whether this status is for a backup or a restore.
	// +optional
	Operation OperationType `json:"operation,omitempty"`
	// Phase represents the current phase of the backup or restore operation.
	Phase OperationPhase `json:"phase"`
	// StartTime is the time at which the operation was started.
	StartTime metav1.Time `json:"startTime"`
	// CompletionTime is the time at which the operation completed (success or failure).
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
	// Duration is how long operation took to complete.
	// +optional
	Duration string `json:"duration,omitempty"`
	// Progress contains information about the progress of the operation.
	// +optional
	Progress *OperationProgress `json:"progress,omitempty"`
	// Message provides a short description of the snapshot’s state
	// +optional
	Message string `json:"message,omitempty"`
	// Error contains details if the operation encountered an error.
	// +optional
	Error *SnapshotError `json:"error,omitempty"`
	// Path represents the directory inside the SnapshotStorage where this backup was stored.
	// +optional
	Path string `json:"path,omitempty"`
	// Repository is the Restic repository path/url where the snapshot is stored
	// +optional
	Repository string `json:"repository,omitempty"`
	// SnapshotID is the identifier of the Restic snapshot involved in the operation.
	// +optional
	SnapshotID string `json:"snapshotID,omitempty"`
	// Version keeps track of restic binary or backup engine version used
	// +optional
	Version string `json:"version,omitempty"`
}

type OperationProgress struct {
	// SecondsElapsed represents the seconds elapsed
	// +optional
	SecondsElapsed int64 `json:"secondsElapsed,omitempty"`

	// PercentDone represents the percentage of the backup/restore that has been completed
	//+optional
	PercentDone string `json:"percentDone,omitempty"`

	// TotalFiles represents the total number of files that need to be transferred during the backup/restore
	// +optional
	TotalFiles int64 `json:"totalFiles,omitempty"`

	// FilesDone represents the number of files done
	// +optional
	FilesDone int64 `json:"filesDone,omitempty"`

	// TransferDone represents the amount of data has been transferred
	// +optional
	TransferDone string `json:"transferDone,omitempty"`

	// Total represents the total amount of data that needs to be transferred during the backup
	// +optional
	Total string `json:"total,omitempty"`

	// Speed represents the transfer speed during the backup
	Speed string `json:"speed,omitempty"`
}

type SnapshotError struct {
	Code    string `json:"code,omitempty"`    // e.g., "RepositoryNotReachable", "VolumeMountFailed"
	Message string `json:"message,omitempty"` // human-readable error
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.snapshot.phase"
// +kubebuilder:printcolumn:name="Progess",type="string",JSONPath=".status.snapshot.progress.percentDone"

// LogicalVolume is the Schema for the logicalvolumes API
type LogicalVolume struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LogicalVolumeSpec   `json:"spec,omitempty"`
	Status LogicalVolumeStatus `json:"status,omitempty"`
}

// IsCompatibleWith returns true if the LogicalVolume is compatible.
func (lv *LogicalVolume) IsCompatibleWith(lv2 *LogicalVolume) bool {
	if lv.Spec.Name != lv2.Spec.Name {
		return false
	}
	if lv.Spec.Source != lv2.Spec.Source {
		return false
	}
	if lv.Spec.Size.Cmp(lv2.Spec.Size) != 0 {
		return false
	}
	return true
}

//+kubebuilder:object:root=true

// LogicalVolumeList contains a list of LogicalVolume
type LogicalVolumeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LogicalVolume `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LogicalVolume{}, &LogicalVolumeList{})
}
