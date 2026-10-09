package main

import (
	"reflect"
	"testing"
)

func TestModelOrderConfigReloadAndIsolation(t *testing.T) {
	old := featureRuntime.Load()
	t.Cleanup(func() { featureRuntime.Store(old) })
	defer setModelOverlayForTest(modelOverlay{Add: []string{"custom"}})()
	runtime := &modelRuntime{}
	initial, err := parseFeatureRuntime([]byte("hidden_models: [hidden]\nmodel_order: [a, b]\n"))
	if err != nil {
		t.Fatal(err)
	}
	runtime.commitFeatureRuntime(initial)
	generation := runtime.configGeneration.Load()
	next, err := parseFeatureRuntime([]byte("hidden_models: [hidden]\nmodel_order: [b, a]\n"))
	if err != nil {
		t.Fatal(err)
	}
	runtime.commitFeatureRuntime(next)
	if runtime.configGeneration.Load() != generation {
		t.Fatal("sorting invalidated catalog generation")
	}
	overlay, _ := loadedModelOverlay()
	if !reflect.DeepEqual(overlay.Order, []string{"b", "a"}) || !reflect.DeepEqual(overlay.Hide, []string{"hidden"}) || !reflect.DeepEqual(overlay.Add, []string{"custom"}) {
		t.Fatalf("overlay: %#v", overlay)
	}
	next.modelOrder[0] = "mutated"
	got := currentFeatureRuntime()
	if got.modelOrder[0] != "b" {
		t.Fatal("commit aliases caller order")
	}
	got.modelOrder[0] = "mutated"
	if currentFeatureRuntime().modelOrder[0] != "b" {
		t.Fatal("read aliases stored order")
	}
	// Simulate process-local overlay loss and reload the saved config.
	syncOverlayModelOrder(nil)
	reloaded, err := parseFeatureRuntime([]byte("hidden_models: [hidden]\nmodel_order: [b, a]\n"))
	if err != nil {
		t.Fatal(err)
	}
	runtime.commitFeatureRuntime(reloaded)
	overlay, _ = loadedModelOverlay()
	if !reflect.DeepEqual(overlay.Order, []string{"b", "a"}) {
		t.Fatal("saved order was not restored")
	}
}

func TestModelOrderRejectsMalformedConfig(t *testing.T) {
	for _, raw := range []string{"model_order: string\n", "model_order: [123]\n", "model_order: ['']\n"} {
		if _, err := parseFeatureRuntime([]byte(raw)); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
}
