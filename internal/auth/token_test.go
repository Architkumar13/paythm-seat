package auth

import (
	"testing"
	"time"
)

func TestTokenRoundTrip(t *testing.T) {
	issuer := NewIssuer("test-secret-value-32bytes", time.Hour)
	signed, err := issuer.Sign("user-1")
	if err != nil {
		t.Fatal(err)
	}
	got, err := issuer.Parse(signed)
	if err != nil {
		t.Fatal(err)
	}
	if got != "user-1" {
		t.Fatalf("user = %s", got)
	}
	if _, err := issuer.Parse(signed + "x"); err == nil {
		t.Fatal("expected tampered token to fail")
	}
}

func TestSecretEqual(t *testing.T) {
	if !SecretEqual("paytm-demo-admin", "paytm-demo-admin") {
		t.Fatal("equal secrets did not match")
	}
	if SecretEqual("paytm-demo-admin", "paytm-demo-admi") {
		t.Fatal("unequal secrets matched")
	}
}
