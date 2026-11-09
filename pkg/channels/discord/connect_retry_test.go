package discord

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

func TestIsPermanentConnectError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil error is not permanent", err: nil, want: false},
		{
			name: "401 invalid token is permanent",
			err: &discordgo.RESTError{
				Response: &http.Response{StatusCode: http.StatusUnauthorized},
			},
			want: true,
		},
		{
			name: "403 missing intents is permanent",
			err: &discordgo.RESTError{
				Response: &http.Response{StatusCode: http.StatusForbidden},
			},
			want: true,
		},
		{
			name: "500 is transient",
			err: &discordgo.RESTError{
				Response: &http.Response{StatusCode: http.StatusInternalServerError},
			},
			want: false,
		},
		{
			name: "429 rate limit is transient",
			err: &discordgo.RESTError{
				Response: &http.Response{StatusCode: http.StatusTooManyRequests},
			},
			want: false,
		},
		{
			name: "RESTError without response is transient",
			err:  &discordgo.RESTError{},
			want: false,
		},
		{
			name: "DNS failure is transient",
			// The exact shape of the boot failure this retry logic exists for.
			err: errors.New("failed to get bot user: dial tcp: lookup discord.com on 192.168.1.1:53: " +
				"dial udp 192.168.1.1:53: connect: network is unreachable"),
			want: false,
		},
		{
			name: "wrapped 401 is still permanent",
			err: fmt.Errorf("failed to get bot user: %w", &discordgo.RESTError{
				Response: &http.Response{StatusCode: http.StatusUnauthorized},
			}),
			want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := isPermanentConnectError(tc.err); got != tc.want {
				t.Errorf("isPermanentConnectError() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNextConnectRetryBackoff(t *testing.T) {
	t.Parallel()

	// Sequence must start at the initial delay, double each time, and saturate
	// at the cap rather than growing without bound.
	want := []time.Duration{
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		32 * time.Second,
		60 * time.Second, // capped from 64s
		60 * time.Second,
		60 * time.Second,
	}

	got := defaultConnectRetryInitial
	for i, expected := range want {
		if got != expected {
			t.Fatalf("attempt %d: backoff = %v, want %v", i, got, expected)
		}
		got = nextConnectRetryBackoff(got)
	}
}

func TestNextConnectRetryBackoffHandlesZero(t *testing.T) {
	t.Parallel()

	// A zero (or negative) current delay must not produce a busy loop.
	if got := nextConnectRetryBackoff(0); got != defaultConnectRetryInitial {
		t.Errorf("nextConnectRetryBackoff(0) = %v, want %v", got, defaultConnectRetryInitial)
	}
	if got := nextConnectRetryBackoff(-time.Second); got != defaultConnectRetryInitial {
		t.Errorf("nextConnectRetryBackoff(-1s) = %v, want %v", got, defaultConnectRetryInitial)
	}
}

// TestConnectRetryLoopStopsOnCancel verifies the retry goroutine exits when the
// channel context is canceled, so Stop() does not leak a goroutine that keeps
// probing Discord after shutdown.
func TestConnectRetryLoopStopsOnCancel(t *testing.T) {
	t.Parallel()

	ch := &DiscordChannel{}
	ctx, cancel := context.WithCancel(context.Background())
	ch.ctx = ctx

	done := make(chan struct{})
	go func() {
		ch.connectRetryLoop()
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("connectRetryLoop did not exit after context cancellation")
	}
}
