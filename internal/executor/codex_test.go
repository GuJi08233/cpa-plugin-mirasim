package executor

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCodexNonStreamPayloadAggregatesTerminalSSE(t *testing.T) {
	raw := []byte("data: {\"type\":\"response.output_item.done\",\"output_index\":1,\"item\":{\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"lookup\",\"arguments\":\"{}\"}}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"OK\"}]}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n" +
		"data: [DONE]\n")
	terminal, errPayload := codexNonStreamPayload(raw)
	if errPayload != nil {
		t.Fatalf("codexNonStreamPayload() error = %v", errPayload)
	}
	var event struct {
		Type     string `json:"type"`
		Response struct {
			Output []struct {
				Type string `json:"type"`
			} `json:"output"`
		} `json:"response"`
	}
	if errDecode := json.Unmarshal(terminal, &event); errDecode != nil {
		t.Fatalf("terminal event is invalid JSON: %v\n%s", errDecode, terminal)
	}
	if event.Type != "response.completed" || len(event.Response.Output) != 2 {
		t.Fatalf("terminal event = %s", terminal)
	}
	if event.Response.Output[0].Type != "message" || event.Response.Output[1].Type != "function_call" {
		t.Fatalf("output order = %#v", event.Response.Output)
	}
}

func TestCodexNonStreamPayloadWrapsDirectResponse(t *testing.T) {
	terminal, errPayload := codexNonStreamPayload([]byte(`{"id":"resp_1","object":"response","output":[]}`))
	if errPayload != nil {
		t.Fatalf("codexNonStreamPayload() error = %v", errPayload)
	}
	if !strings.Contains(string(terminal), `"type":"response.completed"`) || !strings.Contains(string(terminal), `"id":"resp_1"`) {
		t.Fatalf("terminal = %s", terminal)
	}
}

func TestCodexNonStreamPayloadReturnsStreamError(t *testing.T) {
	_, errPayload := codexNonStreamPayload([]byte("data: {\"type\":\"error\",\"error\":{\"code\":\"no_upstream\",\"message\":\"no upstream available\"}}\n"))
	if errPayload == nil || !strings.Contains(errPayload.Error(), "no_upstream") {
		t.Fatalf("error = %v", errPayload)
	}
}

func TestCodexEventErrorKeepsCodeWithoutMessage(t *testing.T) {
	for _, raw := range []string{
		`{"type":"error","error":{"code":"server_error"}}`,
		`{"type":"response.failed","response":{"error":{"code":"server_error"}}}`,
		`{"type":"error","code":"server_error"}`,
	} {
		errEvent := codexEventError([]byte(raw))
		if !strings.Contains(errEvent.Error(), "server_error") || !strings.Contains(errEvent.Error(), "upstream stream failed") {
			t.Fatalf("error code lost: %v", errEvent)
		}
	}
}
