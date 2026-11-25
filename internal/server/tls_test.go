package server

import (
	"strings"
	"testing"

	"go.uber.org/zap"
)

func TestWithTLS(t *testing.T) {
	s := New(":0", nil, nil, nil, nil, nil, nil, nil, nil, "", zap.NewNop(),
		WithTLS("/etc/strangle/tls.crt", "/etc/strangle/tls.key"))
	cert, key, err := s.tlsFiles()
	if err != nil {
		t.Fatalf("tlsFiles() error: %v", err)
	}
	if cert != "/etc/strangle/tls.crt" || key != "/etc/strangle/tls.key" {
		t.Fatalf("tlsFiles() = %q, %q", cert, key)
	}
}

func TestTLSRequiresBothFiles(t *testing.T) {
	for _, tc := range []struct{ cert, key string }{
		{cert: "/only/cert"},
		{key: "/only/key"},
	} {
		s := New(":0", nil, nil, nil, nil, nil, nil, nil, nil, "", zap.NewNop(),
			WithTLS(tc.cert, tc.key))
		if _, _, err := s.tlsFiles(); err == nil ||
			!strings.Contains(err.Error(), "must be set together") {
			t.Fatalf("tlsFiles(%q, %q) error = %v", tc.cert, tc.key, err)
		}
	}

	// Without the option, plain HTTP is selected.
	s := New(":0", nil, nil, nil, nil, nil, nil, nil, nil, "", zap.NewNop())
	cert, key, err := s.tlsFiles()
	if err != nil || cert != "" || key != "" {
		t.Fatalf("tlsFiles() = %q, %q, %v; want plain HTTP", cert, key, err)
	}
}
