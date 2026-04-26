package idempotency

import (
	"testing"
)

func TestHashRequest_StableAndDifferent(t *testing.T) {
	a := HashRequest([]byte(`{"x":1}`))
	b := HashRequest([]byte(`{"x":1}`))
	c := HashRequest([]byte(`{"x":2}`))
	if a != b {
		t.Errorf("same body should hash same: %s vs %s", a, b)
	}
	if a == c {
		t.Errorf("different body should hash differently")
	}
	if len(a) != 64 {
		t.Errorf("sha256 hex should be 64 chars, got %d", len(a))
	}
}
