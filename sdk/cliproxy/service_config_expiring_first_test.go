package cliproxy

import (
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestExpiringFirstRoutingSelector(t *testing.T) {
	for _, raw := range []string{"expiring-first", "ExpiringFirst", " ef "} {
		state := normalizedRoutingRuntimeState(&internalconfig.Config{
			Routing: internalconfig.RoutingConfig{Strategy: raw},
		})
		if state.strategy != "expiring-first" {
			t.Fatalf("strategy(%q) = %q, want expiring-first", raw, state.strategy)
		}
		if _, ok := newRoutingSelector(state).(*coreauth.ExpiringFirstSelector); !ok {
			t.Fatalf("selector type = %T, want *auth.ExpiringFirstSelector", newRoutingSelector(state))
		}
	}
}

func TestExpiringFirstRoutingSelectorWithSessionAffinity(t *testing.T) {
	state := normalizedRoutingRuntimeState(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{Strategy: "expiring-first", SessionAffinity: true},
	})
	if _, ok := newRoutingSelector(state).(*coreauth.SessionAffinitySelector); !ok {
		t.Fatalf("selector type = %T, want *auth.SessionAffinitySelector wrapping expiring-first", newRoutingSelector(state))
	}
}
