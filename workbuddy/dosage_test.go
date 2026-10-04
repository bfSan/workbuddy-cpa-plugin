package main

import "testing"

// TestAggregatePackages_TotalDosageMustNotInflateUsed is the regression test for
// the plan-upgrade display bug.
//
// Upstream TotalDosage is a lifetime counter: it accumulates every package the
// account has ever been granted, including the dozens of 签到裂变包 that have long
// expired. TotalRemain sums only the spendable ones. The two count different
// sets, so deriving spend as TotalDosage - TotalRemain produces a number that
// means nothing.
//
// Observed on 2026-10-04 after a plan upgrade, with the real package list:
//
//	remain=10199  used=99500  size=109699   <- what the panel showed
//	remain=10199  used= 4530  size= 14729   <- what the 59 packages summed to
//
// The displayed figures are self-contradictory: remain+used should equal size,
// and 10199+4530 = 14729, nowhere near 109699.
func TestAggregatePackages_TotalDosageMustNotInflateUsed(t *testing.T) {
	var packs []resourcePackage
	// 56 expired 签到裂变包, fully consumed. These are the bulk of the lifetime
	// TotalDosage and none of them are spendable.
	for i := 0; i < 56; i++ {
		packs = append(packs, resourcePackage{
			PackageName:         "CodeBuddy个人版国内运营裂变包",
			CycleCapacityRemain: 0,
			CycleCapacityUsed:   100,
			CycleCapacitySize:   100,
		})
	}
	// Three live packages from the upgraded plan.
	packs = append(packs,
		resourcePackage{PackageName: "Buddy AI个人高级版", CycleCapacityRemain: 4000, CycleCapacityUsed: 0, CycleCapacitySize: 4000},
		resourcePackage{PackageName: "Codebuddy版本赠送包", CycleCapacityRemain: 5000, CycleCapacityUsed: 0, CycleCapacitySize: 5000},
		resourcePackage{PackageName: "CodeBuddy个人版拉新权益包", CycleCapacityRemain: 100, CycleCapacityUsed: 0, CycleCapacitySize: 100},
	)
	// 56*100 expired fission credits plus 9100 still spendable.
	// The lifetime figure upstream reports is 109699.
	const lifetime = 109699

	sum := aggregatePackages(packs, lifetime)

	if sum.TotalSize != 14700 {
		t.Errorf("TotalSize = %d, want 14700 (sum of package sizes)", sum.TotalSize)
	}
	// Only the 56 fully-consumed fission packs count as used.
	if sum.TotalUsed != 5600 {
		t.Errorf("TotalUsed = %d, want 5600; TotalDosage leaked into it", sum.TotalUsed)
	}
	if sum.TotalRemain != 9100 {
		t.Errorf("TotalRemain = %d, want 9100", sum.TotalRemain)
	}
	// The invariant the panel's arithmetic depends on.
	if sum.TotalRemain+sum.TotalUsed != sum.TotalSize {
		t.Errorf("remain+used = %d but size = %d; the three figures are inconsistent",
			sum.TotalRemain+sum.TotalUsed, sum.TotalSize)
	}
	// The lifetime figure is preserved, but strictly as a separate field.
	if sum.TotalDosage != lifetime {
		t.Errorf("TotalDosage = %d, want %d", sum.TotalDosage, lifetime)
	}
}

// TotalDosage being small or absent must not change anything: it is display-only.
func TestAggregatePackages_TotalDosageIsDisplayOnly(t *testing.T) {
	packs := []resourcePackage{
		{PackageName: "p", CycleCapacityRemain: 300, CycleCapacityUsed: 700, CycleCapacitySize: 1000},
	}
	base := aggregatePackages(packs, 0)
	for _, lifetime := range []int64{1, 1000, 1000, 999999} {
		got := aggregatePackages(packs, lifetime)
		if got.TotalSize != base.TotalSize || got.TotalUsed != base.TotalUsed || got.TotalRemain != base.TotalRemain {
			t.Errorf("lifetime=%d changed the totals: size %d→%d used %d→%d remain %d→%d",
				lifetime, base.TotalSize, got.TotalSize, base.TotalUsed, got.TotalUsed,
				base.TotalRemain, got.TotalRemain)
		}
	}
}

// A plan upgrade adds packages. That is a capacity grant, and it must raise size
// and remain without being counted as negative consumption.
func TestAggregatePackages_UpgradeAddsCapacity(t *testing.T) {
	before := aggregatePackages([]resourcePackage{
		{PackageName: "fission", CycleCapacityRemain: 0, CycleCapacityUsed: 100, CycleCapacitySize: 100},
	}, 100)
	after := aggregatePackages([]resourcePackage{
		{PackageName: "fission", CycleCapacityRemain: 0, CycleCapacityUsed: 100, CycleCapacitySize: 100},
		{PackageName: "premium", CycleCapacityRemain: 4000, CycleCapacityUsed: 0, CycleCapacitySize: 4000},
	}, 4100)
	if after.TotalSize <= before.TotalSize {
		t.Errorf("size did not grow on upgrade: %d → %d", before.TotalSize, after.TotalSize)
	}
	if after.TotalRemain != before.TotalRemain+4000 {
		t.Errorf("remain = %d, want %d", after.TotalRemain, before.TotalRemain+4000)
	}
	if after.TotalUsed != before.TotalUsed {
		t.Errorf("used changed on upgrade: %d → %d; a grant is not consumption", before.TotalUsed, after.TotalUsed)
	}
}

// The per-package totals and the aggregate must agree, because the panel shows
// both and a mismatch between them is the confusing part of the report.
func TestAggregatePackages_HeaderMatchesRows(t *testing.T) {
	packs := []resourcePackage{
		{PackageName: "a", CycleCapacityRemain: 500, CycleCapacityUsed: 0, CycleCapacitySize: 500},
		{PackageName: "b", CycleCapacityRemain: 0, CycleCapacityUsed: 1500, CycleCapacitySize: 1500},
		{PackageName: "c", CycleCapacityRemain: 200, CycleCapacityUsed: 300, CycleCapacitySize: 500},
	}
	sum := aggregatePackages(packs, 99999)
	var r, u, s int64
	for _, p := range sum.Packages {
		r += p.Remain
		u += p.Used
		s += p.Size
	}
	if r != sum.TotalRemain || u != sum.TotalUsed || s != sum.TotalSize {
		t.Errorf("header (%d/%d/%d) disagrees with rows (%d/%d/%d)",
			sum.TotalRemain, sum.TotalUsed, sum.TotalSize, r, u, s)
	}
	if sum.PackCount != len(sum.Packages) {
		t.Errorf("PackCount = %d, want %d", sum.PackCount, len(sum.Packages))
	}
}

// No packages at all must not produce a bogus size or a negative used.
func TestAggregatePackages_Empty(t *testing.T) {
	sum := aggregatePackages(nil, 0)
	if sum.TotalRemain != 0 || sum.TotalUsed != 0 || sum.TotalSize != 0 {
		t.Errorf("empty aggregate = %+v, want all zero", sum)
	}
	if sum.PackCount != 0 {
		t.Errorf("PackCount = %d, want 0", sum.PackCount)
	}
}
