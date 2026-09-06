package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestPatch_DropsCaseDuplicateVersion pins the one correction the client
// needs: a schema declaring both `Version` and `version` keeps the lowercase
// one. The counterfactual is a document that never had the duplicate, which
// must pass through with no report and no change.
func TestPatch_DropsCaseDuplicateVersion(t *testing.T) {
	t.Parallel()
	in := []byte(`{"openapi":"3.0.1","components":{"schemas":{
		"AppSecretsUpdateResp":{"properties":{"Version":{"type":"integer"},"version":{"type":"integer"},"secrets":{"type":"array"}}},
		"Clean":{"properties":{"name":{"type":"string"}}}}}}`)
	out, report, err := patch(in)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]any `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	got := doc.Components.Schemas["AppSecretsUpdateResp"].Properties
	if _, ok := got["Version"]; ok {
		t.Error("Version survived; oapi-codegen would emit two Version fields")
	}
	if _, ok := got["version"]; !ok {
		t.Error("version was dropped; the wire field is the lowercase one")
	}
	if len(report) != 1 || !strings.Contains(report[0], `dropped property "Version"`) {
		t.Errorf("report = %q, want one line naming the dropped property", report)
	}
	if _, ok := doc.Components.Schemas["Clean"].Properties["name"]; !ok {
		t.Error("a schema with no duplicate lost a property")
	}
}

// TestPatch_IsIdempotent: running the tool over its own output changes
// nothing, which is what lets the scheduled refresh tell "upstream moved"
// from "the encoder reordered keys".
func TestPatch_IsIdempotent(t *testing.T) {
	t.Parallel()
	in := []byte(`{"openapi":"3.0.1","info":{"version":"1.0","x":1.0},"components":{"schemas":{"A":{"properties":{"Version":{},"version":{}}}}}}`)
	once, _, err := patch(in)
	if err != nil {
		t.Fatal(err)
	}
	twice, report, err := patch(once)
	if err != nil {
		t.Fatal(err)
	}
	if string(once) != string(twice) {
		t.Errorf("second pass changed the document:\n%s\n---\n%s", once, twice)
	}
	if len(report) != 0 {
		t.Errorf("second pass reported %q; nothing was left to patch", report)
	}
	if !strings.Contains(string(once), `"x": 1.0`) {
		t.Errorf("number was rewritten on re-encode:\n%s", once)
	}
}

// TestPatch_RejectsSwagger2: the old document location served Swagger 2.0,
// and oapi-codegen cannot consume that; refusing is better than writing a
// file that fails at generate time with a less specific error.
func TestPatch_RejectsSwagger2(t *testing.T) {
	t.Parallel()
	_, _, err := patch([]byte(`{"swagger":"2.0","paths":{}}`))
	if err == nil {
		t.Fatal("a Swagger 2.0 document was accepted")
	}
}
