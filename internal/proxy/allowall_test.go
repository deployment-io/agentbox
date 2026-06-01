package proxy

import "testing"

// TestAllowAllList confirms allow-all admits arbitrary public hosts but still
// honors the hard-deny set. (The IP-literal and private-IP gates live in the
// CONNECT/tunnel path, not in Allows, so they're unaffected by this mode.)
func TestAllowAllList(t *testing.T) {
	a := NewAllowAllList()

	for _, h := range []string{"example.com", "registry.npmjs.org", "storage.googleapis.com", "anything.whatever.io"} {
		if !a.Allows(h) {
			t.Errorf("allow-all should permit %q", h)
		}
	}
	for _, h := range []string{"localhost", "127.0.0.1", "0.0.0.0", "::1"} {
		if a.Allows(h) {
			t.Errorf("allow-all must still hard-deny %q", h)
		}
	}
}
