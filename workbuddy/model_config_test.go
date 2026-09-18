package main

import (
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func idsOf(models []pluginapi.ModelInfo) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.ID)
	}
	return out
}

// managementRequestWithBody builds a management request carrying a JSON body.
func managementRequestWithBody(body string) pluginapi.ManagementRequest {
	return pluginapi.ManagementRequest{Body: []byte(body)}
}

func TestApplyModelOverlay_HideRemovesModel(t *testing.T) {
	base := []pluginapi.ModelInfo{
		defaultModelInfo("a", ""),
		defaultModelInfo("b", ""),
		defaultModelInfo("c", ""),
	}
	got := idsOf(applyModelOverlay(base, modelOverlay{Hide: []string{"b"}}))
	want := []string{"a", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("hide: got %v, want %v", got, want)
	}
}

func TestApplyModelOverlay_OrderPinsToFront(t *testing.T) {
	base := []pluginapi.ModelInfo{
		defaultModelInfo("a", ""),
		defaultModelInfo("b", ""),
		defaultModelInfo("c", ""),
	}
	got := idsOf(applyModelOverlay(base, modelOverlay{Order: []string{"c", "a"}}))
	want := []string{"c", "a", "b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order: got %v, want %v", got, want)
	}
}

func TestApplyModelOverlay_AddAppendsAfterBase(t *testing.T) {
	base := []pluginapi.ModelInfo{defaultModelInfo("a", "")}
	got := idsOf(applyModelOverlay(base, modelOverlay{Add: []string{"z"}}))
	want := []string{"a", "z"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("add: got %v, want %v", got, want)
	}
}

func TestApplyModelOverlay_HideWinsOverAdd(t *testing.T) {
	base := []pluginapi.ModelInfo{defaultModelInfo("a", "")}
	got := idsOf(applyModelOverlay(base, modelOverlay{
		Hide: []string{"z"},
		Add:  []string{"z"},
	}))
	want := []string{"a"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("hide over add: got %v, want %v", got, want)
	}
}

func TestApplyModelOverlay_EmptyOverlayIsIdentity(t *testing.T) {
	base := []pluginapi.ModelInfo{
		defaultModelInfo("a", ""),
		defaultModelInfo("b", ""),
	}
	if got, want := idsOf(applyModelOverlay(base, modelOverlay{})), []string{"a", "b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("identity: got %v, want %v", got, want)
	}
}

func TestApplyModelOverlay_DoesNotMutateInput(t *testing.T) {
	base := []pluginapi.ModelInfo{
		defaultModelInfo("a", ""),
		defaultModelInfo("b", ""),
	}
	_ = applyModelOverlay(base, modelOverlay{Order: []string{"b"}})
	if got, want := idsOf(base), []string{"a", "b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("input mutated: got %v, want %v", got, want)
	}
}

func TestStoreModelOverlay_RejectsBlankAndDuplicate(t *testing.T) {
	if _, err := storeModelOverlay(modelOverlay{Hide: []string{" "}}); err == nil {
		t.Fatal("expected blank entry to be rejected")
	}
	if _, err := storeModelOverlay(modelOverlay{Add: []string{"x", "x"}}); err == nil {
		t.Fatal("expected duplicate entry to be rejected")
	}
}

func TestStoreModelOverlay_BumpsRevision(t *testing.T) {
	defer setModelOverlayForTest(modelOverlay{})()
	first, err := storeModelOverlay(modelOverlay{Hide: []string{"a"}})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	second, err := storeModelOverlay(modelOverlay{Hide: []string{"b"}})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if second.Revision <= first.Revision {
		t.Fatalf("revision did not advance: %d then %d", first.Revision, second.Revision)
	}
	loaded, _ := loadedModelOverlay()
	if !reflect.DeepEqual(loaded.Hide, []string{"b"}) {
		t.Fatalf("overlay not committed: %v", loaded.Hide)
	}
}

