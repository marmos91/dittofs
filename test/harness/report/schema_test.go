package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// schema checks JSON values against testdata/ctrf.schema.json, CTRF's own
// schema at v0.1.0, copied unchanged from
// https://github.com/ctrf-io/ctrf/blob/v0.1.0/schema/ctrf.schema.json
// (sha256 742dee4cffda469902a9c30c4a87b0b7000d59d2ee502dcccb12ec471cea96a2).
// It knows the keywords that schema uses and fails on any other, so a schema
// that asks for more than it checks can't pass unchecked. format is an
// annotation, as JSON Schema's draft 7 has it by default.
type schema struct {
	root map[string]any
}

func loadSchema(t *testing.T) schema {
	t.Helper()
	data, err := os.ReadFile("testdata/ctrf.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	root, ok := decode(t, data).(map[string]any)
	if !ok {
		t.Fatal("the schema isn't an object")
	}
	return schema{root}
}

// decode reads JSON with numbers kept as json.Number, so an integer can be
// told from a fraction.
func decode(t *testing.T, data []byte) any {
	t.Helper()
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// check returns every way v breaks the schema node, each at its path.
func (s schema) check(node map[string]any, v any, at string) []string {
	if ref, ok := node["$ref"].(string); ok {
		defs, _ := s.root["definitions"].(map[string]any)
		name, _ := strings.CutPrefix(ref, "#/definitions/")
		def, ok := defs[name].(map[string]any)
		if !ok {
			return []string{at + ": unresolved $ref " + ref}
		}
		node = def
	}
	var errs []string
	fail := func(format string, args ...any) { errs = append(errs, at+": "+fmt.Sprintf(format, args...)) }
	obj, _ := v.(map[string]any)
	for key, kw := range node {
		switch key {
		case "$schema", "$ref", "title", "description", "definitions", "format":
		case "type":
			if !isType(v, kw.(string)) {
				fail("not of type %s", kw)
			}
		case "const":
			if v != kw {
				fail("%v isn't %v", v, kw)
			}
		case "enum":
			if !slices.Contains(kw.([]any), v) {
				fail("%v isn't one of %v", v, kw)
			}
		case "required":
			for _, name := range kw.([]any) {
				if _, ok := obj[name.(string)]; !ok {
					fail("lacks %s", name)
				}
			}
		case "properties":
			for name, sub := range kw.(map[string]any) {
				if x, ok := obj[name]; ok {
					errs = append(errs, s.check(sub.(map[string]any), x, at+"."+name)...)
				}
			}
		case "additionalProperties":
			props, _ := node["properties"].(map[string]any)
			for name, x := range obj {
				if _, known := props[name]; known {
					continue
				}
				switch extra := kw.(type) {
				case bool:
					if !extra {
						fail("has the field %s, which the schema doesn't allow", name)
					}
				case map[string]any:
					errs = append(errs, s.check(extra, x, at+"."+name)...)
				}
			}
		case "items":
			arr, _ := v.([]any)
			for i, x := range arr {
				errs = append(errs, s.check(kw.(map[string]any), x, fmt.Sprintf("%s[%d]", at, i))...)
			}
		case "minItems":
			if arr, ok := v.([]any); ok && len(arr) < number(kw) {
				fail("has fewer than %v items", kw)
			}
		case "minLength":
			if str, ok := v.(string); ok && utf8.RuneCountInString(str) < number(kw) {
				fail("is shorter than %v", kw)
			}
		case "minimum":
			if n, ok := v.(json.Number); ok && number(n) < number(kw) {
				fail("is below %v", kw)
			}
		case "pattern":
			if str, ok := v.(string); ok && !regexp.MustCompile(kw.(string)).MatchString(str) {
				fail("%q doesn't match %s", str, kw)
			}
		case "anyOf":
			if !slices.ContainsFunc(kw.([]any), func(sub any) bool { return len(s.check(sub.(map[string]any), v, at)) == 0 }) {
				fail("matches none of anyOf")
			}
		default:
			fail("the check doesn't know the keyword %q", key)
		}
	}
	return errs
}

func isType(v any, typ string) bool {
	switch x := v.(type) {
	case map[string]any:
		return typ == "object"
	case []any:
		return typ == "array"
	case string:
		return typ == "string"
	case bool:
		return typ == "boolean"
	case json.Number:
		_, err := x.Int64()
		return typ == "number" || typ == "integer" && err == nil
	}
	return false
}

// number is a schema's or a document's whole number; the schema's bounds
// are all whole.
func number(v any) int {
	n, _ := v.(json.Number).Int64()
	return int(n)
}
