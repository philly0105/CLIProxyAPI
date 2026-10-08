package management

import "testing"

func TestNormalizeRoutingStrategyExpiringFirst(t *testing.T) {
	for _, raw := range []string{"expiring-first", "expiringfirst", "EF"} {
		got, ok := normalizeRoutingStrategy(raw)
		if !ok || got != "expiring-first" {
			t.Fatalf("normalizeRoutingStrategy(%q) = %q, %v; want expiring-first, true", raw, got, ok)
		}
	}
}
