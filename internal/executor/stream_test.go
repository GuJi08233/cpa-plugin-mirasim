package executor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func collectTranslatedStream(ctx context.Context, to sdktranslator.Format, chunks ...pluginapi.HTTPStreamChunk) ([]byte, []error) {
	input := make(chan pluginapi.HTTPStreamChunk, len(chunks))
	for _, chunk := range chunks {
		input <- chunk
	}
	close(input)
	var payload []byte
	var streamErrors []error
	for chunk := range translateStream(ctx, sdktranslator.FormatCodex, to, "gpt-6-astra", []byte(`{}`), []byte(`{}`), input) {
		payload = append(payload, chunk.Payload...)
		if chunk.Err != nil {
			streamErrors = append(streamErrors, chunk.Err)
		}
	}
	return payload, streamErrors
}

func TestTranslateStreamPreservesCodexFailures(t *testing.T) {
	formats := []sdktranslator.Format{sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, sdktranslator.FormatGemini, sdktranslator.FormatOpenAIResponse, sdktranslator.FormatCodex}
	for _, format := range formats {
		for name, event := range map[string]string{
			"failed response": `{"type":"response.failed","response":{"id":"resp_test","status":"failed","error":{"code":"server_error","message":"sentinel upstream failed"}}}`,
			"nested error":    `{"type":"error","error":{"code":"server_error","message":"sentinel upstream failed"}}`,
			"top-level error": `{"type":"error","code":"server_error","message":"sentinel upstream failed"}`,
		} {
			t.Run(format.String()+"/"+name, func(t *testing.T) {
				// Split the SSE line across transport chunks and omit its final newline.
				payload, streamErrors := collectTranslatedStream(context.Background(), format,
					pluginapi.HTTPStreamChunk{Payload: []byte("event: " + name + "\r\ndata: " + event[:len(event)/2])},
					pluginapi.HTTPStreamChunk{Payload: []byte(event[len(event)/2:])},
				)
				if format == sdktranslator.FormatOpenAIResponse || format == sdktranslator.FormatCodex {
					if len(streamErrors) != 0 || !strings.Contains(string(payload), event) {
						t.Fatalf("native Responses failure changed: payload=%s errors=%v", payload, streamErrors)
					}
					return
				}
				if len(streamErrors) != 1 || !strings.Contains(streamErrors[0].Error(), "server_error") || !strings.Contains(streamErrors[0].Error(), "sentinel upstream failed") {
					t.Fatalf("upstream failure lost: payload=%s errors=%v", payload, streamErrors)
				}
			})
		}
	}
}

func TestTranslateStreamRejectsTruncatedCodexResponse(t *testing.T) {
	for _, format := range []sdktranslator.Format{sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, sdktranslator.FormatGemini, sdktranslator.FormatOpenAIResponse} {
		for name, wire := range map[string]string{
			"empty":       "",
			"created":     "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_test\"}}\n\n",
			"done marker": "data: [DONE]\n\n",
		} {
			t.Run(format.String()+"/"+name, func(t *testing.T) {
				_, streamErrors := collectTranslatedStream(context.Background(), format, pluginapi.HTTPStreamChunk{Payload: []byte(wire)})
				if len(streamErrors) != 1 || !strings.Contains(streamErrors[0].Error(), "before response.completed") {
					t.Fatalf("truncated stream errors=%v", streamErrors)
				}
			})
		}
	}
}

func TestTranslateStreamAcceptsCodexTerminalEvents(t *testing.T) {
	for _, format := range []sdktranslator.Format{sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, sdktranslator.FormatGemini, sdktranslator.FormatOpenAIResponse} {
		for _, terminalType := range []string{"response.completed", "response.incomplete"} {
			t.Run(format.String()+"/"+terminalType, func(t *testing.T) {
				payload, streamErrors := collectTranslatedStream(context.Background(), format, pluginapi.HTTPStreamChunk{Payload: []byte(`data: {"type":"` + terminalType + `","response":{"id":"resp_test","model":"gpt-6-astra","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`)})
				if len(streamErrors) != 0 || len(payload) == 0 {
					t.Fatalf("terminal event translation: payload=%s errors=%v", payload, streamErrors)
				}
			})
		}
	}
}

func TestTranslateStreamReportsTransportFailureOnce(t *testing.T) {
	sentinel := errors.New("sentinel transport failure")
	_, streamErrors := collectTranslatedStream(context.Background(), sdktranslator.FormatOpenAI,
		pluginapi.HTTPStreamChunk{Err: sentinel},
		pluginapi.HTTPStreamChunk{Payload: []byte(`data: {"type":"response.failed","response":{"error":{"message":"secondary failure"}}}`)},
	)
	if len(streamErrors) != 1 || !errors.Is(streamErrors[0], sentinel) {
		t.Fatalf("transport failure was replaced or duplicated: %v", streamErrors)
	}
}

func TestTranslateStreamDrainsAfterCodexFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	input := make(chan pluginapi.HTTPStreamChunk)
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		defer close(input)
		for _, wire := range []string{
			`data: {"type":"response.failed","response":{"error":{"message":"sentinel upstream failed"}}}` + "\n\n",
			`data: {"type":"response.completed","response":{"output":[]}}` + "\n\n",
		} {
			select {
			case input <- pluginapi.HTTPStreamChunk{Payload: []byte(wire)}:
			case <-ctx.Done():
				return
			}
		}
	}()
	var streamErrors int
	for chunk := range translateStream(ctx, sdktranslator.FormatCodex, sdktranslator.FormatOpenAI, "gpt-6-astra", nil, nil, input) {
		if len(chunk.Payload) != 0 {
			t.Fatalf("stream emitted a success after failing: %s", chunk.Payload)
		}
		if chunk.Err != nil {
			streamErrors++
		}
	}
	<-sent
	if streamErrors != 1 {
		t.Fatalf("stream errors=%d, want 1", streamErrors)
	}
}

func TestTranslateStreamCancellationDoesNotReportTruncation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	input := make(chan pluginapi.HTTPStreamChunk)
	for chunk := range translateStream(ctx, sdktranslator.FormatCodex, sdktranslator.FormatOpenAI, "gpt-6-astra", nil, nil, input) {
		t.Fatalf("cancelled stream emitted a chunk: %+v", chunk)
	}
}
