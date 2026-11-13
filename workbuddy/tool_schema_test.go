package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestFitToolsInPlaceRepairsNamesSchemasAndReferences(t *testing.T) {
	var body map[string]any
	raw := []byte(`{
		"tool_choice":{"type":"function","function":{"name":"repo.read:file"}},
		"tools":[
			{"type":"function","function":{"name":"repo.read:file","parameters":{
				"type":["OBJECT","bogus","object"],
				"required":["path",9],
				"properties":{
					"path":{"type":"string","pattern":"^foo\\0bar$"},
					"choice":{"type":["string","number"],"enum":["x",2,null]},
					"tuple":{"type":"array","items":[{"type":"string"},{"type":"integer"}],"additionalItems":false},
					"boolTuple":{"type":"array","items":[false]},
					"gate":true,
					"type":{"type":"boolean","const":true,"default":false,"readOnly":true},
					"enum":{"type":"string","enum":["x","y"]},
					"typedMixed":{"type":"string","enum":["x",2]},
					"required":{"type":"array","items":{"type":"string"}}
				},
				"additionalProperties":false,
				"default":{"pattern":"^raw\\0$","items":[{"pattern":"^nested\\0$"}]},
				"examples":[{"pattern":"^example\\0$","items":[{"pattern":"^nested-example\\0$"}]}]
			}}},
			{"type":"function","function":{"name":"repo_read_file","parameters":null}}
		],
		"messages":[
			{"role":"assistant","tool_calls":[{"function":{"name":"repo.read:file","arguments":"{}"}}]},
			{"role":"tool","name":"repo.read:file","content":"ok"}
		]
	}`)
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	// normalizeToolsInPlace runs before fitToolsInPlace in the real pipeline.
	normalizeToolsInPlace(body)
	mapping := fitToolsInPlace(body)

	safe := mapping.OriginalToSafe["repo.read:file"]
	if !validWorkBuddyToolName(safe) || !strings.HasPrefix(safe, "tool_") {
		t.Fatalf("safe tool name = %q", safe)
	}
	if got := mapping.SafeToOriginal[safe]; got != "repo.read:file" {
		t.Fatalf("reverse tool name = %q", got)
	}
	if got := body["tool_choice"]; got != safe {
		t.Fatalf("tool_choice = %#v, want %q", got, safe)
	}

	tools := body["tools"].([]any)
	firstFn := tools[0].(map[string]any)["function"].(map[string]any)
	if firstFn["name"] != safe {
		t.Fatalf("upstream tool name = %#v", firstFn["name"])
	}
	schema := firstFn["parameters"].(map[string]any)
	if schema["type"] != "object" {
		t.Fatalf("root type = %#v", schema["type"])
	}
	if !reflect.DeepEqual(schema["required"], []any{"path"}) {
		t.Fatalf("required = %#v", schema["required"])
	}
	properties := schema["properties"].(map[string]any)
	if got := properties["path"].(map[string]any)["pattern"]; got != `^foo\u0000bar$` {
		t.Fatalf("pattern = %#v", got)
	}
	choice := properties["choice"].(map[string]any)
	if _, ok := choice["enum"]; ok {
		t.Fatalf("mixed enum must move into anyOf: %#v", choice)
	}
	if len(choice["anyOf"].([]any)) != 3 {
		t.Fatalf("mixed anyOf = %#v", choice["anyOf"])
	}
	tuple := properties["tuple"].(map[string]any)
	items := tuple["items"].(map[string]any)
	if len(items["anyOf"].([]any)) != 2 {
		t.Fatalf("tuple items = %#v", items)
	}
	if _, ok := tuple["additionalItems"]; ok {
		t.Fatal("additionalItems must be removed after tuple conversion")
	}
	if got := properties["boolTuple"].(map[string]any)["items"]; !reflect.DeepEqual(got, map[string]any{"not": map[string]any{}}) {
		t.Fatalf("false tuple subschema lost deny semantics: %#v", got)
	}
	if got := properties["gate"]; !reflect.DeepEqual(got, map[string]any{}) {
		t.Fatalf("boolean subschema = %#v", got)
	}
	namedType := properties["type"].(map[string]any)
	for key, want := range map[string]any{"type": "boolean", "const": true, "default": false, "readOnly": true} {
		if got := namedType[key]; !reflect.DeepEqual(got, want) {
			t.Fatalf("properties.type.%s = %#v, want %#v", key, got, want)
		}
	}
	if got := properties["enum"].(map[string]any)["enum"]; !reflect.DeepEqual(got, []any{"x", "y"}) {
		t.Fatalf("property named enum was corrupted: %#v", got)
	}
	typedMixed := properties["typedMixed"].(map[string]any)
	if typedMixed["type"] != "string" || !reflect.DeepEqual(typedMixed["enum"], []any{"x", float64(2)}) {
		t.Fatalf("explicit single-type enum was widened: %#v", typedMixed)
	}
	if _, ok := properties["required"].(map[string]any); !ok {
		t.Fatalf("property named required was corrupted: %#v", properties["required"])
	}
	if got := schema["additionalProperties"]; got != false {
		t.Fatalf("additionalProperties bool must stay legal: %#v", got)
	}
	wantDefault := map[string]any{"pattern": `^raw\0$`, "items": []any{map[string]any{"pattern": `^nested\0$`}}}
	if got := schema["default"]; !reflect.DeepEqual(got, wantDefault) {
		t.Fatalf("ordinary default data was rewritten as schema: %#v", got)
	}
	wantExamples := []any{map[string]any{"pattern": `^example\0$`, "items": []any{map[string]any{"pattern": `^nested-example\0$`}}}}
	if got := schema["examples"]; !reflect.DeepEqual(got, wantExamples) {
		t.Fatalf("ordinary examples data was rewritten as schema: %#v", got)
	}
	secondFn := tools[1].(map[string]any)["function"].(map[string]any)
	if !reflect.DeepEqual(secondFn["parameters"], map[string]any{"type": "object", "properties": map[string]any{}}) {
		t.Fatalf("invalid parameters = %#v", secondFn["parameters"])
	}

	messages := body["messages"].([]any)
	callName := messages[0].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["name"]
	if callName != safe {
		t.Fatalf("history tool call name = %#v", callName)
	}
	if got := messages[1].(map[string]any)["name"]; got != safe {
		t.Fatalf("tool result name = %#v", got)
	}
}

