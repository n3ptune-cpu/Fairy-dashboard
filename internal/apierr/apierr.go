// Package apierr: an error that maps to an HTTP status and a safe {"detail": ...} body (FastAPI-compatible).
package apierr

import "fmt"

type Error struct {
	Status int
	Detail string
}

func (e *Error) Error() string { return fmt.Sprintf("%d: %s", e.Status, e.Detail) }

func New(status int, detail string) *Error { return &Error{status, detail} }

func Errf(status int, f string, a ...any) *Error { return &Error{status, fmt.Sprintf(f, a...)} }
