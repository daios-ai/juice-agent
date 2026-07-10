package main

import (
	"strings"
	"testing"
)

func TestReplyArtifactEmbedded(t *testing.T) {
	// The prebuilt echo artifact must be embedded and look like base64 WASM.
	if len(strings.TrimSpace(replyArtifact)) < 1000 {
		t.Fatalf("embedded reply artifact too small: %d bytes", len(replyArtifact))
	}
	if !strings.HasPrefix(strings.TrimSpace(replyArtifact), "AGFzbQ") { // base64 of "\0asm"
		t.Fatalf("artifact does not start with a WASM magic base64 prefix")
	}
}

func TestReplySchemaRequiresAnswer(t *testing.T) {
	in, out := replySchema()
	req, _ := in["required"].([]any)
	if len(req) != 1 || req[0] != "answer" {
		t.Fatalf("reply input schema required = %v, want [answer]", req)
	}
	if out["type"] != "object" {
		t.Fatalf("reply output schema type = %v", out["type"])
	}
}

func TestPublishSchemaShape(t *testing.T) {
	in, _ := publishSchema()
	props, _ := in["properties"].(map[string]any)
	for _, k := range []string{"name", "artifact", "input_schema", "output_schema", "test_inputs"} {
		if _, ok := props[k]; !ok {
			t.Fatalf("publish schema missing property %q", k)
		}
	}
	req, _ := in["required"].([]any)
	hasName, hasArtifact := false, false
	for _, r := range req {
		if r == "name" {
			hasName = true
		}
		if r == "artifact" {
			hasArtifact = true
		}
	}
	if !hasName || !hasArtifact {
		t.Fatalf("publish required = %v, want name+artifact", req)
	}
}
