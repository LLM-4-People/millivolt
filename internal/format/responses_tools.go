package format

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"sort"
	"strings"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

// announcedToolCall is one open, unclaimed function call in output order.
type announcedToolCall struct {
	key  string
	idx  int
	name string
}

// resolveToolCallKey maps a terminal function call to the announced call's
// canonical position-qualified key. The identity spelling wins over the wire
// position, so a terminal array that renumbers items still reaches the block;
// position is the fallback only when the identity is absent or ambiguous.
func (d *ResponsesStreamDecoder) resolveToolCallKey(callID, itemID string, outputIndex int) string {
	key := functionCallKey(callID, itemID, outputIndex)
	if identity := functionCallIdentityKey(callID, itemID); identity != "" {
		if canonical, ok := d.toolAliases[identity]; ok && canonical != "" {
			return canonical
		}
	}
	return key
}

// openAnnouncedToolCalls lists the open announced function calls the terminal
// walk has not claimed yet, in output order.
func (d *ResponsesStreamDecoder) openAnnouncedToolCalls(claimed map[string]bool) []announcedToolCall {
	positions := make([]int, 0, len(d.keyByOutput))
	for outputIndex := range d.keyByOutput {
		positions = append(positions, outputIndex)
	}
	sort.Ints(positions)
	out := make([]announcedToolCall, 0, len(positions))
	for _, outputIndex := range positions {
		key := d.keyByOutput[outputIndex]
		if !d.openToolKeys[key] || claimed[key] {
			continue
		}
		idx, ok := d.itemBlockByKey[key]
		if !ok || !d.open[idx] {
			continue
		}
		out = append(out, announcedToolCall{key: key, idx: idx, name: d.toolNames[key]})
	}
	return out
}

// terminalToolIdentityCompatible reports whether an identified terminal call
// may still be the announced call. A call id on both sides with different
// values is a different call; the name fallback is for mixed item-id and
// call-id spellings and for id-less entries.
func terminalToolIdentityCompatible(callID, itemID, candidateKey string) bool {
	if callID != "" && strings.HasPrefix(candidateKey, "call:") {
		return false
	}
	if callID == "" && itemID != "" && strings.HasPrefix(candidateKey, "item:") {
		return false
	}
	return true
}

// toolArgumentsFingerprint hashes the canonical argument value when it is
// small enough to canonicalize cheaply. Whitespace is removed, object keys are
// sorted, and numeric literals are preserved; a missing, null or whitespace
// value is the empty object the target emitted. An invalid, partial, non-object
// or over-bound value cannot be canonicalized within the memory guard, so its
// raw bytes are hashed instead.
func toolArgumentsFingerprint(raw string) [sha256.Size]byte {
	if toolArgumentsMissing(raw) {
		return sha256.Sum256([]byte("{}"))
	}
	data := []byte(raw)
	if !toolArgumentsWithinCanonicalBounds(data) {
		return sha256.Sum256(data)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, data); err != nil {
		return sha256.Sum256(data)
	}
	dec := json.NewDecoder(bytes.NewReader(compact.Bytes()))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return sha256.Sum256(data)
	}
	if _, err := dec.Token(); err != io.EOF {
		return sha256.Sum256(data)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return sha256.Sum256(data)
	}
	return sha256.Sum256(canonical)
}

// closedToolCallRepeat reports whether this terminal entry repeats the named
// announced call whose block already closed. A name mismatch at a reused
// position is a different call. An id-less call has only its name and
// effective arguments, so both must agree.
func (d *ResponsesStreamDecoder) closedToolCallRepeat(key string, item responsesWireItem) bool {
	if d.toolNames[key] != item.Name {
		return false
	}
	if strings.HasPrefix(key, "output:") {
		want, ok := d.toolArgsHash[key]
		return ok && want == toolArgumentsFingerprint(item.Arguments)
	}
	return true
}

// matchesClosedIdlessCall reports whether an unmatched id-less terminal entry
// repeats a closed id-less announced call by name and canonical argument
// value.
func (d *ResponsesStreamDecoder) matchesClosedIdlessCall(item responsesWireItem) bool {
	if item.Name == "" {
		return false
	}
	return d.closedIdlessCalls[item.Name][toolArgumentsFingerprint(item.Arguments)] > 0
}

