package models

import (
	"reflect"
	"slices"
	"testing"

	"github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/mirasim"
)

func TestRosterOverridesSpecWithoutAddingModels(t *testing.T) {
	models := exposedModels([]mirasim.RemoteModel{{ID: "gpt-6-astra"}, {ID: "claude-sonnet-5"}})
	applyRoster(models, mirasim.ModelRoster{Version: "live", Agents: map[string][]mirasim.ModelSpec{
		"codex": {{ID: "gpt-6-astra", ContextWindow: 1050000, MaxOutput: 64000, Effort: []string{"high", "max", "ultra"}}, {ID: "gpt-not-in-catalog", ContextWindow: 999}},
	}})
	if len(models) != 2 || models[0].ContextLength != 1050000 || models[0].MaxCompletionTokens != 64000 || len(models[0].Thinking.Levels) != 3 {
		t.Fatalf("models=%+v", models)
	}
	if models[1].ContextLength != 1000000 {
		t.Fatal("missing spec erased static metadata")
	}
}

func TestRosterPublishesThinkingForCatalogOnlyClaude(t *testing.T) {
	const id = "claude-future-9"
	for _, test := range []struct {
		name     string
		agent    bool
		spec     mirasim.ModelSpec
		adaptive bool
	}{
		{name: "top-level adaptive", spec: mirasim.ModelSpec{Adaptive: true, AdaptiveSet: true}, adaptive: true},
		{name: "agent adaptive", agent: true, spec: mirasim.ModelSpec{Adaptive: true, AdaptiveSet: true}, adaptive: true},
		{name: "top-level budget", spec: mirasim.ModelSpec{AdaptiveSet: true}},
		{name: "agent budget", agent: true},
		{name: "effort only uses relay default", adaptive: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			models := exposedModels([]mirasim.RemoteModel{{ID: id}})
			test.spec.ID = id
			test.spec.ContextWindow = 1000000
			test.spec.MaxOutput = 128000
			test.spec.Effort = []string{"low", " HIGH ", "high", "off", "unsupported", "max"}
			roster := mirasim.ModelRoster{Version: "live"}
			if test.agent {
				roster.Agents = map[string][]mirasim.ModelSpec{"claude": {test.spec}}
			} else {
				roster.Models = map[string]mirasim.ModelSpec{id: test.spec}
			}
			applyRoster(models, roster)
			model := models[0]
			if model.Thinking == nil || !model.Thinking.ZeroAllowed || !model.Thinking.DynamicAllowed || !reflect.DeepEqual(model.Thinking.Levels, []string{"low", "high", "max"}) {
				t.Fatalf("catalog-only Claude thinking = %+v", model.Thinking)
			}
			if model.ContextLength != 1000000 || model.MaxCompletionTokens != 128000 || !slices.Contains(model.SupportedParameters, "thinking") {
				t.Fatalf("roster metadata = %+v", model)
			}
			if test.adaptive {
				if model.Thinking.Min != 0 || model.Thinking.Max != 0 || !slices.Contains(model.SupportedParameters, "output_config") {
					t.Fatalf("adaptive model advertises wrong thinking form: %+v", model)
				}
			} else if model.Thinking.Min != 1024 || model.Thinking.Max != 128000 || slices.Contains(model.SupportedParameters, "output_config") {
				t.Fatalf("budget model advertises wrong thinking form: %+v", model)
			}
		})
	}
}

func TestRosterDoesNotInventThinkingForMetadataOnlyClaude(t *testing.T) {
	const id = "claude-future-9"
	models := exposedModels([]mirasim.RemoteModel{{ID: id}})
	applyRoster(models, mirasim.ModelRoster{Version: "live", Models: map[string]mirasim.ModelSpec{
		id: {ID: id, Label: "Future Claude", ContextWindow: 1000000},
	}})
	if models[0].Thinking != nil || slices.Contains(models[0].SupportedParameters, "thinking") {
		t.Fatalf("metadata-only roster invented thinking support: %+v", models[0])
	}
}

func TestRosterAdaptiveFlagPublishesThinkingWithoutEffortList(t *testing.T) {
	const id = "claude-future-9"
	for _, adaptive := range []bool{true, false} {
		models := exposedModels([]mirasim.RemoteModel{{ID: id}})
		applyRoster(models, mirasim.ModelRoster{Version: "live", Models: map[string]mirasim.ModelSpec{
			id: {ID: id, Adaptive: adaptive, AdaptiveSet: true},
		}})
		thinking := models[0].Thinking
		if thinking == nil || len(thinking.Levels) != 6 {
			t.Fatalf("adaptive=%t did not publish default thinking levels: %+v", adaptive, thinking)
		}
		if adaptive && (thinking.Min != 0 || thinking.Max != 0) || !adaptive && (thinking.Min != 1024 || thinking.Max != 128000) {
			t.Fatalf("adaptive=%t published wrong thinking bounds: %+v", adaptive, thinking)
		}
	}
}

func TestRosterPublishesOnlyTheThinkingFormTheModelAccepts(t *testing.T) {
	models := exposedModels([]mirasim.RemoteModel{{ID: "claude-haiku-4-5"}, {ID: "claude-sonnet-5"}})
	applyRoster(models, mirasim.ModelRoster{Version: "live", Agents: map[string][]mirasim.ModelSpec{
		"claude": {
			{ID: "claude-haiku-4-5", ContextWindow: 200000, Adaptive: false},
			{ID: "claude-sonnet-5", ContextWindow: 1000000, Adaptive: true},
		},
	}})
	if models[0].Thinking == nil || models[0].Thinking.Min != 1024 || models[0].Thinking.Max != 128000 {
		t.Fatalf("non-adaptive model did not publish budget bounds: %+v", models[0].Thinking)
	}
	if models[1].Thinking == nil || models[1].Thinking.Min != 0 || models[1].Thinking.Max != 0 || !models[1].Thinking.DynamicAllowed {
		t.Fatalf("adaptive model published a token budget: %+v", models[1].Thinking)
	}
}

func TestTopLevelRosterSpecEnrichesCatalogOnlyModels(t *testing.T) {
	models := exposedModels([]mirasim.RemoteModel{{ID: "glm-5.3-flash"}, {ID: "deepseek-flash"}, {ID: "claude-sonnet-5"}})
	applyRoster(models, mirasim.ModelRoster{Version: "live", Models: map[string]mirasim.ModelSpec{
		"glm-5.3-flash":   {ID: "glm-5.3-flash", Label: "GLM Live", ContextWindow: 900000, MaxOutput: 75000, Effort: []string{"low", "high", "max"}},
		"deepseek-flash":  {ID: "deepseek-flash", MaxOutput: 380000, Effort: []string{"off", "high"}},
		"claude-sonnet-5": {ID: "claude-sonnet-5", Label: "Sonnet Live", MaxOutput: 64000},
	}})
	if models[0].DisplayName != "GLM Live" || models[0].ContextLength != 900000 || models[0].MaxCompletionTokens != 75000 || len(models[0].Thinking.Levels) != 3 {
		t.Fatalf("top-level GLM spec = %#v", models[0])
	}
	if models[1].MaxCompletionTokens != 380000 || len(models[1].Thinking.Levels) != 2 || models[1].Thinking.Levels[0] != "off" {
		t.Fatalf("top-level DeepSeek spec = %#v", models[1])
	}
	if models[2].DisplayName != "Sonnet Live" || models[2].MaxCompletionTokens != 64000 || models[2].Thinking.Min != 0 {
		t.Fatalf("top-level Sonnet spec = %#v", models[2])
	}
}