func TestFitToolsInPlacePreservesRootBooleanParameters(t *testing.T) {
	body := map[string]any{"tools": []any{
		map[string]any{"type": "function", "function": map[string]any{"name": "allow", "parameters": true}},
		map[string]any{"type": "function", "function": map[string]any{"name": "deny", "parameters": false}},
	}}
	fitToolsInPlace(body)
	tools := body["tools"].([]any)
	allow := tools[0].(map[string]any)["function"].(map[string]any)["parameters"]
	deny := tools[1].(map[string]any)["function"].(map[string]any)["parameters"]
	if !reflect.DeepEqual(allow, map[string]any{}) {
		t.Fatalf("true parameters lost allow semantics: %#v", allow)
	}
	if !reflect.DeepEqual(deny, map[string]any{"not": map[string]any{}}) {
		t.Fatalf("false parameters lost deny semantics: %#v", deny)
	}
}

func TestFitToolsInPlaceRepairsEmptyAndOverlongNames(t *testing.T) {
	long := strings.Repeat("a", workBuddyToolNameMaxBytes+1)
	body := map[string]any{"tools": []any{
		map[string]any{"type": "function", "function": map[string]any{"name": ""}},
		map[string]any{"type": "function", "function": map[string]any{"name": long}},
	}}
	mapping := fitToolsInPlace(body)
	tools := body["tools"].([]any)
	for index, raw := range tools {
		name := raw.(map[string]any)["function"].(map[string]any)["name"].(string)
		if !validWorkBuddyToolName(name) {
			t.Fatalf("tool %d safe name = %q", index, name)
		}
	}
	emptySafe := mapping.OriginalToSafe[""]
	obj := map[string]any{"function": map[string]any{"name": emptySafe}}
	if !restoreToolNamesInObject(obj, mapping) || obj["function"].(map[string]any)["name"] != "" {
		t.Fatalf("empty original name was not reversible: %#v", obj)
	}
}

