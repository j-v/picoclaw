package discord

import (
	"errors"
	"net/http"
	"time"

	"github.com/bwmarrin/discordgo"
)

const (
	// Initial-connection retry policy. A gateway restart can race with the
	// network coming up (for example DNS is not yet reachable at boot), so
	// transient connect failures are retried with capped exponential backoff
	// instead of leaving the bot offline until the process is restarted.
	defaultConnectRetryInitial = 2 * time.Second
	defaultConnectRetryMax     = 60 * time.Second
	connectRetryMultiplier     = 2.0
)

// isPermanentConnectError reports whether a connect failure should not be
// retried. Authentication failures and malformed-token errors are permanent:
// retrying them would hammer the API without ever succeeding and would delay
// surfacing a real configuration problem. Everything else (DNS resolution
// failures, network unreachable, timeouts, 5xx responses) is treated as
// transient, which is the common case when the gateway starts before the
// host has a working network connection.
func isPermanentConnectError(err error) bool {
	if err == nil {
		return false
	}

	var restErr *discordgo.RESTError
	if errors.As(err, &restErr) && restErr.Response != nil {
		switch restErr.Response.StatusCode {
		case http.StatusUnauthorized, // 401: invalid token
			http.StatusForbidden, // 403: missing intents or permissions
			http.StatusNotFound:  // 404: malformed endpoint
			return true
		}
	}

	// Any other error (DNS lookup failure, connection refused, TLS,
	// websocket handshake) is worth retrying.
	return false
}

// nextConnectRetryBackoff returns the delay to use for the next connect
// attempt, growing by connectRetryMultiplier until it reaches the cap. Capping
// rather than aborting means a long outage is still survived: the bot keeps
// probing once a minute and reconnects shortly after the network returns.
func nextConnectRetryBackoff(current time.Duration) time.Duration {
	if current <= 0 {
		return defaultConnectRetryInitial
	}

	next := time.Duration(float64(current) * connectRetryMultiplier)
	if next > defaultConnectRetryMax {
		return defaultConnectRetryMax
	}

	return next
}
