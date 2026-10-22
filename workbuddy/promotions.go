// promotions.go owns the upstream model-promotion (活动/折扣) surface: the
// `data.modelPromotions` and `data.modelTiers` arrays of WorkBuddy's
// /v3/config payload.
//
// Motivation (2026-10-12): the panel reported each model's *base* credit
// multiplier (`models[].credits`, e.g. `"x0.29"`) and nothing else,
// so an operator sizing a request could not see that a model is free between
// 23:00 and 08:00, or that another model is free until a campaign deadline. The upstream
// already publishes those campaigns; the plugin was decoding only `models` and
// dropping the sibling keys. This file keeps them.
//
// Two rules drive the whole design:
//
//  1. `models[].credits` is the BASE price and does NOT move with a promotion.
//     Verified live 2026-10-12 23:33 (inside a 23:00–08:00 free window):
//     credits stayed "x0.29" while the promotion said factor 0. So the
//     effective price has to be composed from both. (Contrast qoder, where the
//     gateway folds its off-peak discount into price_factor itself — the two
//     plugins must therefore NOT share one composition rule.)
//
//  2. Whether a campaign is running *right now* is a function of the clock, so
//     it must not be frozen into the cache. The cache stores the campaign's
//     normalized windows plus the raw text; the panel decides "active" on every
//     render from a server-supplied clock. That is what makes the badge flip by
//     itself at 08:00 instead of going stale until the next refresh.
//
// Time normalization is done here, once, in Go — the panel is dependency-free
// vanilla JS with no timezone library, and the upstream schedules are authored
// in a named zone (Asia/Shanghai) that the browser may not be in. Each daily
// window is therefore converted to a UTC minute-of-day interval, and the panel
// is handed a UTC clock to compare against. See utcMinuteOfDay for why a
// day-of-week mask is deliberately not part of the contract.
package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Bounds. The panel renders these verbatim, so a hostile or buggy upstream
// value must not be able to bloat the cache or the management response.
const (
	maxPromotions        = 128
	maxPromotionModelIDs = 256
	maxPromotionWindows  = 16
	maxPromotionText     = 512
	maxPromotionLabel    = 64
	maxPromotionColor    = 32
	maxPromotionID       = 128
)

// ---------------------------------------------------------------------------
// Wire types (upstream JSON)
// ---------------------------------------------------------------------------

// workBuddyV3ConfigWire is the decoded /v3/config envelope. `models` is decoded
// separately by parseWorkBuddyV3Config so that a promotion schema change can
// never take the model catalog down with it.
type workBuddyV3ConfigWire struct {
	Code *int `json:"code"`
	Data *struct {
		ModelPromotions []workBuddyPromotionWire `json:"modelPromotions"`
		ModelTiers      []workBuddyTierWire      `json:"modelTiers"`
	} `json:"data"`
}

// workBuddyPromotionWire is one entry of data.modelPromotions.
//
// Every member is optional and decoded leniently: upstream ships promotion
// entries with and without `discount` (a badge-only teaser that advertises a
// campaign without changing the price), with and without `schedule`, and with
// either a `daily` window list or a `validFrom`/`validUntil` date range. One
// shape we do not understand must degrade to "less information", never to a
// failed catalog.
type workBuddyPromotionWire struct {
	ID       string                   `json:"id"`
	Kind     string                   `json:"kind"`
	Enabled  *bool                    `json:"enabled"`
	Priority *int                     `json:"priority"`
	ModelIDs []string                 `json:"modelIds"`
	Badge    *workBuddyPromoBadgeWire `json:"badge"`
	Discount *workBuddyPromoDiscWire  `json:"discount"`
	Schedule *workBuddyPromoSchedWire `json:"schedule"`
	Hover    *workBuddyPromoHoverWire `json:"hover"`
}

type workBuddyPromoBadgeWire struct {
	Label   string `json:"label"`
	LabelZh string `json:"labelZh"`
	LabelEn string `json:"labelEn"`
	Color   string `json:"color"`
	// Display is "activeOnly" when upstream itself only wants the badge shown
	// while the campaign is running. Recorded for completeness; the panel
	// applies its own window check either way.
	Display string `json:"display"`
}

type workBuddyPromoDiscWire struct {
	Factor            *float64 `json:"factor"`
	DiscountedCredits string   `json:"discountedCredits"`
	DisplayMode       string   `json:"displayMode"`
}

