package natsrouter

import "fmt"

// RouteError is an error that produces a user-facing response.
// When a handler returns a RouteError, the router sends it as the reply
// instead of the generic "internal error". Use this for expected error
// conditions that the client should see (not found, forbidden, validation, etc.).
//
// Any other error returned by a handler is treated as an internal error —
// it is logged and the client receives "internal error".
//
// Example:
//
//	func (s *Service) GetRoom(ctx context.Context, p Params, req GetRoomReq) (*Room, error) {
//	    room, err := s.store.Find(ctx, req.ID)
//	    if err != nil {
//	        return nil, fmt.Errorf("finding room: %w", err) // → "internal error" to client
//	    }
//	    if room == nil {
//	        return nil, natsrouter.Errorf("room %s not found", req.ID) // → sent to client as-is
//	    }
//	    return room, nil
//	}
type RouteError struct {
	Message string `json:"error"`
	Code    string `json:"code,omitempty"`
}

// Error implements the error interface.
func (e *RouteError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return e.Message
}

// Err creates a RouteError with the given message.
// The client receives: {"error": "message"}
func Err(message string) *RouteError {
	return &RouteError{Message: message}
}

// Errf creates a RouteError with a formatted message.
// The client receives: {"error": "formatted message"}
func Errf(format string, args ...any) *RouteError {
	return &RouteError{Message: fmt.Sprintf(format, args...)}
}

// ErrWithCode creates a RouteError with a machine-readable code and message.
// The client receives: {"error": "message", "code": "code"}
//
// Common codes: "not_found", "forbidden", "bad_request", "conflict"
func ErrWithCode(code, message string) *RouteError {
	return &RouteError{Message: message, Code: code}
}

// Standard error codes.
const (
	CodeBadRequest = "bad_request"
	CodeNotFound   = "not_found"
	CodeForbidden  = "forbidden"
	CodeConflict   = "conflict"
	CodeInternal   = "internal"
	// CodeUnavailable signals the service is temporarily over capacity and
	// the caller should retry. Used by the router's admission control when
	// the per-pod handler concurrency cap is reached.
	CodeUnavailable = "unavailable"
)

// ErrBadRequest creates a user-facing bad request error.
func ErrBadRequest(message string) *RouteError { return ErrWithCode(CodeBadRequest, message) }

// ErrNotFound creates a user-facing not found error.
func ErrNotFound(message string) *RouteError { return ErrWithCode(CodeNotFound, message) }

// ErrForbidden creates a user-facing forbidden error.
func ErrForbidden(message string) *RouteError { return ErrWithCode(CodeForbidden, message) }

// ErrConflict creates a user-facing conflict error.
func ErrConflict(message string) *RouteError { return ErrWithCode(CodeConflict, message) }

// ErrInternal creates a user-facing internal error.
func ErrInternal(message string) *RouteError { return ErrWithCode(CodeInternal, message) }

// ErrUnavailable creates a user-facing service-busy error. Returned by the
// router's admission control when the per-pod handler concurrency cap is
// reached. Callers should retry with backoff.
func ErrUnavailable(message string) *RouteError { return ErrWithCode(CodeUnavailable, message) }
