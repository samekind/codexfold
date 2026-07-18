package mountid

import (
	"strings"
	"testing"
)

func TestVersionTwoIdentityCarriesBuildSHA256(t *testing.T) {
	build := strings.Repeat("a", 64)
	value, err := New(build)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := Parse([]byte(value))
	if err != nil {
		t.Fatal(err)
	}
	if identity.Version != 2 || identity.BuildSHA256 != build || len(identity.Nonce) != 32 {
		t.Fatalf("identity = %#v", identity)
	}
}

func TestLegacyIdentityRemainsProbeCompatibleWithoutBuild(t *testing.T) {
	identity, err := Parse([]byte("codexfold-v1:" + strings.Repeat("a", 32)))
	if err != nil {
		t.Fatal(err)
	}
	if identity.Version != 1 || identity.BuildSHA256 != "" {
		t.Fatalf("legacy identity = %#v", identity)
	}
}

func TestIdentityRejectsInvalidBuild(t *testing.T) {
	if _, err := New("invalid"); err == nil {
		t.Fatal("invalid build SHA-256 was accepted")
	}
	if _, err := Parse([]byte("codexfold-v2:" + strings.Repeat("a", 32) + ":invalid")); err == nil {
		t.Fatal("invalid v2 payload was accepted")
	}
}