func TestFitToolsInPlaceRewritesLegacyFunctionRequest(t *testing.T) {
	body := map[string]any{
		"functions":     []any{map[string]any{"name": "bad.name", "parameters": map[string]any{"type": "object"}}},
		"function_call": map[string]any{"name": "bad.name"},
		"messages": []any{
			map[string]any{"role": "assistant", "function_call": map[string]any{"name": "bad.name", "arguments": "{}"}},
			map[string]any{"role": "function", "name": "bad.name", "content": "ok"},
		},
	}
	mapping := fitToolsInPlace(body)
	safe := mapping.OriginalToSafe["bad.name"]
	if safe == "" {
		t.Fatal("legacy function name was not mapped")
	}
	fn := body["functions"].([]any)[0].(map[string]any)
	if fn["name"] != safe || body["function_call"].(map[string]any)["name"] != safe {
		t.Fatalf("legacy definition/choice not rewritten: %#v", body)
	}
	messages := body["messages"].([]any)
	if messages[0].(map[string]any)["function_call"].(map[string]any)["name"] != safe || messages[1].(map[string]any)["name"] != safe {
		t.Fatalf("legacy history not rewritten: %#v", messages)
	}
}

func TestFitToolsInPlaceDoesNotInventMissingHistoryNames(t *testing.T) {
	body := map[string]any{
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": ""}}},
		"messages": []any{
			map[string]any{"role": "tool", "tool_call_id": "call-1", "content": "ok"},
			map[string]any{"role": "assistant", "tool_calls": []any{
				map[string]any{"id": "call-2", "type": "function"},
				map[string]any{"id": "call-3", "type": "function", "function": map[string]any{"arguments": "{}"}},
			}},
		},
	}
	fitToolsInPlace(body)
	messages := body["messages"].([]any)
	if _, exists := messages[0].(map[string]any)["name"]; exists {
		t.Fatalf("missing tool result name was invented: %#v", messages[0])
	}
	calls := messages[1].(map[string]any)["tool_calls"].([]any)
	if _, exists := calls[0].(map[string]any)["function"]; exists {
		t.Fatalf("missing function object was invented: %#v", calls[0])
	}
	if _, exists := calls[1].(map[string]any)["function"].(map[string]any)["name"]; exists {
		t.Fatalf("missing function name was invented: %#v", calls[1])
	}
}

