package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/JDinSeattle/quorum-market/internal/obs"
)

// maxRequestBody caps how much of a request body is read.
//
// Without a cap an unauthenticated client can make a service allocate until it
// dies simply by streaming. Every body in this system is a small JSON
// document, so 1MiB is generous by three orders of magnitude.
const maxRequestBody = 1 << 20

// JSON writes v with the given status.
func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status line is already sent, so this cannot be turned into an
		// error response; recording it is all that is left.
		slog.Error("encoding response failed", "err", err)
	}
}

// errorBody is the single shape every error response takes, so a client never
// has to guess where the message is.
type errorBody struct {
	Error     string `json:"error"`
	RequestID string `json:"requestId,omitempty"`
}

// WriteError renders err at its associated status.
//
// Internal errors are deliberately not echoed to the caller: an unwrapped
// error can carry a connection string, an internal hostname, or a stack
// detail. The caller gets a generic message plus the request id, which is
// enough to find the real cause in the logs.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	requestID := obs.RequestIDFrom(r.Context())

	var apiErr *APIError
	if errors.As(err, &apiErr) {
		if apiErr.Status >= 500 {
			slog.ErrorContext(r.Context(), "request failed",
				"path", r.URL.Path, "status", apiErr.Status, "err", err)
			JSON(w, apiErr.Status, errorBody{Error: apiErr.Msg, RequestID: requestID})
			return
		}
		JSON(w, apiErr.Status, errorBody{Error: apiErr.Msg, RequestID: requestID})
		return
	}

	slog.ErrorContext(r.Context(), "unhandled error", "path", r.URL.Path, "err", err)
	JSON(w, http.StatusInternalServerError, errorBody{
		Error:     "internal error",
		RequestID: requestID,
	})
}

// DecodeJSON reads a request body into v.
//
// Unknown fields are rejected so a misspelled key fails loudly instead of
// silently defaulting to zero — the difference between a 400 and an order
// quietly placed for the wrong quantity.
func DecodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxRequestBody))
	dec.DisallowUnknownFields()

	if err := dec.Decode(v); err != nil {
		return Wrap(http.StatusBadRequest, err, "malformed JSON body")
	}
	return nil
}

// Handler is an http.HandlerFunc that may return an error.
type Handler func(http.ResponseWriter, *http.Request) error

// H adapts a Handler into an http.HandlerFunc.
func H(h Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			WriteError(w, r, err)
		}
	}
}
