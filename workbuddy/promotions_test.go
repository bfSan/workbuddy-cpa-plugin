package main

import (
	"testing"
	"time"
)

func TestParseWorkBuddyPromotionsNormalizesWindowAndDeadline(t *testing.T) {
	raw := []byte(`{
		"code":0,
		"data":{
			"modelPromotions":[{
				"id":"night-demo",
				"kind":"discount",
				"enabled":true,
				"priority":50,
				"modelIds":["demo-model"],
				"badge":{"label":"夜间免费","color":"#009273"},
				"discount":{"factor":0,"discountedCredits":"0.00x","displayMode":"strikethrough"},
				"schedule":{"timezone":"Asia/Shanghai","daily":[{"start":"23:00","end":"08:00"}],"validFrom":"2026-10-12T00:00:00+08:00","validUntil":"2026-10-26T00:00:00+08:00"},
				"hover":{"textZh":"每晚免费"}
			}],
			"modelTiers":[]
		}
	}`)
	promos, tiers, err := parseWorkBuddyV3Promotions(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(tiers) != 0 || len(promos) != 1 {
		t.Fatalf("promos=%d tiers=%d", len(promos), len(tiers))
	}
	p := promos[0]
	if p.Factor == nil || *p.Factor != 0 {
		t.Fatalf("factor=%v", p.Factor)
	}
	if p.ValidUntilDisplay != "2026-10-25" {
		t.Fatalf("display deadline=%q, want 2026-10-25", p.ValidUntilDisplay)
	}
	if len(p.Windows) != 1 || p.Windows[0].StartMin != 900 || p.Windows[0].DurationMin != 540 {
		t.Fatalf("normalized window=%+v, want UTC 15:00 for 9h", p.Windows)
	}
}

func TestPromotionClockUsesUTCMinute(t *testing.T) {
	clock := promotionClock([]modelPromotion{{Timezone: "Asia/Shanghai"}}, time.Date(2026, 10, 13, 0, 5, 0, 0, time.FixedZone("CST", 8*3600)))
	if clock.Minute != 16*60+5 {
		t.Fatalf("clock minute=%d, want 965", clock.Minute)
	}
	if clock.Zone != "Asia/Shanghai" {
		t.Fatalf("clock zone=%q", clock.Zone)
	}
}

func TestPromotionsForModelPriorityOrder(t *testing.T) {
	got := promotionsForModel([]modelPromotion{
		{ID: "low", Priority: 1, ModelIDs: []string{"m"}},
		{ID: "high", Priority: 10, ModelIDs: []string{"m"}},
	}, "m")
	if len(got) != 2 || got[0].ID != "high" {
		t.Fatalf("order=%v", got)
	}
}
