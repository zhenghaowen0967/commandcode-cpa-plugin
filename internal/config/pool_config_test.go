package config

import (
	"testing"
	"time"
)

func TestEmptyPoolConfigDefaults(t *testing.T) {
	c, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.APIKeys) != 0 || c.Pool.DefaultLimit != 2 || c.Pool.QuotaMaxAge != 5*time.Minute || c.Pool.QuotaRefreshInterval != time.Minute {
		t.Fatalf("unexpected pool defaults: %+v", c.Pool)
	}
}

func TestPoolConfigurationValidation(t *testing.T) {
	for _, doc := range []string{
		"pool:\n  max-concurrency: 0\n",
		"pool:\n  max-concurrency: 1001\n",
		"pool:\n  quota-max-age: 0s\n",
		"pool:\n  quota-refresh-interval: 1ms\n",
		"pool:\n  quota-max-age: bad\n",
	} {
		if _, err := Load([]byte(doc)); err == nil {
			t.Fatalf("accepted invalid pool configuration")
		}
	}
	c, err := Load([]byte("pool:\n  auth-dir: /tmp/test-pool\n  state-path: /tmp/test-pool/state.json\n  max-concurrency: 7\n  quota-max-age: 2m\n  quota-refresh-interval: 10s\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Pool.AuthDir != "/tmp/test-pool" || c.Pool.DefaultLimit != 7 || c.Pool.QuotaMaxAge != 2*time.Minute || c.Pool.QuotaRefreshInterval != 10*time.Second {
		t.Fatalf("pool fields not loaded: %+v", c.Pool)
	}
}
