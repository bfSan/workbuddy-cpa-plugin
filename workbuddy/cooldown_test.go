package main

import (
	"encoding/json"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// resetCooldowns empties the table and restores the clock around a test.
func resetCooldowns(t *testing.T) {
	t.Helper()
	cooldownMu.Lock()
	prev := cooldownTable
	cooldownTable = make(map[modelCooldownKey]modelCooldownEntry)
	cooldownMu.Unlock()
	cooldownNowFn = time.Now
	t.Cleanup(func() {
		cooldownMu.Lock()
		cooldownTable = prev
		cooldownMu.Unlock()
		cooldownNowFn = time.Now
	})
}

func TestMarkModelCooldown_RequiresBothKeys(t *testing.T) {
	resetCooldowns(t)
	markModelCooldown("wb-a", "", cooldownReasonRateLimit)
	markModelCooldown("", "model-1", cooldownReasonRateLimit)
	if got := cooldownSnapshotAll(); len(got) != 0 {
		t.Fatalf("blank auth or model must not create an entry, got %+v", got)
	}
}

func TestMarkModelCooldown_IsScopedToModel(t *testing.T) {
	resetCooldowns(t)
	markModelCooldown("wb-a", "model-1", cooldownReasonRateLimit)
	if !modelIsCooling("wb-a", "model-1") {
		t.Fatal("model-1 should be cooling")
	}
	if modelIsCooling("wb-a", "model-2") {
		t.Fatal("model-2 must stay usable: cooldown is per model, not per account")
	}
	if modelIsCooling("wb-b", "model-1") {
		t.Fatal("another account must stay usable for the same model")
	}
}

func TestModelIsCooling_ExpiresOnItsOwn(t *testing.T) {
	resetCooldowns(t)
	base := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	now := base
	cooldownNowFn = func() time.Time { return now }
	markModelCooldown("wb-a", "model-1", cooldownReasonUnknown)
	if !modelIsCooling("wb-a", "model-1") {
		t.Fatal("should cool right after the failure")
	}
	now = base.Add(modelCooldownUnknown + time.Second)
	if modelIsCooling("wb-a", "model-1") {
		t.Fatal("should recover once the TTL passes")
	}
	if got := cooldownSnapshotAll(); len(got) != 0 {
		t.Fatalf("expired entries should be swept, got %+v", got)
	}
}

func TestRecordUpstreamFailure_LeavesHardCreditToLifecycle(t *testing.T) {
	resetCooldowns(t)
	recordUpstreamFailure("wb-a", "model-1", 402, `{"message":"insufficient credit"}`)
	if got := cooldownSnapshotAll(); len(got) != 0 {
		t.Fatalf("402 is the lifecycle path's job, got %+v", got)
	}
	recordUpstreamFailure("wb-a", "model-1", 429, `{"message":"too many requests"}`)
	if !modelIsCooling("wb-a", "model-1") {
		t.Fatal("pure 429 should cool the pair")
	}
	// No model ID → nothing to key on; the account path owns that case.
	resetCooldowns(t)
	recordUpstreamFailure("wb-a", "   ", 429, `{"message":"too many requests"}`)
	if got := cooldownSnapshotAll(); len(got) != 0 {
		t.Fatalf("missing model must not freeze the account, got %+v", got)
	}
}

func TestClearModelCooldown_OnePairOrWholeAccount(t *testing.T) {
	resetCooldowns(t)
	markModelCooldown("wb-a", "model-1", cooldownReasonRateLimit)
	markModelCooldown("wb-a", "model-2", cooldownReasonRateLimit)
	markModelCooldown("wb-b", "model-1", cooldownReasonRateLimit)
	if got := clearModelCooldown("wb-a", "model-1"); got != 1 {
		t.Fatalf("clearing one pair removed %d", got)
	}
	if modelIsCooling("wb-a", "model-1") || !modelIsCooling("wb-a", "model-2") {
		t.Fatal("only the named pair should be cleared")
	}
	if got := clearModelCooldown("wb-a", ""); got != 1 {
		t.Fatalf("clearing the account removed %d, want 1", got)
	}
	if modelIsCooling("wb-a", "model-2") {
		t.Fatal("account-wide clear should drop every pair")
	}
	if !modelIsCooling("wb-b", "model-1") {
		t.Fatal("clearing wb-a must not touch wb-b")
	}
}

func TestCooldownSnapshotFor_OnlyOwnEntries(t *testing.T) {
	resetCooldowns(t)
	markModelCooldown("wb-a", "model-2", cooldownReasonRateLimit)
	markModelCooldown("wb-a", "model-1", cooldownReasonRateLimit)
	rows := cooldownSnapshotFor("wb-a")
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %+v", rows)
	}
	if rows[0]["model"] != "model-1" {
		t.Fatalf("rows should be sorted by model, got %+v", rows[0])
	}
	for _, r := range rows {
		if r["auth_id"] != nil {
			t.Fatalf("per-account snapshot should not repeat auth_id, got %+v", r)
		}
		if _, ok := r["seconds"].(int); !ok {
			t.Fatalf("seconds should be a number, got %+v", r)
		}
	}
	if got := cooldownSnapshotFor(""); got != nil {
		t.Fatalf("blank auth should return nil, got %+v", got)
	}
}

