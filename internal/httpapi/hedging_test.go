package httpapi

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/events"
)

func hedgeTestResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func TestHedgePrimaryWinnerDoesNotWaitForLoserShutdown(t *testing.T) {
	s := &Server{bus: events.New(32)}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	secondaryCanceled := make(chan struct{})
	start := time.Now()
	out := s.hedgedUpstreamDo(
		ctx,
		"req-primary-wins",
		"primary",
		"secondary",
		5*time.Millisecond,
		func(context.Context) (*http.Response, error) {
			time.Sleep(25 * time.Millisecond)
			return hedgeTestResponse("primary"), nil
		},
		func(ctx context.Context) (*http.Response, error) {
			<-ctx.Done()
			close(secondaryCanceled)
			return nil, ctx.Err()
		},
	)
	elapsed := time.Since(start)

	if out.err != nil {
		t.Fatalf("unexpected hedge error: %v", out.err)
	}
	if out.secondaryWon || !out.hedgeLaunched {
		t.Fatalf("unexpected hedge outcome: %+v", out)
	}
	if out.resp == nil {
		t.Fatal("primary winner response is nil")
	}
	defer out.resp.Body.Close()
	if elapsed > 150*time.Millisecond {
		t.Fatalf("primary winner was blocked behind loser shutdown: %s", elapsed)
	}
	select {
	case <-secondaryCanceled:
	case <-time.After(150 * time.Millisecond):
		t.Fatal("secondary loser was not cancelled promptly")
	}
}

func TestHedgeSecondaryWinnerCancelsPrimaryImmediately(t *testing.T) {
	s := &Server{bus: events.New(32)}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	primaryCanceled := make(chan struct{})
	out := s.hedgedUpstreamDo(
		ctx,
		"req-secondary-wins",
		"primary",
		"secondary",
		5*time.Millisecond,
		func(ctx context.Context) (*http.Response, error) {
			<-ctx.Done()
			close(primaryCanceled)
			return nil, ctx.Err()
		},
		func(context.Context) (*http.Response, error) {
			time.Sleep(20 * time.Millisecond)
			return hedgeTestResponse("secondary"), nil
		},
	)

	if out.err != nil {
		t.Fatalf("unexpected hedge error: %v", out.err)
	}
	if !out.secondaryWon || !out.hedgeLaunched {
		t.Fatalf("secondary should have won: %+v", out)
	}
	if out.resp == nil {
		t.Fatal("secondary winner response is nil")
	}
	defer out.resp.Body.Close()
	select {
	case <-primaryCanceled:
	case <-time.After(150 * time.Millisecond):
		t.Fatal("primary loser was not cancelled promptly")
	}
}

func TestHedgeBothFailuresReturnsPrimaryErrorDeterministically(t *testing.T) {
	s := &Server{bus: events.New(32)}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	primaryErr := context.DeadlineExceeded
	secondaryErr := context.Canceled
	out := s.hedgedUpstreamDo(
		ctx,
		"req-both-fail",
		"primary",
		"secondary",
		time.Millisecond,
		func(context.Context) (*http.Response, error) {
			time.Sleep(15 * time.Millisecond)
			return nil, primaryErr
		},
		func(context.Context) (*http.Response, error) {
			time.Sleep(5 * time.Millisecond)
			return nil, secondaryErr
		},
	)
	if out.err != primaryErr {
		t.Fatalf("both failures should report primary error: got %v want %v", out.err, primaryErr)
	}
	if !out.hedgeLaunched {
		t.Fatal("expected hedge to launch")
	}
}
