package testutil

import (
	"encoding/json"
	"reflect"
	"testing"
)

// AssertJSONEqual reports to t a failure when got and want are not
// semantically equal as JSON documents.
func AssertJSONEqual(t *testing.T, got, want []byte) {
	t.Helper()
	if !JSONEqual(got, want) {
		t.Errorf("JSON documents differ\ngot:  %s\nwant: %s", got, want)
	}
}

// JSONEqual reports whether two JSON documents are semantically equal.
// PostgreSQL's jsonb normalizes whitespace and key order, so raw string
// equality is too strict.
func JSONEqual(got, want []byte) bool {
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		return false
	}
	if err := json.Unmarshal(want, &wantValue); err != nil {
		return false
	}
	return reflect.DeepEqual(gotValue, wantValue)
}