type workBuddyPromoSchedWire struct {
	Timezone   string                  `json:"timezone"`
	Daily      []workBuddyPromoDayWire `json:"daily"`
	ValidFrom  string                  `json:"validFrom"`
	ValidUntil string                  `json:"validUntil"`
}

type workBuddyPromoDayWire struct {
	Start string `json:"start"` // "23:00"
	End   string `json:"end"`   // "7:50"
}

type workBuddyPromoHoverWire struct {
	TextZh string `json:"textZh"`
	TextEn string `json:"textEn"`
}

// workBuddyTierWire is one entry of data.modelTiers ("订阅优先" style rows).
// Tiers gate scheduling priority, not price, so they are reported separately
// from promotions and never participate in the effective-rate composition.
type workBuddyTierWire struct {
	ID               string   `json:"id"`
	Tier             string   `json:"tier"`
	ModelIDs         []string `json:"modelIds"`
	Priority         *int     `json:"priority"`
	Enabled          *bool    `json:"enabled"`
	RequiredUserType string   `json:"requiredUserType"`
	Hover            *struct {
		TextZh string `json:"textZh"`
		TextEn string `json:"textEn"`
	} `json:"hover"`
	Badge *struct {
		Label string `json:"label"`
	} `json:"badge"`
}

// ---------------------------------------------------------------------------
// Stored / served types
// ---------------------------------------------------------------------------

// modelPromotion is one normalized campaign. It is what the cache persists and
// what the panel receives; the upstream shape is never passed through.
type modelPromotion struct {
	ID         string   `json:"id"`
	Kind       string   `json:"kind"`
	ModelIDs   []string `json:"modelIds"`
	Priority   int      `json:"priority"`
	BadgeLabel string   `json:"badgeLabel,omitempty"`
	BadgeColor string   `json:"badgeColor,omitempty"`
	Text       string   `json:"text,omitempty"`

	// Factor is the discount multiplier applied to the base credits. nil means
	// the campaign does not change the price (a badge-only teaser) — which is a
	// different claim from factor 0 (free), so it must not collapse to 0.
	Factor *float64 `json:"factor,omitempty"`
	// Discounted is upstream's own rendered result ("0.00x"). Preferred for
	// display over recomputing base*factor, because it carries upstream's own
	// rounding.
	Discounted  string `json:"discounted,omitempty"`
	DisplayMode string `json:"displayMode,omitempty"` // "replace" | "strikethrough"

	// Windows are UTC minute-of-day intervals. Empty means "no daily
	// restriction" (the campaign is bounded only by ValidFrom/ValidUntil).
	Windows []promoWindow `json:"windows,omitempty"`

	ValidFromUnix  *int64 `json:"validFromUnix,omitempty"`
	ValidUntilUnix *int64 `json:"validUntilUnix,omitempty"`
	// Raw RFC3339 bounds, kept for the tooltip and for auditing what upstream
	// actually said.
	ValidFromRaw  string `json:"validFromRaw,omitempty"`
	ValidUntilRaw string `json:"validUntilRaw,omitempty"`
	// ValidUntilDisplay is the LAST DAY the campaign runs, i.e. validUntil
	// minus one second rendered as a date. Upstream states the bound as an
	// exclusive midnight (a campaign can have validUntil at 00:00 while its
	// own copy says the previous calendar day), so printing the raw bound as
	// the end date would overstate every campaign by a day.
	ValidUntilDisplay string `json:"validUntilDisplay,omitempty"`
	Timezone          string `json:"timezone,omitempty"`
}

// promoWindow is one recurring daily interval, already converted out of the
// campaign's timezone.
type promoWindow struct {
	// StartMin is the UTC minute-of-day the window opens (0..1439).
	StartMin int `json:"startMin"`
	// DurationMin is the window length. A value >= 1440 means "all day".
	DurationMin int `json:"durationMin"`
	// StartLabel/EndLabel are the upstream local wall-clock labels ("23:00",
	// "7:50"), kept verbatim for display. They are NOT what the panel matches
	// against — StartMin/DurationMin are.
	StartLabel string `json:"startLabel,omitempty"`
	EndLabel   string `json:"endLabel,omitempty"`
}

// modelTier is one normalized tier row.
type modelTier struct {
	ID       string   `json:"id"`
	Tier     string   `json:"tier,omitempty"`
	ModelIDs []string `json:"modelIds,omitempty"`
	Priority int      `json:"priority"`
	Label    string   `json:"label,omitempty"`
	Text     string   `json:"text,omitempty"`
	Required string   `json:"requiredUserType,omitempty"`
}

