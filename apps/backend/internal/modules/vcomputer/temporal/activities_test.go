package temporal

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	sdktemporal "go.temporal.io/sdk/temporal"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/vcomputer/domain"
)

// stubClient is the control plane as an activity sees it: every call answers the same error,
// which is what makes a test of the classification independent of any route.
type stubClient struct {
	err error
}

func (s stubClient) EnsureSession(context.Context, string) (domain.Session, error) {
	return domain.Session{}, s.err
}

func (s stubClient) StopSession(context.Context, string) error { return s.err }

func (s stubClient) Screenshot(context.Context, string) ([]byte, error) { return nil, s.err }

func (s stubClient) Click(context.Context, string, int, int) error { return s.err }

func (s stubClient) Type(context.Context, string, string) error { return s.err }

func (s stubClient) Navigate(context.Context, string, string) error { return s.err }

func (s stubClient) Exec(context.Context, string, string) (domain.ExecResult, error) {
	return domain.ExecResult{}, s.err
}

// TestConfigurationMistakesAreNotRetryable is the half of the error contract that only exists
// here: the client classifies a missing control plane and a refused token as ErrNotConfigured,
// and the activity has to turn that into a Temporal error that stops the retry. Otherwise a
// deployment with a wrong token spends its attempts sending the same header again and reports
// the failure three times as late.
//
// Every activity is exercised, because each one is a place the wrapping could be forgotten.
func TestConfigurationMistakesAreNotRetryable(t *testing.T) {
	activities := New(Deps{Computer: stubClient{err: domain.ErrNotConfigured}})
	ctx := context.Background()

	for _, call := range []struct {
		name string
		call func() error
	}{
		{"EnsureSession", func() error {
			_, err := activities.EnsureSession(ctx, SessionRequest{BotID: "bot-1"})
			return err
		}},
		{"StopSession", func() error {
			return activities.StopSession(ctx, SessionRequest{BotID: "bot-1"})
		}},
		{"Screenshot", func() error {
			_, err := activities.Screenshot(ctx, SessionRequest{BotID: "bot-1"})
			return err
		}},
		{"Click", func() error {
			return activities.Click(ctx, ClickRequest{BotID: "bot-1", X: 1, Y: 2})
		}},
		{"Type", func() error {
			return activities.Type(ctx, TypeRequest{BotID: "bot-1", Text: "halo"})
		}},
		{"Navigate", func() error {
			return activities.Navigate(ctx, NavigateRequest{BotID: "bot-1", URL: "http://example.com/"})
		}},
		{"Exec", func() error {
			_, err := activities.Exec(ctx, ExecRequest{BotID: "bot-1", Command: "echo hi"})
			return err
		}},
	} {
		t.Run(call.name, func(t *testing.T) {
			err := call.call()

			// The sentinel is still in the chain here: the wrapping keeps its cause, so
			// a caller on this side of the activity boundary can branch on it. Only the
			// type and the text cross into the workflow, which is why the workflow's own
			// test asserts on the message.
			require.ErrorIs(t, err, domain.ErrNotConfigured)

			var application *sdktemporal.ApplicationError
			require.ErrorAs(t, err, &application)
			require.True(t, application.NonRetryable(),
				"a configuration mistake must not be retried, got %v", err)
		})
	}
}

// TestAnUnconfiguredDeploymentIsNotRetried: the same classification, one layer up. A
// deployment with no control plane builds no client at all, and the activities still have to
// answer with the non-retryable error rather than a bare sentinel — that path never goes
// through the HTTP client.
func TestAnUnconfiguredDeploymentIsNotRetried(t *testing.T) {
	activities := New(Deps{})

	_, err := activities.EnsureSession(context.Background(), SessionRequest{BotID: "bot-1"})

	require.ErrorIs(t, err, domain.ErrNotConfigured)
	var application *sdktemporal.ApplicationError
	require.ErrorAs(t, err, &application)
	require.True(t, application.NonRetryable())
}

// TestFailedNavigationIsNotRetried is the difference between a page that is not there
// and a control plane that is not answering. The URL already failed once; the retry
// policy would run the same navigation again and leave the browser on the same error
// page, so the activity answers non-retryable and the run reports the URL's own reason.
func TestFailedNavigationIsNotRetried(t *testing.T) {
	cause := fmt.Errorf("%w: navigation failed (HTTP 404)", domain.ErrNavigationFailed)
	activities := New(Deps{Computer: stubClient{err: cause}})

	err := activities.Navigate(context.Background(), NavigateRequest{
		BotID: "bot-1",
		URL:   "http://vcomputer-fixture:8080/tidak-ada-404",
	})

	require.ErrorIs(t, err, domain.ErrNavigationFailed)

	var application *sdktemporal.ApplicationError
	require.ErrorAs(t, err, &application)
	require.True(t, application.NonRetryable(),
		"a navigation that did not load must not be retried, got %v", err)
	require.Equal(t, navigationFailedType, application.Type())
	// The control plane's sentence travels: the 404 is what tells an operator which URL
	// was missing rather than which sandbox was broken.
	require.Contains(t, err.Error(), "404")
}

// TestTransientFailuresStayRetryable is the other side of the same decision: an unreachable
// control plane and a request the control plane refused are not configuration, so the retry
// policy keeps them. Marking everything non-retryable would trade one bug for another — a
// sandbox that is not up yet would never be asked twice.
func TestTransientFailuresStayRetryable(t *testing.T) {
	for _, err := range []error{
		fmt.Errorf("%w: dial tcp 127.0.0.1:8090: connect: connection refused", domain.ErrUnavailable),
		fmt.Errorf("%w: the scheme is not one of the four", domain.ErrInvalidInput),
		fmt.Errorf("%w: no session for that bot", domain.ErrNoSession),
	} {
		t.Run(err.Error(), func(t *testing.T) {
			activities := New(Deps{Computer: stubClient{err: err}})

			_, got := activities.EnsureSession(context.Background(), SessionRequest{BotID: "bot-1"})

			require.ErrorIs(t, got, err)
			var application *sdktemporal.ApplicationError
			if errors.As(got, &application) {
				require.False(t, application.NonRetryable(), "%v must stay retryable", got)
			}
		})
	}
}
