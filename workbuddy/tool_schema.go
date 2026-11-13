// Portions of the WorkBuddy schema-fitting algorithm are adapted from
// OmniRoute 3.8.54 (MIT, Copyright (c) 2026 diegosouzapw). See
// THIRD_PARTY_NOTICES.md.
package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

// WorkBuddy's chat gateway accepts a narrower JSON-Schema and function-name
// dialect than the OpenAI API. Keep the adaptation request-local: callers see
// their original names, while only the upstream wire uses safe aliases.
const workBuddyToolNameMaxBytes = 64

var workBuddyToolNameOK = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

var workBuddySchemaTypes = map[string]struct{}{
	"string": {}, "number": {}, "integer": {}, "boolean": {},
	"object": {}, "array": {}, "null": {},
}

var workBuddyBooleanSchemaAllowed = map[string]struct{}{
	"additionalProperties":  {},
	"unevaluatedProperties": {},
}

var workBuddySubSchemaOne = []string{
	"items", "additionalItems", "unevaluatedItems", "contains", "not",
	"if", "then", "else", "propertyNames", "additionalProperties",
	"unevaluatedProperties",
}

var workBuddySubSchemaList = []string{"anyOf", "oneOf", "allOf", "prefixItems"}
var workBuddySubSchemaMap = []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas"}

// toolNameMapping is deliberately carried alongside one execution rather than
// stored globally. Two callers may publish different tool sets with colliding
// names, and a process-global map could restore one caller's alias to another
// caller's private name.
type toolNameMapping struct {
	SafeToOriginal   map[string]string
	OriginalToSafe   map[string]string
	PassthroughNames map[string]struct{}
}

func newToolNameMapping() toolNameMapping {
	return toolNameMapping{
		SafeToOriginal:   make(map[string]string),
		OriginalToSafe:   make(map[string]string),
		PassthroughNames: make(map[string]struct{}),
	}
}

func (m toolNameMapping) empty() bool { return len(m.SafeToOriginal) == 0 }

func (m toolNameMapping) knownExact(candidate string) bool {
	if candidate == "" {
		return false
	}
	if _, exact := m.SafeToOriginal[candidate]; exact {
		return true
	}
	_, exact := m.PassthroughNames[candidate]
	return exact
}

func validWorkBuddyToolName(name string) bool {
	return len(name) > 0 && len(name) <= workBuddyToolNameMaxBytes && workBuddyToolNameOK.MatchString(name)
}

func safeWorkBuddyToolName(name string, taken map[string]struct{}) string {
	// Every renamed alias has the same fixed length. This makes the mapping set
	// prefix-free by construction, so an exact SSE fragment can be restored
	// immediately even when the same chunk also starts `arguments:""`.
	for salt := 0; ; salt++ {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d", name, salt)))
		candidate := fmt.Sprintf("tool_%x", sum[:16])
		if _, exists := taken[candidate]; exists {
			continue
		}
		taken[candidate] = struct{}{}
		return candidate
	}
}

