package executor

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	thinkingpkg "github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/thinking"
	"github.com/tidwall/gjson"
)

func TestGeminiUsesMessagesForEveryClientProtocol(t *testing.T) {
	for format, payload := range map[sdktranslator.Format]string{
		sdktranslator.FormatOpenAI:         `{"messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{}}}}]}`,
		sdktranslator.FormatOpenAIResponse: `{"input":"hello","tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{}}}]}`,
		sdktranslator.FormatClaude:         `{"messages":[{"role":"user","content":"hello"}],"max_tokens":65536,"thinking":{"type":"enabled","budget_tokens":8192},"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{}}}]}`,
		sdktranslator.FormatGemini:         `{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"tools":[{"functionDeclarations":[{"name":"lookup","parameters":{"type":"object","properties":{}}}]}]}`,
	} {
		t.Run(format.String(), func(t *testing.T) {
			for _, stream := range []bool{false, true} {
				body, route, err := buildProviderRequest(pluginapi.ExecutorRequest{Model: "mirasim/gemini-3.1-pro-preview", Format: format.String(), Payload: []byte(payload)}, stream, thinkingpkg.ShapeUnknown)
				if err != nil || route.Path != "/v1/messages" || route.Format != sdktranslator.FormatClaude {
					t.Fatalf("Gemini route=%+v err=%v", route, err)
				}
				if gjson.GetBytes(body, "model").String() != "gemini-3.1-pro-preview" || !gjson.GetBytes(body, "messages").IsArray() || gjson.GetBytes(body, "stream").Bool() != stream || gjson.GetBytes(body, "tools.0.name").String() != "lookup" {
					t.Fatalf("Gemini Messages request lost content: %s", body)
				}
				if format == sdktranslator.FormatClaude && (gjson.GetBytes(body, "thinking.type").String() != "enabled" || gjson.GetBytes(body, "thinking.budget_tokens").Int() != 8192) {
					t.Fatalf("Gemini manual budget was rewritten as adaptive: %s", body)
				}
			}
		})
	}
}

func TestGeminiThinkingSuffixesUseMessagesBudget(t *testing.T) {
	for _, effort := range []string{"off", "minimal", "low", "medium", "high"} {
		t.Run(effort, func(t *testing.T) {
			body, route, err := buildProviderRequest(pluginapi.ExecutorRequest{
				Model: "gemini-3.1-pro-preview(" + effort + ")", Format: sdktranslator.FormatClaude.String(),
				Payload: []byte(`{"messages":[{"role":"user","content":"hello"}],"max_tokens":65536}`),
			}, true, thinkingpkg.ShapeUnknown)
			if err != nil || route.Path != "/v1/messages" {
				t.Fatalf("Gemini effort failed: route=%+v err=%v", route, err)
			}
			if effort == "off" {
				if gjson.GetBytes(body, "thinking.type").String() != "disabled" {
					t.Fatalf("off enables thinking: %s", body)
				}
			} else if budget := gjson.GetBytes(body, "thinking.budget_tokens").Int(); gjson.GetBytes(body, "thinking.type").String() != "enabled" || budget < 1024 || budget >= 65536 || gjson.GetBytes(body, "output_config.effort").Exists() {
				t.Fatalf("Gemini does not use the Messages budget form: %s", body)
			}
		})
	}
}
