package sse

// Differential coverage for the tool_calls fast path: feedToolCalls must
// account a block exactly like the typed decode it replaced, for well-formed
// and malformed shapes alike. Both paths share the fallback, so any input has
// to agree - the fast path may only skip work, never change a verdict.

import (
	"bytes"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// toolCallState renders the analyzer state feedToolCalls may change, so two
// analyzers can be compared directly.
func toolCallState(a *Analyzer) string {
	indexes := make([]int, 0, len(a.toolCallIndexes))
	for index := range a.toolCallIndexes {
		indexes = append(indexes, index)
	}
	slices.Sort(indexes)
	return fmt.Sprintf("calls=%d indexes=%v names=%q", a.toolCalls, indexes, a.toolNames)
}

// seedToolCallState pre-loads a counted index and a retained name so the
// already-counted and duplicate-name branches are exercised too.
func seedToolCallState(a *Analyzer) {
	a.toolCalls = 3
	a.toolCallIndexes = map[int]struct{}{7: {}}
	a.retainToolName("seed")
}

func compareToolCallPaths(t *testing.T, block []byte) {
	t.Helper()
	var fast, decoded Analyzer
	seedToolCallState(&fast)
	seedToolCallState(&decoded)
	fast.feedToolCalls(block)
	decoded.feedToolCallsDecoded(block)
	if got, want := toolCallState(&fast), toolCallState(&decoded); got != want {
		t.Fatalf("fast path diverged for block %q:\nfast:    %s\ndecoded: %s", block, got, want)
	}
}

// toolCallFastPathCorpus covers every branch the lexical scanner can take:
// index forms accepted by jsonnum and rejected by it, id presence and last-wins
// duplicates, nested and escaped keys, wrong value types, and malformed or
// truncated blocks that must reach the fallback.
var toolCallFastPathCorpus = []string{
	`[]`,
	`[ ]`,
	`[{}]`,
	`[{"index":0}]`,
	`[{"index":0,"id":"call_1"}]`,
	`[{"index":0,"id":"call_1","function":{"name":"search"}}]`,
	`[{"index":0,"function":{"name":"search"}}]`,
	`[{"index":0,"function":{"name":""}}]`,
	`[{"index":0,"id":"","function":{"name":""}}]`,
	`[{"id":"call_1"}]`,
	`[{"id":""}]`,
	`[{"id":null}]`,
	`[{"function":{"name":"search"}}]`,
	`[{"index":0,"id":"a"},{"index":1,"id":"b"}]`,
	`[{"index":0,"id":"a"},{"index":0,"id":"b"}]`,
	`[{"index":0},{"index":0,"function":{"name":"later"}}]`,
	`[{"index":0.0,"id":"a"}]`,
	`[{"index":1e3,"id":"a"}]`,
	`[{"index":0.5,"id":"a"}]`,
	`[{"index":-1,"id":"a"}]`,
	`[{"index":1e20,"id":"a"}]`,
	`[{"index":"0","id":"a"}]`,
	`[{"index":null,"id":"a"}]`,
	`[{"index":true,"id":"a"}]`,
	`[{"index":{},"id":"a"}]`,
	`[{"index":[],"id":"a"}]`,
	`[{"index":01,"id":"a"}]`,
	`[{"index":+1,"id":"a"}]`,
	`[{"index":0,"index":1,"id":"a"}]`,
	`[{"iD":"a"}]`,
	`[{"ID":"a","Index":0}]`,
	`[{"index":0,"FUNCTION":{"NAME":"search"}}]`,
	`[{"index":0,"ID":"a","Id":""}]`,
	`[{"index":0,"ID":"a","Id":null}]`,
	`[{"index":0,"function":{"NAME":"a"},"function":{"Name":""}}]`,
	`[{"id":"a","id":""}]`,
	`[{"id":"","id":"b"}]`,
	`[{"id":"a","id":null}]`,
	`[{"id":7}]`,
	`[{"id":true}]`,
	`[{"function":null}]`,
	`[{"function":null,"index":0,"id":"a"}]`,
	`[{"function":{"name":"a"},"function":{}}]`,
	`[{"function":{},"function":{"name":"a"}}]`,
	`[{"function":{"name":null}}]`,
	`[{"function":{"name":5}}]`,
	`[{"function":"search"}]`,
	`[{"function":[]}]`,
	`[{"index":0,"function":{"name":"na\u006de"}}]`,
	`[{"index":0,"id":"call\u005f1","function":{"name":"search"}}]`,
	`[{"ind\u0065x":0,"id":"a"}]`,
	`[{"function":{"na\u006de":"search"}}]`,
	`[{"index":0,"function":{"name":"search","id":"nested"}}]`,
	`[{"index":0,"function":{"name":"search","arguments":"{\"q\":[1,2]}"}}]`,
	`[{"index":0,"id":"a","extra":{"index":9,"id":"x","function":{"name":"n"}}}]`,
	`[{"extra":[{"index":9}],"index":0,"id":"a"}]`,
	`[{"index":0,"function":{"name":"search"},"extra":true}]`,
	`[{"index":0,"function":{"name":"search"},"extra":null}]`,
	`[{"index":0,"function":{"name":"search"},"extra":-0.5e+2}]`,
	`[{"index":0,"function":{"name":"search"},"extra":false}]`,
	`[{"index":0,"function":{"name":"search"},"extra":[]}]`,
	`[{"index":0,"function":{"name":"search"},"extra":{}}]`,
	"[{\"index\":0,\"id\":\"a\",\"function\":{\"name\":\"tab\tname\"}}]",
	"[{\"index\":0,\"id\":\"a\",\"function\":{\"name\":\"bad\nescape\"}}]",
	`[{"index":0,"id":"a","function":{"name":"bad\q"}}]`,
	`[{"index":0,"id":"a","function":{"name":"bad\u12"}}]`,
	`[{"index":0,"id":"a","function":{"name":"bad\u12zz"}}]`,
	`[{"index":0,"id":"a","function":{"name":"终止"}}]`,
	"[{\"index\":0,\"id\":\"a\",\"function\":{\"name\":\"raw\xffbyte\"}}]",
	`  [ {"index" : 0 , "id" : "a" } ]  `,
	`[{"index":0}]x`,
	`[{"index":0}`,
	`[{"index":0,}]`,
	`[{"index":0} {"index":1}]`,
	`[,{"index":0}]`,
	`[{"index":0},]`,
	`[{"index":0},{}]`,
	`[{"index":0}]`,
	`null`,
	`{}`,
	`[1]`,
	`[null]`,
	`["index"]`,
	`[[{"index":0}]]`,
	`[{"index":0,"function":{"name":"` + strings.Repeat("x", 200) + `"}}]`,
	`[{"index":0,"function":{"name":"` + strings.Repeat("x", 128) + `a"}}]`,
	`[{"index":0,"function":{"name":"` + strings.Repeat("x", 128) + `b"}}]`,
	`[{"index":0,"function":{"name":"` + strings.Repeat("x", 128) + `a"}},` +
		`{"index":1,"function":{"name":"` + strings.Repeat("x", 128) + `a"}}]`,
}

func TestToolCallFastPathMatchesDecode(t *testing.T) {
	for _, block := range toolCallFastPathCorpus {
		compareToolCallPaths(t, []byte(block))
	}
}

// TestToolCallFastPathIndexBoundary pins the retained-index bound and the
// fallback counter across it, since the fast loop owns both.
func TestToolCallFastPathIndexBoundary(t *testing.T) {
	var block bytes.Buffer
	block.WriteByte('[')
	for index := range metrics.RetainedToolCallIDsMax {
		if index > 0 {
			block.WriteByte(',')
		}
		fmt.Fprintf(&block, `{"index":%d,"id":"c%d"}`, index, index)
	}
	block.WriteString(`]`)
	compareToolCallPaths(t, block.Bytes())
}

// TestToolCallFastPathRandomEquivalence mutates corpus blocks by byte so the
// plain run covers shapes the fixed table does not spell out.
func TestToolCallFastPathRandomEquivalence(t *testing.T) {
	const alphabet = `{}[],:"\ 0123456789.eE+-truefalsn`
	rng := rand.New(rand.NewSource(11))
	for iteration := 0; iteration < 4000; iteration++ {
		block := []byte(toolCallFastPathCorpus[rng.Intn(len(toolCallFastPathCorpus))])
		for edits := rng.Intn(4); edits >= 0; edits-- {
			switch rng.Intn(3) {
			case 0: // insert
				at := rng.Intn(len(block) + 1)
				c := alphabet[rng.Intn(len(alphabet))]
				block = append(block[:at], append([]byte{c}, block[at:]...)...)
			case 1: // delete
				if len(block) > 0 {
					at := rng.Intn(len(block))
					block = slices.Delete(block, at, at+1)
				}
			case 2: // replace
				if len(block) > 0 {
					block[rng.Intn(len(block))] = alphabet[rng.Intn(len(alphabet))]
				}
			}
		}
		compareToolCallPaths(t, block)
	}
}

func FuzzToolCallFastPathEquivalence(f *testing.F) {
	for _, seed := range toolCallFastPathCorpus {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, block []byte) {
		if len(block) > 1<<16 {
			t.Skip()
		}
		compareToolCallPaths(t, block)
	})
}