func TestHandleModelOverlayAction_HideThenRestore(t *testing.T) {
	defer setModelOverlayForTest(modelOverlay{})()
	res := handleModelOverlayAction(managementRequestWithBody(`{"action":"hide","id":"m1"}`))
	if res["success"] != true {
		t.Fatalf("hide failed: %v", res)
	}
	loaded, _ := loadedModelOverlay()
	if !reflect.DeepEqual(loaded.Hide, []string{"m1"}) {
		t.Fatalf("hide did not persist: %v", loaded)
	}
	res = handleModelOverlayAction(managementRequestWithBody(`{"action":"restore","id":"m1"}`))
	if res["success"] != true {
		t.Fatalf("restore failed: %v", res)
	}
	loaded, _ = loadedModelOverlay()
	if len(loaded.Hide) != 0 {
		t.Fatalf("restore left hide entries: %v", loaded.Hide)
	}
}

func TestHandleModelOverlayAction_MoveSwapsNeighbours(t *testing.T) {
	defer setModelOverlayForTest(modelOverlay{Order: []string{"a", "b", "c"}})()
	res := handleModelOverlayAction(managementRequestWithBody(`{"action":"move","id":"c","offset":-1}`))
	if res["success"] != true {
		t.Fatalf("move failed: %v", res)
	}
	loaded, _ := loadedModelOverlay()
	want := []string{"a", "c", "b"}
	if !reflect.DeepEqual(loaded.Order, want) {
		t.Fatalf("move: got %v, want %v", loaded.Order, want)
	}
}

func TestHandleModelOverlayAction_MoveBeyondEdgeIsNoop(t *testing.T) {
	defer setModelOverlayForTest(modelOverlay{Order: []string{"a", "b"}})()
	res := handleModelOverlayAction(managementRequestWithBody(`{"action":"move","id":"a","offset":-1}`))
	if res["success"] != true {
		t.Fatalf("move at edge should still succeed: %v", res)
	}
	loaded, _ := loadedModelOverlay()
	if !reflect.DeepEqual(loaded.Order, []string{"a", "b"}) {
		t.Fatalf("edge move changed order: %v", loaded.Order)
	}
}

func TestHandleModelOverlayAction_AddThenHideClearsPin(t *testing.T) {
	defer setModelOverlayForTest(modelOverlay{})()
	if res := handleModelOverlayAction(managementRequestWithBody(`{"action":"add","id":"custom-1"}`)); res["success"] != true {
		t.Fatalf("add failed: %v", res)
	}
	loaded, _ := loadedModelOverlay()
	if !reflect.DeepEqual(loaded.Add, []string{"custom-1"}) {
		t.Fatalf("add did not persist: %v", loaded.Add)
	}
	if res := handleModelOverlayAction(managementRequestWithBody(`{"action":"hide","id":"custom-1"}`)); res["success"] != true {
		t.Fatalf("hide failed: %v", res)
	}
	loaded, _ = loadedModelOverlay()
	if len(loaded.Add) != 0 {
		t.Fatalf("hide should drop the synthetic add: %v", loaded.Add)
	}
}

func TestHandleModelOverlayAction_RejectsUnknownAction(t *testing.T) {
	res := handleModelOverlayAction(managementRequestWithBody(`{"action":"explode","id":"x"}`))
	if res["success"] != false {
		t.Fatalf("unknown action should fail: %v", res)
	}
}

func TestHandleModelOverlayAction_RejectsMissingID(t *testing.T) {
	res := handleModelOverlayAction(managementRequestWithBody(`{"action":"hide"}`))
	if res["success"] != false {
		t.Fatalf("missing id should fail: %v", res)
	}
}

func TestModelSourceRankOrdering(t *testing.T) {
	if modelSourceRank(modelSourceConfig) <= modelSourceRank(modelSourceFresh) {
		t.Fatal("config should outrank fresh")
	}
	if modelSourceRank(modelSourceFresh) <= modelSourceRank(modelSourceCache) {
		t.Fatal("fresh should outrank cache")
	}
	if modelSourceRank(modelSourceNone) != 0 {
		t.Fatal("none should rank lowest")
	}
}
