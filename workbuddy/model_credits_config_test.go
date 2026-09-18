package main

import (
	"strings"
	"testing"
)

func TestParseFeatureRuntimeModelCreditsDefaults(t *testing.T) {
	for _, tt := range []struct {
		name string
		raw  string
	}{
		{name: "absent", raw: ""},
		{name: "null", raw: "model_credits: null\n"},
		{name: "empty inline", raw: "model_credits: {}\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := parseFeatureRuntime([]byte(tt.raw))
			if err != nil {
				t.Fatal(err)
			}
			if len(cfg.configuredCredits) != 0 {
				t.Fatalf("configured credits = %#v, want none", cfg.configuredCredits)
			}
		})
	}
}

func TestParseFeatureRuntimeModelCreditsPreservesValues(t *testing.T) {
	raw := "model_credits:\n  glm-5.2: \"x0.79 credits\"\n  hy3: x0.00\n  deep-model: null\n"
	cfg, err := parseFeatureRuntime([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.configuredCredits["glm-5.2"] != "x0.79 credits" {
		t.Fatalf("glm-5.2 = %q", cfg.configuredCredits["glm-5.2"])
	}
	if cfg.configuredCredits["hy3"] != "x0.00" {
		t.Fatalf("hy3 = %q", cfg.configuredCredits["hy3"])
	}
	// An explicit null un-pins the model rather than pinning it to "".
	if value, ok := cfg.configuredCredits["deep-model"]; !ok || value != "" {
		t.Fatalf("deep-model = %q, present=%v", value, ok)
	}
}

func TestParseFeatureRuntimeModelCreditsRejectsInvalidYAML(t *testing.T) {
	for _, tt := range []struct {
		name string
		raw  string
	}{
		{name: "scalar", raw: "model_credits: fixed\n"},
		{name: "sequence", raw: "model_credits: [glm-5.2]\n"},
		{name: "null-tagged mapping", raw: "model_credits: !!null {glm-5.2: x1}\n"},
		{name: "custom-tagged mapping", raw: "model_credits: !credits {glm-5.2: x1}\n"},
		{name: "non-specific tagged", raw: "model_credits: ! {glm-5.2: x1}\n"},
		{name: "number key", raw: "model_credits:\n  123: x1\n"},
		{name: "empty key", raw: "model_credits:\n  \" \": x1\n"},
		{name: "duplicate after trim", raw: "model_credits:\n  glm-5.2: x1\n  ' glm-5.2 ': x2\n"},
		{name: "number value", raw: "model_credits:\n  glm-5.2: 1\n"},
		{name: "boolean value", raw: "model_credits:\n  glm-5.2: true\n"},
		{name: "nested value", raw: "model_credits:\n  glm-5.2: {rate: x1}\n"},
		{name: "multiline value", raw: "model_credits:\n  glm-5.2: \"x1\\nx2\"\n"},
		{name: "oversized value", raw: "model_credits:\n  glm-5.2: \"x1" + strings.Repeat("0", 200) + "\"\n"},
		{name: "key over 512 bytes", raw: "model_credits:\n  '" + strings.Repeat("x", maxDiscoveredModelIDBytes+1) + "': x1\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if cfg, err := parseFeatureRuntime([]byte(tt.raw)); err == nil {
				t.Fatalf("configured credits = %#v, want error", cfg.configuredCredits)
			}
		})
	}
}

func TestCommitFeatureRuntimeAppliesConfiguredCredits(t *testing.T) {
	resetModelCredits(t)
	cfg, err := parseFeatureRuntime([]byte("model_credits:\n  glm-5.2: x0.40\n"))
	if err != nil {
		t.Fatal(err)
	}
	runtime := newModelRuntime(nil, nil)
	runtime.commitFeatureRuntime(cfg)
	if value, source := modelCredits("glm-5.2"); value != "x0.40" || source != "override" {
		t.Fatalf("configured pin not applied: %q/%q", value, source)
	}
	// A later reload without the entry un-pins it.
	cfg2, err := parseFeatureRuntime(nil)
	if err != nil {
		t.Fatal(err)
	}
	runtime.commitFeatureRuntime(cfg2)
	if value, _ := modelCredits("glm-5.2"); value != "" {
		t.Fatalf("pin should be dropped after reload, got %q", value)
	}
}