// fitToolsInPlace normalizes tool names and parameter schemas for WorkBuddy.
// The returned map is used to restore upstream tool-call names before they are
// delivered to the caller.
func fitToolsInPlace(body map[string]any) toolNameMapping {
	mapping := newToolNameMapping()
	var definitions []map[string]any
	if tools, ok := body["tools"].([]any); ok {
		for _, raw := range tools {
			tool, _ := raw.(map[string]any)
			fn, _ := tool["function"].(map[string]any)
			if fn != nil {
				definitions = append(definitions, fn)
			}
		}
	}
	if functions, ok := body["functions"].([]any); ok {
		for _, raw := range functions {
			if fn, ok := raw.(map[string]any); ok && fn != nil {
				definitions = append(definitions, fn)
			}
		}
	}
	if len(definitions) == 0 {
		return mapping
	}

	type toolRecord struct {
		fn       map[string]any
		original string
		rename   bool
		safe     string
	}
	records := make([]*toolRecord, 0, len(definitions))
	for _, fn := range definitions {
		original, _ := fn["name"].(string)
		records = append(records, &toolRecord{fn: fn, original: original, rename: !validWorkBuddyToolName(original)})
	}

	// A renamed alias must also be prefix-free relative to preserved valid tool
	// names. Otherwise a valid tool named `tool` would be buffered as the prefix
	// of `tool_<hash>`, or a longer valid name could be mis-restored. Promote only
	// those conflicting valid names into the reversible mapping, then recompute
	// until the complete upstream name set is unambiguous.
	for {
		taken := make(map[string]struct{}, len(records))
		for _, record := range records {
			if !record.rename {
				taken[record.original] = struct{}{}
			}
		}
		for _, record := range records {
			if record.rename {
				record.safe = safeWorkBuddyToolName(record.original, taken)
			}
		}
		changed := false
		var renamedSnapshot, preservedSnapshot []*toolRecord
		for _, record := range records {
			if record.rename {
				renamedSnapshot = append(renamedSnapshot, record)
			} else {
				preservedSnapshot = append(preservedSnapshot, record)
			}
		}
		for _, renamed := range renamedSnapshot {
			for _, preserved := range preservedSnapshot {
				if strings.HasPrefix(renamed.safe, preserved.original) || strings.HasPrefix(preserved.original, renamed.safe) {
					preserved.rename = true
					changed = true
				}
			}
		}
		if len(renamedSnapshot) > 0 {
			for left := 0; left < len(preservedSnapshot); left++ {
				leftRecord := preservedSnapshot[left]
				if leftRecord.rename {
					continue
				}
				for right := left + 1; right < len(preservedSnapshot); right++ {
					rightRecord := preservedSnapshot[right]
					if rightRecord.rename || leftRecord.original == rightRecord.original {
						continue
					}
					switch {
					case strings.HasPrefix(rightRecord.original, leftRecord.original):
						rightRecord.rename = true
						changed = true
					case strings.HasPrefix(leftRecord.original, rightRecord.original):
						leftRecord.rename = true
						changed = true
					}
					if leftRecord.rename {
						break
					}
				}
			}
		}
		if !changed {
			break
		}
	}

	for _, record := range records {
		if record.rename {
			record.fn["name"] = record.safe
			mapping.SafeToOriginal[record.safe] = record.original
			mapping.OriginalToSafe[record.original] = record.safe
		} else {
			mapping.PassthroughNames[record.original] = struct{}{}
		}
		if parameters, present := record.fn["parameters"]; present {
			var schema map[string]any
			switch typed := parameters.(type) {
			case map[string]any:
				schema = typed
			case bool:
				if typed {
					schema = map[string]any{}
				} else {
					schema = map[string]any{"not": map[string]any{}}
				}
				record.fn["parameters"] = schema
			default:
				schema = map[string]any{"type": "object", "properties": map[string]any{}}
				record.fn["parameters"] = schema
			}
			fitWorkBuddySchema(schema, 0)
			fitWorkBuddyShape(schema, 0)
		}
	}

	if len(mapping.OriginalToSafe) == 0 {
		return mapping
	}
	rewriteToolChoice(body, "tool_choice", mapping.OriginalToSafe)
	rewriteToolChoice(body, "function_call", mapping.OriginalToSafe)
	for _, key := range []string{"messages", "input"} {
		if messages, ok := body[key].([]any); ok {
			for _, raw := range messages {
				rewriteMessageToolNames(raw, mapping.OriginalToSafe)
			}
		}
	}
	return mapping
}

func rewriteToolChoice(body map[string]any, key string, originalToSafe map[string]string) {
	value, present := body[key]
	if !present {
		return
	}
	if name, ok := value.(string); ok {
		if safe, exists := originalToSafe[name]; exists {
			body[key] = safe
		}
		return
	}
	choice, _ := value.(map[string]any)
	if choice == nil {
		return
	}
	if fn, ok := choice["function"].(map[string]any); ok {
		rewriteNamedFunction(fn, originalToSafe)
		return
	}
	rewriteNamedFunction(choice, originalToSafe)
}

func rewriteNamedFunction(fn map[string]any, originalToSafe map[string]string) {
	if fn == nil {
		return
	}
	rawName, present := fn["name"]
	if !present {
		return
	}
	name, ok := rawName.(string)
	if !ok {
		return
	}
	if safe, exists := originalToSafe[name]; exists {
		fn["name"] = safe
	}
}

