package fold

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestDiskDigestSetCountsExactUniqueDigests(t *testing.T) {
	set, err := newDiskDigestSet()
	if err != nil {
		t.Fatal(err)
	}
	defer set.Close()
	for _, value := range []string{"alpha", "beta", "alpha", "gamma", "beta"} {
		digest := sha256.Sum256([]byte(value))
		if err := set.Add(hex.EncodeToString(digest[:])); err != nil {
			t.Fatal(err)
		}
	}
	count, err := set.Count(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("unique digest count = %d, want 3", count)
	}
}

func TestDiskDigestSetRejectsInvalidDigest(t *testing.T) {
	set, err := newDiskDigestSet()
	if err != nil {
		t.Fatal(err)
	}
	defer set.Close()
	if err := set.Add("not-a-digest"); err == nil {
		t.Fatal("invalid digest was accepted")
	}
}
