package main

import (
	"strings"

	"gopkg.in/yaml.v3"
)

func normalizedModelContextConfig(node yaml.Node) (map[string]int64, error) {
	if node.Kind == 0 || (node.Kind == yaml.ScalarNode && node.Tag == "!!null" && node.Style&yaml.TaggedStyle == 0) {
		return nil, nil
	}
	if node.Kind != yaml.MappingNode || node.Tag != "!!map" || node.Style&yaml.TaggedStyle != 0 {
		return nil, &modelConfigError{field: "model_context", msg: "must be a map of model ID to positive integer"}
	}
	out := make(map[string]int64, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || key.Style&yaml.TaggedStyle != 0 {
			return nil, &modelConfigError{field: "model_context", msg: "keys must be strings"}
		}
		id := strings.TrimSpace(key.Value)
		if id == "" || len(id) > maxDiscoveredModelIDBytes {
			return nil, &modelConfigError{field: "model_context", msg: "model IDs must be non-empty and bounded"}
		}
		if _, dup := out[id]; dup {
			return nil, &modelConfigError{field: "model_context", msg: "model IDs must not be duplicated"}
		}
		if value.Kind != yaml.ScalarNode || value.Tag != "!!int" || value.Style&yaml.TaggedStyle != 0 {
			return nil, &modelConfigError{field: "model_context", msg: "values must be positive integers"}
		}
		var n int64
		if err := value.Decode(&n); err != nil || n <= 0 || n > maxContextWindowValue {
			return nil, &modelConfigError{field: "model_context", msg: "values must be positive and within the supported maximum"}
		}
		out[id] = n
	}
	return out, nil
}

func currentModelContextOverrides() map[string]int64 {
	cfg := currentFeatureRuntime()
	out := make(map[string]int64)
	if cfg == nil {
		return out
	}
	for id, value := range cfg.modelContext {
		out[id] = value
	}
	return out
}

func setModelContextOverride(id string, value *int64) map[string]int64 {
	id = strings.TrimSpace(id)
	cfg := currentFeatureRuntime()
	if cfg == nil {
		return map[string]int64{}
	}
	next := *cfg
	next.modelContext = currentModelContextOverrides()
	if value == nil || *value <= 0 {
		delete(next.modelContext, id)
	} else {
		next.modelContext[id] = *value
	}
	currentModelRuntime().commitFeatureRuntime(&next)
	return next.modelContext
}