func TestRestoreToolNamesInObject(t *testing.T) {
	mapping := toolNameMapping{
		SafeToOriginal: map[string]string{"repo_read_file": "repo.read:file", "plain": "plain"},
		OriginalToSafe: map[string]string{"repo.read:file": "repo_read_file", "plain": "plain"},
	}
	obj := map[string]any{"choices": []any{map[string]any{"message": map[string]any{
		"tool_calls": []any{map[string]any{"function": map[string]any{"name": "repo_read_file"}}},
	}}}}
	if !restoreToolNamesInObject(obj, mapping) {
		t.Fatal("restore reported no change")
	}
	name := obj["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["name"]
	if name != "repo.read:file" {
		t.Fatalf("restored name = %#v", name)
	}
}

func TestStreamToolNameRestorerBuffersSplitAlias(t *testing.T) {
	mapping := toolNameMapping{
		SafeToOriginal: map[string]string{"repo_read_file": "repo.read:file"},
		OriginalToSafe: map[string]string{"repo.read:file": "repo_read_file"},
	}
	r := newStreamToolNameRestorer(mapping)
	first := toolNameDelta("repo_read_", 0)
	if !r.restoreChunk(first) {
		t.Fatal("first fragment should be buffered")
	}
	if got := deltaToolName(first); got != "" {
		t.Fatalf("first emitted fragment = %q", got)
	}
	second := toolNameDelta("file", 0)
	if !r.restoreChunk(second) {
		t.Fatal("second fragment should be restored")
	}
	if got := deltaToolName(second); got != "repo.read:file" {
		t.Fatalf("restored stream name = %q", got)
	}
}

func TestFitToolsInPlaceRenamesValidPrefixConflicts(t *testing.T) {
	body := map[string]any{
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{"name": "bad.name"}},
			map[string]any{"type": "function", "function": map[string]any{"name": "tool"}},
			map[string]any{"type": "function", "function": map[string]any{"name": "alpha"}},
		},
		"tool_choice": "tool",
		"messages": []any{map[string]any{"role": "assistant", "tool_calls": []any{
			map[string]any{"function": map[string]any{"name": "tool"}},
		}}},
	}
	mapping := fitToolsInPlace(body)
	badSafe := mapping.OriginalToSafe["bad.name"]
	validSafe := mapping.OriginalToSafe["tool"]
	if badSafe == "" || validSafe == "" {
		t.Fatalf("prefix conflict was not mapped: %#v", mapping)
	}
	if mapping.OriginalToSafe["alpha"] != "" {
		t.Fatalf("unrelated valid tool was unnecessarily renamed: %#v", mapping)
	}
	if _, preserved := mapping.PassthroughNames["alpha"]; !preserved {
		t.Fatalf("unrelated valid tool not preserved: %#v", mapping.PassthroughNames)
	}
	if strings.HasPrefix(badSafe, validSafe) || strings.HasPrefix(validSafe, badSafe) {
		t.Fatalf("upstream aliases remain prefix-related: %q / %q", badSafe, validSafe)
	}
	if body["tool_choice"] != validSafe {
		t.Fatalf("valid conflicting tool_choice not rewritten: %#v", body["tool_choice"])
	}
	callName := body["messages"].([]any)[0].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["name"]
	if callName != validSafe {
		t.Fatalf("valid conflicting history not rewritten: %#v", callName)
	}
}

func TestFitToolsInPlaceRenamesPreservedPrefixPairsWhenMappingActive(t *testing.T) {
	body := map[string]any{"tools": []any{
		map[string]any{"type": "function", "function": map[string]any{"name": "bad.name"}},
		map[string]any{"type": "function", "function": map[string]any{"name": "foo"}},
		map[string]any{"type": "function", "function": map[string]any{"name": "foobar"}},
	}}
	mapping := fitToolsInPlace(body)
	if mapping.OriginalToSafe["bad.name"] == "" || mapping.OriginalToSafe["foobar"] == "" {
		t.Fatalf("prefix pair was not disambiguated: %#v", mapping)
	}
	if _, preserved := mapping.PassthroughNames["foo"]; !preserved {
		t.Fatalf("short exact name should remain passthrough: %#v", mapping.PassthroughNames)
	}
	tools := body["tools"].([]any)
	names := make([]string, 0, len(tools))
	for _, raw := range tools {
		names = append(names, raw.(map[string]any)["function"].(map[string]any)["name"].(string))
	}
	for left := range names {
		for right := left + 1; right < len(names); right++ {
			if strings.HasPrefix(names[left], names[right]) || strings.HasPrefix(names[right], names[left]) {
				t.Fatalf("upstream names remain prefix-related: %#v", names)
			}
		}
	}
}

