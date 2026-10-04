package main

import "testing"

func TestSummarizeCredits(t *testing.T) {
	accounts := []wbAccount{
		{
			Region:  "cn",
			Credits: &creditsSummary{TotalRemain: 100, TotalUsed: 50, Packages: []packageSummary{{Name: "a"}}},
		},
		{
			Region:  "global",
			Credits: &creditsSummary{TotalRemain: 20, TotalUsed: 230, Packages: []packageSummary{{Name: "b"}}},
		},
		{
			Region:   "cn",
			Disabled: true,
			// unknown credits — ignored in totals
		},
		{
			Region:    "cn",
			Exhausted: true,
			Credits:   &creditsSummary{TotalRemain: 0, TotalUsed: 10, Packages: []packageSummary{{Name: "c"}}},
		},
	}
	sum := summarizeCredits(accounts)
	if sum["total_remain"].(int64) != 120 {
		t.Fatalf("remain=%v want 120", sum["total_remain"])
	}
	if sum["total_used"].(int64) != 290 {
		t.Fatalf("used=%v want 290", sum["total_used"])
	}
	if sum["cn_remain"].(int64) != 100 {
		t.Fatalf("cn_remain=%v", sum["cn_remain"])
	}
	if sum["global_used"].(int64) != 230 {
		t.Fatalf("global_used=%v", sum["global_used"])
	}
	if sum["known_count"].(int) != 3 {
		t.Fatalf("known=%v", sum["known_count"])
	}
	if sum["disabled_count"].(int) != 1 {
		t.Fatalf("disabled=%v", sum["disabled_count"])
	}
	if sum["exhausted_count"].(int) != 1 {
		t.Fatalf("exhausted=%v", sum["exhausted_count"])
	}
}

// The silent `if size > total { total = size }` that used to live here is what
// concealed the TotalDosage bug: an account whose size was inflated to 109699
// while its packages summed to 14729 still produced a plausible total, so
// nothing indicated the figures were wrong. A disagreement between two numbers
// that are supposed to be equal is now reported rather than smoothed over.
func TestSummarizeCreditsFlagsInconsistentTotals(t *testing.T) {
	// Consistent: size == remain+used.
	ok := summarizeCredits([]wbAccount{{
		Credits: &creditsSummary{TotalRemain: 100, TotalUsed: 50, TotalSize: 150},
	}})
	if got := ok["inconsistent"].(int64); got != 0 {
		t.Errorf("inconsistent = %d, want 0 for consistent figures", got)
	}
	if got := ok["total"].(int64); got != 150 {
		t.Errorf("total = %d, want 150", got)
	}

	// The real shape of the bug: size far exceeds remain+used.
	bad := summarizeCredits([]wbAccount{{
		Credits: &creditsSummary{TotalRemain: 10194, TotalUsed: 4535, TotalSize: 109699},
	}})
	if got := bad["inconsistent"].(int64); got != 1 {
		t.Errorf("inconsistent = %d, want 1; the disagreement must be visible", got)
	}
	// total still covers the pool so the bar is not understated, but only
	// alongside the flag saying it is untrustworthy.
	if got := bad["total"].(int64); got != 109699 {
		t.Errorf("total = %d, want the larger figure 109699", got)
	}
}

// A plan that has granted nothing yet has size 0, which is not an inconsistency.
func TestSummarizeCreditsZeroSizeIsNotInconsistent(t *testing.T) {
	sum := summarizeCredits([]wbAccount{{
		Credits: &creditsSummary{TotalRemain: 40, TotalUsed: 10},
	}})
	if got := sum["inconsistent"].(int64); got != 0 {
		t.Errorf("inconsistent = %d, want 0 when size is 0", got)
	}
	if got := sum["total"].(int64); got != 50 {
		t.Errorf("total = %d, want 50", got)
	}
}
