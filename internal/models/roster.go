package models

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/mirasim"
)

// Overlay specifications only on models the account's live catalog exposes.
func applyRoster(models []pluginapi.ModelInfo, roster mirasim.ModelRoster) {
	for i := range models {
		m := &models[i]
		spec, ok := roster.Spec(m.ID)
		if !ok {
			continue
		}
		if spec.ContextWindow > 0 {
			m.ContextLength = spec.ContextWindow
			m.InputTokenLimit = spec.ContextWindow
		}
		if spec.MaxOutput > 0 {
			m.MaxCompletionTokens = spec.MaxOutput
			m.OutputTokenLimit = spec.MaxOutput
		}
		if spec.Label != "" {
			m.DisplayName = spec.Label
		}
		// CPA model metadata has no auto-compaction-ratio field. Do not mislabel it
		// as the model's context limit; compaction remains the caller's responsibility.
		levels := []string{}
		seen := map[string]bool{}
		for _, level := range spec.Effort {
			level = strings.ToLower(strings.TrimSpace(level))
			if rosterEffortSupported(m.Type, level) && !seen[level] {
				levels = append(levels, level)
				seen[level] = true
			}
		}
		// The roster's adaptive flag is the only thing that selects the upstream
		// thinking form, so publish the bounds that form actually accepts: an
		// effort string carries no token budget, and a budget model cannot take
		// an effort string. Advertising both invites a request the relay rejects.
		adaptive, shapeKnown := roster.ThinkingAdaptive(m.ID)
		if m.Type == "claude" && (shapeKnown || len(levels) > 0) {
			// A catalog-only model uses the same roster shape as the executor.
			// An effort-only specification keeps the relay's adaptive default.
			if adaptive || !shapeKnown {
				if m.Thinking == nil {
					m.Thinking = adaptiveRelayThinking()
				}
				m.Thinking.Min, m.Thinking.Max = 0, 0
				m.Thinking.DynamicAllowed = true
				m.SupportedParameters = appendUnique(m.SupportedParameters, "thinking", "output_config")
			} else {
				if m.Thinking == nil {
					m.Thinking = budgetRelayThinking()
				}
				m.Thinking.Min, m.Thinking.Max = 1024, 128000
				m.SupportedParameters = appendUnique(m.SupportedParameters, "thinking")
			}
		}
		if len(levels) > 0 {
			if m.Thinking == nil {
				m.Thinking = &pluginapi.ThinkingSupport{}
			}
			m.Thinking.Levels = levels
			m.SupportedParameters = appendUnique(m.SupportedParameters, "thinking")
		}
	}
}

func rosterEffortSupported(modelType, level string) bool {
	switch modelType {
	case "deepseek":
		return level == "off" || level == "low" || level == "high" || level == "max"
	case "glm", "kimi":
		return level == "low" || level == "high" || level == "max"
	case "gemini":
		return level == "off" || level == "minimal" || level == "low" || level == "medium" || level == "high"
	default:
		switch level {
		case "low", "medium", "high", "xhigh", "max", "ultra":
			return true
		default:
			return false
		}
	}
}

func appendUnique(values []string, extra ...string) []string {
	for _, v := range extra {
		found := false
		for _, old := range values {
			if old == v {
				found = true
				break
			}
		}
		if !found {
			values = append(values, v)
		}
	}
	return values
}