func rewriteMessageToolNames(raw any, originalToSafe map[string]string) {
	message, _ := raw.(map[string]any)
	if message == nil {
		return
	}
	if calls, ok := message["tool_calls"].([]any); ok {
		for _, rawCall := range calls {
			call, _ := rawCall.(map[string]any)
			if call == nil {
				continue
			}
			fn, _ := call["function"].(map[string]any)
			rewriteNamedFunction(fn, originalToSafe)
		}
	}
	if legacy, ok := message["function_call"].(map[string]any); ok {
		rewriteNamedFunction(legacy, originalToSafe)
	}
	role, _ := message["role"].(string)
	if strings.EqualFold(role, "tool") || strings.EqualFold(role, "function") {
		rawName, present := message["name"]
		name, ok := rawName.(string)
		if !present || !ok {
			return
		}
		if safe, exists := originalToSafe[name]; exists {
			message["name"] = safe
		}
	}
}

// fitWorkBuddySchema performs only shape-agnostic repairs, but still traverses
// exclusively through declared JSON-Schema slots. In particular it must never
// enter annotations/data such as default, examples or const: those may contain
// ordinary objects with fields named `items` or `pattern`.
func fitWorkBuddySchema(value any, depth int) {
	if depth > 64 {
		return
	}
	schema, ok := value.(map[string]any)
	if !ok || schema == nil {
		return
	}
	fitWorkBuddyPattern(schema)
	fitWorkBuddyTupleItems(schema)
	for _, key := range workBuddySubSchemaOne {
		if child, present := schema[key]; present {
			fitWorkBuddySchema(child, depth+1)
		}
	}
	for _, key := range workBuddySubSchemaList {
		children, _ := schema[key].([]any)
		for _, child := range children {
			fitWorkBuddySchema(child, depth+1)
		}
	}
	for _, key := range workBuddySubSchemaMap {
		children, _ := schema[key].(map[string]any)
		for _, child := range children {
			fitWorkBuddySchema(child, depth+1)
		}
	}
}

// fitWorkBuddyShape applies gateway-specific JSON-Schema normalization only to
// known schema slots. Ordinary metadata booleans such as const/default/readOnly
// are never treated as boolean sub-schemas.
func fitWorkBuddyShape(value any, depth int) {
	if depth > 64 {
		return
	}
	schema, ok := value.(map[string]any)
	if !ok || schema == nil {
		return
	}
	fitWorkBuddyType(schema)
	fitWorkBuddyRequired(schema)
	fitWorkBuddyEnum(schema)

	for _, key := range workBuddySubSchemaOne {
		if child, present := schema[key]; present {
			schema[key] = fitWorkBuddyShapeSlot(child, key, depth)
		}
	}
	for _, key := range workBuddySubSchemaList {
		children, _ := schema[key].([]any)
		for index, child := range children {
			children[index] = fitWorkBuddyShapeSlot(child, key, depth)
		}
	}
	for _, key := range workBuddySubSchemaMap {
		children, _ := schema[key].(map[string]any)
		for name, child := range children {
			children[name] = fitWorkBuddyShapeSlot(child, key, depth)
		}
	}
}

func fitWorkBuddyShapeSlot(value any, key string, depth int) any {
	if flag, ok := value.(bool); ok {
		if _, allowed := workBuddyBooleanSchemaAllowed[key]; !allowed {
			if flag {
				value = map[string]any{}
			} else {
				value = map[string]any{"not": map[string]any{}}
			}
		}
	}
	fitWorkBuddyShape(value, depth+1)
	return value
}

func fitWorkBuddyPattern(schema map[string]any) {
	pattern, _ := schema["pattern"].(string)
	if !strings.Contains(pattern, `\0`) {
		return
	}
	var out strings.Builder
	out.Grow(len(pattern) + 4)
	changed := false
	for i := 0; i < len(pattern); {
		if pattern[i] == '\\' && i+1 < len(pattern) {
			next := pattern[i+1]
			if next == '\\' {
				out.WriteString(`\\`)
				i += 2
				continue
			}
			if next == '0' && (i+2 >= len(pattern) || pattern[i+2] < '0' || pattern[i+2] > '9') {
				out.WriteString(`\u0000`)
				i += 2
				changed = true
				continue
			}
		}
		out.WriteByte(pattern[i])
		i++
	}
	if changed {
		schema["pattern"] = out.String()
	}
}

func fitWorkBuddyTupleItems(schema map[string]any) {
	items, ok := schema["items"].([]any)
	if !ok {
		return
	}
	valid := make([]any, 0, len(items))
	for _, item := range items {
		switch typed := item.(type) {
		case map[string]any:
			valid = append(valid, typed)
		case bool:
			if typed {
				valid = append(valid, map[string]any{})
			} else {
				valid = append(valid, map[string]any{"not": map[string]any{}})
			}
		}
	}
	switch len(valid) {
	case 0:
		schema["items"] = map[string]any{}
	case 1:
		schema["items"] = valid[0]
	default:
		schema["items"] = map[string]any{"anyOf": valid}
	}
	delete(schema, "additionalItems")
}