func TestSafeWorkBuddyToolNamesArePrefixFree(t *testing.T) {
	taken := make(map[string]struct{})
	first := safeWorkBuddyToolName("a.b", taken)
	second := safeWorkBuddyToolName("a.b.c", taken)
	if !strings.HasPrefix(first, "tool_") || !strings.HasPrefix(second, "tool_") || len(first) != len(second) {
		t.Fatalf("aliases are not fixed-length digests: %q / %q", first, second)
	}
	if strings.HasPrefix(first, second) || strings.HasPrefix(second, first) {
		t.Fatalf("aliases are prefix-related: %q / %q", first, second)
	}
	again := safeWorkBuddyToolName("a.b", make(map[string]struct{}))
	if again != first {
		t.Fatalf("alias is not deterministic: %q != %q", again, first)
	}
}

func TestAggregateSSEObservedRestoresSplitToolName(t *testing.T) {
	mapping := toolNameMapping{
		SafeToOriginal: map[string]string{"repo_read_file": "repo.read:file"},
		OriginalToSafe: map[string]string{"repo.read:file": "repo_read_file"},
	}
	wire := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"repo_read_","arguments":""}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"file","arguments":"{}"}}]}}]}`,
		`data: [DONE]`,
	}, "\n")
	chunks, err := aggregateSSEObservedWithTools(strings.NewReader(wire), false, nil, nil, mapping)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 {
		t.Fatalf("chunks = %d, want 2", len(chunks))
	}
	if strings.Contains(string(chunks[0].Payload), "repo_read_") {
		t.Fatalf("safe-name prefix leaked in first chunk: %s", chunks[0].Payload)
	}
	if !strings.Contains(string(chunks[1].Payload), `"name":"repo.read:file"`) {
		t.Fatalf("original name not restored in second chunk: %s", chunks[1].Payload)
	}
}

func TestAggregateSSERestoresSplitPassthroughName(t *testing.T) {
	mapping := toolNameMapping{
		SafeToOriginal:   map[string]string{"tool_deadbeef": "bad.name"},
		OriginalToSafe:   map[string]string{"bad.name": "tool_deadbeef"},
		PassthroughNames: map[string]struct{}{"tree": {}},
	}
	wire := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"t"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"ree","arguments":"{}"}}]}}]}`,
		`data: [DONE]`,
	}, "\n")
	chunks, err := aggregateSSEObservedWithTools(strings.NewReader(wire), false, nil, nil, mapping)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 || !strings.Contains(string(chunks[1].Payload), `"name":"tree"`) {
		t.Fatalf("split passthrough name not emitted whole: %#v", chunks)
	}
}

func TestAggregateSSEAllowsNameAfterIDOnlyDelta(t *testing.T) {
	mapping := toolNameMapping{
		SafeToOriginal: map[string]string{"repo_read_file": "repo.read:file"},
		OriginalToSafe: map[string]string{"repo.read:file": "repo_read_file"},
	}
	wire := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function"}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"repo_read_file","arguments":"{}"}}]}}]}`,
		`data: [DONE]`,
	}, "\n")
	chunks, err := aggregateSSEObservedWithTools(strings.NewReader(wire), false, nil, nil, mapping)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 || !strings.Contains(string(chunks[1].Payload), `"name":"repo.read:file"`) {
		t.Fatalf("late name was not restored: %#v", chunks)
	}
}

