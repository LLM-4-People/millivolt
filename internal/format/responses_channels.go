package format

import (
	"sort"
	"strconv"
	"strings"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

// partChannel is one independently concatenated content channel of an item.
// Terminal reconciliation never compares text bytes with refusal or reasoning
// bytes, and reasoning summary parts form their own channel.
type partChannel uint8

const (
	partChannelText partChannel = iota
	partChannelRefusal
	partChannelReasoning
)

// responsesPartBlock maps one Responses content-part payload to its canonical
// block. It reports false for a provider part with no target representation.
// The added and done events share this owner so a part's channel cannot change
// between them.
func responsesPartBlock(typ, text, refusal string) (ir.Block, bool) {
	switch typ {
	case "output_text", "input_text", "text":
		return ir.Block{Type: ir.BlockText, Text: text}, true
	case "refusal":
		return ir.Block{Type: ir.BlockRefusal, Refusal: refusal}, true
	case "reasoning_text":
		return ir.Block{
			Type: ir.BlockReasoning,
			Reasoning: &ir.Reasoning{
				Text: text, Provider: ir.Responses,
			},
		}, true
	}
	return ir.Block{}, false
}

// responsesPartBlockStart returns the type-only block a queued done payload
// starts. The payload's value is emitted as a delta after the start, so the
// block must not repeat it in its start payload.
func responsesPartBlockStart(block ir.Block) ir.Block {
	switch block.Type {
	case ir.BlockReasoning:
		return ir.Block{Type: ir.BlockReasoning, Reasoning: &ir.Reasoning{Provider: ir.Responses}}
	case ir.BlockRefusal:
		return ir.Block{Type: ir.BlockRefusal}
	}
	return ir.Block{Type: ir.BlockText}
}

// responsesPartValue returns the text a mapped part carries on its channel.
func responsesPartValue(block ir.Block) string {
	switch block.Type {
	case ir.BlockRefusal:
		return block.Refusal
	case ir.BlockReasoning:
		if block.Reasoning != nil {
			return block.Reasoning.Text
		}
		return ""
	}
	return block.Text
}

func partChannelForBlock(t ir.BlockType) partChannel {
	switch t {
	case ir.BlockRefusal:
		return partChannelRefusal
	case ir.BlockReasoning:
		return partChannelReasoning
	}
	return partChannelText
}

// channelPart is one part of a concatenated channel. key is the target part
// identity (content part key or summary part key), index is its source order,
// and order is its terminal-document position.
type channelPart struct {
	key     string
	index   int
	value   string
	channel partChannel
	block   ir.Block
	order   int
}

// channelReconciliation is the terminal channel tail that still has to be
// emitted. approximated reports that the terminal partition or text differs
// from what was streamed, so the target wire is a best-effort representation.
type channelReconciliation struct {
	emit         []channelPart
	approximated bool
}

// reconcileChannel compares the concatenated bytes already emitted for one
// channel with the concatenated terminal bytes. A terminal document that only
// re-partitions the same text contributes nothing; a longer terminal
// contributes only the missing tail once, preserving the terminal partition
// for the new bytes. A shorter or diverged terminal never repeats a visible
// prefix; only a strict continuation of the same part or a genuinely new part
// contributes bytes, and the mismatch is recorded as an approximation.
func reconcileChannel(emitted, terminal []channelPart) channelReconciliation {
	var res channelReconciliation
	if len(terminal) == 0 {
		return res
	}
	var emittedBuilder, terminalBuilder strings.Builder
	for _, part := range emitted {
		emittedBuilder.WriteString(part.value)
	}
	for _, part := range terminal {
		terminalBuilder.WriteString(part.value)
	}
	emittedTotal, terminalTotal := emittedBuilder.String(), terminalBuilder.String()
	if emittedTotal == terminalTotal {
		return res
	}
	if strings.HasPrefix(terminalTotal, emittedTotal) {
		covered := len(emittedTotal)
		// A terminal part that straddles the end of the streamed bytes is the
		// continuation of the LAST emitted part, whatever terminal index it
		// carries. Keeping its terminal key would resolve to an earlier open
		// block and append the tail there (the C1 text-order corruption).
		// Fully new tail parts keep their own keys and order.
		lastKey := ""
		if len(emitted) > 0 {
			lastKey = emitted[len(emitted)-1].key
		}
		offset := 0
		for _, part := range terminal {
			start := offset
			offset += len(part.value)
			if offset <= covered {
				continue
			}
			if start < covered {
				part.value = part.value[covered-start:]
				part.key = lastKey
			}
			if part.value != "" {
				res.emit = append(res.emit, part)
			}
		}
		return res
	}
	res.approximated = true
	// A partition-only difference in whitespace is already represented by the
	// streamed words; emitting the terminal form would repeat them.
	if strings.Join(strings.Fields(emittedTotal), "") == strings.Join(strings.Fields(terminalTotal), "") {
		return res
	}
	emittedByIndex := make(map[int]string, len(emitted))
	for _, part := range emitted {
		emittedByIndex[part.index] = part.value
	}
	for _, part := range terminal {
		if part.value == "" {
			continue
		}
		if emittedValue, ok := emittedByIndex[part.index]; ok {
			// Only a strict continuation of the same streamed part is a
			// genuinely new tail. A shorter, reordered or diverged value is
			// not representable without repeating visible bytes.
			if strings.HasPrefix(part.value, emittedValue) {
				suffix := part.value[len(emittedValue):]
				if suffix != "" && !strings.Contains(emittedTotal, suffix) {
					part.value = suffix
					res.emit = append(res.emit, part)
				}
			}
			continue
		}
		if strings.Contains(emittedTotal, part.value) {
			continue
		}
		res.emit = append(res.emit, part)
	}
	return res
}

// partKeyIndex parses the trailing integer identity from a content or summary
// part key. The key owner appends it last, so an item id containing colons is
// harmless.
func partKeyIndex(key string) (int, bool) {
	i := strings.LastIndexByte(key, ':')
	if i < 0 || i == len(key)-1 {
		return 0, false
	}
	index, err := strconv.Atoi(key[i+1:])
	if err != nil || index < 0 {
		return 0, false
	}
	return index, true
}

// setPartOwner records the logical item that owns one part and keeps the
// owner-to-parts index in step. A part key belongs to one item; a later
// reassignment removes the old index entry.
func (d *ResponsesStreamDecoder) setPartOwner(partKey, owner string) {
	if previous, ok := d.partOwner[partKey]; ok {
		if previous == owner {
			return
		}
		delete(d.ownerParts[previous], partKey)
		if len(d.ownerParts[previous]) == 0 {
			delete(d.ownerParts, previous)
		}
	}
	d.partOwner[partKey] = owner
	if d.ownerParts[owner] == nil {
		d.ownerParts[owner] = map[string]struct{}{}
	}
	d.ownerParts[owner][partKey] = struct{}{}
}

// ownerPartsFor returns the part keys indexed under one logical item. The
// production lookup is one map read; the optional test hook reports how many
// owners this lookup visited so a full emittedText scan cannot pass as the
// owner-index shape.
func (d *ResponsesStreamDecoder) ownerPartsFor(itemKey string) map[string]struct{} {
	parts := d.ownerParts[itemKey]
	if d.testOwnerVisits != nil {
		d.testOwnerVisits(len(parts))
	}
	return parts
}

// emittedContentParts returns the non-empty streamed bytes for one item and
// content channel, ordered by the part index the client saw.
func (d *ResponsesStreamDecoder) emittedContentParts(itemKey string, channel partChannel) []channelPart {
	var out []channelPart
	for key := range d.ownerPartsFor(itemKey) {
		builder := d.emittedText[key]
		if builder == nil {
			continue
		}
		value := builder.String()
		if value == "" || d.partKind[key] != channel {
			continue
		}
		index, ok := partKeyIndex(key)
		if !ok {
			continue
		}
		out = append(out, channelPart{key: key, index: index, value: value, channel: channel, block: d.partSeed[key]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].index != out[j].index {
			return out[i].index < out[j].index
		}
		return out[i].key < out[j].key
	})
	return out
}

// isSummaryPartKey reports whether one part key belongs to the summary
// channel. The key owner encodes the channel as a prefix, so a reasoning
// content part sharing the same item and index is a different channel.
func isSummaryPartKey(key string) bool {
	return strings.HasPrefix(key, "summary:")
}

// emittedSummaryParts returns the non-empty streamed summary bytes for one
// reasoning item, ordered by summary index.
func (d *ResponsesStreamDecoder) emittedSummaryParts(itemKey string) []channelPart {
	var out []channelPart
	for key := range d.ownerPartsFor(itemKey) {
		if !isSummaryPartKey(key) {
			continue
		}
		builder := d.emittedText[key]
		if builder == nil {
			continue
		}
		value := builder.String()
		if value == "" {
			continue
		}
		index, ok := partKeyIndex(key)
		if !ok {
			continue
		}
		out = append(out, channelPart{key: key, index: index, value: value, channel: partChannelReasoning})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].index < out[j].index })
	return out
}

