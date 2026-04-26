package worker

import (
	"testing"
	"time"
)

// retryDelayWithJitter should bound the delay to [base/2, 1.5*base]
// and should actually produce variance across calls — otherwise we're
// right back to the thundering-herd bug.
func TestRetryDelayWithJitter_BoundsAndVariance(t *testing.T) {
	const n = 4 // base = 16s
	base := time.Duration(1<<uint(n)) * time.Second

	seen := map[time.Duration]struct{}{}
	for i := 0; i < 200; i++ {
		d := retryDelayWithJitter(n, nil, nil)
		if d < base/2 || d >= base/2+base {
			t.Fatalf("delay %v out of [%v, %v)", d, base/2, base/2+base)
		}
		seen[d] = struct{}{}
	}
	if len(seen) < 50 {
		t.Errorf("expected broad variance, got %d distinct values out of 200 samples", len(seen))
	}
}

// Delay is hard-capped at 10 minutes. The previous implementation capped
// `base` before jitter, so the returned delay could reach 15 minutes —
// breaking the "cap 10 min" claim in DESIGN.md. The cap now applies to
// the POST-jitter value.
func TestRetryDelayWithJitter_HardCap_10m(t *testing.T) {
	// n = 20: raw base is ~12 days; pre-cap is 10m.
	for i := 0; i < 200; i++ {
		d := retryDelayWithJitter(20, nil, nil)
		if d > 10*time.Minute {
			t.Fatalf("delay %v exceeds 10m cap", d)
		}
		if d < time.Second {
			t.Fatalf("delay %v below 1s floor", d)
		}
	}
}

// Large retry counts must not panic (the previous code called
// rand.Int63n on a negative or zero base — the clamp at n=30 prevents
// 1<<n from overflowing int64 nanoseconds).
func TestRetryDelayWithJitter_LargeNDoesNotPanic(t *testing.T) {
	for _, n := range []int{30, 40, 63, 100, 1 << 20} {
		n := n
		t.Run("n", func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic on n=%d: %v", n, r)
				}
			}()
			d := retryDelayWithJitter(n, nil, nil)
			if d < time.Second || d > 10*time.Minute {
				t.Errorf("n=%d returned %v, want [1s, 10m]", n, d)
			}
		})
	}
}

// Negative n (shouldn't happen but defensively handled) must not crash.
func TestRetryDelayWithJitter_NegativeN(t *testing.T) {
	d := retryDelayWithJitter(-5, nil, nil)
	// n<0 is clamped to 0, so base=1s and delay is in [0.5s, 1.5s)
	// but the floor clamps to 1s.
	if d < time.Second || d > 2*time.Second {
		t.Errorf("negative n returned %v, want near 1s", d)
	}
}
