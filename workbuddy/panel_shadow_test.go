package main

import (
	"regexp"
	"strings"
	"testing"
)

// The panel is a single HTML file of plain JavaScript, so a function parameter
// can silently shadow a global helper. That is not hypothetical: loadModels took
// its first parameter as `toast`, shadowing the global toast() helper, and the
// refresh path then called it as if it were the helper. Every refresh click died
// with "toast is not a function" — and because the failure only happens on the
// branch that calls the helper, the bug stayed latent until the branch was used.
//
// A Go test cannot execute the panel's DOM code, but it can catch exactly this
// class of mistake, which is what shipped the broken refresh button.
func TestPanelFunctionsDoNotShadowGlobalHelpers(t *testing.T) {
	html := string(panelHTML)
	globals := panelGlobalNames(html)
	if len(globals) < 50 {
		t.Fatalf("found only %d global names; the extractor is probably broken", len(globals))
	}

	// Sanity check the extractor against the helper that was actually shadowed.
	if !globals["toast"] {
		t.Fatal("expected `toast` to be detected as a global helper")
	}

	re := regexp.MustCompile(`\bfunction\s+([A-Za-z_$][A-Za-z0-9_$]*)\s*\(`)
	for _, m := range re.FindAllStringSubmatchIndex(html, -1) {
		name := html[m[2]:m[3]]
		open := m[1] - 1 // index of "("
		closeIdx := scanBalanced(html, open, '(', ')')
		if closeIdx < 0 {
			continue
		}
		params := html[open+1 : closeIdx]

		braceIdx := strings.IndexByte(html[closeIdx:], '{')
		if braceIdx < 0 {
			continue
		}
		bodyStart := closeIdx + braceIdx
		bodyEnd := scanBalanced(html, bodyStart, '{', '}')
		if bodyEnd < 0 {
			continue
		}
		body := html[bodyStart : bodyEnd+1]

		for _, param := range strings.Split(params, ",") {
			param = strings.TrimSpace(param)
			if eq := strings.IndexByte(param, '='); eq >= 0 {
				param = strings.TrimSpace(param[:eq])
			}
			if param == "" || !globals[param] || param == name {
				continue
			}
			// Only a problem when the shadowed name is actually called, which
			// is the case that throws at runtime.
			if !strings.Contains(body, param+"(") {
				continue
			}
			line := strings.Count(html[:m[0]], "\n") + 1
			t.Errorf("panel.html:%d: parameter %q of %s() shadows the global %s() and is called as a function in the body; rename the parameter",
				line, param, name, param)
		}
	}
}

// panelGlobalNames collects the names a top-level call site would resolve to:
// function declarations and const/let/var bindings that sit at column zero.
// Nested (indented) declarations are local and cannot be shadowed this way.
func panelGlobalNames(src string) map[string]bool {
	out := map[string]bool{}
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`(?m)^(?:async\s+)?function\s+([A-Za-z_$][A-Za-z0-9_$]*)`),
		regexp.MustCompile(`(?m)^(?:const|let|var)\s+([A-Za-z_$][A-Za-z0-9_$]*)\s*=`),
	} {
		for _, m := range re.FindAllStringSubmatch(src, -1) {
			out[m[1]] = true
		}
	}
	return out
}

// scanBalanced returns the index of the delimiter that closes the one at start,
// skipping JavaScript string literals and comments so that braces and parens
// inside them do not confuse the depth count.
func scanBalanced(src string, start int, open, close byte) int {
	depth := 0
	for i := start; i < len(src); {
		switch c := src[i]; c {
		case '\'', '"', '`':
			i = skipJSString(src, i)
			continue
		case '/':
			if i+1 < len(src) {
				if src[i+1] == '/' {
					for i < len(src) && src[i] != '\n' {
						i++
					}
					continue
				}
				if src[i+1] == '*' {
					i += 2
					for i+1 < len(src) && !(src[i] == '*' && src[i+1] == '/') {
						i++
					}
					i += 2
					continue
				}
			}
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return i
			}
		}
		i++
	}
	return -1
}

// skipJSString returns the index just past the string literal starting at i.
func skipJSString(src string, i int) int {
	quote := src[i]
	i++
	for i < len(src) {
		if src[i] == '\\' {
			i += 2
			continue
		}
		if src[i] == quote {
			return i + 1
		}
		if quote != '`' && src[i] == '\n' {
			// Unterminated single-line string; bail out rather than run away.
			return i
		}
		i++
	}
	return i
}
