package fingerprint

import (
	"context"
	"testing"
	"time"
)

func TestJitterDisabledIsNoop(t *testing.T) {
	j := Jitter{Enabled: false}
	start := time.Now()
	j.Apply(context.Background(), "navigation")
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("disabled jitter slept %v", elapsed)
	}
	if f := j.DelayFactor(); f != 1.0 {
		t.Fatalf("disabled DelayFactor = %v want 1.0", f)
	}
}

func TestJitterApplyRespectsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	j := Jitter{Enabled: true}
	start := time.Now()
	j.Apply(ctx, "navigation")
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("cancelled context still slept %v", elapsed)
	}
}

func TestJitterApplyKnownProfileSleeps(t *testing.T) {
	// api_call 区间为 50–300ms：至少应睡满下界（留一点调度余量）。
	j := Jitter{Enabled: true}
	start := time.Now()
	j.Apply(context.Background(), "api_call")
	elapsed := time.Since(start)
	if elapsed < 40*time.Millisecond {
		t.Fatalf("api_call slept only %v, want >= ~50ms", elapsed)
	}
	if elapsed > time.Second {
		t.Fatalf("api_call slept way too long: %v", elapsed)
	}
}

func TestJitterApplyUnknownProfileUsesDefault(t *testing.T) {
	// 未知场景走默认区间（50–200ms），不应 panic 也不应立即返回。
	j := Jitter{Enabled: true}
	start := time.Now()
	j.Apply(context.Background(), "no-such-profile")
	elapsed := time.Since(start)
	if elapsed < 40*time.Millisecond {
		t.Fatalf("default profile slept only %v", elapsed)
	}
}

func TestJitterDelayFactorRange(t *testing.T) {
	j := Jitter{Enabled: true}
	for i := 0; i < 100; i++ {
		f := j.DelayFactor()
		if f < 0.8 || f >= 1.2 {
			t.Fatalf("DelayFactor out of [0.8,1.2): %v", f)
		}
	}
}
