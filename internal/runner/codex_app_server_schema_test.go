package runner

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// These tests validate the EXACT request payloads the runner sends to
// `codex app-server` against the authoritative protocol schema vendored from
// `codex app-server generate-json-schema --out <dir> --experimental`
// (codexProtocolSchemaFile). They replace a hand-written 6-line assertion
// snapshot whose checks could not catch new required fields, renamed fields,
// enum changes, or structural drift (the #446 placebo). The schema is generated
// WITH --experimental because the runner enables the experimental API and sends
// experimental fields (thread/start dynamicTools); see codex_version.go.

// codexSchemaCompiler compiles the vendored bundle once and binds it under the
// "codex.json" base URI so definitions resolve via JSON-Pointer fragments.
func codexSchemaCompiler(t *testing.T) *jsonschema.Compiler {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", codexProtocolSchemaFile))
	if err != nil {
		t.Fatalf("read vendored codex schema %q: %v", codexProtocolSchemaFile, err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parse vendored codex schema: %v", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("codex.json", doc); err != nil {
		t.Fatalf("add codex schema resource: %v", err)
	}
	return c
}

func compileCodexDef(t *testing.T, c *jsonschema.Compiler, def string) *jsonschema.Schema {
	t.Helper()
	sch, err := c.Compile("codex.json#/definitions/" + def)
	if err != nil {
		t.Fatalf("compile #/definitions/%s: %v", def, err)
	}
	return sch
}

// validateWire marshals payload exactly as appServerClient.send would, parses
// the bytes back through the validator's value model, and validates them
// against def. It returns the validation error so positive and negative tests
// share one path.
func validateWire(t *testing.T, sch *jsonschema.Schema, payload any) error {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parse marshaled payload: %v", err)
	}
	return sch.Validate(inst)
}

// codexSchemaTestInput mirrors appServerInput but configures Linear auth so
// appServerDynamicToolSpecs emits a real linear_graphql DynamicToolSpec, letting
// the thread/start test exercise the DynamicToolSpec definition (not just an
// empty dynamicTools array).
func codexSchemaTestInput(t *testing.T) RunInput {
	t.Helper()
	in := appServerInput(filepath.Join(t.TempDir(), "clone"))
	in.Workflow.Config.Tracker.Kind = "linear"
	in.Workflow.Config.Tracker.APIKey = "linear-secret"
	return in
}

func TestCodexAppServerInitializePayloadMatchesSchema(t *testing.T) {
	sch := compileCodexDef(t, codexSchemaCompiler(t), "InitializeParams")
	if err := validateWire(t, sch, buildInitializeParams()); err != nil {
		t.Errorf("buildInitializeParams() failed InitializeParams schema: %v", err)
	}
}

func TestCodexAppServerThreadStartPayloadMatchesSchema(t *testing.T) {
	in := codexSchemaTestInput(t)
	payload := buildThreadStartParams(in, in.Workflow.Config.Codex.ApprovalPolicy)

	// Guard: the input must actually carry a dynamic tool, else this would
	// validate the optional-absent path and never exercise DynamicToolSpec.
	if tools, _ := payload["dynamicTools"].([]map[string]any); len(tools) == 0 {
		t.Fatalf("buildThreadStartParams dynamicTools = %#v; want at least one tool so the DynamicToolSpec definition is exercised", payload["dynamicTools"])
	}
	sch := compileCodexDef(t, codexSchemaCompiler(t), "ThreadStartParams")
	if err := validateWire(t, sch, payload); err != nil {
		t.Errorf("buildThreadStartParams() failed ThreadStartParams schema: %v", err)
	}
}

func TestCodexAppServerTurnStartPayloadMatchesSchema(t *testing.T) {
	in := codexSchemaTestInput(t)
	approval := in.Workflow.Config.Codex.ApprovalPolicy
	payload := buildTurnStartParams(in, "thread-1", "do the task", 1, approval)
	sch := compileCodexDef(t, codexSchemaCompiler(t), "TurnStartParams")
	if err := validateWire(t, sch, payload); err != nil {
		t.Errorf("buildTurnStartParams() failed TurnStartParams schema: %v", err)
	}
}

