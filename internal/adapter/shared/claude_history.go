package shared

import (
	"strings"
	"unicode"

	"commandcode-cpa-plugin/internal/errclass"
)

func NormalizeClaudeHistory(messages []ClaudeMessageRecord, endpoint string) ([]ClaudeMessageRecord, *errclass.Error) {
	out := make([]ClaudeMessageRecord, 0, len(messages))
	var deferred []ClaudeMessageRecord
	var pendingIDs []string
	var results []ClaudeBlock
	flushPending := func() {
		if len(results) > 0 {
			out = append(out, ClaudeMessageRecord{Role: "user", Blocks: alignClaudeResults(results, pendingIDs)})
		}
		out = append(out, deferred...)
		results, deferred, pendingIDs = nil, nil, nil
	}
	for _, message := range messages {
		switch message.Role {
		case "system":
			text, eErr := claudeHistoryReminder(message, endpoint)
			if eErr != nil {
				return nil, eErr
			}
			if text == "" {
				continue
			}
			reminder := ClaudeMessageRecord{Role: "user", Content: text}
			if len(pendingIDs) > 0 {
				deferred = append(deferred, reminder)
			} else {
				out = append(out, reminder)
			}
		case "user":
			var turnResults, content []ClaudeBlock
			for _, block := range message.Blocks {
				if block.Kind == "tool_result" {
					turnResults = append(turnResults, block)
				} else {
					content = append(content, block)
				}
			}
			ordinary := ClaudeMessageRecord{Role: "user", Content: message.Content, Blocks: content}
			if len(pendingIDs) > 0 {
				results = append(results, turnResults...)
				if ordinary.Content != "" || len(ordinary.Blocks) > 0 {
					deferred = append(deferred, ordinary)
				}
				if _, complete := orderedClaudeResults(results, pendingIDs); complete {
					flushPending()
				}
			} else {
				if len(turnResults) > 0 {
					out = append(out, ClaudeMessageRecord{Role: "user", Blocks: turnResults})
				}
				if ordinary.Content != "" || len(ordinary.Blocks) > 0 {
					out = append(out, ordinary)
				}
			}
		default:
			flushPending()
			out = append(out, message)
			if message.Role == "assistant" {
				for _, block := range message.Blocks {
					if block.Kind == "tool_use" && block.CallID != "" {
						pendingIDs = append(pendingIDs, block.CallID)
					}
				}
			}
		}
	}
	flushPending()
	return out, nil
}

func claudeHistoryReminder(message ClaudeMessageRecord, endpoint string) (string, *errclass.Error) {
	var parts []string
	appendText := func(text string) {
		if text != "" && !strings.HasPrefix(strings.TrimLeftFunc(text, unicode.IsSpace), "x-anthropic-billing-header:") {
			parts = append(parts, text)
		}
	}
	appendText(message.Content)
	for _, block := range message.Blocks {
		if block.Kind != "text" {
			return "", UnsupportedPartType(block.Kind, endpoint)
		}
		appendText(block.Text)
	}
	text := strings.Join(parts, "\n")
	if strings.TrimSpace(text) == "" {
		return "", nil
	}
	return "<system-reminder>\n" + text + "\n</system-reminder>", nil
}

func alignClaudeResults(results []ClaudeBlock, pendingIDs []string) []ClaudeBlock {
	ordered, _ := orderedClaudeResults(results, pendingIDs)
	return ordered
}

func orderedClaudeResults(results []ClaudeBlock, pendingIDs []string) ([]ClaudeBlock, bool) {
	if len(pendingIDs) == 0 || len(results) != len(pendingIDs) {
		return results, false
	}
	ordered := make([]ClaudeBlock, 0, len(results))
	used := make([]bool, len(results))
	for _, id := range pendingIDs {
		matched := -1
		for i, result := range results {
			if !used[i] && result.CallID == id {
				matched = i
				break
			}
		}
		if matched < 0 {
			return results, false
		}
		used[matched] = true
		ordered = append(ordered, results[matched])
	}
	return ordered, true
}
