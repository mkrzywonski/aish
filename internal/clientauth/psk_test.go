package clientauth

import (
	"encoding/hex"
	"testing"
)

func TestPSKDeterministic(t *testing.T) {
	psk, _ := hex.DecodeString("a224ce1a25d790662846c199e3f50ced1ba99719187f156429cf176b3870b868")
	id1, err := FromPSK(psk)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := FromPSK(psk)
	if err != nil {
		t.Fatal(err)
	}
	if id1.public != id2.public {
		t.Fatalf("PSK derivation not deterministic:\n  %s\n  %s", id1.public, id2.public)
	}
	t.Logf("PSK public key: %s", id1.public)
}
