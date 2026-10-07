package executor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// messagesStreamEvent recognizes both successful and failed terminal events.
// A transport EOF or an OpenAI [DONE] marker does not complete a Messages turn.
func messagesStreamEvent(line []byte) (bool, error) {
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte("data:")) {
		return false, nil
	}
	var event struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(bytes.TrimSpace(line[len("data:"):]), &event) != nil {
		return false, nil
	}
	switch event.Type {
	case "message_stop":
		return true, nil
	case "error":
		message := strings.TrimSpace(event.Error.Message)
		if message == "" {
			message = "upstream stream failed"
		}
		if len(message) > 4096 {
			message = message[:4096] + "..."
		}
		if code := strings.TrimSpace(event.Error.Type); code != "" {
			return true, fmt.Errorf("Mirasim Messages stream failed (%s): %s", code, message)
		}
		return true, fmt.Errorf("Mirasim Messages stream failed: %s", message)
	default:
		return false, nil
	}
}
