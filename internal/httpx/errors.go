// Package httpx holds the HTTP plumbing every service shares: an error type
// that carries a status, JSON helpers, middleware, a router that instruments
// itself, a resilient client, and a server that drains before it stops.
package httpx

import (
	"errors"
	"fmt"
	"net/http"
)

// APIError is an error that knows the HTTP status it should surface as.
//
// Service code returns these and the handler wrapper renders them. When one
// comes back from a downstream call it carries *that* call's status, which is
// how the cart service tells "out of stock" (409) apart from "card declined"
// (402) without parsing response bodies.
type APIError struct {
	Status int
	Msg    string
	Err    error
}

func (e *APIError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%d: %s: %v", e.Status, e.Msg, e.Err)
	}
	return fmt.Sprintf("%d: %s", e.Status, e.Msg)
}

func (e *APIError) Unwrap() error { return e.Err }

// Errorf builds an APIError with a formatted message.
func Errorf(status int, format string, args ...any) *APIError {
	return &APIError{Status: status, Msg: fmt.Sprintf(format, args...)}
}

// Wrap builds an APIError that retains a cause for logging.
func Wrap(status int, err error, format string, args ...any) *APIError {
	return &APIError{Status: status, Msg: fmt.Sprintf(format, args...), Err: err}
}

// StatusOf reports the status an error should surface as, defaulting to 500
// for anything that is not an APIError.
func StatusOf(err error) int {
	if err == nil {
		return http.StatusOK
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status
	}
	return http.StatusInternalServerError
}

// IsStatus reports whether err carries a particular status.
func IsStatus(err error, status int) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == status
}
