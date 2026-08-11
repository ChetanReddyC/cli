package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
)

// normalizeLegacyTrailJSON accepts the former BFF's snake_case trail payloads
// while the CLI moves to entire-api's camelCase contract. It recursively
// re-cases object keys so existing cached/test payloads remain readable; native
// camelCase keys pass through unchanged.
func normalizeLegacyTrailJSON(data []byte) ([]byte, error) {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, fmt.Errorf("decode trail JSON: %w", err)
	}
	normalized, err := json.Marshal(normalizeTrailJSONValue(value))
	if err != nil {
		return nil, fmt.Errorf("encode normalized trail JSON: %w", err)
	}
	return normalized, nil
}

func decodeNormalizedTrailJSON(data []byte, dest any) error {
	normalized, err := normalizeLegacyTrailJSON(data)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(normalized, dest); err != nil {
		return fmt.Errorf("decode normalized trail JSON: %w", err)
	}
	return nil
}

func legacyTrailRequestBody(body any) (any, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal trail request: %w", err)
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, fmt.Errorf("decode trail request: %w", err)
	}
	return snakeCaseTrailJSONValue(value), nil
}

func snakeCaseTrailJSONValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, child := range v {
			out[lowerCamelToSnake(key)] = snakeCaseTrailJSONValue(child)
		}
		return out
	case []any:
		for i := range v {
			v[i] = snakeCaseTrailJSONValue(v[i])
		}
	}
	return value
}

func lowerCamelToSnake(value string) string {
	var out strings.Builder
	for i, r := range value {
		if unicode.IsUpper(r) {
			if i > 0 {
				out.WriteByte('_')
			}
			r = unicode.ToLower(r)
		}
		out.WriteRune(r)
	}
	return out.String()
}

func normalizeTrailJSONValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, child := range v {
			out[snakeToLowerCamel(key)] = normalizeTrailJSONValue(child)
		}
		return out
	case []any:
		for i := range v {
			v[i] = normalizeTrailJSONValue(v[i])
		}
	}
	return value
}

func snakeToLowerCamel(value string) string {
	if !strings.ContainsRune(value, '_') {
		return value
	}
	var out strings.Builder
	upperNext := false
	for _, r := range value {
		if r == '_' {
			upperNext = true
			continue
		}
		if upperNext {
			r = unicode.ToUpper(r)
			upperNext = false
		}
		out.WriteRune(r)
	}
	return out.String()
}