// hasEmittedParts reports whether any part of an item already reached the
// target wire. A terminal item that was announced with only empty parts still
// needs the announced identity, so this only gates the aggregate owner key.
func (d *ResponsesStreamDecoder) hasEmittedParts(itemKey string) bool {
	for key := range d.ownerPartsFor(itemKey) {
		if d.emittedText[key] != nil {
			return true
		}
	}
	return false
}

// hasEmittedContent reports whether a message item already emitted text or
// refusal bytes. Reasoning summary and content parts can share the aggregate
// item key after a renumber, so a message terminal must not treat reasoning
// coverage as its own.
func (d *ResponsesStreamDecoder) hasEmittedContent(itemKey string) bool {
	for key := range d.ownerPartsFor(itemKey) {
		if d.emittedText[key] == nil {
			continue
		}
		if channel := d.partKind[key]; channel == partChannelText || channel == partChannelRefusal {
			return true
		}
	}
	return false
}

// claimTerminalItem reserves one announced message key for a terminal output
// position. A second terminal-only sibling with the same provider id gets a
// different position and synthesizes its own block instead of reusing the
// first sibling's identity.
func (d *ResponsesStreamDecoder) claimTerminalItem(key string, outputIndex int) bool {
	if claimed, ok := d.terminalItemClaims[key]; ok && claimed != outputIndex {
		return false
	}
	d.terminalItemClaims[key] = outputIndex
	return true
}

