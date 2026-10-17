package temporal

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	sdktemporal "go.temporal.io/sdk/temporal"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/vcomputer/domain"
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

// navigateStub is the control plane as the Navigate activity sees it: one method, and
// the error the test scripted for it.
//
// The embedded interface is nil on purpose. Navigate is the one activity whose
// classification needs a double of its own — one route, one scripted answer — and a second
// full implementation of domain.ComputerUseClient would be eight methods of noise that the
// compiler checks nothing about. A method the test does not call panics if it is ever
// called, which is the right answer for a double that was never meant to answer it.
type navigateStub struct {
	domain.ComputerUseClient

	err       error
	navigated string
}

func (s *navigateStub) Navigate(_ context.Context, _ string, url string) error {
	s.navigated = url
	return s.err
}

// TestNavigateMakesAFailedNavigationNonRetryable is the Navigate half of the classification
// this package does, and it is what keeps the retry policy off a URL that cannot start
// working: the control plane already answered that the page did not load, so the second and
// third attempts would run the same navigation, spend the same timeout, and end in the same
// place — while leaving the browser on an error page each time.
//
// The assertion is on the SDK's own view of the error rather than on the value the
// activity returned, because the SDK is what decides whether to retry: NonRetryable is
// the field the worker reads, and a plain wrapped error would carry the sentinel and
// still be retried.
func TestNavigateMakesAFailedNavigationNonRetryable(t *testing.T) {
	stub := &navigateStub{err: fmt.Errorf("%w: navigation failed: the page answered HTTP 404", domain.ErrNavigationFailed)}
	activities := New(Deps{Computer: stub})

	err := activities.Navigate(t.Context(), NavigateRequest{BotID: "bot-a", URL: "http://vcomputer-fixture:8080/tidak-ada-404"})
	if err == nil {
		t.Fatal("navigate: want an error")
	}
	if stub.navigated != "http://vcomputer-fixture:8080/tidak-ada-404" {
		t.Fatalf("navigated = %q, want the url the caller asked for", stub.navigated)
	}

	var applicationError *sdktemporal.ApplicationError
	if !errors.As(err, &applicationError) {
		t.Fatalf("err = %v, want an *ApplicationError", err)
	}
	if !applicationError.NonRetryable() {
		t.Fatalf("err = %v, want it non-retryable: the same URL answers the same way", err)
	}
	if applicationError.Type() != navigationFailedType {
		t.Fatalf("type = %q, want %q", applicationError.Type(), navigationFailedType)
	}
	if !errors.Is(err, domain.ErrNavigationFailed) {
		t.Fatalf("err = %v, want the sentinel in its chain", err)
	}
	// The control plane's sentence has to survive: it is what a person reads in the
	// workflow history, and the 404 is in it.
	if message := applicationError.Message(); message == "" {
		t.Fatal("the failure carries no message")
	}
}

// TestNavigateLeavesOtherFailuresAlone is the other half: an error that is not the
// control plane's verdict on a URL — a refused request, a missing session, an
// unreachable control plane — must come back as it arrived, so the workflow's retry
// policy keeps working on the failures that may pass.
func TestNavigateLeavesOtherFailuresAlone(t *testing.T) {
	for _, want := range []error{domain.ErrUnavailable, domain.ErrNoSession, domain.ErrInvalidInput} {
		stub := &navigateStub{err: want}
		activities := New(Deps{Computer: stub})

		err := activities.Navigate(t.Context(), NavigateRequest{BotID: "bot-a", URL: "http://vcomputer-fixture:8080/"})
		if !errors.Is(err, want) {
			t.Fatalf("err = %v, want %v", err, want)
		}
		var applicationError *sdktemporal.ApplicationError
		if errors.As(err, &applicationError) {
			t.Fatalf("err = %v, want it not to be wrapped: %v may pass, and the retry policy is what decides", err, want)
		}
	}
}
