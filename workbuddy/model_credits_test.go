package main

import (
	"testing"
)

func resetModelCredits(t *testing.T) {
	t.Helper()
	creditsMu.Lock()
	prevUpstream := upstreamCredits
	prevOverride := overrideCredits
	upstreamCredits = make(map[string]string)
	overrideCredits = make(map[string]string)
	creditsMu.Unlock()
	t.Cleanup(func() {
		creditsMu.Lock()
		upstreamCredits = prevUpstream
		overrideCredits = prevOverride
		creditsMu.Unlock()
	})
}

func TestParseModelCreditsRate_HandlesUpstreamFormats(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"x0.29", 0.29},
		{"x2.20 credits", 2.20},
		{"x0.00", 0},
		{"0.50x", 0.5},
		{"x1", 1},
		{"  x0.65  ", 0.65},
		{"", 0},
		{"free", 0},
		{"-x1", 0},
		{"x1 2", 0},
	}
	for _, c := range cases {
		if got := parseModelCreditsRate(c.in); got != c.want {
			t.Fatalf("parseModelCreditsRate(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestModelCredits_OverrideWinsOverUpstream(t *testing.T) {
	resetModelCredits(t)
	recordModelCredits("glm-5.2", "x0.79 credits")
	if value, source := modelCredits("glm-5.2"); value != "x0.79 credits" || source != "upstream" {
		t.Fatalf("upstream value = %q/%q", value, source)
	}
	setModelCreditsOverride("glm-5.2", "x0.40")
	if value, source := modelCredits("glm-5.2"); value != "x0.40" || source != "override" {
		t.Fatalf("override should win, got %q/%q", value, source)
	}
	// Clearing the override falls back to upstream rather than to nothing.
	setModelCreditsOverride("glm-5.2", "")
	if value, source := modelCredits("glm-5.2"); value != "x0.79 credits" || source != "upstream" {
		t.Fatalf("cleared override should reveal upstream, got %q/%q", value, source)
	}
}

func TestRecordModelCredits_EmptyClearsStaleValue(t *testing.T) {
	resetModelCredits(t)
	recordModelCredits("hy3", "x0.00")
	// A source that stops reporting the field must not leave a stale number.
	recordModelCredits("hy3", "")
	if value, source := modelCredits("hy3"); value != "" || source != "" {
		t.Fatalf("stale upstream value survived: %q/%q", value, source)
	}
	recordModelCredits(" ", "x1")
	if entries := modelCreditsSnapshot(); len(entries) != 0 {
		t.Fatalf("blank model ID must be ignored, got %+v", entries)
	}
}

func TestNormalizeModelCredits_BoundsHostileValues(t *testing.T) {
	if got := normalizeModelCredits("x1" + string(make([]byte, 0)) + multiString("0", 200)); got != "" {
		t.Fatalf("oversized value accepted: %q", got)
	}
	if got := normalizeModelCredits("x0.5\ncredits"); got != "" {
		t.Fatalf("multiline value accepted: %q", got)
	}
	if got := normalizeModelCredits("  x0.29  "); got != "x0.29" {
		t.Fatalf("trim = %q", got)
	}
}

func multiString(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}

func TestModelCreditsSnapshot_SortedAndSourceTagged(t *testing.T) {
	resetModelCredits(t)
	recordModelCredits("zeta", "x0.9")
	recordModelCredits("alpha", "x0.1")
	setModelCreditsOverride("beta", "x0.5")
	entries := modelCreditsSnapshot()
	if len(entries) != 3 {
		t.Fatalf("want 3 entries, got %+v", entries)
	}
	if entries[0]["id"] != "alpha" || entries[1]["id"] != "beta" || entries[2]["id"] != "zeta" {
		t.Fatalf("not sorted by model ID: %+v", entries)
	}
	byID := map[string]string{}
	for _, e := range entries {
		byID[e["id"].(string)] = e["source"].(string)
	}
	if byID["beta"] != "override" || byID["alpha"] != "upstream" {
		t.Fatalf("source tags wrong: %+v", byID)
	}
}

func TestSyncConfiguredCredits_ReplacesOverrideTable(t *testing.T) {
	resetModelCredits(t)
	setModelCreditsOverride("pinned-by-config", "x1.5")
	setModelCreditsOverride("panel-only", "x2.5")
	// A reload is authoritative: pins absent from config are dropped.
	syncConfiguredCredits(map[string]string{"pinned-by-config": "x3.0", "unpinned": ""})
	if value, source := modelCredits("pinned-by-config"); value != "x3.0" || source != "override" {
		t.Fatalf("config value should win on reload, got %q/%q", value, source)
	}
	if value, _ := modelCredits("panel-only"); value != "" {
		t.Fatalf("panel-only pin should be dropped on reload, got %q", value)
	}
	if value, _ := modelCredits("unpinned"); value != "" {
		t.Fatalf("empty config value should not pin, got %q", value)
	}
}

func TestModelCreditsField_UnknownIsBlankNotZero(t *testing.T) {
	resetModelCredits(t)
	field := modelCreditsField("never-seen")
	if field["value"] != "" || field["source"] != "" {
		t.Fatalf("unknown model should report blank, got %+v", field)
	}
	recordModelCredits("hy3", "x0.00")
	field = modelCreditsField("hy3")
	if field["value"] != "x0.00" || field["source"] != "upstream" {
		t.Fatalf("known model = %+v", field)
	}
	// x0.00 parses to numeric zero, which must still be distinguishable from
	// "unknown" via the source field.
	if rate, _ := field["rate"].(float64); rate != 0 {
		t.Fatalf("x0.00 rate = %v", rate)
	}
}
