package main

import (
	"encoding/json"
	"testing"
)

// triggerHostAuthReregister exists to make the host re-run registration for one
// credential (that is the only moment it calls model.for_auth, and therefore the
// only way a stale per-account catalog gets rebuilt). It must be a pure rewrite:
// every field the operator or lifecycle layer cares about has to survive.
func TestTriggerHostAuthReregisterPreservesMetadata(t *testing.T) {
	base, err := json.Marshal(map[string]any{
		"type":            providerName,
		"provider":        providerName,
		"disabled":        true,
		"disabled_reason": "manual",
		"custom_key":      "keep-me",
		"note":            "Global · 已禁用 · 耗尽 · 余0 已用100",
		"account":         map[string]any{"uid": "uid-rereg", "nickname": "bfengsan"},
		"auth":            map[string]any{"accessToken": "tok", "domain": "www.workbuddy.ai"},
	})
	if err != nil {
		t.Fatal(err)
	}

	var saved []byte
	origGet := hostAuthGetPhysicalFn
	origSave := hostAuthSaveJSONFn
	hostAuthGetPhysicalFn = func(authIndex string) (*hostAuthPhysical, error) {
		return &hostAuthPhysical{AuthIndex: authIndex, Name: "workbuddy-uid-rereg.json", JSON: base}, nil
	}
	hostAuthSaveJSONFn = func(name string, raw []byte) error {
		saved = append([]byte(nil), raw...)
		return nil
	}
	t.Cleanup(func() {
		hostAuthGetPhysicalFn = origGet
		hostAuthSaveJSONFn = origSave
	})

	if err := triggerHostAuthReregister("idx-rereg"); err != nil {
		t.Fatalf("triggerHostAuthReregister: %v", err)
	}
	if len(saved) == 0 {
		t.Fatal("nothing was written back")
	}
	var written map[string]any
	if err := json.Unmarshal(saved, &written); err != nil {
		t.Fatal(err)
	}
	if written["disabled"] != true {
		t.Fatalf("disabled flipped: %v", written["disabled"])
	}
	if written["disabled_reason"] != "manual" {
		t.Fatalf("disabled_reason lost: %v", written["disabled_reason"])
	}
	if written["custom_key"] != "keep-me" {
		t.Fatalf("unknown key dropped: %v", written["custom_key"])
	}
	if written["note"] != "Global · 已禁用 · 耗尽 · 余0 已用100" {
		t.Fatalf("note changed: %v", written["note"])
	}
}
