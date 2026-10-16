package temporal

import (
	"context"
	"errors"
	"fmt"
	"testing"

	sdktemporal "go.temporal.io/sdk/temporal"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/vcomputer/domain"
)

// navigateStub is the control plane as the Navigate activity sees it: one method, and
// the error the test scripted for it.
//
// The embedded interface is nil on purpose. This package has one activity worth testing
// on its own — the one that classifies — and a second full implementation of
// domain.ComputerUseClient would be eight methods of noise that the compiler checks
// nothing about. A method the test does not call panics if it is ever called, which is
// the right answer for a double that was never meant to answer it.
type navigateStub struct {
	domain.ComputerUseClient

	err       error
	navigated string
}

func (s *navigateStub) Navigate(_ context.Context, _ string, url string) error {
	s.navigated = url
	return s.err
}

// TestNavigateMakesAFailedNavigationNonRetryable is the one thing this package does with
// an error, and it is what keeps the retry policy off a URL that cannot start working:
// the control plane already answered that the page did not load, so the second and third
// attempts would run the same navigation, spend the same timeout, and end in the same
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
