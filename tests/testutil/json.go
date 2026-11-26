package testutil

import (
	"encoding/json"
	"reflect"
	"testing"
)

// AssertJSONEqual compares JSON documents semantically: PostgreSQL's jsonb
// normalizes whitespace and key order, so raw string equality is too strict.
func AssertJSONEqual(t *testing.T, got, want []byte) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("decode got JSON %q: %v", got, err)
	}
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatalf("decode want JSON %q: %v", want, err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Errorf("JSON = %s, want %s", got, want)
	}
}