// promoClock is the server's wall clock in the zone the windows were
// normalized against. The panel advances it locally with elapsed time so a
// long-open tab flips the badge at the window boundary without a refetch, and
// so the verdict never depends on the browser's own timezone.
type promoClock struct {
	Unix    int64  `json:"unix"`
	Minute  int    `json:"minute"` // UTC minute-of-day, 0..1439
	Zone    string `json:"zone"`
	Unknown bool   `json:"unknown,omitempty"` // true when no schedule zone could be resolved
}

// ---------------------------------------------------------------------------
// Parsing
// ---------------------------------------------------------------------------

// parseWorkBuddyV3Promotions decodes data.modelPromotions / data.modelTiers
// from the same /v3/config body parseWorkBuddyV3Config reads.
//
// It is deliberately a separate pass over the same bytes rather than a change
// to parseWorkBuddyV3Config's signature: the model-catalog parse has a large
// test surface and is on the critical path for every request, while this data
// is panel-only. A malformed promotions block returns an error that the caller
// logs and drops — it never fails the catalog.
func parseWorkBuddyV3Promotions(raw []byte) ([]modelPromotion, []modelTier, error) {
	var response workBuddyV3ConfigWire
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, nil, fmt.Errorf("decode v3 promotions: %w", err)
	}
	if response.Code == nil || *response.Code != 0 {
		return nil, nil, fmt.Errorf("v3 config business code is not successful")
	}
	if response.Data == nil {
		return nil, nil, fmt.Errorf("v3 config data is missing")
	}
	return normalizeModelPromotions(response.Data.ModelPromotions),
		normalizeModelTiers(response.Data.ModelTiers), nil
}

// normalizeModelPromotions converts the upstream entries into the stored shape,
// dropping anything unusable. A single bad entry is skipped rather than failing
// the batch: upstream ships campaigns on its own release schedule, and one
// malformed row must not hide the other seven.
func normalizeModelPromotions(entries []workBuddyPromotionWire) []modelPromotion {
	now := time.Now()
	out := make([]modelPromotion, 0, len(entries))
	for _, entry := range entries {
		promo, ok := normalizeOnePromotion(entry, now)
		if !ok {
			continue
		}
		out = append(out, promo)
		if len(out) >= maxPromotions {
			break
		}
	}
	return out
}

func normalizeOnePromotion(entry workBuddyPromotionWire, now time.Time) (modelPromotion, bool) {
	id := boundedString(entry.ID, maxPromotionID)
	if id == "" {
		return modelPromotion{}, false
	}
	// An absent `enabled` means the campaign is on; only an explicit false
	// switches it off. Upstream omits the key on some entries.
	if entry.Enabled != nil && !*entry.Enabled {
		return modelPromotion{}, false
	}
	modelIDs := boundedStringList(entry.ModelIDs, maxPromotionModelIDs)
	if len(modelIDs) == 0 {
		return modelPromotion{}, false
	}

	promo := modelPromotion{
		ID:       id,
		Kind:     boundedString(entry.Kind, maxPromotionLabel),
		ModelIDs: modelIDs,
	}
	if entry.Priority != nil {
		promo.Priority = *entry.Priority
	}
	if entry.Badge != nil {
		// labelZh/labelEn are honoured before label: upstream's Global realm
		// ships `label: "Free now"` while still filling labelZh, so the
		// language-specific keys are the more precise source when present.
		promo.BadgeLabel = boundedString(
			firstNonEmptyModelDescription(entry.Badge.LabelZh, entry.Badge.LabelEn, entry.Badge.Label),
			maxPromotionLabel)
		promo.BadgeColor = boundedString(entry.Badge.Color, maxPromotionColor)
	}
	if entry.Hover != nil {
		promo.Text = boundedString(
			firstNonEmptyModelDescription(entry.Hover.TextZh, entry.Hover.TextEn),
			maxPromotionText)
	}
	if entry.Discount != nil {
		promo.Factor = cloneFloat64Ptr(entry.Discount.Factor)
		promo.Discounted = boundedString(entry.Discount.DiscountedCredits, maxPromotionLabel)
		promo.DisplayMode = boundedString(entry.Discount.DisplayMode, maxPromotionLabel)
	}
	if entry.Schedule != nil {
		applyPromotionSchedule(&promo, *entry.Schedule, now)
	}
	return promo, true
}

