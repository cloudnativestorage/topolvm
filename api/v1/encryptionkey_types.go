package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EncryptionKeySpec configures which external key custody backend wraps the DEK.
type EncryptionKeySpec struct {
	// Provider is the KeyProvider name (vault, aws-kms, gcp-kms, azure-kv, pkcs11).
	// +kubebuilder:validation:Required
	Provider string `json:"provider"`
	// KeyRef is the provider-specific KEK identifier.
	// +kubebuilder:validation:Required
	KeyRef string `json:"keyRef"`
}

// EncryptionKeyStatus holds the wrapped DEK (ciphertext only) and bookkeeping
// used to bind the key to the consuming volumes and to drive KEK rotation.
type EncryptionKeyStatus struct {
	// WrappedDEK is the base64-encoded ciphertext of the LUKS passphrase
	// wrapped by the provider's current KEK. Never plaintext.
	// +optional
	WrappedDEK string `json:"wrappedDEK,omitempty"`
	// KEKVersion is the provider version used to wrap the DEK.
	// +optional
	KEKVersion string `json:"kekVersion,omitempty"`
	// BoundVolumeID is the encryption-context binding (the CSI volume id at
	// provisioning time, used as the provider's encryption context).
	// +optional
	BoundVolumeID string `json:"boundVolumeID,omitempty"`
	// Keyslot records which LUKS2 keyslot this passphrase occupies.
	// +optional
	Keyslot int32 `json:"keyslot,omitempty"`
	// Consumers lists the volumes/snapshots depending on this key.
	// +optional
	Consumers []string `json:"consumers,omitempty"`
	// LastRewrapAt records the most recent successful KEK rewrap.
	// +optional
	LastRewrapAt *metav1.Time `json:"lastRewrapAt,omitempty"`
	// RetiredAt is set when this key has been superseded but is still pinned
	// by a snapshot or other consumer.
	// +optional
	RetiredAt *metav1.Time `json:"retiredAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=enckey
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.provider`
// +kubebuilder:printcolumn:name="KEKVersion",type=string,JSONPath=`.status.kekVersion`
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// EncryptionKey is the Schema for the encryptionkeys API. It stores only the
// wrapped (ciphertext) per-volume passphrase. Plaintext key material lives
// only inside the kernel device-mapper context while a volume is open.
type EncryptionKey struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   EncryptionKeySpec   `json:"spec,omitempty"`
	Status EncryptionKeyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// EncryptionKeyList contains a list of EncryptionKey.
type EncryptionKeyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EncryptionKey `json:"items"`
}

func init() {
	SchemeBuilder.Register(&EncryptionKey{}, &EncryptionKeyList{})
}
