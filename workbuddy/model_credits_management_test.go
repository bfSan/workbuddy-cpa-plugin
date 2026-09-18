package main

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestHandleModelCreditsWrite_PinsAndClears(t *testing.T) {
	resetModelCredits(t)
	recordModelCredits("glm-5.2", "x0.79 credits")

	res := handleModelCreditsWrite(pluginapi.ManagementRequest{
		Body: mustMarshal(t, map[string]string{"model": "glm-5.2", "credits": "x0.40"}),
	})
	if ok, _ := res["success"].(bool); !ok {
		t.Fatalf("pin failed: %+v", res)
	}
	if res["source"] != "override" || res["credits"] != "x0.40" {
		t.Fatalf("pin result = %+v", res)
	}

	// Clearing restores the upstream value rather than leaving it blank.
	res = handleModelCreditsWrite(pluginapi.ManagementRequest{
		Body: mustMarshal(t, map[string]string{"model": "glm-5.2", "credits": ""}),
	})
	if res["credits"] != "x0.79 credits" || res["source"] != "upstream" {
		t.Fatalf("clear result = %+v", res)
	}
}

func TestHandleModelCreditsWrite_RejectsBadInput(t *testing.T) {
	resetModelCredits(t)
	for _, tt := range []struct {
		name string
		body string
	}{
		{name: "no model", body: `{"credits":"x1"}`},
		{name: "blank model", body: `{"model":"  ","credits":"x1"}`},
		{name: "broken json", body: `{not json`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			res := handleModelCreditsWrite(pluginapi.ManagementRequest{Body: []byte(tt.body)})
			if ok, _ := res["success"].(bool); ok {
				t.Fatalf("should reject: %+v", res)
			}
		})
	}
	if entries := modelCreditsSnapshot(); len(entries) != 0 {
		t.Fatalf("rejected writes must not change state: %+v", entries)
	}
}

func TestHandleModelCreditsWrite_AcceptsIDAlias(t *testing.T) {
	resetModelCredits(t)
	res := handleModelCreditsWrite(pluginapi.ManagementRequest{
		Body: mustMarshal(t, map[string]string{"id": "hy3", "credits": "x0.01"}),
	})
	if ok, _ := res["success"].(bool); !ok {
		t.Fatalf("id alias rejected: %+v", res)
	}
	if value, source := modelCredits("hy3"); value != "x0.01" || source != "override" {
		t.Fatalf("hy3 = %q/%q", value, source)
	}
}

func TestHandleModelCreditsQuery_ListsSources(t *testing.T) {
	resetModelCredits(t)
	recordModelCredits("zeta", "x0.9")
	setModelCreditsOverride("alpha", "x0.1")
	res := handleModelCreditsQuery()
	if res["count"] != 2 {
		t.Fatalf("count = %v", res["count"])
	}
	entries, _ := res["credits"].([]map[string]any)
	if len(entries) != 2 || entries[0]["id"] != "alpha" {
		t.Fatalf("entries = %+v", entries)
	}
	if entries[0]["source"] != "override" || entries[1]["source"] != "upstream" {
		t.Fatalf("sources = %+v", entries)
	}
}
