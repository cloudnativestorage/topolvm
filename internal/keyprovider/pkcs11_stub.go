//go:build kms_pkcs11 && !kms_pkcs11_real

package keyprovider

import "errors"

// newCrypto11Resolver returns an error when the real backend has not been
// compiled in (no kms_pkcs11_real tag). The provider is still usable from
// tests via NewPKCS11ProviderWithResolver.
func newCrypto11Resolver(_ PKCS11Config) (KEKResolver, error) {
	return nil, errors.New("keyprovider/pkcs11: built without kms_pkcs11_real; rebuild with -tags=\"kms_pkcs11 kms_pkcs11_real\" to enable the real HSM backend")
}
