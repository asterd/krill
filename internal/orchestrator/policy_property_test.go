package orchestrator

import (
	"testing"
	"testing/quick"
)

func TestProperty_NoInfiniteHandoffLoopsUnderBoundedPolicy(t *testing.T) {
	fn := func(raw uint8) bool {
		maxHops := int(raw%8) + 1
		policy := HandoffPolicy{MaxHops: maxHops}
		state := WorkflowState{}

		for hop := 1; hop <= 128; hop++ {
			decision := policy.Evaluate(HandoffCommand{Hop: hop}, state)
			if !decision.Allow {
				return hop == maxHops+1
			}
			state.Hop = hop
		}
		return false
	}
	if err := quick.Check(fn, &quick.Config{MaxCount: 128}); err != nil {
		t.Fatal(err)
	}
}
