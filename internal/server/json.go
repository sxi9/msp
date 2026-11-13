package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/sxi9/msp/internal/protocol"
)

// writeJSON writes v as the response body with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		// A marshal failure is a programming error in a handler; answer 500
		// with the standard error body rather than a half-written response.
		b = []byte(`{"error":"internal","detail":"encode response"}`)
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(b)
}

// writeError writes the standard {"error":code,"detail":detail} body.
func writeError(w http.ResponseWriter, status int, code, detail string) {
	writeJSON(w, status, protocol.ErrorResponse{Error: code, Detail: detail})
}

// readJSON decodes the request body into v. Unknown fields are allowed (so
// newer agents can talk to older servers); trailing data after the first JSON
// value and an empty body are rejected. The error text is safe to return as
// the invalid_request detail.
func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		var maxErr *http.MaxBytesError
		switch {
		case errors.As(err, &maxErr):
			return fmt.Errorf("body exceeds %d bytes", maxErr.Limit)
		case errors.Is(err, io.EOF):
			return errors.New("empty body")
		}
		return fmt.Errorf("invalid JSON: %v", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return errors.New("trailing data after JSON body")
	}
	return nil
}

// pathInt reads a positive integer path value ({id}) from a Go 1.22 pattern.
func pathInt(r *http.Request, name string) (int64, bool) {
	n, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}
