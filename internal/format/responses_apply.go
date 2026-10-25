package format

import (
	"fmt"
	"sort"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

// collectTerminalOutput walks the terminal document in its own order. It
// fills announced blocks that are still open, queues every block that must
// start fresh (unannounced parts/items and deferred empty announcements), and
// keeps each queued value's event position. The caller closes streamed blocks
// before emitting the queue; perfect ordering is impossible only when a later
// announced block already streamed bytes, because its target index is fixed.
func (d *ResponsesStreamDecoder) collectTerminalOutput(output []responsesWireItem, policy responsesToolArgumentPolicy) ([]terminalEmission, error) {
	var pending []terminalEmission
	claimedTools := map[string]bool{}
	for i, item := range output {
		typ := item.Type
		if typ == "" && item.Role != "" {
			typ = "message"
		}
		switch typ {
		case "function_call":
			if item.CallID == "" && item.ID == "" && d.matchesClosedIdlessCall(item) {
				// An id-less entry that repeats a closed id-less call by name
				// and effective arguments must be skipped before its terminal
				// position can assign it to a different open call. The closed
				// call's announcement position and the terminal array position
				// are independent.
				continue
			}
			key := d.resolveToolCallKey(item.CallID, item.ID, i)
			idx, ok := d.itemBlockByKey[key]
			if d.announcedToolKeys[key] && !d.openToolKeys[key] && d.closedToolCallRepeat(key, item) {
				// This terminal entry repeats an announced call whose block
				// already closed. Its arguments belong to that call, not to a
				// different open call with the same name. Non-tool
				// announcements are absent from this set, so a cross-kind
				// reorder still reaches the name fallback.
				continue
			}
			if ok && !d.openToolKeys[key] {
				// An identity can also name an announced non-tool item at the
				// same position or with the same id. Only a function call this
				// stream announced is a candidate for its arguments.
				ok = false
				idx = 0
			}
			if ok && item.CallID == "" && item.ID == "" && item.Name != "" && d.toolNames[key] != item.Name {
				// An id-less terminal item has only its name and its array
				// position. When the position names a different call but a
				// unique open call carries this name, the name is the stronger
				// correlation.
				if matchedKey, matchedIdx, matched := d.matchTerminalToolCall(item, i, claimedTools); matched {
					key, idx, ok = matchedKey, matchedIdx, true
				} else {
					// The position names a different call and no open call
					// carries this name. Keeping the positional key would
					// attribute these arguments to the wrong call.
					ok = false
				}
			}
			if !ok {
				if matchedKey, matchedIdx, matched := d.matchTerminalToolCall(item, i, claimedTools); matched {
					key, idx, ok = matchedKey, matchedIdx, true
				}
			}
			if ok && d.open[idx] {
				claimedTools[key] = true
				if err := d.flushToolArguments(key, idx, item.Arguments, policy); err != nil {
					return nil, err
				}
				continue
			}
			if d.terminalToolCallAnnounced(key, i, item.ID) {
				continue
			}
			admitted, err := d.announceItem(key)
			if err != nil {
				return nil, err
			}
			if !admitted {
				continue
			}
			emission, ok, err := functionCallTerminalEmission(item, i, policy)
			if err != nil {
				if policy == responsesToolArgumentsSkip {
					// A failed, errored or truncated terminal owns the
					// outcome; drop the call whose arguments the source never
					// completed rather than replacing the provider error or
					// truncation with a translator failure.
					continue
				}
				return nil, err
			}
			d.sawToolCall = true
			if ok {
				if err := reserveToolID(d.toolIDs, emission.block.ToolCall.ID); err != nil {
					return nil, err
				}
				pending = append(pending, emission)
			}
		case "message":
			if err := d.applyMessageContent(item, i, &pending); err != nil {
				return nil, err
			}
		case "reasoning":
			key, announced := d.terminalAnnouncedItemKey(i, item.ID)
			if !announced {
				admitted, err := d.announceItem(key)
				if err != nil {
					return nil, err
				}
				if !admitted {
					continue
				}
				emission, ok, err := reasoningTerminalEmission(item)
				if err != nil {
					return nil, err
				}
				if ok {
					pending = append(pending, emission)
				}
				continue
			}
			if err := d.applyReasoningContent(item, key, i, &pending); err != nil {
				return nil, err
			}
		}
	}
	return pending, nil
}

// applyMessageContent reconciles one message item's content, whether it
// arrived in output_item.done or in the terminal document. Open blocks receive
// only the missing bytes; parts that never reached the target wire are queued
// after every streamed block closes.
func (d *ResponsesStreamDecoder) applyMessageContent(item responsesWireItem, outputIndex int, pending *[]terminalEmission) error {
	if err := checkNativeIdentity("message id", item.ID); err != nil {
		return err
	}
	parts, err := terminalMessageParts(item.Content)
	if err != nil {
		return err
	}
	itemKey := d.terminalContentItemKey(item.ID, outputIndex)
	var channels []partChannel
	grouped := map[partChannel][]channelPart{}
	for order, part := range parts {
		channel := partChannelForBlock(part.block.Type)
		if _, ok := grouped[channel]; !ok {
			channels = append(channels, channel)
		}
		grouped[channel] = append(grouped[channel], channelPart{
			key:     d.terminalPartKey(itemKey, item.ID, outputIndex, part.contentIndex),
			index:   part.contentIndex,
			value:   part.delta.Text,
			channel: channel,
			block:   part.block,
			order:   order,
		})
	}
	var emit []channelPart
	for _, channel := range channels {
		rec := d.reconcileChannel("content_partition", d.emittedContentParts(itemKey, channel), grouped[channel])
		emit = append(emit, rec.emit...)
	}
	sort.SliceStable(emit, func(a, b int) bool { return emit[a].order < emit[b].order })
	for _, part := range emit {
		if !d.announcedParts[part.key] {
			admitted, err := d.announcePart(part.key)
			if err != nil {
				return err
			}
			if !admitted {
				continue
			}
		}
		if _, owned := d.partOwner[part.key]; !owned {
			d.setPartOwner(part.key, itemKey)
		}
		if _, kind := d.partKind[part.key]; !kind {
			d.partKind[part.key] = part.channel
		}
		before := d.emittedValue(part.key)
		d.appendContentValue(part, pending)
		if err := d.setEmittedText(part.key, before+part.value); err != nil {
			return err
		}
	}
	return nil
}

// applyReasoningContent reconciles one reasoning item's summary and content
// channels, whether the payload arrived in output_item.done or in the terminal
// document. The open item block receives only the missing bytes.
func (d *ResponsesStreamDecoder) applyReasoningContent(item responsesWireItem, key string, outputIndex int, pending *[]terminalEmission) error {
	// A reasoning item's id is used to build content-part keys. Validate it at
	// this shared owner so output_item.done and the terminal document cannot
	// retain an over-bound identity after the added event was accepted.
	if err := checkNativeIdentity("reasoning id", item.ID); err != nil {
		return err
	}
	// Summary parts are one concatenated reasoning channel; content
	// parts are another. Reconcile each as a whole before emitting so
	// a terminal re-partition cannot repeat streamed bytes.
	var summaryTerminal []channelPart
	for summaryIndex, summary := range item.Summary {
		if summary.Text == "" {
			continue
		}
		summaryTerminal = append(summaryTerminal, channelPart{
			key:     summaryPartKey(key, summaryIndex),
			index:   summaryIndex,
			value:   summary.Text,
			channel: partChannelReasoning,
			order:   summaryIndex,
		})
	}
	summaryRec := d.reconcileChannel("summary_partition", d.emittedSummaryParts(key), summaryTerminal)
	for _, part := range summaryRec.emit {
		if _, err := d.announcePart(part.key); err != nil {
			return err
		}
		// Record the item that owns this summary part before the value is
		// stored. emittedSummaryParts filters by owner, so a done-only summary
		// without this assignment looks uncovered to the terminal copy and is
		// emitted a second time.
		d.setPartOwner(part.key, key)
		d.partKind[part.key] = partChannelReasoning
		before := d.emittedValue(part.key)
		d.appendReasoningValue(item, key, part.value, pending)
		if err := d.setEmittedText(part.key, before+part.value); err != nil {
			return err
		}
	}
	parts, err := terminalMessageParts(item.Content)
	if err != nil {
		return err
	}
	var contentTerminal []channelPart
	for order, part := range parts {
		if part.block.Type != ir.BlockText {
			continue
		}
		contentTerminal = append(contentTerminal, channelPart{
			key:     d.terminalPartKey(key, item.ID, outputIndex, part.contentIndex),
			index:   part.contentIndex,
			value:   part.delta.Text,
			channel: partChannelReasoning,
			block:   reasoningTerminalBlock(item),
			order:   order,
		})
	}
	contentRec := d.reconcileChannel("reasoning_content_partition", d.emittedContentParts(key, partChannelReasoning), contentTerminal)
	for _, part := range contentRec.emit {
		if !d.announcedParts[part.key] {
			admitted, err := d.announcePart(part.key)
			if err != nil {
				return err
			}
			if !admitted {
				continue
			}
		}
		if _, owned := d.partOwner[part.key]; !owned {
			d.setPartOwner(part.key, key)
		}
		if _, kind := d.partKind[part.key]; !kind {
			d.partKind[part.key] = partChannelReasoning
		}
		before := d.emittedValue(part.key)
		d.appendContentValue(part, pending)
		if err := d.setEmittedText(part.key, before+part.value); err != nil {
			return err
		}
	}
	return nil
}

// terminalToolCallAnnounced reports whether a terminal function call's stable
// identity was already announced by a streamed tool item. The positional
// fallback requires the position to name an announced TOOL call, never a
// reasoning or message announcement: a foreign item at the same position is
// not an identity and must not claim the call. The item-id agreement still
// guards a tool announcement that carries an id.
func (d *ResponsesStreamDecoder) terminalToolCallAnnounced(key string, outputIndex int, itemID string) bool {
	if d.announcedToolKeys[key] {
		return true
	}
	fallback, ok := d.keyByOutput[outputIndex]
	if !ok || !d.announcedToolKeys[fallback] {
		return false
	}
	announcedID := d.itemIDByOutput[outputIndex]
	return itemID == "" || announcedID == "" || announcedID == itemID
}

// terminalPartKey resolves the announcement identity of one terminal content
// part. The item key already accounts for terminal renumbering; the content
// index is the part identity. The positional fallback is used only when both
// sides agree on the item id (or one side has none).
func (d *ResponsesStreamDecoder) terminalPartKey(itemKey, itemID string, outputIndex, contentIndex int) string {
	key := contentPartKey(itemKey, contentIndex)
	if d.announcedParts[key] {
		return key
	}
	posKey, ok := d.keyByPart[[2]int{outputIndex, contentIndex}]
	if !ok {
		return key
	}
	announcedID := d.itemIDByOutput[outputIndex]
	if itemID == "" || announcedID == "" || announcedID == itemID {
		return posKey
	}
	return key
}

// queueTerminal coalesces one authoritative terminal value into the pending
// queue by part identity. A repeated delta for the same unopened part appends
// its bytes to the existing emission, so a stream cannot turn the 8 MiB text
// bound into one entry per delta. Every caller announces the part first, and
// announcePart caps distinct keys at WireNestedCollectionMax, so the queue
// shares that real bound.
func (d *ResponsesStreamDecoder) queueTerminal(key string, block ir.Block, delta *ir.Delta) error {
	if idx, ok := d.terminalPendingByKey[key]; ok {
		d.terminalPending[idx].add(delta)
		return nil
	}
	if len(d.terminalPending) >= WireNestedCollectionMax {
		// Defensive only: the announced-part gate above already bounds the
		// distinct keys, so no current call path reaches this branch. It stays
		// as a local invariant for a future caller that queues a terminal
		// value without announcing its part first.
		return fmt.Errorf("responses stream exceeds %d pending terminal parts", WireNestedCollectionMax)
	}
	emission := terminalEmission{block: block, delta: &ir.Delta{}}
	emission.add(delta)
	d.terminalPendingByKey[key] = len(d.terminalPending)
	d.terminalPending = append(d.terminalPending, emission)
	return nil
}
