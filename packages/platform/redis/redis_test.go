package redis

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNewRejectsBadConfiguration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tests := []struct {
		name string
		url  string
	}{
		{name: "empty url", url: ""},
		{name: "unparsable url", url: "://not-a-url"},
		{name: "unreachable server", url: "redis://127.0.0.1:1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, err := New(ctx, tt.url)
			require.Error(t, err, "a misconfigured Redis must fail at startup")
			require.Nil(t, client)
		})
	}
}

// TestNilClientIsSafe documents the contract the health probe relies on: a
// process without Redis must report "not configured" instead of panicking.
func TestNilClientIsSafe(t *testing.T) {
	var client *Client

	require.Nil(t, client.Raw())
	require.Error(t, client.Ping(context.Background()))
	require.NoError(t, client.Close())
}
