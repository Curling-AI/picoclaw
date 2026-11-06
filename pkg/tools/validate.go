package tools

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
)

// coerceToolArgs fixes the most common LLM tool-call typing slips in place —
// numeric values sent as strings ({"projectId": "39679"} against an integer
// schema) and stringified booleans — so they don't burn a whole agent
// iteration on a validation error. Only unambiguous conversions are applied;
// anything else is left untouched for validateToolArgs to report.
func coerceToolArgs(schema map[string]any, args map[string]any) {
	props, ok := schema["properties"].(map[string]any)
	if !ok || args == nil {
		return
	}
	for key, val := range args {
		propSchema, ok := props[key].(map[string]any)
		if !ok {
			continue
		}
		typeName, _ := propSchema["type"].(string)
		switch typeName {
		case "integer":
			if s, ok := val.(string); ok {
				if n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
					args[key] = n
				}
			}
		case "number":
			if s, ok := val.(string); ok {
				if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
					args[key] = f
				}
			}
		case "boolean":
			if s, ok := val.(string); ok {
				switch strings.TrimSpace(strings.ToLower(s)) {
				case "true":
					args[key] = true
				case "false":
					args[key] = false
				}
			}
		case "object":
			// Models that write calls as markup (GLM) or stringify JSON hand
			// an object over as its text.
			if s, ok := val.(string); ok {
				var decoded map[string]any
				if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &decoded); err == nil && decoded != nil {
					args[key], val = decoded, decoded
				}
			}
			if obj, ok := val.(map[string]any); ok {
				coerceToolArgs(propSchema, obj)
			}
		case "array":
			if s, ok := val.(string); ok {
				var decoded []any
				if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &decoded); err == nil && decoded != nil {
					args[key], val = decoded, decoded
				}
			}
			itemSchema, ok := propSchema["items"].(map[string]any)
			if !ok {
				continue
			}
			arr, ok := val.([]any)
			if !ok {
				continue
			}
			wrapper := map[string]any{"properties": map[string]any{"item": itemSchema}}
			for i, elem := range arr {
				cell := map[string]any{"item": elem}
				coerceToolArgs(wrapper, cell)
				arr[i] = cell["item"]
			}
		}
	}
}

// dropUnknownToolArgs removes, in place, the properties the schema does not
// declare and names what it removed, so a stray argument does not burn an
// iteration on a validation error. Removing instead of letting them through
// keeps internal arguments such as __channel out of the model's reach.
func dropUnknownToolArgs(schema map[string]any, args map[string]any) []string {
	props, ok := schema["properties"].(map[string]any)
	if !ok || args == nil {
		return nil
	}
	var dropped []string
	for key, val := range args {
		propSchema, known := props[key].(map[string]any)
		if _, declared := props[key]; !declared && !allowsAdditional(schema) {
			delete(args, key)
			dropped = append(dropped, key)
			continue
		}
		if !known {
			continue
		}
		for _, nested := range dropUnknownNestedArgs(propSchema, val) {
			dropped = append(dropped, key+"."+nested)
		}
	}
	slices.Sort(dropped)
	return dropped
}

func dropUnknownNestedArgs(propSchema map[string]any, val any) []string {
	switch v := val.(type) {
	case map[string]any:
		return dropUnknownToolArgs(propSchema, v)
	case []any:
		itemSchema, ok := propSchema["items"].(map[string]any)
		if !ok {
			return nil
		}
		var dropped []string
		for _, item := range v {
			dropped = append(dropped, dropUnknownNestedArgs(itemSchema, item)...)
		}
		return dropped
	}
	return nil
}

// validateToolArgs validates args against a JSON Schema-like map.
// schema is expected to have optional keys: "properties", "required", "additionalProperties".
func validateToolArgs(schema map[string]any, args map[string]any) error {
	if len(schema) == 0 {
		return nil
	}

	if args == nil {
		args = map[string]any{}
	}

	if err := checkRequired(schema, args); err != nil {
		return err
	}

	propsRaw, ok := schema["properties"]
	if !ok {
		return nil // no properties defined — accept any args
	}

	props, ok := propsRaw.(map[string]any)
	if !ok {
		return nil
	}

	additional := allowsAdditional(schema)

	for key, val := range args {
		propSchemaRaw, known := props[key]
		if !known {
			if !additional {
				accepted := slices.Sorted(maps.Keys(props))
				return fmt.Errorf("unexpected property %q (accepted: %s)", key, strings.Join(accepted, ", "))
			}
			continue
		}
		propSchema, ok := propSchemaRaw.(map[string]any)
		if !ok {
			continue // can't validate without a proper schema map
		}
		if err := checkType(key, val, propSchema); err != nil {
			return err
		}
	}

	return nil
}

