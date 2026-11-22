// Package uicatalog validates generated dashboard layout specs against the UI
// catalog in contracts/mobile/ui-catalog.json. It implements the same rules, in
// the same order, as scripts/ui-catalog.mjs, and TestSharedVectors holds the
// two to the shared vectors.
package uicatalog

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Error locates one reason a spec was refused. Path is a JSON Pointer into the
// spec and Code is a stable reason.
type Error struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ErrNotJSON is returned for a spec that is not one well-formed JSON value.
var ErrNotJSON = errors.New("spec is not valid JSON")

// Result is the outcome of validating one spec.
type Result struct {
	OK     bool
	Errors []Error
}

// Validate checks a spec given as raw JSON. A spec that is not JSON at all is
// an ErrNotJSON; every other fault is reported in the Result.
func Validate(raw []byte) (Result, error) {
	c, err := get()
	if err != nil {
		return Result{}, err
	}
	spec, err := parse(raw)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrNotJSON, err)
	}
	v := &validator{c: c, counted: map[string]bool{}}
	switch blocks := spec.(type) {
	case []any:
		if len(blocks) == 0 {
			v.fail("", "SPEC_EMPTY", "A spec needs at least one block")
		}
		for i, block := range blocks {
			v.node(block, fmt.Sprintf("/%d", i), 1)
		}
	default:
		v.fail("", "SPEC_NOT_ARRAY", "A spec is an ordered array of blocks")
	}
	return Result{OK: len(v.errors) == 0, Errors: v.errors}, nil
}

type validator struct {
	c            *catalog
	errors       []Error
	nodes        int
	limitReached bool
	counted      map[string]bool
}

func (v *validator) fail(path, code, message string) {
	v.errors = append(v.errors, Error{Path: path, Code: code, Message: message})
}

func (v *validator) count(path string) {
	v.counted[path] = true
	v.nodes++
	if v.nodes > v.c.Limits["max_nodes"] && !v.limitReached {
		v.limitReached = true
		v.fail(path, "TOO_MANY_NODES", fmt.Sprintf("A spec holds at most %d nodes", v.c.Limits["max_nodes"]))
	}
}

func (v *validator) node(node any, path string, depth int) {
	if !v.counted[path] {
		v.count(path)
	}
	o, ok := node.(*object)
	if !ok {
		v.fail(path, "NODE_INVALID", "A node must be an object")
		return
	}
	kind, ok := o.values["type"].(string)
	if !ok || kind == "" {
		v.fail(path, "MISSING_TYPE", "A node needs a string type")
		return
	}
	definition, known := v.c.Components[kind]
	if !known {
		return
	}
	if depth > v.c.Limits["max_depth"] {
		v.fail(path, "TOO_DEEP", "Nesting is limited to two levels")
		return
	}
	for _, key := range o.keys {
		if key == "type" {
			continue
		}
		if !slices.ContainsFunc(definition.Props, func(p propDefinition) bool { return p.Name == key }) {
			v.fail(path+"/"+key, "UNKNOWN_PROP", fmt.Sprintf("%s has no prop %s", kind, key))
		}
	}
	for _, name := range definition.Required {
		if !o.has(name) {
			v.fail(path+"/"+name, "MISSING_PROP", fmt.Sprintf("%s requires %s", kind, name))
		}
	}
	if group := definition.ExactlyOne; len(group) > 0 {
		present := 0
		for _, name := range group {
			if o.has(name) {
				present++
			}
		}
		if present != 1 {
			v.fail(path, "ONE_OF", fmt.Sprintf("%s needs exactly one of %s", kind, strings.Join(group, ", ")))
		}
	}
	for _, prop := range definition.Props {
		if o.has(prop.Name) {
			v.prop(path, prop, o.values[prop.Name], kind)
		}
	}
}

