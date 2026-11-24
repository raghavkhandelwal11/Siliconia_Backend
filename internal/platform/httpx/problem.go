// Package httpx holds shared HTTP helpers: RFC 9457 problem responses,
// JSON writing and the middleware chain (LLD 4.1, 4.6).
package httpx

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// Problem is an RFC 9457 problem-details body with a stable error code.
type Problem struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Code      string `json:"code"`
	Detail    string `json:"detail,omitempty"`
	RequestID string `json:"requestId,omitempty"`
}

// Error is an application error that maps to a problem response.
type Error struct {
	Status int
	Code   string
	Title  string
	Detail string
}

func (e *Error) Error() string { return e.Code + ": " + e.Title }

// NewError builds an application error.
func NewError(status int, code, title string) *Error {
	return &Error{Status: status, Code: code, Title: title}
}

// WithDetail returns a copy of the error with a detail message.
func (e *Error) WithDetail(detail string) *Error {
	c := *e
	c.Detail = detail
	return &c
}

// Common errors from the LLD 4.6 catalogue.
var (
	ErrValidation   = NewError(http.StatusBadRequest, "VALIDATION_FAILED", "The request is not valid")
	ErrAuthRequired = NewError(http.StatusUnauthorized, "AUTH_REQUIRED", "Sign in to continue")
	ErrForbidden    = NewError(http.StatusForbidden, "FORBIDDEN_ROLE", "Your account type cannot do this")
	ErrNotFound     = NewError(http.StatusNotFound, "NOT_FOUND", "Not found")
	ErrRateLimited  = NewError(http.StatusTooManyRequests, "RATE_LIMITED", "Too many requests, please wait a moment")
	ErrInternal     = NewError(http.StatusInternalServerError, "INTERNAL", "Something went wrong on our side")
	ErrUnavailable  = NewError(http.StatusServiceUnavailable, "PROVIDER_UNAVAILABLE", "A service we depend on is unavailable")
)

// WriteError writes err as application/problem+json. Unknown errors become 500
// without leaking their message.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	var appErr *Error
	if !errors.As(err, &appErr) {
		appErr = ErrInternal
	}
	p := Problem{
		Type:      "https://siliconics.in/errors/" + slug(appErr.Code),
		Title:     appErr.Title,
		Status:    appErr.Status,
		Code:      appErr.Code,
		Detail:    appErr.Detail,
		RequestID: RequestIDFrom(r.Context()),
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}

// WriteJSON writes v as JSON with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func slug(code string) string {
	return strings.ReplaceAll(strings.ToLower(code), "_", "-")
}
