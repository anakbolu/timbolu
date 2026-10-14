package engine

import (
	"testing"

	"github.com/stretchr/testify/require"
	waLog "go.mau.fi/whatsmeow/util/log"
	"go.uber.org/zap"
)

// The WhatsApp log is a switch rather than a stream: whatsmeow writes a line per
// frame, and mixing that into the structured log would bury the product's own
// events. What is under test is that the switch turns on without panicking and
// that a deployment with no logger gets the no-op.

func TestWALoggerWithoutALoggerIsNoop(t *testing.T) {
	require.Equal(t, waLog.Noop, WALogger(nil))
}

func TestWALoggerForwardsToTheStructuredLog(t *testing.T) {
	logger := zap.NewNop()
	wa := WALogger(logger)
	require.NotNil(t, wa)

	// Every level and the two ways whatsmeow asks for a narrower logger.
	wa.Debugf("debug %d", 1)
	wa.Infof("info %s", "x")
	wa.Warnf("warn")
	wa.Errorf("error %v", "y")
	require.NotNil(t, wa.Sub("client"))
}
