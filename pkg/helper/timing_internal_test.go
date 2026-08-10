package helper

import (
	"strings"
	"testing"
	"time"
)

// The breakdown only earns its place if it is off unless asked for, safe to
// call from the concurrent paths it instruments, and honest about the fact that
// overlapping phases sum to more than the wall clock.

func envFrom(pairs map[string]string) func(string) string {
	return func(key string) string { return pairs[key] }
}

func TestPhaseTimerIsOffUnlessAsked(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"", false},
		{"0", false},
		{"false", false},
		{"FALSE", false},
		{"1", true},
		{"yes", true},
	} {
		timer := newPhaseTimer(envFrom(map[string]string{timingEnv: tc.value}))
		if timer.enabled != tc.want {
			t.Errorf("%s=%q enabled=%v, want %v", timingEnv, tc.value, timer.enabled, tc.want)
		}
	}
}

// TestPhaseTimerDisabledCostsNothingAndReportsNothing: the call sites are
// unconditional, so a disabled timer has to be usable and silent rather than
// merely quiet.
func TestPhaseTimerDisabledCostsNothingAndReportsNothing(t *testing.T) {
	timer := newPhaseTimer(envFrom(nil))
	timer.phase("something")()

	var out strings.Builder
	timer.report(&out)
	if out.Len() != 0 {
		t.Errorf("a disabled timer wrote %q", out.String())
	}

	// A nil timer is what a Helper built by a test without NewHelper has, and
	// it must not panic through the instrumented paths.
	var nilTimer *phaseTimer
	nilTimer.phase("something")()
	nilTimer.report(&out)
}

func TestPhaseTimerAccumulatesPerPhase(t *testing.T) {
	timer := newPhaseTimer(envFrom(map[string]string{timingEnv: "1"}))
	for i := 0; i < 3; i++ {
		stop := timer.phase("fetch")
		time.Sleep(time.Millisecond)
		stop()
	}
	timer.phase("push")()

	var out strings.Builder
	timer.report(&out)
	got := out.String()

	if !strings.Contains(got, "(3 calls)") {
		t.Errorf("the three fetch phases were not counted:\n%s", got)
	}
	if !strings.Contains(got, "(1 call)") {
		t.Errorf("the single push phase should read \"1 call\", not \"1 calls\":\n%s", got)
	}
	// Slowest first, because the top line is the answer to the question the
	// output exists to answer.
	if strings.Index(got, "fetch") > strings.Index(got, "push") {
		t.Errorf("phases are not ordered slowest first:\n%s", got)
	}
	if !strings.Contains(got, "wall") {
		t.Errorf("no wall time reported:\n%s", got)
	}
	if !strings.Contains(got, "overlap") {
		t.Errorf("the output does not say the phases overlap, so the totals look wrong:\n%s", got)
	}
}

// TestPhaseTimerIsConcurrencySafe: every path it instruments fans out over a
// worker pool, so this is the normal case rather than an edge one.
func TestPhaseTimerIsConcurrencySafe(t *testing.T) {
	timer := newPhaseTimer(envFrom(map[string]string{timingEnv: "1"}))

	done := make(chan struct{})
	for i := 0; i < 16; i++ {
		go func() {
			defer close2(done)
			for j := 0; j < 50; j++ {
				timer.phase("concurrent")()
			}
		}()
	}
	for i := 0; i < 16; i++ {
		<-done
	}

	var out strings.Builder
	timer.report(&out)
	if !strings.Contains(out.String(), "(800 calls)") {
		t.Errorf("lost phases under concurrency:\n%s", out.String())
	}
}

// close2 sends rather than closes, so the counter above can wait on each
// goroutine individually.
func close2(ch chan struct{}) { ch <- struct{}{} }

func TestPhaseTimerReportsWhenNothingRan(t *testing.T) {
	timer := newPhaseTimer(envFrom(map[string]string{timingEnv: "1"}))
	var out strings.Builder
	timer.report(&out)
	if !strings.Contains(out.String(), "no phases recorded") {
		t.Errorf("an empty run should say so, not print a bare header:\n%s", out.String())
	}
}
