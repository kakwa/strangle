package testutil

import (
	"encoding/json"
	"reflect"
)

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
