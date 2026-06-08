package driver

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"

	topolvmv1 "github.com/topolvm/topolvm/api/v1"
	"github.com/topolvm/topolvm/internal/crypt"
	"github.com/topolvm/topolvm/internal/driver/internal/k8s"
	"github.com/topolvm/topolvm/internal/keyprovider"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// encryptionCoordinator centralizes the node-side encryption flow: unwrap key,
// optional luksFormat on first stage, luksOpen, and status reporting.
type encryptionCoordinator struct {
	crypt       crypt.Manager
	keyProvider keyprovider.KeyProvider
	encKeys     *k8s.EncryptionKeyService
	k8sLV       *k8s.LogicalVolumeService
}

// newEncryptionCoordinator builds the coordinator from the wired dependencies.
// When keyProvider or crypt is nil, the coordinator returns a sentinel that
// rejects encrypted operations; callers should pre-check.
func newEncryptionCoordinator(c crypt.Manager, kp keyprovider.KeyProvider, ek *k8s.EncryptionKeyService, lv *k8s.LogicalVolumeService) *encryptionCoordinator {
	return &encryptionCoordinator{crypt: c, keyProvider: kp, encKeys: ek, k8sLV: lv}
}

func (e *encryptionCoordinator) enabled() bool {
	return e != nil && e.crypt != nil && e.keyProvider != nil && e.encKeys != nil
}

// dmName returns the device-mapper name for a volumeID. Bounded to 64 chars to
// stay within the kernel dm-name limit.
func dmName(volumeID string) string {
	h := sha1.Sum([]byte(volumeID))
	return "topolvm-" + hex.EncodeToString(h[:8])
}

// resolvedKey carries the plaintext passphrase plus the EncryptionKey object
// used to derive it. Callers must Destroy() the passphrase.
type resolvedKey struct {
	pass    crypt.SecretBuf
	keyID   string
	keyslot int32
}

// unwrap fetches the EncryptionKey for an encrypted LV and returns its
// plaintext passphrase in a SecretBuf the caller must Destroy().
func (e *encryptionCoordinator) unwrap(ctx context.Context, lv *topolvmv1.LogicalVolume) (*resolvedKey, error) {
	if !e.enabled() {
		return nil, errors.New("encryption coordinator not configured")
	}
	if lv.Status.Encryption == nil || lv.Status.Encryption.ActiveKeyID == "" {
		return nil, fmt.Errorf("encrypted volume %s has no active key id", lv.Status.VolumeID)
	}
	ek, err := e.encKeys.Get(ctx, lv.Status.Encryption.ActiveKeyID)
	if err != nil {
		return nil, fmt.Errorf("get EncryptionKey %s: %w", lv.Status.Encryption.ActiveKeyID, err)
	}
	if ek.Status.WrappedDEK == "" {
		return nil, fmt.Errorf("EncryptionKey %s has no wrapped DEK yet", ek.Name)
	}
	ct, err := base64.StdEncoding.DecodeString(ek.Status.WrappedDEK)
	if err != nil {
		return nil, fmt.Errorf("decode wrapped DEK: %w", err)
	}
	pass, err := e.keyProvider.Unwrap(ctx, keyprovider.WrappedKey{
		Ciphertext: ct,
		KeyRef:     ek.Spec.KeyRef,
		KEKVersion: ek.Status.KEKVersion,
		Provider:   ek.Spec.Provider,
	}, ek.Status.BoundVolumeID)
	if err != nil {
		return nil, fmt.Errorf("unwrap DEK for %s: %w", ek.Name, err)
	}
	return &resolvedKey{pass: pass, keyID: ek.Name, keyslot: ek.Status.Keyslot}, nil
}

// openOrFormat ensures the LUKS header exists (formatting on first stage) and
// returns the mapper device path.
func (e *encryptionCoordinator) openOrFormat(ctx context.Context, lv *topolvmv1.LogicalVolume, devicePath string, rk *resolvedKey) (string, error) {
	dm := dmName(lv.Status.VolumeID)

	isLuks, err := e.crypt.IsLuks(ctx, devicePath)
	if err != nil {
		return "", err
	}
	if !isLuks {
		opts := crypt.FormatOpts{
			Cipher:  lv.Spec.Encryption.Cipher,
			KeySize: int(lv.Spec.Encryption.KeySize),
		}
		if err := e.crypt.Format(ctx, devicePath, rk.pass, opts); err != nil {
			return "", err
		}
		uuid, err := e.crypt.HeaderUUID(ctx, devicePath)
		if err != nil {
			return "", err
		}
		if err := e.markFormatted(ctx, lv.Name, uuid); err != nil {
			return "", err
		}
	}

	open, err := e.crypt.IsOpen(ctx, dm)
	if err != nil {
		return "", err
	}
	if !open {
		if _, err := e.crypt.Open(ctx, devicePath, dm, rk.pass); err != nil {
			return "", err
		}
	}
	if err := e.markOpened(ctx, lv.Name); err != nil {
		return "", err
	}
	return crypt.MapperPath(dm), nil
}

