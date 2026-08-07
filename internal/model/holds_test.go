package model

import "testing"

// Holds is read by the version comparison to decide what may change under a running
// instance, and the zero value means "nothing can be sitting here". A new action type that
// forgets to declare itself gets that answer by default and the comparison silently stops
// reporting type changes under it — so the omission has to fail loudly here instead.
func TestHolds_EveryActionTypeIsDecided(t *testing.T) {
	decided := map[ActionType]bool{
		ActionTypeFetch:     true, // runs inside one advance: the zero value is the answer
		ActionTypeChild:     true,
		ActionTypeChildMap:  true,
		ActionTypeChildList: true,
		ActionTypeDelay:     true,
		ActionTypeExternal:  true,
	}
	for _, at := range AllActionTypes {
		if !decided[at] {
			t.Errorf("action type %q has no decided Holds: add a case to ActionType.Holds "+
				"and an entry here, or the comparison will treat it as holding nothing", at)
		}
	}
	if len(decided) != len(AllActionTypes) {
		t.Errorf("this test knows %d action types and AllActionTypes has %d; they must agree",
			len(decided), len(AllActionTypes))
	}
}

// The two halves are independent and both are load-bearing, so a change that collapses them
// has to fail: a delay holds an instance without holding data, an external holds both.
func TestHolds_ATimerIsNotAValue(t *testing.T) {
	delay := ActionTypeDelay.Holds()
	if !delay.Anything() {
		t.Error("a delay holds a live instance — an action type may not change under it")
	}
	if delay.Result {
		t.Error("a delay holds no value: it is WaitStateNone with a wake_at, so there is no " +
			"result schema to compare")
	}
	if got := ActionTypeExternal.Holds(); !got.Result || !got.Anything() {
		t.Error("an external holds both a live instance and a submitted result")
	}
	if ActionTypeFetch.Holds().Anything() {
		t.Error("a fetch completes inside one advance, so an instance at that task is at entry")
	}
}