// checkRequired verifies that every field listed in schema["required"] is present in args.
func checkRequired(schema map[string]any, args map[string]any) error {
	reqRaw, ok := schema["required"]
	if !ok {
		return nil
	}

	var required []string

	switch r := reqRaw.(type) {
	case []string:
		required = r
	case []any:
		for _, v := range r {
			s, ok := v.(string)
			if ok {
				required = append(required, s)
			}
		}
	default:
		return nil
	}

	for _, field := range required {
		if _, present := args[field]; !present {
			return fmt.Errorf("missing required property %q", field)
		}
	}
	return nil
}

// allowsAdditional returns true when the schema explicitly sets
// "additionalProperties" to true or to a schema (a map with free-form keys, such
// as a spreadsheet's sheet names). Absent or false rejects extras.
func allowsAdditional(schema map[string]any) bool {
	switch v := schema["additionalProperties"].(type) {
	case bool:
		return v
	case map[string]any:
		return true
	}
	return false
}

// checkType validates that val matches the JSON Schema type declared in propSchema.
func checkType(key string, val any, propSchema map[string]any) error {
	typeRaw, ok := propSchema["type"]
	if !ok {
		return nil // no type constraint
	}
	typeName, ok := typeRaw.(string)
	if !ok {
		return nil
	}

	switch typeName {
	case "string":
		if _, ok := val.(string); !ok {
			return fmt.Errorf("property %q: expected string, got %T", key, val)
		}
	case "integer":
		switch v := val.(type) {
		case float64:
			if v != math.Trunc(v) {
				return fmt.Errorf("property %q: expected integer, got float64 with fractional part", key)
			}
		case int:
			// ok
		case int64:
			// ok
		default:
			return fmt.Errorf("property %q: expected integer, got %T", key, val)
		}
	case "number":
		switch val.(type) {
		case float64, int, int64:
			// ok
		default:
			return fmt.Errorf("property %q: expected number, got %T", key, val)
		}
	case "boolean":
		if _, ok := val.(bool); !ok {
			return fmt.Errorf("property %q: expected boolean, got %T", key, val)
		}
	case "array":
		arr, ok := val.([]any)
		if !ok {
			return fmt.Errorf("property %q: expected array, got %T", key, val)
		}
		if err := checkArrayItems(key, arr, propSchema); err != nil {
			return err
		}
	case "object":
		obj, ok := val.(map[string]any)
		if !ok {
			return fmt.Errorf("property %q: expected object, got %T", key, val)
		}
		if err := validateToolArgs(propSchema, obj); err != nil {
			return fmt.Errorf("property %q: %w", key, err)
		}
	}

	if err := checkEnum(key, val, propSchema); err != nil {
		return err
	}

	return nil
}

// checkArrayItems validates each element of arr against the "items" sub-schema.
func checkArrayItems(key string, arr []any, propSchema map[string]any) error {
	itemsRaw, ok := propSchema["items"]
	if !ok {
		return nil
	}
	itemSchema, ok := itemsRaw.(map[string]any)
	if !ok {
		return nil
	}
	for i, elem := range arr {
		elemKey := fmt.Sprintf("%s[%d]", key, i)
		if err := checkType(elemKey, elem, itemSchema); err != nil {
			return err
		}
	}
	return nil
}

// checkEnum validates that val is one of the allowed enum values in propSchema.
func checkEnum(key string, val any, propSchema map[string]any) error {
	enumRaw, ok := propSchema["enum"]
	if !ok {
		return nil
	}

	switch ev := enumRaw.(type) {
	case []any:
		for _, allowed := range ev {
			if val == allowed {
				return nil
			}
		}
	case []string:
		s, ok := val.(string)
		if ok {
			for _, allowed := range ev {
				if s == allowed {
					return nil
				}
			}
		}
	default:
		return nil // unknown enum format, skip
	}

	return fmt.Errorf("property %q: value %v is not in enum", key, val)
}
