package proxy

import (
	"bytes"

	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/segmentio/asm/ascii"
)

// parseLLMRequestFast extracts routing and prompt composition from the common
// minimal chat body without the typed document decode. It is deliberately
// narrow: every member, string, and value must match one exact shape, and any
// other byte leaves the body to parseLLMRequestSlow. Success therefore implies
// the same JSON validity, bounds, and field semantics as the slow path.
//
// The recognized shape is a top-level object with only these members, each at
// most once: model (plain ASCII string), stream (true/false), messages (array
// of objects carrying only role and content), and system (plain ASCII string
// or null). Content strings are unescaped ASCII without raw controls. Every
// parameter collection, escape, duplicate, Unicode string, content part array,
// null routing value, and malformed shape falls back untouched. The caller
// passes a fresh record, as both production and test call sites do.
func parseLLMRequestFast(body []byte, rec *metrics.Record) bool {
	i := metrics.SkipSpace(body, 0)
	if i >= len(body) || body[i] != '{' {
		return false
	}
	i = metrics.SkipSpace(body, i+1)

	var (
		model         []byte
		haveModel     bool
		stream        bool
		haveStream    bool
		haveMessages  bool
		messagesStart int
		systemChars   int
		haveSystem    bool
	)
	for {
		if i >= len(body) || body[i] != '"' {
			return false
		}
		keyStart := i
		keyEnd, ok := scanFastJSONString(body, i)
		if !ok {
			return false
		}
		key := body[keyStart+1 : keyEnd-1]
		i = metrics.SkipSpace(body, keyEnd)
		if i >= len(body) || body[i] != ':' {
			return false
		}
		i = metrics.SkipSpace(body, i+1)
		switch {
		case bytes.Equal(key, []byte("model")):
			if haveModel {
				return false
			}
			start := i
			end, ok := scanFastJSONString(body, i)
			if !ok {
				return false
			}
			model = body[start+1 : end-1]
			haveModel = true
			i = end
		case bytes.Equal(key, []byte("stream")):
			if haveStream {
				return false
			}
			switch {
			case bytes.HasPrefix(body[i:], []byte("true")):
				stream = true
				i += len("true")
			case bytes.HasPrefix(body[i:], []byte("false")):
				i += len("false")
			default:
				return false
			}
			haveStream = true
		case bytes.Equal(key, []byte("messages")):
			if haveMessages {
				return false
			}
			if i >= len(body) || body[i] != '[' {
				return false
			}
			// Defer content validation until every top-level member is known
			// to be part of the minimal shape. A later tools/parameter member
			// must not pay a full content scan before falling back.
			next := metrics.SkipJSONValue(body, i)
			if next <= i {
				return false
			}
			messagesStart = i
			haveMessages = true
			i = next
		case bytes.Equal(key, []byte("system")):
			if haveSystem {
				return false
			}
			if i < len(body) && body[i] == 'n' {
				if !bytes.HasPrefix(body[i:], []byte("null")) {
					return false
				}
				i += len("null")
			} else {
				start := i
				end, ok := scanFastJSONString(body, i)
				if !ok {
					return false
				}
				systemChars = end - start - 2
				i = end
			}
			haveSystem = true
		default:
			return false
		}
		i = metrics.SkipSpace(body, i)
		if i >= len(body) {
			return false
		}
		if body[i] == '}' {
			i++
			break
		}
		if body[i] != ',' {
			return false
		}
		i = metrics.SkipSpace(body, i+1)
	}
	if i = metrics.SkipSpace(body, i); i != len(body) {
		return false
	}
	var counts fastMessageCounts
	if haveMessages {
		var ok bool
		_, counts, ok = parseFastMessages(body, messagesStart)
		if !ok {
			return false
		}
	}

	rec.Model = retainedText(string(model), retainedLabelMax)
	rec.Stream = stream
	rec.CharsSystem = systemChars + counts.charsSystem
	rec.TurnsUser += counts.turnsUser
	rec.CharsUser += counts.charsUser
	rec.TurnsAssistant += counts.turnsAssistant
	rec.CharsAssistant += counts.charsAssistant
	rec.TurnsTool += counts.turnsTool
	rec.CharsTool += counts.charsTool
	rec.LastTurnRole = counts.lastRole
	return true
}

type fastMessageCounts struct {
	turnsUser      int
	turnsAssistant int
	turnsTool      int
	charsUser      int
	charsAssistant int
	charsTool      int
	charsSystem    int
	lastRole       string
}

