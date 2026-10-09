package readiness

import (
	"encoding/json"
	"reflect"
	"strings"
)

// AnalysisSchema matches the closed analysis DTO. Semantic/provenance validation
// remains mandatory after provider-side structured output validation.
func AnalysisSchema() string {
	var schema func(reflect.Type) map[string]any
	schema = func(t reflect.Type) map[string]any {
		switch t.Kind() {
		case reflect.Struct:
			props := map[string]any{}
			required := []string{}
			for i := 0; i < t.NumField(); i++ {
				f := t.Field(i)
				name := strings.Split(f.Tag.Get("json"), ",")[0]
				props[name] = schema(f.Type)
				required = append(required, name)
			}
			return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
		case reflect.Slice:
			return map[string]any{"type": "array", "items": schema(t.Elem())}
		case reflect.Bool:
			return map[string]any{"type": "boolean"}
		case reflect.Int:
			return map[string]any{"type": "integer"}
		default:
			return map[string]any{"type": "string"}
		}
	}
	raw, _ := json.Marshal(schema(reflect.TypeOf(Analysis{})))
	return string(raw)
}
