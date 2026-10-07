package executor

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	thinkingpkg "github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/thinking"
	"github.com/tidwall/gjson"
)

func TestClientImageInputSurvivesRelayTranslation(t *testing.T) {
	// Synthetic bytes exercise protocol conversion without an external upload.
	for format, payload := range map[sdktranslator.Format]string{
		sdktranslator.FormatOpenAI:         `{"messages":[{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}]}]}`,
		sdktranslator.FormatOpenAIResponse: `{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"describe"},{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8="}]}]}`,
		sdktranslator.FormatClaude:         `{"messages":[{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]}],"max_tokens":2048}`,
		sdktranslator.FormatGemini:         `{"contents":[{"role":"user","parts":[{"text":"describe"},{"inlineData":{"mimeType":"image/png","data":"aGVsbG8="}}]}]}`,
	} {
		for _, model := range []string{"claude-opus-5-5[1m]", "gpt-6-astra", "deepseek-flash", "kimi-k3", "glm-5.3-flash", "gemini-3.1-pro-preview"} {
			t.Run(format.String()+"/"+model, func(t *testing.T) {
				body, route, err := buildProviderRequest(pluginapi.ExecutorRequest{Model: model, Format: format.String(), Payload: []byte(payload)}, true, thinkingpkg.ShapeUnknown)
				if err != nil {
					t.Fatal(err)
				}
				if route.Format == sdktranslator.FormatCodex {
					found := false
					for _, item := range gjson.GetBytes(body, "input").Array() {
						found = found || item.Get(`content.#(type=="input_image").image_url`).String() == "data:image/png;base64,aGVsbG8="
					}
					if !found {
						t.Fatalf("Responses wire lost image: %s", body)
					}
				} else {
					source := gjson.GetBytes(body, `messages.0.content.#(type=="image").source`)
					if source.Get("type").String() != "base64" || source.Get("media_type").String() != "image/png" || source.Get("data").String() != "aGVsbG8=" {
						t.Fatalf("Messages wire lost image: %s", body)
					}
				}
			})
		}
	}
}