// applyPromotionSchedule converts one upstream schedule into absolute bounds
// plus UTC-normalized daily windows.
func applyPromotionSchedule(promo *modelPromotion, sched workBuddyPromoSchedWire, now time.Time) {
	zoneName := strings.TrimSpace(sched.Timezone)
	loc := resolvePromotionLocation(zoneName)
	promo.Timezone = zoneName
	if promo.Timezone == "" {
		promo.Timezone = loc.String()
	}

	if raw := strings.TrimSpace(sched.ValidFrom); raw != "" {
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			unix := t.Unix()
			promo.ValidFromUnix = &unix
			promo.ValidFromRaw = raw
		}
	}
	if raw := strings.TrimSpace(sched.ValidUntil); raw != "" {
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			unix := t.Unix()
			promo.ValidUntilUnix = &unix
			promo.ValidUntilRaw = raw
			// The bound is exclusive, so the last day the campaign covers is
			// the instant just before it. Subtracting one second (rather than a
			// whole day) is what keeps a non-midnight bound from losing a day:
			// 2026-10-26T18:00 -> 2026-10-26, not 2026-10-25.
			promo.ValidUntilDisplay = t.Add(-time.Second).In(loc).Format("2006-01-02")
		}
	}

	_, offsetSeconds := now.In(loc).Zone()
	offsetMinutes := offsetSeconds / 60
	for _, day := range sched.Daily {
		if len(promo.Windows) >= maxPromotionWindows {
			break
		}
		window, ok := normalizePromoWindow(day, offsetMinutes)
		if !ok {
			continue
		}
		promo.Windows = append(promo.Windows, window)
	}
}

// resolvePromotionLocation loads the schedule's named zone, falling back to the
// process-local zone when the name is missing or unknown. It never returns nil:
// a window normalized against the wrong zone is still better than dropping the
// campaign, and the zone actually used is reported in promoClock.Zone so an
// operator can see which midnight the panel matched against.
func resolvePromotionLocation(zoneName string) *time.Location {
	if zoneName == "" {
		return time.Local
	}
	loc, err := time.LoadLocation(zoneName)
	if err != nil {
		return time.Local
	}
	return loc
}

// normalizePromoWindow turns one "23:00"→"7:50" pair into a UTC minute-of-day
// interval.
//
// offsetMinutes is the campaign zone's offset from UTC at the current instant.
// Applying one offset to every window is correct for the zones upstream uses
// (Asia/Shanghai has no DST) and is deliberately preferred over per-date
// resolution, which would need a reference day and would silently pick the
// wrong side of a DST transition anyway.
func normalizePromoWindow(day workBuddyPromoDayWire, offsetMinutes int) (promoWindow, bool) {
	startLocal, okStart := parseClockMinute(day.Start)
	endLocal, okEnd := parseClockMinute(day.End)
	if !okStart || !okEnd {
		return promoWindow{}, false
	}
	// Length in local minutes. A window that ends where it starts is read as
	// "all day": upstream expresses a full-day campaign by omitting `daily`
	// entirely, so an explicit equal pair is the only way left to say it.
	duration := ((endLocal-startLocal)%1440 + 1440) % 1440
	if duration == 0 {
		duration = 1440
	}
	startUTC := ((startLocal-offsetMinutes)%1440 + 1440) % 1440
	return promoWindow{
		StartMin:    startUTC,
		DurationMin: duration,
		StartLabel:  boundedString(strings.TrimSpace(day.Start), maxPromotionLabel),
		EndLabel:    boundedString(strings.TrimSpace(day.End), maxPromotionLabel),
	}, true
}

// parseClockMinute parses "23:00" / "7:50" into minutes since local midnight.
// Accepts a bare hour ("8") and an optional ":ss" suffix because upstream has
// shipped both across other config surfaces.
func parseClockMinute(value string) (int, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, false
	}
	parts := strings.Split(trimmed, ":")
	if len(parts) > 3 {
		return 0, false
	}
	hour, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || hour < 0 || hour > 24 {
		return 0, false
	}
	minute := 0
	if len(parts) > 1 {
		minute, err = strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil || minute < 0 || minute > 59 {
			return 0, false
		}
	}
	if hour == 24 && minute != 0 {
		return 0, false
	}
	return hour*60 + minute, true
}

