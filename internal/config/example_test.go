package config

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// TestExampleMatchesDefaults keeps config.example.yaml honest: apart from the
// values it must set, it documents exactly the built-in defaults.
func TestExampleMatchesDefaults(t *testing.T) {
	b, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	const required = "library:\n  path: ~/Podcasts/Library\nserver:\n  base_url: https://my-mac.example-tailnet.ts.net\n"

	for _, inject := range []bool{false, true} {
		example, minimal := string(b), required
		if inject {
			example = strings.Replace(example, "  enabled: false", "  enabled: true", 1)
			minimal += "inject:\n  enabled: true\n"
		}
		got, err := Parse([]byte(example), "")
		if err != nil {
			t.Fatalf("inject=%v: parse example: %v", inject, err)
		}
		want, err := Parse([]byte(minimal), "")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("inject=%v: config.example.yaml differs from the defaults\nexample:  %+v\ndefaults: %+v", inject, got, want)
		}
	}
}
