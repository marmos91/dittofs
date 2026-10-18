package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"testing"
	"unicode/utf8"
)

// loadSchema reads testdata/ctrf.schema.json, CTRF's own schema at v0.1.0,
// which check checks JSON values against, copied unchanged from
// https://github.com/ctrf-io/ctrf/blob/v0.1.0/schema/ctrf.schema.json
// (sha256 742dee4cffda469902a9c30c4a87b0b7000d59d2ee502dcccb12ec471cea96a2).
// The check knows the keywords of the parts of that schema dt's output
// reaches, and fails on any other, so a field dt starts writing whose schema
// asks for more than the check knows can't pass unchecked.
func loadSchema(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile("testdata/ctrf.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	root, ok := decode(t, data).(map[string]any)
	if !ok {
		t.Fatal("the schema isn't an object")
	}
	return root
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
func check(node map[string]any, v any, at string) []string {
	var errs []string
	fail := func(format string, args ...any) { errs = append(errs, at+": "+fmt.Sprintf(format, args...)) }
	obj, _ := v.(map[string]any)
	for key, kw := range node {
		switch key {
		case "$schema", "title", "description", "definitions":
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
					errs = append(errs, check(sub.(map[string]any), x, at+"."+name)...)
				}
			}
		case "additionalProperties": // false wherever dt's output reaches
			props, _ := node["properties"].(map[string]any)
			for name := range obj {
				if props[name] == nil {
					fail("has the field %s, which the schema doesn't allow", name)
				}
			}
		case "items":
			arr, _ := v.([]any)
			for i, x := range arr {
				errs = append(errs, check(kw.(map[string]any), x, fmt.Sprintf("%s[%d]", at, i))...)
			}
		case "minItems":
			if arr, ok := v.([]any); ok && len(arr) < number(kw) {
				fail("has fewer than %v items", kw)
			}
		case "minLength":
			if str, ok := v.(string); ok && utf8.RuneCountInString(str) < number(kw) {
				fail("is shorter than %v", kw)
			}
		case "pattern":
			if str, ok := v.(string); ok && !regexp.MustCompile(kw.(string)).MatchString(str) {
				fail("%q doesn't match %s", str, kw)
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

// number is a schema's bound, which is a whole number.
func number(v any) int {
	n, _ := v.(json.Number).Int64()
	return int(n)
}
