package driver

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/topolvm/topolvm"
	topolvmv1 "github.com/topolvm/topolvm/api/v1"
	"github.com/topolvm/topolvm/internal/crypt"
)

// parseEncryptionParameters reads the encryption-related StorageClass
// parameters from a CreateVolume request and returns an EncryptionSpec when
// the StorageClass opts in. It returns (nil, nil) when encryption is not
// requested so the existing unencrypted code path is untouched.
func parseEncryptionParameters(params map[string]string) (*topolvmv1.EncryptionSpec, error) {
	val := params[topolvm.GetEncryptionStorageClassKey()]
	if !isTruthy(val) {
		return nil, nil
	}
	provider := params[topolvm.GetEncryptionKeyProviderKey()]
	if provider == "" {
		return nil, fmt.Errorf("storage class requested encryption but %s is empty", topolvm.GetEncryptionKeyProviderKey())
	}
	keyRef := params[topolvm.GetEncryptionKeyRefKey()]
	if keyRef == "" {
		return nil, fmt.Errorf("storage class requested encryption but %s is empty", topolvm.GetEncryptionKeyRefKey())
	}
	cipher := params[topolvm.GetEncryptionCipherKey()]
	if cipher == "" {
		cipher = crypt.DefaultCipher
	}
	keySize := int32(crypt.DefaultKeySize)
	if v := params[topolvm.GetEncryptionKeySizeKey()]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("invalid %s=%q", topolvm.GetEncryptionKeySizeKey(), v)
		}
		keySize = int32(n)
	}
	return &topolvmv1.EncryptionSpec{
		Enabled:  true,
		Provider: provider,
		KeyRef:   keyRef,
		Cipher:   cipher,
		KeySize:  keySize,
	}, nil
}

func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// EncryptionKeyObjectName derives a stable EncryptionKey CR name from a
// volume id (or snapshot id). The "vk-" prefix and short hash keep names
// within k8s name limits while remaining unique.
func EncryptionKeyObjectName(volumeID string) string {
	h := sha1.Sum([]byte(volumeID))
	return "vk-" + hex.EncodeToString(h[:6])
}