// parseFastMessages reads the messages array at i. Every element must be an
// object whose only members are role and content, role must name one of the
// roles the slow composition loop recognizes, and content must be a plain
// ASCII string or null. The element cap is checked here so an over-cap body
// still reaches the slow rejection path.
func parseFastMessages(body []byte, i int) (int, fastMessageCounts, bool) {
	var counts fastMessageCounts
	i = metrics.SkipSpace(body, i)
	if i >= len(body) || body[i] != '[' {
		return 0, counts, false
	}
	i = metrics.SkipSpace(body, i+1)
	if i < len(body) && body[i] == ']' {
		return i + 1, counts, true
	}
	count := 0
	for {
		i = metrics.SkipSpace(body, i)
		if i >= len(body) || body[i] != '{' {
			return 0, counts, false
		}
		i = metrics.SkipSpace(body, i+1)
		var (
			role        []byte
			haveRole    bool
			chars       int
			haveContent bool
		)
		if i < len(body) && body[i] == '}' {
			i++
		} else {
			for {
				if i >= len(body) || body[i] != '"' {
					return 0, counts, false
				}
				keyStart := i
				keyEnd, ok := scanFastJSONString(body, i)
				if !ok {
					return 0, counts, false
				}
				key := body[keyStart+1 : keyEnd-1]
				i = metrics.SkipSpace(body, keyEnd)
				if i >= len(body) || body[i] != ':' {
					return 0, counts, false
				}
				i = metrics.SkipSpace(body, i+1)
				switch {
				case bytes.Equal(key, []byte("role")):
					if haveRole {
						return 0, counts, false
					}
					start := i
					end, ok := scanFastJSONString(body, i)
					if !ok {
						return 0, counts, false
					}
					role = body[start+1 : end-1]
					haveRole = true
					i = end
				case bytes.Equal(key, []byte("content")):
					if haveContent {
						return 0, counts, false
					}
					if i < len(body) && body[i] == 'n' {
						if !bytes.HasPrefix(body[i:], []byte("null")) {
							return 0, counts, false
						}
						i += len("null")
					} else {
						start := i
						end, ok := scanFastJSONString(body, i)
						if !ok {
							return 0, counts, false
						}
						chars = end - start - 2
						i = end
					}
					haveContent = true
				default:
					return 0, counts, false
				}
				i = metrics.SkipSpace(body, i)
				if i >= len(body) {
					return 0, counts, false
				}
				if body[i] == '}' {
					i++
					break
				}
				if body[i] != ',' {
					return 0, counts, false
				}
				i = metrics.SkipSpace(body, i+1)
			}
		}
		if haveRole && len(role) > 0 {
			switch {
			case bytes.Equal(role, []byte("user")):
				counts.turnsUser++
				counts.charsUser += chars
				counts.lastRole = "user"
			case bytes.Equal(role, []byte("assistant")):
				counts.turnsAssistant++
				counts.charsAssistant += chars
				counts.lastRole = "assistant"
			case bytes.Equal(role, []byte("tool")):
				counts.turnsTool++
				counts.charsTool += chars
				counts.lastRole = "tool"
			case bytes.Equal(role, []byte("function")):
				counts.turnsTool++
				counts.charsTool += chars
				counts.lastRole = "function"
			case bytes.Equal(role, []byte("system")):
				counts.charsSystem += chars
				counts.lastRole = "system"
			case bytes.Equal(role, []byte("developer")):
				counts.charsSystem += chars
				counts.lastRole = "developer"
			default:
				return 0, counts, false
			}
		}
		count++
		if count > requestMetadataMaxElements {
			return 0, counts, false
		}
		i = metrics.SkipSpace(body, i)
		if i < len(body) && body[i] == ',' {
			i = metrics.SkipSpace(body, i+1)
			continue
		}
		if i < len(body) && body[i] == ']' {
			return i + 1, counts, true
		}
		return 0, counts, false
	}
}

// scanFastJSONString returns the index just past the closing quote of the
// unescaped printable-ASCII JSON string at i. An escape, raw control byte,
// invalid or non-ASCII UTF-8, or unterminated string reports false so the slow
// parser owns that input. ascii.ValidPrint accepts the 0x20-0x7e subset
// (excluding DEL), which is deliberately narrower than JSON's string range.
func scanFastJSONString(data []byte, i int) (int, bool) {
	if i >= len(data) || data[i] != '"' {
		return 0, false
	}
	rest := data[i+1:]
	j := bytes.IndexByte(rest, '"')
	if j < 0 {
		return 0, false
	}
	inner := rest[:j]
	if bytes.IndexByte(inner, '\\') >= 0 || !ascii.ValidPrint(inner) {
		return 0, false
	}
	return i + 1 + j + 1, true
}
