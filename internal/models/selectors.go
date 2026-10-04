package models

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPIPlugins/mirasim/internal/thinking"
)

// withRelayAliases republishes a relay model under the extra selectors this
// plugin has shipped. A selector an operator already configured has to keep
// routing, and CPA rejects a model it was never told about, so the alias must
// be registered rather than only stripped in the executor. The alias entry
// carries the metadata of the model it names.
func withRelayAliases(models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	out := append([]pluginapi.ModelInfo(nil), models...)
	seen := make(map[string]bool, len(models))
	for _, m := range models {
		seen[strings.ToLower(m.ID)] = true
	}
	for _, m := range models {
		for _, alias := range thinking.SelectorAliasesFor(m.ID) {
			if seen[alias] {
				continue
			}
			entry := m
			entry.ID = alias
			entry.Name = alias
			entry.Thinking = cloneThinking(m.Thinking)
			entry.SupportedParameters = cloneStrings(m.SupportedParameters)
			entry.SupportedGenerationMethods = cloneStrings(m.SupportedGenerationMethods)
			entry.SupportedInputModalities = cloneStrings(m.SupportedInputModalities)
			entry.SupportedOutputModalities = cloneStrings(m.SupportedOutputModalities)
			out = append(out, entry)
			seen[alias] = true
		}
	}
	return out
}

// Register selectors with CPA as well as stripping them in the executor;
// otherwise model routing would reject the selector before reaching the plugin.
func withLongContextAliases(models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	out := append([]pluginapi.ModelInfo(nil), models...)
	seen := map[string]bool{}
	for _, m := range models {
		seen[strings.ToLower(m.ID)] = true
	}
	for _, m := range models {
		if !strings.HasPrefix(strings.ToLower(m.ID), "claude-") || strings.Contains(m.ID, "[") || m.ContextLength < 1000000 {
			continue
		}
		id := m.ID + "[1m]"
		if seen[strings.ToLower(id)] {
			continue
		}
		m.ID = id
		m.Name = id
		m.DisplayName += " [1m]"
		m.Thinking = cloneThinking(m.Thinking)
		out = append(out, m)
		seen[strings.ToLower(id)] = true
	}
	return out
}
