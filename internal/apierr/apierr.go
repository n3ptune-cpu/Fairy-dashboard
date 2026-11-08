// Package apierr defines the error type that HTTP handlers return to send a client a specific status code.
//
// The web package turns an *Error into a JSON body {"detail": ...} (the same shape FastAPI used in the Python
// prototype), so Detail must be safe to show to the client: no secrets, URLs or internal error text.
package apierr

import (
	"fmt"
	"net/http"
)

// Upstream is the status for a failure that an agent, its BMC or Proxmox reported: they answered, but with an
// error (424 Failed Dependency). Never 502/504 for these: Cloudflare and other proxies replace the body of a
// 502/504 with their own error page, so the user would see "invalid or incomplete response" instead of, say,
// "BMC refused the fan mode". 502 stays for "could not reach it at all".
const Upstream = http.StatusFailedDependency

// Error is a client-facing failure: an HTTP status code plus a JSON-encodable detail (usually a string; the
// step-up response uses an object).
type Error struct {
	Status int
	Detail any
}

// Error formats the status and detail (for logs; clients get the JSON body).
func (e *Error) Error() string { return fmt.Sprintf("%d: %v", e.Status, e.Detail) }

// New returns an *Error with the given status and detail.
func New(status int, detail any) *Error { return &Error{status, detail} }

// Errf returns an *Error whose detail is a formatted string.
func Errf(status int, f string, a ...any) *Error { return &Error{status, fmt.Sprintf(f, a...)} }
