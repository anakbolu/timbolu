package migrate

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMigrateErrors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	unreachable := "postgres://user:pass@127.0.0.1:1/none?sslmode=disable&connect_timeout=1"

	require.Error(t, MigrateUp(ctx, unreachable))
	require.Error(t, MigrateDown(ctx, unreachable))
	_, _, err := MigrateVersion(ctx, "://nope")
	require.Error(t, err)
}