func (v *validator) prop(path string, p propDefinition, value any, kind string) {
	at := path + "/" + p.Name
	switch p.Kind {
	case "label":
		if problem := labelProblem(value); problem != "" {
			v.fail(at, problem, p.Name+" is not an acceptable plain label")
			return
		}
		if limit := v.c.Limits[p.Max]; runeCount(value.(string)) > limit {
			v.fail(at, "LABEL_TOO_LONG", fmt.Sprintf("%s exceeds %d characters", p.Name, limit))
		}
	case "bind":
		v.bind(at, value, p.Target)
	case "enum":
		text, ok := value.(string)
		if !ok {
			v.fail(at, "BAD_TYPE", p.Name+" must be a string")
		} else if !slices.Contains(p.Values, text) {
			v.fail(at, "BAD_ENUM", fmt.Sprintf("%s must be one of %s", p.Name, strings.Join(p.Values, ", ")))
		}
	case "format":
		text, ok := value.(string)
		if !ok {
			v.fail(at, "BAD_TYPE", "format must be a string")
		} else if !v.c.Formats[text] {
			v.fail(at, "BAD_FORMAT", "Unknown metric format: "+text)
		}
	case "caption":
		text, ok := value.(string)
		if !ok {
			v.fail(at, "BAD_TYPE", "caption must be a string")
		} else if !v.c.Captions[text] {
			v.fail(at, "BAD_CAPTION", "Unknown metric caption: "+text)
		}
	case "navigate":
		text, ok := value.(string)
		if !ok {
			v.fail(at, "BAD_TYPE", "navigate must be a path string")
		} else if !v.c.Paths[text] {
			v.fail(at, "NAV_FORBIDDEN", "navigate must be an approved in-app path")
		}
	case "children":
		v.children(at, value, p, kind)
	}
}

func (v *validator) bind(path string, value any, target string) {
	pointer, ok := value.(string)
	if !ok {
		v.fail(path, "BAD_TYPE", "bind must be a JSON Pointer string")
		return
	}
	result := v.c.resolveBind(pointer)
	switch {
	case result.code == "BIND_SYNTAX":
		v.fail(path, "BIND_SYNTAX", "bind must be an RFC 6901 pointer")
	case result.code != "":
		v.fail(path, "BIND_UNRESOLVED", fmt.Sprintf("%s is not a field of %s", pointer, v.c.BindSchema))
	case target == "array":
		if result.kind != "array" {
			v.fail(path, "BIND_NOT_ARRAY", pointer+" must be an array")
		}
	case !result.scalar:
		v.fail(path, "BIND_NOT_SCALAR", pointer+" must be a scalar")
	case target == "integer" && result.kind != "integer":
		v.fail(path, "BIND_NOT_INTEGER", pointer+" must be an integer")
	}
}

func (v *validator) children(at string, value any, p propDefinition, parent string) {
	items, ok := value.([]any)
	if !ok {
		v.fail(at, "BAD_TYPE", "children must be an array")
		return
	}
	if len(items) == 0 {
		v.fail(at, "CHILDREN_EMPTY", "children must not be empty")
		return
	}
	limit := v.c.Limits[p.Max]
	if len(items) > limit {
		code := "LIST_TOO_LARGE"
		if parent == "MetricGrid" {
			code = "GRID_TOO_LARGE"
		}
		v.fail(at, code, fmt.Sprintf("%s holds at most %d children", parent, limit))
	}
	for i, child := range items {
		childPath := fmt.Sprintf("%s/%d", at, i)
		if o, ok := child.(*object); ok {
			if kind, ok := o.values["type"].(string); ok {
				if _, known := v.c.Components[kind]; known {
					v.count(childPath)
					if !slices.Contains(p.Allowed, kind) {
						v.fail(childPath, "CHILD_NOT_ALLOWED", fmt.Sprintf("%s is not allowed inside %s", kind, parent))
						if _, nested := o.values["children"].([]any); nested {
							v.fail(childPath+"/children", "TOO_DEEP", "Nesting is limited to two levels")
						}
						continue
					}
				}
			}
		}
		v.node(child, childPath, 2)
	}
}
