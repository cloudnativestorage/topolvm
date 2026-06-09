package keyprovider

import (
	"strings"
	"testing"
)

func TestRedactErrorBody(t *testing.T) {
	got := redact(`{"plaintext":"AAAA","ciphertext":"BBBB","key":"CCCC"}`)
	for _, s := range []string{"plaintext", "ciphertext", "key"} {
		if strings.Contains(got, s) {
			t.Fatalf("redact left %q in %q", s, got)
		}
	}
}