// terminalContentItemKey resolves the aggregate item identity of a terminal
// content item. The item id is stable across renumbering, so a unique
// announced message identity wins over the terminal array position; when one
// side has no id, the announced output position identifies the same logical
// item. A terminal scan claims the chosen identity: two terminal-only siblings
// that repeat one announced id cannot both resolve to the first sibling's key.
func (d *ResponsesStreamDecoder) terminalContentItemKey(itemID string, outputIndex int, claim bool) string {
	primary := streamItemKey(itemID, outputIndex)
	if d.hasEmittedContent(primary) {
		if !claim || d.claimTerminalItem(primary, outputIndex) {
			return primary
		}
	}
	announcedID := d.itemIDByOutput[outputIndex]
	if itemID == "" || announcedID == "" || announcedID == itemID {
		positional := streamItemKey(announcedID, outputIndex)
		if d.hasEmittedContent(positional) {
			if !claim || d.claimTerminalItem(positional, outputIndex) {
				return positional
			}
		}
	}
	if announced, ok := d.announcedItemKey(itemKindMessage, itemID); ok {
		if !claim || d.claimTerminalItem(announced, outputIndex) {
			return announced
		}
	}
	return primary
}

// warnApproximated records one streaming approximation per field. The decoder
// runs before the target status line is committed for non-streaming bodies,
// but a streamed status is already sent, so this is decoder-owned diagnostic
// state rather than a header.
func (d *ResponsesStreamDecoder) warnApproximated(field, detail string) {
	for _, warning := range d.warnings {
		if warning.Field == field && warning.Detail == detail {
			return
		}
	}
	d.warnings.Add(ir.WarningApproximated, field, detail)
}

func (d *ResponsesStreamDecoder) reconcileChannel(field string, emitted, terminal []channelPart) channelReconciliation {
	res := reconcileChannel(emitted, terminal)
	if res.approximated {
		d.warnApproximated(field, "terminal partition differs from the streamed parts; emitted only non-overlapping bytes")
	}
	return res
}

func contentChannelDelta(channel partChannel, value string) *ir.Delta {
	if channel == partChannelReasoning {
		return &ir.Delta{Reasoning: value}
	}
	return &ir.Delta{Text: value}
}

// appendContentValue emits one reconciled content tail. It prefers the open
// block the client already has. A part that never reached the target wire,
// including a deferred announcement, is queued so it starts after every
// streamed block has closed; starting it here would fix its position ahead of
// an earlier terminal-only part.
func (d *ResponsesStreamDecoder) appendContentValue(part channelPart, pending *[]terminalEmission) {
	if part.value == "" {
		return
	}
	if idx, ok := d.partBlockByKey[part.key]; ok && d.open[idx] {
		d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: idx, Delta: contentChannelDelta(part.channel, part.value)})
		return
	}
	*pending = append(*pending, terminalEmission{
		block: part.block,
		delta: contentChannelDelta(part.channel, part.value),
	})
}

// appendReasoningValue emits one reconciled summary or reasoning-content tail
// on the open item block, or queues a fresh reasoning block after it closes.
func (d *ResponsesStreamDecoder) appendReasoningValue(item responsesWireItem, itemKey, value string, pending *[]terminalEmission) {
	if value == "" {
		return
	}
	if idx, ok := d.itemBlockByKey[itemKey]; ok && d.open[idx] {
		d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: idx, Delta: &ir.Delta{Reasoning: value}})
		return
	}
	*pending = append(*pending, terminalEmission{block: reasoningTerminalBlock(item), delta: &ir.Delta{Reasoning: value}})
}
