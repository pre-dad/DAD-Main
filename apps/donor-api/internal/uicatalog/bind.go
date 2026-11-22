package uicatalog

import (
	"errors"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxSchemaDepth = 32

var refPattern = regexp.MustCompile(`^#/components/schemas/([^/]+)$`)

type alternative = map[string]any

func (c *catalog) alternatives(schema any, depth int) ([]alternative, error) {
	if depth > maxSchemaDepth {
		return nil, errors.New("schema nesting too deep")
	}
	s, ok := schema.(map[string]any)
	if !ok {
		return nil, nil
	}
	if ref, ok := s["$ref"].(string); ok && ref != "" {
		match := refPattern.FindStringSubmatch(ref)
		if match == nil {
			return nil, errors.New("unresolvable schema reference " + ref)
		}
		target, ok := c.schemas[match[1]]
		if !ok {
			return nil, errors.New("unresolvable schema reference " + ref)
		}
		return c.alternatives(target, depth+1)
	}
	branches, ok := s["oneOf"].([]any)
	if !ok {
		branches, ok = s["anyOf"].([]any)
	}
	if ok {
		var out []alternative
		for _, branch := range branches {
			alts, err := c.alternatives(branch, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, alts...)
		}
		return out, nil
	}
	if parts, ok := s["allOf"].([]any); ok {
		var flat []alternative
		for _, part := range parts {
			alts, err := c.alternatives(part, depth+1)
			if err != nil {
				return nil, err
			}
			flat = append(flat, alts...)
		}
		properties := map[string]any{}
		for _, part := range flat {
			if p, ok := part["properties"].(map[string]any); ok {
				for key, value := range p {
					properties[key] = value
				}
			}
		}
		merged := alternative{}
		for _, part := range flat {
			if kind, _ := part["type"].(string); kind != "" {
				for key, value := range part {
					merged[key] = value
				}
				break
			}
		}
		if len(properties) > 0 {
			merged["properties"] = properties
		}
		return []alternative{merged}, nil
	}
	var types []string
	switch t := s["type"].(type) {
	case []any:
		for _, item := range t {
			if name, ok := item.(string); ok {
				types = append(types, name)
			}
		}
	case string:
		types = []string{t}
	default:
		types = []string{""}
	}
	var out []alternative
	for _, kind := range types {
		if kind == "null" {
			continue
		}
		alt := alternative{}
		for key, value := range s {
			alt[key] = value
		}
		alt["type"] = kind
		out = append(out, alt)
	}
	return out, nil
}

// parsePointer splits an RFC 6901 pointer, or returns nil when it is malformed.
func parsePointer(pointer string) []string {
	if !strings.HasPrefix(pointer, "/") {
		return nil
	}
	segments := strings.Split(pointer[1:], "/")
	for _, segment := range segments {
		if segment == "" {
			return nil
		}
		for i := 0; i < len(segment); i++ {
			if segment[i] == '~' && (i+1 >= len(segment) || (segment[i+1] != '0' && segment[i+1] != '1')) {
				return nil
			}
		}
	}
	out := make([]string, len(segments))
	for i, segment := range segments {
		out[i] = strings.ReplaceAll(strings.ReplaceAll(segment, "~1", "/"), "~0", "~")
	}
	return out
}

var scalarTypes = map[string]bool{"string": true, "integer": true, "number": true, "boolean": true}

type resolved struct {
	kind   string
	scalar bool
	code   string
}

// resolveBind resolves a pointer against the bound response schema. A segment
// resolves when at least one oneOf branch carries it, so the fields of a ready
// dashboard are reachable while the preparing branch renders nothing for them.
func (c *catalog) resolveBind(pointer string) resolved {
	segments := parsePointer(pointer)
	if segments == nil {
		return resolved{code: "BIND_SYNTAX"}
	}
	current, err := c.alternatives(c.schemas[c.BindSchema], 0)
	if err != nil {
		return resolved{code: "BIND_UNRESOLVED"}
	}
	for _, segment := range segments {
		var next []alternative
		for _, alt := range current {
			if alt["type"] != "object" {
				continue
			}
			properties, ok := alt["properties"].(map[string]any)
			if !ok {
				continue
			}
			property, ok := properties[segment]
			if !ok {
				continue
			}
			alts, err := c.alternatives(property, 0)
			if err != nil {
				return resolved{code: "BIND_UNRESOLVED"}
			}
			next = append(next, alts...)
		}
		if len(next) == 0 {
			return resolved{code: "BIND_UNRESOLVED"}
		}
		current = next
	}
	kinds := map[string]bool{}
	scalar := true
	var kind string
	for _, alt := range current {
		name, _ := alt["type"].(string)
		kinds[name] = true
		kind = name
		scalar = scalar && scalarTypes[name]
	}
	if len(kinds) != 1 {
		kind = "mixed"
	}
	return resolved{kind: kind, scalar: scalar}
}

var (
	schemeLike   = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])(?:[a-z][a-z0-9+.-]{1,30}://|(?:https?|javascript|data|mailto|tel|sms|file|ftp|blob|vbscript):|www\.)`)
	markdownLink = regexp.MustCompile(`(?m)\][\s\p{Z}\x{feff}]*\(|\][\s\p{Z}\x{feff}]*\[|^[\s\p{Z}\x{feff}]*\[[^\]]*\]:[\s\p{Z}\x{feff}]`)
)

// jsSpace is the set of characters String.prototype.trim removes.
func jsSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xA0, 0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
		return true
	}
	return r >= 0x2000 && r <= 0x200A
}

func containsControlOrFormat(value string) bool {
	for _, r := range value {
		if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp) {
			return true
		}
	}
	return false
}

// labelProblem returns the error code for text that is not an acceptable plain
// label, or "".
func labelProblem(value any) string {
	text, ok := value.(string)
	if !ok {
		return "BAD_TYPE"
	}
	if strings.TrimFunc(text, jsSpace) == "" {
		return "LABEL_EMPTY"
	}
	if strings.ContainsAny(text, "<>") || containsControlOrFormat(text) {
		return "LABEL_CONTENT"
	}
	if schemeLike.MatchString(text) || markdownLink.MatchString(text) {
		return "LABEL_CONTENT"
	}
	return ""
}

func runeCount(value string) int { return utf8.RuneCountInString(value) }
