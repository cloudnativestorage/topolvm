package driver

import (
	"testing"

	"github.com/topolvm/topolvm"
)

func TestParseEncryptionParameters_Disabled(t *testing.T) {
	cases := []map[string]string{
		nil,
		{},
		{topolvm.GetEncryptionStorageClassKey(): "false"},
		{topolvm.GetEncryptionStorageClassKey(): ""},
	}
	for _, c := range cases {
		got, err := parseEncryptionParameters(c)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != nil {
			t.Fatalf("expected nil EncryptionSpec, got %+v", got)
		}
	}
}

func TestParseEncryptionParameters_RequiresProvider(t *testing.T) {
	params := map[string]string{
		topolvm.GetEncryptionStorageClassKey(): "true",
	}
	_, err := parseEncryptionParameters(params)
	if err == nil {
		t.Fatal("expected error when provider is missing")
	}
}

func TestParseEncryptionParameters_RequiresKeyRef(t *testing.T) {
	params := map[string]string{
		topolvm.GetEncryptionStorageClassKey():  "true",
		topolvm.GetEncryptionKeyProviderKey():   "vault",
	}
	_, err := parseEncryptionParameters(params)
	if err == nil {
		t.Fatal("expected error when keyRef is missing")
	}
}

func TestParseEncryptionParameters_Integrity(t *testing.T) {
	params := map[string]string{
		topolvm.GetEncryptionStorageClassKey():  "true",
		topolvm.GetEncryptionKeyProviderKey():   "vault",
		topolvm.GetEncryptionKeyRefKey():        "k",
		topolvm.GetEncryptionIntegrityKey():     "hmac-sha256",
		topolvm.GetEncryptionIntegrityNoWipeKey(): "true",
	}
	spec, err := parseEncryptionParameters(params)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if spec.Integrity != "hmac-sha256" {
		t.Fatalf("integrity: %q", spec.Integrity)
	}
	if !spec.IntegrityNoWipe {
		t.Fatal("expected NoWipe=true")
	}
}

func TestParseEncryptionParameters_NoWipeRequiresIntegrity(t *testing.T) {
	params := map[string]string{
		topolvm.GetEncryptionStorageClassKey():    "true",
		topolvm.GetEncryptionKeyProviderKey():     "vault",
		topolvm.GetEncryptionKeyRefKey():          "k",
		topolvm.GetEncryptionIntegrityNoWipeKey(): "true",
	}
	_, err := parseEncryptionParameters(params)
	if err == nil {
		t.Fatal("expected error: NoWipe with no integrity")
	}
}

func TestParseEncryptionParameters_RejectsUnknownIntegrity(t *testing.T) {
	params := map[string]string{
		topolvm.GetEncryptionStorageClassKey(): "true",
		topolvm.GetEncryptionKeyProviderKey():  "vault",
		topolvm.GetEncryptionKeyRefKey():       "k",
		topolvm.GetEncryptionIntegrityKey():    "blake3",
	}
	_, err := parseEncryptionParameters(params)
	if err == nil {
		t.Fatal("expected error for unsupported integrity")
	}
}

func TestIntegrityMatches(t *testing.T) {
	cases := []struct {
		onDisk, want string
		ok           bool
	}{
		{"", "", true},
		{"(no)", "", true},
		{"none", "", true},
		{"hmac-sha256", "hmac-sha256", true},
		{"", "hmac-sha256", false},
		{"hmac-sha256", "", false},
	}
	for _, c := range cases {
		if got := integrityMatches(c.onDisk, c.want); got != c.ok {
			t.Fatalf("integrityMatches(%q,%q) = %v, want %v", c.onDisk, c.want, got, c.ok)
		}
	}
}

func TestParseEncryptionParameters_Defaults(t *testing.T) {
	params := map[string]string{
		topolvm.GetEncryptionStorageClassKey():  "true",
		topolvm.GetEncryptionKeyProviderKey():   "vault",
		topolvm.GetEncryptionKeyRefKey():        "transit/keys/topolvm",
	}
	spec, err := parseEncryptionParameters(params)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !spec.Enabled || spec.Provider != "vault" || spec.KeyRef != "transit/keys/topolvm" {
		t.Fatalf("unexpected: %+v", spec)
	}
	if spec.Cipher != "aes-xts-plain64" || spec.KeySize != 512 {
		t.Fatalf("defaults missing: %+v", spec)
	}
}

func TestEncryptionKeyObjectName_StableAndUnique(t *testing.T) {
	a1 := EncryptionKeyObjectName("pvc-1")
	a2 := EncryptionKeyObjectName("pvc-1")
	b := EncryptionKeyObjectName("pvc-2")
	if a1 != a2 {
		t.Fatal("name not stable across calls")
	}
	if a1 == b {
		t.Fatal("collision between distinct volume ids")
	}
	if len(a1) > 63 || len(a1) < 4 {
		t.Fatalf("invalid name length: %s", a1)
	}
}
