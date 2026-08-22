package engine

import (
	"testing"
	"time"

	"genroc/internal/model"
)

// nominal is the un-jittered delay backoff is allowed to land under, written independently
// of the implementation: base scaled by factor per attempt, clamped at the ceiling.
func nominal(attempt int, base time.Duration, factor float64, ceiling time.Duration) time.Duration {
	d := float64(base)
	for i := 1; i < attempt; i++ {
		d *= factor
		if d >= float64(ceiling) {
			return ceiling
		}
	}
	if d >= float64(ceiling) {
		return ceiling
	}
	return time.Duration(d)
}

func TestBackoff_StaysWithinTheNominalWindow(t *testing.T) {
	curves := []struct {
		name    string
		base    time.Duration
		factor  float64
		ceiling time.Duration
	}{
		{"defaults", model.DefaultRetryDelay, model.DefaultRetryFactor, model.DefaultRetryMaxDelay},
		{"constant", 30 * time.Second, 1, 5 * time.Minute},
		{"slow start, high ceiling", time.Minute, 3, time.Hour},
		{"base above the default ceiling", time.Hour, 2, time.Hour},
	}
	// Includes the attempt counts where the old shift-based curve overflowed: 63 wrapped
	// negative and 64+ shifted to zero, both of which retried with no backoff at all.
	for _, c := range curves {
		for _, attempt := range []int{-1, 0, 1, 2, 8, 9, 10, 33, 62, 63, 64, 100, 1000} {
			want := nominal(attempt, c.base, c.factor, c.ceiling)
			for i := 0; i < 50; i++ {
				got := backoff(attempt, c.base, c.factor, c.ceiling)
				if got < want/2 || got > want {
					t.Fatalf("backoff(%d, %s) = %v, want within [%v, %v]: a delay outside the window is either "+
						"a hot retry loop or a backoff past the authored ceiling", attempt, c.name, got, want/2, want)
				}
			}
		}
	}
}

func TestBackoff_IsJittered(t *testing.T) {
	seen := map[time.Duration]bool{}
	for i := 0; i < 50; i++ {
		seen[backoff(6, model.DefaultRetryDelay, model.DefaultRetryFactor, model.DefaultRetryMaxDelay)] = true
	}
	if len(seen) < 2 {
		t.Fatalf("backoff returned one value across 50 calls; without jitter every instance that "+
			"failed on the same outage wakes at the same instant (%v)", seen)
	}
}

// The first retry waits the authored delay, not the delay already scaled once. Every other
// engine documents `delay` as the wait before the first retry, and an off-by-one here is
// invisible: it just makes every curve start one step in.
func TestBackoff_FirstRetryWaitsTheBaseDelay(t *testing.T) {
	base := 30 * time.Second
	got := backoff(1, base, 4, time.Hour)
	if got < base/2 || got > base {
		t.Fatalf("first retry = %v, want within [%v, %v] (the base delay, jittered)", got, base/2, base)
	}
}

// --immediate-retries must override the authored curve, not merely the default one, or the
// tick suites park for minutes on any definition that sets a delay.
func TestRetryDelay_ImmediateRetriesOverridesAnAuthoredCurve(t *testing.T) {
	hour, err := model.ParseRetryDuration("1h")
	if err != nil {
		t.Fatalf("ParseRetryDuration: %v", err)
	}
	policy, err := model.Retry{Attempts: model.RetryCount(3), Delay: hour}.Resolve(nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if got := (&Engine{immediateRetries: true}).retryDelay(1, policy); got != 0 {
		t.Fatalf("with immediate retries the delay is %v, want 0", got)
	}
	if got := (&Engine{}).retryDelay(1, policy); got < 30*time.Minute || got > time.Hour {
		t.Fatalf("without it the delay is %v, want the authored hour (jittered to [30m, 1h])", got)
	}
}

// An unset slot must read as its default, not as its zero value: a zero base is a hot retry
// loop and a zero ceiling clamps every wait to nothing.
func TestRetryDefaults_ApplyPerSlot(t *testing.T) {
	only, err := model.Retry{Attempts: model.RetryCount(3)}.Resolve(nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if only.Base != model.DefaultRetryDelay || only.Factor != model.DefaultRetryFactor || only.Ceiling != model.DefaultRetryMaxDelay {
		t.Fatalf("bare attempts resolved to %v/%v/%v, want the default curve %v/%v/%v",
			only.Base, only.Factor, only.Ceiling,
			model.DefaultRetryDelay, model.DefaultRetryFactor, model.DefaultRetryMaxDelay)
	}

	hour, err := model.ParseRetryDuration("1h")
	if err != nil {
		t.Fatalf("ParseRetryDuration: %v", err)
	}
	slow, err := model.Retry{Attempts: model.RetryCount(3), Delay: hour}.Resolve(nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if slow.Ceiling != time.Hour {
		t.Fatalf("delay 1h with no max_delay resolved to ceiling %v; the 5m default would clamp the "+
			"only slot the author set back to 5m", slow.Ceiling)
	}
}
