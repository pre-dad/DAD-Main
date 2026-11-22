package uicatalog

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"dad/donor-api/api"
)

// The catalog and links copies are kept byte-identical to the contracts by
// TestEmbeddedContractsMatchTheRepository; the module cannot embed files from
// outside its own directory.
var (
	//go:embed ui-catalog.json
	catalogJSON []byte
	//go:embed links.json
	linksJSON []byte
)

type propDefinition struct {
	Name    string
	Kind    string
	Max     string
	Target  string
	Values  []string
	Allowed []string
}

type component struct {
	Props      []propDefinition
	Required   []string
	ExactlyOne []string
}

type catalog struct {
	Version    int
	BindSchema string
	Limits     map[string]int
	Formats    map[string]bool
	Captions   map[string]bool
	Paths      map[string]bool
	Components map[string]component
	schemas    map[string]any
}

var (
	loadOnce sync.Once
	loaded   catalog
	loadErr  error
)

func get() (*catalog, error) {
	loadOnce.Do(func() { loadErr = load(&loaded) })
	return &loaded, loadErr
}

// Version is the catalog_version this server validates specs against.
func Version() int {
	c, err := get()
	if err != nil {
		return 0
	}
	return c.Version
}

// Preload reports a malformed embedded contract at startup instead of on the
// first request.
func Preload() error {
	_, err := get()
	return err
}

func asObject(value any, what string) (*object, error) {
	o, ok := value.(*object)
	if !ok {
		return nil, fmt.Errorf("ui catalog: %s is not an object", what)
	}
	return o, nil
}

func asInt(value any, what string) (int, error) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, fmt.Errorf("ui catalog: %s is not a number", what)
	}
	n, err := number.Int64()
	return int(n), err
}

func load(c *catalog) error {
	root, err := parse(catalogJSON)
	if err != nil {
		return fmt.Errorf("ui catalog: %w", err)
	}
	top, err := asObject(root, "catalog")
	if err != nil {
		return err
	}
	if c.Version, err = asInt(top.values["catalog_version"], "catalog_version"); err != nil {
		return err
	}
	bind, err := asObject(top.values["bind"], "bind")
	if err != nil {
		return err
	}
	c.BindSchema, _ = bind.values["schema"].(string)
	limits, err := asObject(top.values["limits"], "limits")
	if err != nil {
		return err
	}
	c.Limits = map[string]int{}
	for name, value := range limits.values {
		if c.Limits[name], err = asInt(value, "limit "+name); err != nil {
			return err
		}
	}
	c.Formats = keySet(top.values["formats"])
	c.Captions = keySet(top.values["metric_captions"])
	var links struct {
		AccountPaths []string `json:"account_paths"`
	}
	if err = json.Unmarshal(linksJSON, &links); err != nil {
		return fmt.Errorf("links: %w", err)
	}
	navigate, err := asObject(top.values["navigate"], "navigate")
	if err != nil {
		return err
	}
	c.Paths = map[string]bool{}
	for _, name := range stringList(navigate.values["path_lists"]) {
		if name != "account_paths" {
			return errors.New("ui catalog: unsupported navigate path list " + name)
		}
		for _, path := range links.AccountPaths {
			c.Paths[path] = true
		}
	}
	components, err := asObject(top.values["components"], "components")
	if err != nil {
		return err
	}
	c.Components = map[string]component{}
	for _, name := range components.keys {
		definition, err := asObject(components.values[name], "component "+name)
		if err != nil {
			return err
		}
		parsed := component{Required: stringList(definition.values["required"]), ExactlyOne: stringList(definition.values["exactly_one_of"])}
		props, err := asObject(definition.values["props"], name+" props")
		if err != nil {
			return err
		}
		for _, prop := range props.keys {
			fields, err := asObject(props.values[prop], name+"."+prop)
			if err != nil {
				return err
			}
			p := propDefinition{Name: prop}
			p.Kind, _ = fields.values["kind"].(string)
			p.Max, _ = fields.values["max"].(string)
			p.Target, _ = fields.values["target"].(string)
			p.Values = stringList(fields.values["values"])
			p.Allowed = stringList(fields.values["allowed"])
			parsed.Props = append(parsed.Props, p)
		}
		c.Components[name] = parsed
	}
	var document struct {
		Components struct {
			Schemas map[string]any `json:"schemas"`
		} `json:"components"`
	}
	if err = json.Unmarshal(api.OpenAPI, &document); err != nil {
		return fmt.Errorf("openapi: %w", err)
	}
	c.schemas = document.Components.Schemas
	if _, ok := c.schemas[c.BindSchema]; !ok {
		return fmt.Errorf("ui catalog: schema %s is not in the OpenAPI document", c.BindSchema)
	}
	return nil
}

func keySet(value any) map[string]bool {
	out := map[string]bool{}
	if o, ok := value.(*object); ok {
		for key := range o.values {
			out[key] = true
		}
	}
	return out
}

func stringList(value any) []string {
	items, _ := value.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
