package gitea

import (
	"strings"
	"testing"
)

func TestSign_RoundTripsThroughVerifySignature(t *testing.T) {
	secret := "shared-secret"
	body := []byte(`{"action":"created"}`)
	sig := Sign(secret, body)
	if !strings.HasPrefix(sig, "sha256=") {
		t.Fatalf("Sign should return sha256=...; got %q", sig)
	}
	if !VerifySignature(secret, sig, body) {
		t.Fatalf("VerifySignature rejected the value Sign produced: %q", sig)
	}
	if VerifySignature("other-secret", sig, body) {
		t.Fatalf("VerifySignature accepted a wrong secret")
	}
	if VerifySignature(secret, sig, []byte(`{"action":"edited"}`)) {
		t.Fatalf("VerifySignature accepted a wrong body")
	}
}