// matchTerminalToolCall resolves a terminal function call that did not match
// by identity. A named call matches only an open announced call with the same
// name; an id-less terminal item then uses its wire position, a lone open call,
// or the first open id-less call in output order. A named item never falls back
// to a differently named open call, because the terminal array position is not
// an identity.
func (d *ResponsesStreamDecoder) matchTerminalToolCall(item responsesWireItem, index int, claimed map[string]bool) (string, int, bool) {
	open := d.openAnnouncedToolCalls(claimed)
	if item.Name != "" {
		named := make([]announcedToolCall, 0, len(open))
		for _, candidate := range open {
			if candidate.name == item.Name {
				named = append(named, candidate)
			}
		}
		switch len(named) {
		case 1:
			if terminalToolIdentityCompatible(item.CallID, item.ID, named[0].key) {
				return named[0].key, named[0].idx, true
			}
		case 0:
			return "", 0, false
		default:
			// Several open calls share the name. Only the announcement at the
			// terminal position can disambiguate them, and only when that
			// announcement is one of the same-name candidates and the terminal
			// identity can address it. Position is not an identity: an
			// unannounced call at the same position must not take the block.
			if positional, ok := d.keyByOutput[index]; ok {
				for _, candidate := range named {
					if candidate.key == positional &&
						terminalToolIdentityCompatible(item.CallID, item.ID, candidate.key) {
						return candidate.key, candidate.idx, true
					}
				}
			}
		}
		return "", 0, false
	}
	if item.CallID != "" || item.ID != "" {
		return "", 0, false
	}
	if fallback, exists := d.keyByOutput[index]; exists && d.openToolKeys[fallback] && !claimed[fallback] {
		if fallbackIdx, open := d.itemBlockByKey[fallback]; open && d.open[fallbackIdx] {
			return fallback, fallbackIdx, true
		}
	}
	if len(open) == 1 {
		return open[0].key, open[0].idx, true
	}
	for _, candidate := range open {
		if strings.HasPrefix(candidate.key, "output:") {
			return candidate.key, candidate.idx, true
		}
	}
	return "", 0, false
}

// toolArgumentsDelta records one incremental argument delta. The buffered
// output_item.added payload is emitted first, then the delta is appended
// verbatim: OpenAI's document says each delta is added to the previous value,
// so a prefix heuristic would corrupt a legitimate first delta that happens to
// start with the inline prefix.
func (d *ResponsesStreamDecoder) toolArgumentsDelta(key string, blockIndex int, delta string) {
	if delta == "" || key == "" {
		return
	}
	if strings.TrimSpace(delta) != "" {
		d.argumentEvidence[key] = true
	}
	if d.argsEmitted[key] == nil {
		if initial := d.initialArgs[key]; initial != "" {
			d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: blockIndex, Delta: &ir.Delta{Arguments: initial}})
			d.recordEmittedArguments(key, initial)
		}
		d.dropInitialArgs(key)
	}
	d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: blockIndex, Delta: &ir.Delta{Arguments: delta}})
	d.recordEmittedArguments(key, delta)
}

// flushToolArguments reconciles the authoritative done/terminal arguments
// with the bytes already streamed. A full value that strictly extends the
// emitted prefix appends only the missing suffix; a divergent value keeps the
// already-committed bytes. A complete retained value must be valid JSON. Once
// coverage is partial, the retained bytes are only a truncated prefix of what
// the client already received: they cannot be reconciled or validated as the
// final value, so the streamed bytes stand and reconciliation stops.
func (d *ResponsesStreamDecoder) flushToolArguments(key string, blockIndex int, full string, policy responsesToolArgumentPolicy) error {
	if key == "" {
		return nil
	}
	if !toolArgumentsMissing(full) {
		d.argumentEvidence[key] = true
	}
	if d.argsEmitted[key] != nil {
		emitted, complete := d.emittedArguments(key)
		if !complete {
			return nil
		}
		if policy == responsesToolArgumentsRequired && !d.argumentEvidence[key] {
			return errToolArgumentsMissing
		}
		if full != "" && len(full) > len(emitted) && strings.HasPrefix(full, emitted) {
			if policy == responsesToolArgumentsSkip {
				// Only append a suffix whose complete value is a valid tool
				// object; a partial terminal value must not reach the wire.
				if err := validateStreamedToolArguments(full); err != nil {
					return nil
				}
			}
			suffix := full[len(emitted):]
			d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: blockIndex, Delta: &ir.Delta{Arguments: suffix}})
			d.recordEmittedArguments(key, suffix)
			emitted += suffix
		}
		if policy == responsesToolArgumentsRequired && strings.TrimSpace(emitted) == "" {
			// A whitespace-only delta supplied no argument evidence. Empty
			// bytes are allowed only when nothing was emitted; whitespace
			// already reached the client and cannot become an executable
			// object.
			return errToolArgumentsMissing
		}
		if policy == responsesToolArgumentsSkip {
			// The bytes already streamed are the client's only copy. A
			// non-clean terminal must not reclassify them.
			return nil
		}
		// Those bytes already reached the client, so the normalizing parser
		// must not run: a completed streamed null or array fails closed
		// instead of passing as the normalized object.
		return validateStreamedToolArguments(emitted)
	}

	args := full
	if args == "" {
		args = d.initialArgs[key]
	}
	d.dropInitialArgs(key)
	if !toolArgumentsMissing(args) {
		d.argumentEvidence[key] = true
	}
	if policy == responsesToolArgumentsRequired && !d.argumentEvidence[key] {
		return errToolArgumentsMissing
	}
	if d.argsEmitted[key] == nil {
		d.argsEmitted[key] = &toolArgumentCoverage{}
	}
	normalized, err := parseToolArguments(args)
	if err != nil {
		if policy == responsesToolArgumentsSkip {
			return nil
		}
		return err
	}
	args = string(normalized)
	if args != "{}" {
		d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: blockIndex, Delta: &ir.Delta{Arguments: args}})
	}
	d.recordEmittedArguments(key, args)
	return nil
}