// TestCodexAppServerTurnStartPayloadRejectsDrift proves the contract test is not
// a placebo: each mutation makes a real, schema-detectable break, and the
// validator must reject it. The image-without-url case deliberately exercises
// the UserInput oneOf discriminator, not just a top-level required key.
func TestCodexAppServerTurnStartPayloadRejectsDrift(t *testing.T) {
	in := codexSchemaTestInput(t)
	approval := in.Workflow.Config.Codex.ApprovalPolicy
	base := buildTurnStartParams(in, "thread-1", "do the task", 1, approval)
	sch := compileCodexDef(t, codexSchemaCompiler(t), "TurnStartParams")

	cases := []struct {
		name   string
		mutate func(m map[string]any)
	}{
		{"missing required text in text input", func(m map[string]any) {
			input := m["input"].([]any)[0].(map[string]any)
			delete(input, "text")
		}},
		{"unknown UserInput variant via image without url", func(m map[string]any) {
			input := m["input"].([]any)[0].(map[string]any)
			input["type"] = "image"
			delete(input, "text")
			delete(input, "text_elements")
		}},
		{"bogus sandboxPolicy type enum", func(m map[string]any) {
			m["sandboxPolicy"].(map[string]any)["type"] = "bogusSandbox"
		}},
		{"missing required threadId", func(m map[string]any) {
			delete(m, "threadId")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(base)
			if err != nil {
				t.Fatalf("marshal base payload: %v", err)
			}
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatalf("decode base payload: %v", err)
			}
			tc.mutate(m)
			if err := validateWire(t, sch, m); err == nil {
				t.Errorf("TurnStartParams accepted drifted payload (%s); want a validation error", tc.name)
			}
		})
	}
}

// TestCodexAppServerPayloadKeysKnownToSchema closes the gap the JSON-Schema
// validation alone cannot: the schemars-generated object schemas omit
// additionalProperties (default true), so a field we send that the schema no
// longer defines (e.g. the removed turn/start `title`) would still validate.
// This asserts every top-level key our builders emit is a known property of the
// target definition, catching stale/extra fields in pure Go at author time
// without needing the codex binary.
func TestCodexAppServerPayloadKeysKnownToSchema(t *testing.T) {
	in := codexSchemaTestInput(t)
	approval := in.Workflow.Config.Codex.ApprovalPolicy
	props := codexSchemaDefProps(t)

	cases := []struct {
		def     string
		payload any
	}{
		{"TurnStartParams", buildTurnStartParams(in, "thread-1", "do the task", 1, approval)},
		{"ThreadStartParams", buildThreadStartParams(in, approval)},
		{"InitializeParams", buildInitializeParams()},
	}
	for _, tc := range cases {
		known := props[tc.def]
		if len(known) == 0 {
			t.Fatalf("schema definition %q has no properties; vendored bundle malformed", tc.def)
		}
		raw, err := json.Marshal(tc.payload)
		if err != nil {
			t.Fatalf("marshal %s payload: %v", tc.def, err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("decode %s payload: %v", tc.def, err)
		}
		for key := range m {
			if !known[key] {
				t.Errorf("%s payload sends key %q absent from schema properties %v (stale/dead field — remove it or it is silently dropped by codex)", tc.def, key, sortedKeys(known))
			}
		}
	}
}

// codexSchemaDefProps returns, per request-param definition name, the set of
// property names declared in the vendored bundle.
func codexSchemaDefProps(t *testing.T) map[string]map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", codexProtocolSchemaFile))
	if err != nil {
		t.Fatalf("read vendored codex schema: %v", err)
	}
	var bundle struct {
		Definitions map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"definitions"`
	}
	if err := json.Unmarshal(raw, &bundle); err != nil {
		t.Fatalf("decode vendored codex schema: %v", err)
	}
	out := map[string]map[string]bool{}
	for name, def := range bundle.Definitions {
		set := map[string]bool{}
		for prop := range def.Properties {
			set[prop] = true
		}
		out[name] = set
	}
	return out
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