func normalizeModelTiers(entries []workBuddyTierWire) []modelTier {
	out := make([]modelTier, 0, len(entries))
	for _, entry := range entries {
		if entry.Enabled != nil && !*entry.Enabled {
			continue
		}
		tier := modelTier{
			ID:       boundedString(entry.ID, maxPromotionID),
			Tier:     boundedString(entry.Tier, maxPromotionLabel),
			ModelIDs: boundedStringList(entry.ModelIDs, maxPromotionModelIDs),
			Required: boundedString(entry.RequiredUserType, maxPromotionLabel),
		}
		if entry.Priority != nil {
			tier.Priority = *entry.Priority
		}
		if entry.Badge != nil {
			tier.Label = boundedString(entry.Badge.Label, maxPromotionLabel)
		}
		if entry.Hover != nil {
			tier.Text = boundedString(
				firstNonEmptyModelDescription(entry.Hover.TextZh, entry.Hover.TextEn),
				maxPromotionText)
		}
		if tier.ID == "" {
			continue
		}
		out = append(out, tier)
	}
	return out
}

// ---------------------------------------------------------------------------
// Selection
// ---------------------------------------------------------------------------

// promotionsForModel lists the campaigns naming one model, ordered the way the
// panel wants to consume them: highest priority first, ties broken by ID so the
// order is stable across refreshes.
//
// The panel still has to decide which of these is running *now* — that is
// time-dependent and is not decided here.
func promotionsForModel(promos []modelPromotion, modelID string) []modelPromotion {
	id := strings.TrimSpace(modelID)
	if id == "" {
		return nil
	}
	var matched []modelPromotion
	for _, promo := range promos {
		for _, candidate := range promo.ModelIDs {
			if candidate == id {
				matched = append(matched, promo)
				break
			}
		}
	}
	sort.SliceStable(matched, func(i, j int) bool {
		if matched[i].Priority != matched[j].Priority {
			return matched[i].Priority > matched[j].Priority
		}
		return matched[i].ID < matched[j].ID
	})
	return matched
}

// ---------------------------------------------------------------------------
// Clock
// ---------------------------------------------------------------------------

// promotionClock builds the clock the panel matches windows against. It reports
// UTC minute-of-day because the windows were normalized to UTC.
//
// zone is informational only — it names the zone the windows were authored in,
// so the panel can say "每晚 23:00—次日 8:00（Asia/Shanghai）" and an operator
// can tell whose midnight is meant.
func promotionClock(promos []modelPromotion, now time.Time) promoClock {
	zone := ""
	for _, promo := range promos {
		if promo.Timezone != "" {
			zone = promo.Timezone
			break
		}
	}
	if zone == "" {
		zone = "UTC"
	}
	utc := now.UTC()
	return promoClock{
		Unix:   utc.Unix(),
		Minute: utc.Hour()*60 + utc.Minute(),
		Zone:   zone,
	}
}

// ---------------------------------------------------------------------------
// Bounded helpers
// ---------------------------------------------------------------------------

func boundedString(value string, limit int) string {
	trimmed := strings.TrimSpace(value)
	if limit <= 0 || len(trimmed) <= limit {
		return trimmed
	}
	// Trim on a rune boundary so a multi-byte label cannot be cut mid-rune.
	cut := trimmed[:limit]
	for len(cut) > 0 && !isUTF8Boundary(cut) {
		cut = cut[:len(cut)-1]
	}
	return strings.TrimSpace(cut)
}

func isUTF8Boundary(s string) bool {
	if s == "" {
		return true
	}
	return s[len(s)-1]&0xC0 != 0x80
}

func boundedStringList(values []string, limit int) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if _, dup := seen[trimmed]; dup {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, boundedString(trimmed, maxPromotionID))
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// clonePromotionList deep-copies a promotion slice, mirroring the cloning the
// model facts get: a snapshot must never alias a cache entry, or a later
// mutation would edit published data in place.
func clonePromotionList(promos []modelPromotion) []modelPromotion {
	if promos == nil {
		return nil
	}
	out := make([]modelPromotion, len(promos))
	for i, promo := range promos {
		out[i] = clonePromotion(promo)
	}
	return out
}

func clonePromotion(promo modelPromotion) modelPromotion {
	cloned := promo
	cloned.ModelIDs = append([]string(nil), promo.ModelIDs...)
	cloned.Factor = cloneFloat64Ptr(promo.Factor)
	cloned.ValidFromUnix = cloneInt64(promo.ValidFromUnix)
	cloned.ValidUntilUnix = cloneInt64(promo.ValidUntilUnix)
	cloned.Windows = append([]promoWindow(nil), promo.Windows...)
	return cloned
}

func cloneTierList(tiers []modelTier) []modelTier {
	if tiers == nil {
		return nil
	}
	out := make([]modelTier, len(tiers))
	for i, tier := range tiers {
		out[i] = tier
		out[i].ModelIDs = append([]string(nil), tier.ModelIDs...)
	}
	return out
}
