package format

import (
	"errors"
	"math"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func countField(field int, n uint64) []byte {
	return appendVarint(appendTag(nil, field, wireVarint), n)
}

func TestRunRejectsUnrepresentableProtoCounts(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		r := &CursorRun{}
		var err error
		if checkpoint {
			raw := appendMessage(nil, fConversationStateTokenDetails, countField(fTokenDetailsUsedTokens, math.MaxUint64))
			err = r.handleCheckpointUpdate(&protoField{wire: wireBytes, raw: raw})
		} else {
			raw := appendMessage(nil, fInteractionUpdateTokenDelta, countField(fTokenDeltaTokens, math.MaxUint64))
			err = r.handleInteractionUpdate(&protoField{wire: wireBytes, raw: raw}, func(runEvent) bool { return true })
		}
		if !errors.Is(err, metrics.ErrMetricRange) {
			t.Fatalf("checkpoint=%v accepted count outside int64: %v", checkpoint, err)
		}
		result := r.finish(TurnErrored, nil, err)
		if result.Prompt != 0 || result.Output != 0 {
			t.Fatalf("invalid usage exposed partial counts: %+v", result)
		}
	}
}

func TestRunCountAccumulationRejectsOverflowAtomically(t *testing.T) {
	r := &CursorRun{usagePrompt: 5, usageDeltaSum: math.MaxInt64}
	raw := appendMessage(nil, fInteractionUpdateTokenDelta, countField(fTokenDeltaTokens, 1))
	err := r.handleInteractionUpdate(&protoField{wire: wireBytes, raw: raw}, func(runEvent) bool { return true })
	if !errors.Is(err, metrics.ErrMetricRange) {
		t.Fatalf("accepted output overflow: %v", err)
	}
	if r.usageDeltaSum != math.MaxInt64 {
		t.Fatalf("failed update mutated counter to %d", r.usageDeltaSum)
	}
	if result := r.finish(TurnErrored, nil, err); result.Prompt != 0 || result.Output != 0 {
		t.Fatalf("partial usage exposed on failure: %+v", result)
	}
}