func fitWorkBuddyType(schema map[string]any) {
	raw, present := schema["type"]
	if !present {
		return
	}
	var candidates []any
	if list, ok := raw.([]any); ok {
		candidates = list
	} else {
		candidates = []any{raw}
	}
	seen := make(map[string]struct{}, len(candidates))
	valid := make([]any, 0, len(candidates))
	for _, candidate := range candidates {
		name, ok := candidate.(string)
		if !ok {
			continue
		}
		name = strings.ToLower(strings.TrimSpace(name))
		if _, ok := workBuddySchemaTypes[name]; !ok {
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		valid = append(valid, name)
	}
	switch len(valid) {
	case 0:
		delete(schema, "type")
	case 1:
		schema["type"] = valid[0]
	default:
		schema["type"] = valid
	}
}

func fitWorkBuddyRequired(schema map[string]any) {
	raw, present := schema["required"]
	if !present {
		return
	}
	list, ok := raw.([]any)
	if !ok {
		delete(schema, "required")
		return
	}
	valid := make([]any, 0, len(list))
	for _, item := range list {
		if _, ok := item.(string); ok {
			valid = append(valid, item)
		}
	}
	schema["required"] = valid
}

func workBuddyListedNonNullTypeCount(schema map[string]any) int {
	raw, present := schema["type"]
	if !present {
		return 0
	}
	values, ok := raw.([]any)
	if !ok {
		values = []any{raw}
	}
	count := 0
	for _, value := range values {
		if name, ok := value.(string); ok && name != "null" {
			count++
		}
	}
	return count
}

func fitWorkBuddyEnum(schema map[string]any) {
	raw, present := schema["enum"]
	if !present {
		return
	}
	values, ok := raw.([]any)
	if !ok || len(values) == 0 {
		delete(schema, "enum")
		return
	}
	if workBuddyListedNonNullTypeCount(schema) == 1 {
		// An explicit single type is the caller's contract. Do not widen it merely
		// because a malformed/mixed enum contains a value of another type.
		return
	}
	groups := make(map[string][]any)
	for _, value := range values {
		kind := workBuddyJSONType(value)
		groups[kind] = append(groups[kind], value)
	}
	if integers := groups["integer"]; len(integers) > 0 && len(groups["number"]) > 0 {
		groups["number"] = append(groups["number"], integers...)
		delete(groups, "integer")
	}

	_, hasNull := groups["null"]
	delete(groups, "null")
	if len(groups) <= 1 {
		var kind string
		for key := range groups {
			kind = key
		}
		var want any = "null"
		if kind != "" {
			want = kind
			if hasNull {
				want = []any{kind, "null"}
			}
		}
		if !reflect.DeepEqual(schema["type"], want) {
			schema["type"] = want
		}
		return
	}

	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]any, 0, len(keys)+1)
	for _, key := range keys {
		parts = append(parts, map[string]any{"type": key, "enum": groups[key]})
	}
	if hasNull {
		parts = append(parts, map[string]any{"type": "null"})
	}
	delete(schema, "enum")
	delete(schema, "type")
	if existing, ok := schema["anyOf"].([]any); ok {
		allOf, _ := schema["allOf"].([]any)
		schema["allOf"] = append(allOf, map[string]any{"anyOf": parts})
		schema["anyOf"] = existing
	} else {
		schema["anyOf"] = parts
	}
}

func workBuddyJSONType(value any) string {
	switch typed := value.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64:
		if typed == float64(int64(typed)) {
			return "integer"
		}
		return "number"
	case json.Number:
		if _, err := typed.Int64(); err == nil {
			return "integer"
		}
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return "string"
	}
}