// recordEmittedArguments adds argument bytes to the bounded coverage for one
// item identity. Coverage past toolInitialBytesMax is not retained.
func (d *ResponsesStreamDecoder) recordEmittedArguments(key, value string) {
	if key == "" || value == "" {
		return
	}
	cov := d.argsEmitted[key]
	if cov == nil {
		cov = &toolArgumentCoverage{}
		d.argsEmitted[key] = cov
	}
	if cov.partial {
		return
	}
	remaining := toolInitialBytesMax - d.argsEmittedBytes
	if len(value) > remaining {
		if remaining > 0 {
			cov.emitted.WriteString(value[:remaining])
			d.argsEmittedBytes += remaining
		}
		cov.partial = true
		return
	}
	cov.emitted.WriteString(value)
	d.argsEmittedBytes += len(value)
}

// emittedArguments returns the retained coverage and whether it is complete.
func (d *ResponsesStreamDecoder) emittedArguments(key string) (string, bool) {
	cov := d.argsEmitted[key]
	if cov == nil {
		return "", true
	}
	return cov.emitted.String(), !cov.partial
}

// dropInitialArgs releases the buffered output_item.added arguments for one
// item identity and its share of the initial-arguments bound.
func (d *ResponsesStreamDecoder) dropInitialArgs(key string) {
	if args, ok := d.initialArgs[key]; ok {
		d.initialArgsBytes -= len(args)
		delete(d.initialArgs, key)
	}
}

// clearToolState releases every per-call entry once a function call closes.
func (d *ResponsesStreamDecoder) clearToolState(key string) {
	d.dropInitialArgs(key)
	delete(d.argumentEvidence, key)
	wasOpen := d.openToolKeys[key]
	delete(d.openToolKeys, key)
	// Keep the name and, for an id-less call, a hash of the emitted arguments.
	// A terminal repeat after the block closes still has to be recognized.
	emitted := "{}"
	if cov := d.argsEmitted[key]; cov != nil {
		emitted = cov.emitted.String()
		d.argsEmittedBytes -= cov.emitted.Len()
		delete(d.argsEmitted, key)
	}
	if wasOpen && strings.HasPrefix(key, "output:") {
		sum := toolArgumentsFingerprint(emitted)
		d.toolArgsHash[key] = sum
		name := d.toolNames[key]
		if d.closedIdlessCalls[name] == nil {
			d.closedIdlessCalls[name] = map[[sha256.Size]byte]int{}
		}
		d.closedIdlessCalls[name][sum]++
	}
}

// startPartBlock starts the target block for an announced content part when
// it has not started yet. A part announced with an empty start payload is
// deferred; the first delta or done event starts it from the stored seed.
func (d *ResponsesStreamDecoder) startPartBlock(key string) (int, bool) {
	if idx, ok := d.partBlockByKey[key]; ok {
		return idx, d.open[idx]
	}
	block, ok := d.partSeed[key]
	if !ok {
		return 0, false
	}
	pos, ok := d.partPosition[key]
	if !ok {
		return 0, false
	}
	idx := d.newBlock()
	d.partBlock[pos] = idx
	d.partBlockByKey[key] = idx
	d.keyByPart[pos] = key
	delete(d.partSeed, key)
	d.q.push(&ir.Event{Type: ir.EventBlockStart, Index: idx, Block: &block})
	return idx, true
}