func TestHandleCooldownList_ReadsQueryScope(t *testing.T) {
	resetCooldowns(t)
	markModelCooldown("wb-a", "model-1", cooldownReasonRateLimit)
	markModelCooldown("wb-b", "model-1", cooldownReasonRateLimit)
	all := handleCooldownList(pluginapi.ManagementRequest{})
	if all["count"] != 2 {
		t.Fatalf("global count = %v, want 2", all["count"])
	}
	rows, _ := all["entries"].([]map[string]any)
	if len(rows) != 2 || rows[0]["auth_id"] != "wb-a" {
		t.Fatalf("should be sorted by auth then model, got %+v", rows)
	}
	one := handleCooldownList(pluginapi.ManagementRequest{Query: url.Values{"auth_id": []string{"wb-b"}}})
	if one["count"] != 1 {
		t.Fatalf("scoped count = %v, want 1", one["count"])
	}
}

func TestHandleCooldownClear_RefusesGlobalWipe(t *testing.T) {
	resetCooldowns(t)
	markModelCooldown("wb-a", "model-1", cooldownReasonRateLimit)
	markModelCooldown("wb-b", "model-1", cooldownReasonRateLimit)
	if ok, _ := handleCooldownClear(pluginapi.ManagementRequest{})["success"].(bool); ok {
		t.Fatal("clearing without auth_id must fail")
	}
	if !modelIsCooling("wb-a", "model-1") || !modelIsCooling("wb-b", "model-1") {
		t.Fatal("a rejected clear must not change state")
	}
	res := handleCooldownClear(pluginapi.ManagementRequest{
		Body: mustMarshal(t, map[string]string{"auth_id": "wb-a"}),
	})
	if ok, _ := res["success"].(bool); !ok {
		t.Fatalf("body-scoped clear failed: %+v", res)
	}
	if modelIsCooling("wb-a", "model-1") {
		t.Fatal("wb-a should be cleared")
	}
	if !modelIsCooling("wb-b", "model-1") {
		t.Fatal("wb-b should survive")
	}
}

func TestHandleCooldownClear_InvalidBodyIsIgnored(t *testing.T) {
	resetCooldowns(t)
	res := handleCooldownClear(pluginapi.ManagementRequest{Body: []byte("{not json")})
	if ok, _ := res["success"].(bool); ok {
		t.Fatalf("broken body must be rejected, got %+v", res)
	}
}

func TestCooldownTable_BoundedByMaxEntries(t *testing.T) {
	resetCooldowns(t)
	for i := 0; i < modelCooldownMaxEntries+16; i++ {
		markModelCooldown("wb-a", "model-"+strconv.Itoa(i), cooldownReasonRateLimit)
	}
	cooldownMu.Lock()
	size := len(cooldownTable)
	cooldownMu.Unlock()
	if size > modelCooldownMaxEntries {
		t.Fatalf("table grew to %d, cap is %d", size, modelCooldownMaxEntries)
	}
}

func TestCooldownSnapshotIsJSONSerializable(t *testing.T) {
	resetCooldowns(t)
	markModelCooldown("wb-a", "model-1", cooldownReasonRateLimit)
	if _, err := json.Marshal(cooldownSnapshotAll()); err != nil {
		t.Fatalf("snapshot must marshal: %v", err)
	}
}