func restoreToolNamesInObject(value any, mapping toolNameMapping) bool {
	if mapping.empty() {
		return false
	}
	changed := false
	var walk func(any)
	walk = func(current any) {
		switch typed := current.(type) {
		case []any:
			for _, item := range typed {
				walk(item)
			}
		case map[string]any:
			for _, field := range []string{"function", "function_call"} {
				if fn, ok := typed[field].(map[string]any); ok {
					if name, ok := fn["name"].(string); ok {
						if original, exists := mapping.SafeToOriginal[name]; exists && original != name {
							fn["name"] = original
							changed = true
						}
					}
				}
			}
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(value)
	return changed
}

// streamToolNameRestorer buffers only name fragments that are prefixes of a
// known safe alias. When the complete alias arrives, one chunk emits the
// caller's original name. Ordinary names pass through unchanged.
type streamToolNameRestorer struct {
	mapping           toolNameMapping
	pending           map[string]string
	seen              map[string]bool
	completed         map[string]bool
	expectsToolCall   map[int]bool
	completedByChoice map[int]bool
	invalid           bool
}

func newStreamToolNameRestorer(mapping toolNameMapping) *streamToolNameRestorer {
	return &streamToolNameRestorer{
		mapping:           mapping,
		pending:           make(map[string]string),
		seen:              make(map[string]bool),
		completed:         make(map[string]bool),
		expectsToolCall:   make(map[int]bool),
		completedByChoice: make(map[int]bool),
	}
}

func (r *streamToolNameRestorer) restoreChunk(value map[string]any) bool {
	if r == nil || r.mapping.empty() {
		return false
	}
	changed := false
	choices, _ := value["choices"].([]any)
	for choicePos, rawChoice := range choices {
		choice, _ := rawChoice.(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		calls, _ := delta["tool_calls"].([]any)
		choiceIndex := numericIndex(choice["index"], choicePos)
		if finish, _ := choice["finish_reason"].(string); finish == "tool_calls" || finish == "function_call" {
			r.expectsToolCall[choiceIndex] = true
		}
		for callPos, rawCall := range calls {
			call, _ := rawCall.(map[string]any)
			fn, _ := call["function"].(map[string]any)
			callIndex := numericIndex(call["index"], callPos)
			key := fmt.Sprintf("%d:%d", choiceIndex, callIndex)
			r.seen[key] = true
			if r.restoreNameFragment(fn, key) {
				changed = true
			}
			if r.completed[key] {
				r.completedByChoice[choiceIndex] = true
			}
		}
		if legacy, ok := delta["function_call"].(map[string]any); ok {
			key := fmt.Sprintf("%d:legacy", choiceIndex)
			r.seen[key] = true
			if r.restoreNameFragment(legacy, key) {
				changed = true
			}
			if r.completed[key] {
				r.completedByChoice[choiceIndex] = true
			}
		}
	}
	return changed
}

func (r *streamToolNameRestorer) restoreNameFragment(fn map[string]any, key string) bool {
	if fn == nil {
		return false
	}
	rawName, present := fn["name"]
	fragment, ok := rawName.(string)
	if !present || !ok || fragment == "" {
		return false
	}
	if r.completed[key] {
		r.invalid = true
		fn["name"] = ""
		return true
	}
	candidate := r.pending[key] + fragment
	if original, exact := r.mapping.SafeToOriginal[candidate]; exact {
		fn["name"] = original
		delete(r.pending, key)
		r.completed[key] = true
		return original != fragment
	}
	if _, exact := r.mapping.PassthroughNames[candidate]; exact {
		fn["name"] = candidate
		changed := r.pending[key] != ""
		delete(r.pending, key)
		r.completed[key] = true
		return changed
	}
	if r.isKnownNamePrefix(candidate) {
		r.pending[key] = candidate
		fn["name"] = ""
		return true
	}
	if previous := r.pending[key]; previous != "" {
		fn["name"] = candidate
		delete(r.pending, key)
		r.invalid = true
		return true
	}
	r.invalid = true
	fn["name"] = ""
	return true
}

func (r *streamToolNameRestorer) hasPending() bool {
	if r == nil {
		return false
	}
	if r.invalid || len(r.pending) > 0 {
		return true
	}
	for key := range r.seen {
		if !r.completed[key] {
			return true
		}
	}
	for choice := range r.expectsToolCall {
		if !r.completedByChoice[choice] {
			return true
		}
	}
	return false
}

func (r *streamToolNameRestorer) isKnownNamePrefix(candidate string) bool {
	for safe := range r.mapping.SafeToOriginal {
		if strings.HasPrefix(safe, candidate) {
			return true
		}
	}
	for name := range r.mapping.PassthroughNames {
		if strings.HasPrefix(name, candidate) {
			return true
		}
	}
	return false
}

func numericIndex(value any, fallback int) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case json.Number:
		if parsed, err := typed.Int64(); err == nil {
			return int(parsed)
		}
	}
	return fallback
}