func TestAggregateSSERejectsDivergedOrMissingToolAlias(t *testing.T) {
	mapping := toolNameMapping{
		SafeToOriginal: map[string]string{"repo_read_file": "repo.read:file"},
		OriginalToSafe: map[string]string{"repo.read:file": "repo_read_file"},
	}
	cases := map[string]string{
		"diverged": strings.Join([]string{
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"repo_"}}]}}]}`,
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"wrong","arguments":"{}"}}]}}]}`,
			`data: [DONE]`,
		}, "\n"),
		"missing": strings.Join([]string{
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"arguments":"{}"}}]}}]}`,
			`data: [DONE]`,
		}, "\n"),
		"unknown-first-fragment": strings.Join([]string{
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"alien","arguments":"{}"}}]}}]}`,
			`data: [DONE]`,
		}, "\n"),
		"finish-without-call": strings.Join([]string{
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
		}, "\n"),
	}
	for name, wire := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := aggregateSSEObservedWithTools(strings.NewReader(wire), false, nil, nil, mapping); err == nil || !strings.Contains(err.Error(), "incomplete tool name") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestAggregateSSERejectsPendingToolAliasAtDone(t *testing.T) {
	mapping := toolNameMapping{
		SafeToOriginal: map[string]string{"repo_read_file": "repo.read:file"},
		OriginalToSafe: map[string]string{"repo.read:file": "repo_read_file"},
	}
	wire := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"repo_","arguments":""}}]}}]}`,
		`data: [DONE]`,
	}, "\n")
	if _, err := aggregateSSEObservedWithTools(strings.NewReader(wire), false, nil, nil, mapping); err == nil || !strings.Contains(err.Error(), "incomplete tool name") {
		t.Fatalf("pending alias error = %v", err)
	}
}

func TestAggregateCompletionRejectsIncompleteOrMissingToolName(t *testing.T) {
	mapping := toolNameMapping{
		SafeToOriginal: map[string]string{"repo_read_file": "repo.read:file"},
		OriginalToSafe: map[string]string{"repo.read:file": "repo_read_file"},
	}
	cases := map[string]string{
		"prefix": `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"repo_","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}
` + "data: [DONE]\n",
		"missing": `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]},"finish_reason":"tool_calls"}]}
` + "data: [DONE]\n",
		"finish-without-call": `data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}
` + "data: [DONE]\n",
	}
	for name, wire := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := aggregateCompletionObservedWithTools(strings.NewReader(wire), "model", nil, mapping); err == nil || !strings.Contains(err.Error(), "incomplete tool name") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestAggregateCompletionRestoresLegacyFunctionCallName(t *testing.T) {
	mapping := toolNameMapping{
		SafeToOriginal: map[string]string{"repo_read_file": "repo.read:file"},
		OriginalToSafe: map[string]string{"repo.read:file": "repo_read_file"},
	}
	wire := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","function_call":{"name":"repo_read_","arguments":""}}}]}`,
		`data: {"choices":[{"index":0,"delta":{"function_call":{"name":"file","arguments":"{}"}},"finish_reason":"function_call"}]}`,
		`data: [DONE]`,
	}, "\n")
	out, err := aggregateCompletionObservedWithTools(strings.NewReader(wire), "model", nil, mapping)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"function_call":{"arguments":"{}","name":"repo.read:file"}`) {
		t.Fatalf("legacy original name not restored: %s", out)
	}
}

func TestAggregateCompletionRestoresToolName(t *testing.T) {
	mapping := toolNameMapping{
		SafeToOriginal: map[string]string{"repo_read_file": "repo.read:file"},
		OriginalToSafe: map[string]string{"repo.read:file": "repo_read_file"},
	}
	wire := strings.Join([]string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"repo_read_","arguments":""}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"file","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n")
	out, err := aggregateCompletionObservedWithTools(strings.NewReader(wire), "model", nil, mapping)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"name":"repo.read:file"`) {
		t.Fatalf("original name not restored in completion: %s", out)
	}
	if strings.Contains(string(out), "repo_read_file") {
		t.Fatalf("safe alias leaked in completion: %s", out)
	}
}

func toolNameDelta(name string, index int) map[string]any {
	return map[string]any{"choices": []any{map[string]any{
		"index": float64(0),
		"delta": map[string]any{"tool_calls": []any{map[string]any{
			"index":    float64(index),
			"function": map[string]any{"name": name},
		}}},
	}}}
}

func deltaToolName(obj map[string]any) string {
	return obj["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["name"].(string)
}