// RotateKeyslot performs an online passphrase rotation on an open or openable
// LUKS device. It adds a new keyslot bound to a freshly generated DEK,
// persists a new EncryptionKey CR, retires the previous slot, and finally
// kills the old one. Pre-rotation snapshots remain readable through their
// own pinned EncryptionKey copies.
func (e *encryptionCoordinator) RotateKeyslot(ctx context.Context, lv *topolvmv1.LogicalVolume, devicePath string) error {
	if !e.enabled() {
		return errors.New("encryption coordinator not configured")
	}
	old, err := e.unwrap(ctx, lv)
	if err != nil {
		return err
	}
	defer old.pass.Destroy()

	// Generate a fresh DEK and persist a new EncryptionKey CR before
	// touching the on-disk header so a crash leaves the old key still
	// usable.
	newPlain, newWrapped, err := e.keyProvider.GenerateDEK(ctx, keyprovider.KeyOpts{VolumeID: lv.Status.VolumeID, KeyRef: lv.Spec.Encryption.KeyRef})
	if err != nil {
		return fmt.Errorf("generate new DEK: %w", err)
	}
	defer newPlain.Destroy()

	newKeyName := fmt.Sprintf("vk-rot-%s", shortHash(fmt.Sprintf("%s-%d", lv.Status.VolumeID, lv.Status.Encryption.Keyslot+1)))
	if _, err := e.encKeys.Create(ctx, newKeyName, lv.Spec.Encryption.Provider, lv.Spec.Encryption.KeyRef, lv.Status.VolumeID, newWrapped.Ciphertext, newWrapped.KEKVersion, 0, []string{lv.Status.VolumeID}); err != nil {
		return fmt.Errorf("create rotated EncryptionKey: %w", err)
	}

	slot, err := e.crypt.AddKey(ctx, devicePath, old.pass, newPlain)
	if err != nil {
		return fmt.Errorf("luksAddKey: %w", err)
	}

	// Switch the LV to the new key before killing the old slot so a
	// concurrent unwrap on this node uses the new blob.
	if err := e.patchEncryptionStatus(ctx, lv.Name, func(es *topolvmv1.EncryptionStatus) {
		es.ActiveKeyID = newKeyName
		es.Keyslot = int32(slot)
	}); err != nil {
		return err
	}

	if err := e.crypt.KillSlot(ctx, devicePath, int(old.keyslot), newPlain); err != nil {
		return fmt.Errorf("luksKillSlot: %w", err)
	}
	// Best-effort retire the old key: drop its consumer if no snapshot pins it.
	if err := e.encKeys.RemoveConsumer(ctx, old.keyID, lv.Status.VolumeID); err != nil {
		return fmt.Errorf("retire old EncryptionKey consumer: %w", err)
	}
	if err := e.encKeys.MaybeDelete(ctx, old.keyID); err != nil {
		return fmt.Errorf("garbage-collect old EncryptionKey: %w", err)
	}
	return nil
}

func shortHash(s string) string {
	h := sha1.Sum([]byte(s))
	return hex.EncodeToString(h[:6])
}

// close releases the dm-crypt mapping if it is open.
func (e *encryptionCoordinator) close(ctx context.Context, volumeID string) error {
	dm := dmName(volumeID)
	open, err := e.crypt.IsOpen(ctx, dm)
	if err != nil {
		return err
	}
	if !open {
		return nil
	}
	return e.crypt.Close(ctx, dm)
}

// markFormatted records the LUKS header UUID and bumps the state to Formatted.
func (e *encryptionCoordinator) markFormatted(ctx context.Context, lvName, headerUUID string) error {
	return e.patchEncryptionStatus(ctx, lvName, func(es *topolvmv1.EncryptionStatus) {
		es.HeaderUUID = headerUUID
		es.State = topolvmv1.EncryptionFormatted
	})
}

// markOpened bumps the state to Opened.
func (e *encryptionCoordinator) markOpened(ctx context.Context, lvName string) error {
	return e.patchEncryptionStatus(ctx, lvName, func(es *topolvmv1.EncryptionStatus) {
		es.State = topolvmv1.EncryptionOpened
	})
}

func (e *encryptionCoordinator) patchEncryptionStatus(ctx context.Context, lvName string, mutate func(*topolvmv1.EncryptionStatus)) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		lv := &topolvmv1.LogicalVolume{}
		if err := e.k8sLV.GetByName(ctx, lvName, lv); err != nil {
			return err
		}
		if lv.Status.Encryption == nil {
			lv.Status.Encryption = &topolvmv1.EncryptionStatus{}
		}
		mutate(lv.Status.Encryption)
		return e.k8sLV.UpdateStatus(ctx, lv)
	})
}

var _ = client.Object((*topolvmv1.LogicalVolume)(nil))
