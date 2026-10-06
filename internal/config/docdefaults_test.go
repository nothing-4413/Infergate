package config

// The two tables of configuration keys in docs/USAGE.md state a default for
// every knob they document. A stale default is the same class of failure the
// strict decoder removed from the loader: a plausible-looking document that
// nothing executes. This test walks the documented keys against
// config.Defaults() and fails on either half of the drift -- a key that no
// longer exists, or a default that is no longer the one the loader uses.
//
// It deliberately only checks rows whose first segment is a real top-level
// section of Config, so the same file's tables of response headers, metric
// names and benchmark numbers are left alone. A row is skipped when its
// documented value is not a scalar (a wildcard row such as `cache.redis.*`, or
// prose such as "全 0"): the check is for keys this test can compare exactly.

import (
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

const usageDocPath = "../../docs/USAGE.md"

// jsonField returns the field of the struct in v whose json tag matches name.
func jsonField(v reflect.Value, name string) (reflect.Value, bool) {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if tag == "" {
			continue
		}
		if strings.Split(tag, ",")[0] == name {
			return v.Field(i), true
		}
	}
	return reflect.Value{}, false
}

// walkDefaults resolves a dotted key such as cache.embedding.provider against
// config.Defaults(), following json tags exactly as the loader does.
func walkDefaults(key string) (reflect.Value, error) {
	cur := reflect.ValueOf(Defaults())
	for i, seg := range strings.Split(key, ".") {
		if cur.Kind() != reflect.Struct {
			return reflect.Value{}, fmt.Errorf("%s: %q is not inside a section", key, strings.Join(strings.Split(key, ".")[:i], "."))
		}
		next, ok := jsonField(cur, seg)
		if !ok {
			return reflect.Value{}, fmt.Errorf("%s: no field is named %q", key, seg)
		}
		cur = next
	}
	return cur, nil
}

// matchesDefault reports whether the documented text is the value the loader
// defaults to. Durations are compared as durations so that the document may
// write 15m where Go prints 15m0s.
func matchesDefault(field reflect.Value, doc string) (bool, string) {
	if field.Type() == reflect.TypeOf(Duration(0)) {
		want, err := time.ParseDuration(doc)
		if err != nil {
			return false, fmt.Sprintf("%q is not a duration", doc)
		}
		got := time.Duration(field.Interface().(Duration))
		return got == want, fmt.Sprintf("the loader uses %v", got)
	}
	got := fmt.Sprintf("%v", field.Interface())
	switch field.Kind() {
	case reflect.Bool:
		want, err := strconv.ParseBool(doc)
		if err != nil {
			return false, fmt.Sprintf("%q is not a boolean", doc)
		}
		return field.Bool() == want, fmt.Sprintf("the loader uses %v", got)
	case reflect.Int, reflect.Int64:
		want, err := strconv.ParseInt(doc, 10, 64)
		if err != nil {
			return false, fmt.Sprintf("%q is not an integer", doc)
		}
		return field.Int() == want, fmt.Sprintf("the loader uses %v", got)
	case reflect.Float64:
		want, err := strconv.ParseFloat(doc, 64)
		if err != nil {
			return false, fmt.Sprintf("%q is not a number", doc)
		}
		return field.Float() == want, fmt.Sprintf("the loader uses %v", got)
	case reflect.String:
		return field.String() == doc, fmt.Sprintf("the loader uses %q", got)
	default:
		return false, "the test cannot compare a " + field.Kind().String()
	}
}

func TestDocumentedDefaultsMatchTheLoader(t *testing.T) {
	raw, err := os.ReadFile(usageDocPath)
	if err != nil {
		t.Fatalf("reading %s: %v", usageDocPath, err)
	}
	checked := 0
	for i, line := range strings.Split(string(raw), "\n") {
		at := fmt.Sprintf("docs/USAGE.md:%d", i+1)
		if !strings.HasPrefix(line, "| ") {
			continue
		}
		cols := strings.Split(line, "|")
		if len(cols) < 4 {
			continue
		}
		key := strings.Trim(strings.TrimSpace(cols[1]), "`")
		doc := strings.Trim(strings.TrimSpace(cols[2]), "`")
		if key == "" || doc == "" || strings.ContainsAny(key, "*[] ") {
			continue
		}
		// Only rows anchored in a real section are configuration keys; the same
		// tables' header and metric rows are not.
		root := strings.Split(key, ".")[0]
		if _, ok := jsonField(reflect.ValueOf(Defaults()), root); !ok {
			continue
		}
		field, err := walkDefaults(key)
		if err != nil {
			t.Errorf("%s: the document names %s, but %v", at, key, err)
			continue
		}
		if field.Kind() == reflect.Struct || field.Kind() == reflect.Map || field.Kind() == reflect.Slice {
			continue // a section or a list, not a scalar with one documented value
		}
		if strings.ContainsAny(doc, " /") {
			continue // the row documents more than one value, or prose
		}
		if ok, why := matchesDefault(field, doc); !ok {
			t.Errorf("%s: %s is documented as %s, but %s", at, key, doc, why)
			continue
		}
		checked++
	}
	if checked < 12 {
		t.Fatalf("only %d documented defaults were compared: the table parsing stopped working", checked)
	}
	t.Logf("compared %d documented defaults against config.Defaults()", checked)
}
