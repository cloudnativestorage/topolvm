package k8s

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	topolvmv1 "github.com/topolvm/topolvm/api/v1"
	clientwrapper "github.com/topolvm/topolvm/internal/client"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// EncryptionKeyService is the controller-side persistence layer for
// EncryptionKey CRs. It stores only ciphertext.
type EncryptionKeyService struct {
	writer interface {
		client.Writer
		client.StatusClient
		client.Reader
	}
}

// NewEncryptionKeyService returns an EncryptionKeyService bound to the
// controller-runtime manager's cache + client.
func NewEncryptionKeyService(mgr manager.Manager) *EncryptionKeyService {
	c := clientwrapper.NewWrappedClient(mgr.GetClient())
	return &EncryptionKeyService{writer: c}
}

// Create creates a new EncryptionKey object with the wrapped DEK and
// initializes its status. WrappedDEK is base64-encoded for round-trip safety
// across JSON boundaries.
func (s *EncryptionKeyService) Create(ctx context.Context, name, provider, keyRef, volumeID string, wrappedDEK []byte, kekVersion string, keyslot int32, consumers []string) (*topolvmv1.EncryptionKey, error) {
	if name == "" {
		return nil, errors.New("encryptionkey: empty name")
	}
	if len(wrappedDEK) == 0 {
		return nil, errors.New("encryptionkey: empty wrapped DEK")
	}
	ek := &topolvmv1.EncryptionKey{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Finalizers: []string{"topolvm.io/encryptionkey-protection"},
		},
		Spec: topolvmv1.EncryptionKeySpec{
			Provider: provider,
			KeyRef:   keyRef,
		},
	}
	if err := s.writer.Create(ctx, ek); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("encryptionkey: create %s: %w", name, err)
		}
		// retrieve so we can update status
		if err := s.writer.Get(ctx, client.ObjectKey{Name: name}, ek); err != nil {
			return nil, err
		}
	}
	// Populate status separately. Status subresources require a second update.
	return ek, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &topolvmv1.EncryptionKey{}
		if err := s.writer.Get(ctx, client.ObjectKey{Name: name}, fresh); err != nil {
			return err
		}
		fresh.Status.WrappedDEK = base64.StdEncoding.EncodeToString(wrappedDEK)
		fresh.Status.KEKVersion = kekVersion
		fresh.Status.BoundVolumeID = volumeID
		fresh.Status.Keyslot = keyslot
		fresh.Status.Consumers = consumers
		return s.writer.Status().Update(ctx, fresh)
	})
}

// Get retrieves an EncryptionKey by name.
func (s *EncryptionKeyService) Get(ctx context.Context, name string) (*topolvmv1.EncryptionKey, error) {
	ek := &topolvmv1.EncryptionKey{}
	if err := s.writer.Get(ctx, client.ObjectKey{Name: name}, ek); err != nil {
		return nil, err
	}
	return ek, nil
}

// AddConsumer adds id to status.consumers if not already present.
func (s *EncryptionKeyService) AddConsumer(ctx context.Context, name, id string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		ek := &topolvmv1.EncryptionKey{}
		if err := s.writer.Get(ctx, client.ObjectKey{Name: name}, ek); err != nil {
			return err
		}
		for _, c := range ek.Status.Consumers {
			if c == id {
				return nil
			}
		}
		ek.Status.Consumers = append(ek.Status.Consumers, id)
		return s.writer.Status().Update(ctx, ek)
	})
}

// RemoveConsumer removes id from status.consumers. If the result is empty
// and the key is retired, the finalizer is also cleared so garbage collection
// can proceed.
func (s *EncryptionKeyService) RemoveConsumer(ctx context.Context, name, id string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		ek := &topolvmv1.EncryptionKey{}
		if err := s.writer.Get(ctx, client.ObjectKey{Name: name}, ek); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		filtered := ek.Status.Consumers[:0]
		for _, c := range ek.Status.Consumers {
			if c != id {
				filtered = append(filtered, c)
			}
		}
		ek.Status.Consumers = filtered
		return s.writer.Status().Update(ctx, ek)
	})
}

// MaybeDelete deletes the EncryptionKey if it has no remaining consumers and
// is not pinned by an active LogicalVolume. Pinning is enforced by the
// finalizer; this method removes the finalizer once the consumer list is
// empty and then issues a delete.
func (s *EncryptionKeyService) MaybeDelete(ctx context.Context, name string) error {
	ek := &topolvmv1.EncryptionKey{}
	if err := s.writer.Get(ctx, client.ObjectKey{Name: name}, ek); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if len(ek.Status.Consumers) > 0 {
		return nil
	}
	// strip our protection finalizer then delete
	out := ek.DeepCopy()
	out.Finalizers = removeString(out.Finalizers, "topolvm.io/encryptionkey-protection")
	if err := s.writer.Update(ctx, out); err != nil {
		return err
	}
	if err := s.writer.Delete(ctx, out); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func removeString(in []string, s string) []string {
	out := in[:0]
	for _, x := range in {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}
