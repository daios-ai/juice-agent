package juice

import (
	"encoding/json"
	"strings"
	"testing"
)

// The schemas the kernel returns for its own natives (GET /v1/actions?ref=, at
// kernel dff95e6), verbatim apart from shortened descriptions.
const (
	kernelChatInput    = `{"additionalProperties":false,"properties":{"messages":{"description":"history","items":{"additionalProperties":false,"properties":{"content":{"description":"text","type":"string"},"role":{"description":"role","type":"string"}},"required":["role","content"],"type":"object"},"type":"array"}},"required":["messages"],"type":"object"}`
	kernelChatOutput   = `{"additionalProperties":false,"properties":{"choices":{"description":"reply","items":{"additionalProperties":false,"properties":{"index":{"description":"position","type":"integer"},"message":{"additionalProperties":false,"description":"the reply","properties":{"content":{"description":"text","type":"string"},"role":{"description":"role","type":"string"}},"type":"object"}},"type":"object"},"type":"array"}},"type":"object"}`
	kernelLookupInput  = `{"additionalProperties":false,"properties":{"limit":{"description":"max","type":"integer"},"query":{"description":"query","type":"string"}},"required":["query"],"type":"object"}`
	kernelLookupOutput = `{"additionalProperties":false,"properties":{"results":{"description":"ranked","items":{"additionalProperties":false,"properties":{"action":{"description":"address","type":"string"},"action_id":{"description":"id","type":"string"},"description":{"description":"what","type":"string"},"input_schema":{"description":"input","type":"object"},"price":{"description":"price","type":"integer"},"score":{"description":"score","type":"number"}},"type":"object"},"type":"array"}},"type":"object"}`
	kernelTimeOutput   = `{"additionalProperties":false,"properties":{"iso":{"description":"RFC 3339","type":"string"},"unix":{"description":"epoch","type":"integer"}},"type":"object"}`
)

func schema(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestKernelNativesMeetTheirContracts(t *testing.T) {
	if err := reasonContract.check(schema(t, kernelChatInput), schema(t, kernelChatOutput)); err != nil {
		t.Fatalf("llm/chat: %v", err)
	}
	if err := actionLookupContract.check(schema(t, kernelLookupInput), schema(t, kernelLookupOutput)); err != nil {
		t.Fatalf("lookup: %v", err)
	}
}

// Each case changes one thing in the kernel's own schemas and names where the
// check must report it, or "" when the change is still compatible.
func TestContractCheckNamesTheShortfall(t *testing.T) {
	r := strings.NewReplacer
	for _, tc := range []struct {
		name          string
		k             contract
		input, output string
		want          string
	}{
		{"wrong action entirely", reasonContract, `{"type":"object"}`, kernelTimeOutput,
			"output.properties.choices: missing (want array)"},
		{"read field missing", actionLookupContract, kernelLookupInput,
			r(`"action":{"description":"address","type":"string"},`, ``).Replace(kernelLookupOutput),
			"output.properties.results.items.properties.action: missing (want string)"},
		{"read field mistyped", actionLookupContract, kernelLookupInput,
			r(`"input_schema":{"description":"input","type":"object"}`, `"input_schema":{"description":"input","type":"string"}`).Replace(kernelLookupOutput),
			"output.properties.results.items.properties.input_schema: type string (want object)"},
		{"nullable read field refused", reasonContract, kernelChatInput,
			r(`"content":{"description":"text","type":"string"},"role"`, `"content":{"description":"text","type":["string","null"]},"role"`).Replace(kernelChatOutput),
			"output.properties.choices.items.properties.message.properties.content: type [string null] (want string)"},
		{"nullable sent field accepted", actionLookupContract,
			r(`"type":"integer"`, `"type":["integer","null"]`).Replace(kernelLookupInput), kernelLookupOutput, ""},
		{"untyped sent field accepted", actionLookupContract,
			r(`"query":{"description":"query","type":"string"}`, `"query":{"description":"query"}`).Replace(kernelLookupInput), kernelLookupOutput, ""},
		{"sent field undeclared", actionLookupContract,
			r(`"limit":{"description":"max","type":"integer"},`, ``).Replace(kernelLookupInput), kernelLookupOutput,
			"input.properties.limit: missing (want integer)"},
		{"required beyond what is sent", actionLookupContract,
			r(`"required":["query"]`, `"required":["query","scope"]`).Replace(kernelLookupInput), kernelLookupOutput,
			"input: requires scope, which the agent does not send"},
		{"role enum without system", reasonContract,
			r(`"role":{"description":"role","type":"string"}`, `"role":{"description":"role","type":"string","enum":["user","assistant"]}`).Replace(kernelChatInput),
			kernelChatOutput, "input.properties.messages.items.properties.role: enum lacks system"},
		{"length limit on sent list", reasonContract,
			r(`"type":"array"}}`, `"type":"array","maxItems":1}}`).Replace(kernelChatInput), kernelChatOutput,
			"input.properties.messages: declares maxItems, which can refuse what the agent sends"},
		{"pattern on sent string", actionLookupContract,
			r(`"query":{"description":"query","type":"string"}`, `"query":{"description":"query","type":"string","pattern":"^[a-z]+$"}`).Replace(kernelLookupInput), kernelLookupOutput,
			"input.properties.query: declares pattern, which can refuse what the agent sends"},
		{"bound admitting the sent range", actionLookupContract,
			r(`"type":"integer"`, `"type":"integer","minimum":1,"maximum":50`).Replace(kernelLookupInput), kernelLookupOutput, ""},
		{"bound refusing the sent range", actionLookupContract,
			r(`"type":"integer"`, `"type":"integer","maximum":5`).Replace(kernelLookupInput), kernelLookupOutput,
			"input.properties.limit: maximum 5 refuses what the agent sends"},
		{"multipleOf on sent number", actionLookupContract,
			r(`"type":"integer"`, `"type":"integer","multipleOf":2`).Replace(kernelLookupInput), kernelLookupOutput,
			"input.properties.limit: multipleOf 2 refuses what the agent sends"},
		{"array without items", reasonContract, kernelChatInput,
			`{"type":"object","properties":{"choices":{"type":"array","description":"reply"}},"additionalProperties":false}`,
			"output.properties.choices.items: missing (want object)"},
	} {
		err := tc.k.check(schema(t, tc.input), schema(t, tc.output))
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: unexpected %v", tc.name, err)
		case tc.want != "" && (err == nil || err.Error() != tc.want):
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
}
