package egress

import (
	"context"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/testutil"
)

func TestLeaseUsesBuildConfiguredDeadline(t *testing.T) {
	manager := NewManager(&config.Config{GrokEgressEnabled: true, GrokEgressNodes: []config.EgressNodeConfig{{Name: "limits-direct", Scope: "all"}}, RequestTimeout: 900})
	lease, err := manager.Acquire(context.Background(), "cli", "test")
	testutil.NoError(t, err)
	defer lease.Release()
	testutil.Equal(t, lease.client.Timeout, 900*time.Second)
}
