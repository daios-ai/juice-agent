package juice

import (
	"fmt"
	"slices"
)

// The two affordances the harness depends on are configured by address, so any
// action may fill either. A contract is the least an action must declare to fill
// one: the input the client sends and the output it reads, written in the
// kernel's canonical schema form (manual, "Action schemas"). The kernel stores
// every action's schemas in that form, so checking a declared schema against a
// contract is a comparison of two small trees. It proves declared compatibility,
// not that every response carries every field; a response lacking an optional
// field is handled where it is read.
type contract struct {
	input, output map[string]any
}

// reasonContract is what Chat sends and reads (the kernel's llm/chat shape).
var reasonContract = contract{
	input: obj(map[string]any{
		"messages": arr("The conversation so far", obj(map[string]any{
			"role": map[string]any{"type": "string", "description": "system, user or assistant",
				"enum": []any{"system", "user", "assistant"}},
			"content": prop("string", "The text of the turn"),
		}, "role", "content")),
	}, "messages"),
	output: obj(map[string]any{
		"choices": arr("The model's reply; the first choice is read", obj(map[string]any{
			"message": obj(map[string]any{"content": prop("string", "The reply text")}),
		})),
	}),
}

// actionLookupContract is what Lookup sends and ParseLookup reads (the kernel's
// lookup shape).
var actionLookupContract = contract{
	input: obj(map[string]any{
		"query": prop("string", "What the agent is looking for"),
		// The agent sends a limit between 1 and 10; a declared bound must admit that range.
		"limit": map[string]any{"type": "integer", "description": "At most this many results",
			"minimum": 1.0, "maximum": 10.0},
	}, "query"),
	output: obj(map[string]any{
		"results": arr("Matching actions, best first", obj(map[string]any{
			"action":       prop("string", "The action's address, owner@kernel/name"),
			"description":  prop("string", "What the action does"),
			"input_schema": prop("object", "The action's input, as JSON Schema"),
		})),
	}),
}

// check reports the first place an action's declared schemas fall short of c.
func (c contract) check(input, output map[string]any) error {
	if err := satisfies(c.input, input, "input", true); err != nil {
		return err
	}
	return satisfies(c.output, output, "output", false)
}

// satisfies reports whether a declared schema node can carry what min describes.
// What the client sends (sending) must be accepted: each sent property declared,
// with a type that is or includes the sent one, an enum that admits the sent
// values, no length or pattern limit (the agent's strings and lists are
// unbounded), numeric bounds that admit the range the agent sends, and nothing
// required beyond what is sent. What the client reads must be
// guaranteed: each read property declared with exactly the read type, so a
// nullable declaration is refused, since the action may return null.
func satisfies(min, declared map[string]any, path string, sending bool) error {
	want, _ := min["type"].(string)
	if declared == nil {
		return fmt.Errorf("%s: missing (want %s)", path, want)
	}
	types := typesOf(declared["type"])
	switch {
	case len(types) == 0 && sending:
		return nil // an untyped node accepts any value
	case sending && !slices.Contains(types, want), !sending && !slices.Equal(types, []string{want}):
		return fmt.Errorf("%s: type %v (want %s)", path, declared["type"], want)
	}
	if sending {
		if err := admits(min, declared, path); err != nil {
			return err
		}
	}
	switch want {
	case "array":
		items, _ := declared["items"].(map[string]any)
		return satisfies(min["items"].(map[string]any), items, path+".items", sending)
	case "object":
		props, declaresProps := declared["properties"].(map[string]any)
		if !declaresProps && sending {
			return nil // an object without declared properties accepts any keys
		}
		minProps, _ := min["properties"].(map[string]any)
		if sending {
			for _, r := range anyStrings(declared["required"]) {
				if _, sent := minProps[r]; !sent {
					return fmt.Errorf("%s: requires %s, which the agent does not send", path, r)
				}
			}
		}
		for _, name := range sortedKeys(minProps) {
			p, _ := props[name].(map[string]any)
			if err := satisfies(minProps[name].(map[string]any), p, path+".properties."+name, sending); err != nil {
				return err
			}
		}
	}
	return nil
}

// admits reports a constraint on a sent node that could refuse what the agent
// sends. These are the constraints the kernel enforces on arguments; format is
// not enforced and is ignored.
func admits(min, declared map[string]any, path string) error {
	for _, k := range []string{"minLength", "maxLength", "pattern", "minItems", "maxItems"} {
		if _, ok := declared[k]; ok {
			return fmt.Errorf("%s: declares %s, which can refuse what the agent sends", path, k)
		}
	}
	if enum, ok := min["enum"].([]any); ok {
		if have, ok := declared["enum"].([]any); ok {
			for _, v := range enum {
				if !slices.Contains(have, v) {
					return fmt.Errorf("%s: enum lacks %v", path, v)
				}
			}
		}
	}
	// A declared bound must admit the contract's range; with no range in the
	// contract, any bound can refuse.
	lo, hasLo := min["minimum"].(float64)
	hi, hasHi := min["maximum"].(float64)
	for _, b := range []struct {
		key     string
		refuses func(v float64) bool
	}{
		{"minimum", func(v float64) bool { return !hasLo || v > lo }},
		{"exclusiveMinimum", func(v float64) bool { return !hasLo || v >= lo }},
		{"maximum", func(v float64) bool { return !hasHi || v < hi }},
		{"exclusiveMaximum", func(v float64) bool { return !hasHi || v <= hi }},
		{"multipleOf", func(v float64) bool { return v != 1 }},
	} {
		if v, ok := declared[b.key].(float64); ok && b.refuses(v) {
			return fmt.Errorf("%s: %s %v refuses what the agent sends", path, b.key, v)
		}
	}
	return nil
}

// typesOf reads a schema's type, a name or a list of names, as a list.
func typesOf(t any) []string {
	if s, ok := t.(string); ok {
		return []string{s}
	}
	return anyStrings(t)
}

func anyStrings(v any) []string {
	var out []string
	list, _ := v.([]any)
	for _, x := range list {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// prop, obj and arr write the contracts in the kernel's canonical form.
func prop(typ, description string) map[string]any {
	return map[string]any{"type": typ, "description": description}
}

func obj(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		req := make([]any, len(required))
		for i, r := range required {
			req[i] = r
		}
		s["required"] = req
	}
	return s
}

func arr(description string, items map[string]any) map[string]any {
	return map[string]any{"type": "array", "description": description, "items": items}
}
