package main

import (
	"net/http"
	"strings"
)

// Version pins are one source of truth per real client family. They must move
// together across OAuth, token refresh, catalog discovery and chat headers.
//
// WorkBuddy versions were read from the user's installed macOS applications on
// 2026-11-13 without launching them:
//   - com.tencent.workbuddy.mac  5.9.3 (CN)
//   - com.workbuddy.workbuddy-ai 5.9.4 (Global)
//
// Static app.asar inspection also verified that the desktop UA is built as two
// WorkBuddy/<appVersion> tokens plus optional CLI/<platformVersion>, and that
// X-IDE-Version uses appVersion. platformVersion is loaded from product config,
// not embedded beside that code; 2.172.1 is the separately reviewed CLI pin.
// The standalone CLI fallback remains a distinct official client family; never
// combine its UA with WorkBuddy IDE headers inside one request.
const (
	codeBuddyCLIVersion      = "2.172.1"
	workBuddyCNVersion       = "5.9.3"
	workBuddyGlobalVersion   = "5.9.4"
	clientUA                 = "CLI/" + codeBuddyCLIVersion + " CodeBuddy/" + codeBuddyCLIVersion
	workBuddyDesktopUACN     = "WorkBuddy/" + workBuddyCNVersion + " WorkBuddy/" + workBuddyCNVersion
	workBuddyDesktopUAGlobal = "WorkBuddy/" + workBuddyGlobalVersion + " WorkBuddy/" + workBuddyGlobalVersion
)

type workBuddyWireProfile struct {
	Name      string
	Mode      string
	Version   string
	UserAgent string
	IDEType   string
	IDEName   string
	Origin    string
	Platform  string
}

var (
	codeBuddyCLIWireProfile = workBuddyWireProfile{
		Name:      "codebuddy-cli-cn",
		Mode:      oauthClientModeCLI,
		Version:   codeBuddyCLIVersion,
		UserAgent: clientUA,
		IDEType:   "CLI",
		IDEName:   "CLI",
		Origin:    originReferer,
		Platform:  loginPlatformCLI,
	}
	workBuddyCNWireProfile = workBuddyWireProfile{
		Name:      "workbuddy-macos-cn",
		Mode:      oauthClientModeWorkBuddy,
		Version:   workBuddyCNVersion,
		UserAgent: workBuddyDesktopUACN,
		IDEType:   "WorkBuddy",
		IDEName:   "WorkBuddy",
		Origin:    "https://www.workbuddy.cn",
		Platform:  loginPlatformCN,
	}
	workBuddyGlobalWireProfile = workBuddyWireProfile{
		Name:      "workbuddy-macos-global",
		Mode:      oauthClientModeWorkBuddy,
		Version:   workBuddyGlobalVersion,
		UserAgent: workBuddyDesktopUAGlobal,
		IDEType:   "WorkBuddy",
		IDEName:   "WorkBuddy",
		Origin:    originRefererGlobal,
		Platform:  loginPlatformGlobal,
	}
)

func wireProfileForStoredAuth(sa *storedAuth) workBuddyWireProfile {
	if sa != nil && isGlobalDomain(sa.Auth.Domain) {
		// Global tokens can only be minted by the WorkBuddy AI flow.
		return workBuddyGlobalWireProfile
	}
	if sa != nil && sa.Auth.ClientMode == oauthClientModeWorkBuddy {
		return workBuddyCNWireProfile
	}
	// Legacy CN files predate clientMode. Their actual historical chat profile
	// was CLI, so keep that deterministic fallback instead of consulting mutable
	// global config and changing identity after a reload.
	return codeBuddyCLIWireProfile
}

func wireProfileForLogin(mode, region string) workBuddyWireProfile {
	if mode != oauthClientModeWorkBuddy {
		return codeBuddyCLIWireProfile
	}
	if normalizeOAuthLoginRegion(region) == oauthLoginRegionGlobal {
		return workBuddyGlobalWireProfile
	}
	return workBuddyCNWireProfile
}

func catalogWireProfileForRealm(sa *storedAuth, realm workBuddyRealm) workBuddyWireProfile {
	if realm == workBuddyRealmGlobal {
		return workBuddyGlobalWireProfile
	}
	if sa != nil && sa.Auth.ClientMode == oauthClientModeWorkBuddy {
		return workBuddyCNWireProfile
	}
	return codeBuddyCLIWireProfile
}

func oauthProfileForStoredAuth(sa *storedAuth) oauthRequestProfile {
	profile := wireProfileForStoredAuth(sa)
	region := oauthLoginRegionCN
	if sa != nil && isGlobalDomain(sa.Auth.Domain) {
		region = oauthLoginRegionGlobal
	}
	return oauthProfileForModeRegion(profile.Mode, region)
}

func applyWireProfileHeaders(req *http.Request, profile workBuddyWireProfile) {
	if req == nil {
		return
	}
	req.Header.Set("User-Agent", profile.UserAgent)
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("X-IDE-Type", profile.IDEType)
	req.Header.Set("X-IDE-Name", profile.IDEName)
	req.Header.Set("X-IDE-Version", profile.Version)
	if profile.Mode == oauthClientModeWorkBuddy {
		req.Header.Set("X-Private-Data", "true")
	} else {
		req.Header.Del("X-Private-Data")
	}
	if origin := strings.TrimSpace(profile.Origin); origin != "" {
		req.Header.Set("Origin", origin)
		req.Header.Set("Referer", origin+"/")
	}
}

func applyWireChatIntentHeaders(req *http.Request) {
	if req == nil {
		return
	}
	req.Header.Set("X-Agent-Intent", "craft")
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-CodeBuddy-Request", "1")
	applyWireTraceHeaders(req)
}

func applyWireTraceHeaders(req *http.Request) {
	if req == nil {
		return
	}
	traceID := randomHex(16)
	spanID := randomHex(8)
	parentSpanID := randomHex(8)
	req.Header.Set("traceparent", "00-"+traceID+"-"+spanID+"-01")
	req.Header.Set("b3", traceID+"-"+spanID+"-1-"+parentSpanID)
	req.Header.Set("X-Trace-Id", traceID)
	req.Header.Set("X-B3-TraceId", traceID)
	req.Header.Set("X-B3-ParentSpanId", parentSpanID)
	req.Header.Set("X-B3-SpanId", spanID)
	req.Header.Set("X-B3-Sampled", "1")
}
