package model

import "fmt"

type Error struct {
	Status  int
	Code    string
	Message string
	Details any
}

type ConflictDetail struct {
	Layer   string `json:"layer,omitempty"`
	Parent  string `json:"parent,omitempty"`
	Reason  string `json:"reason"`
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }
func Invalid(format string, args ...any) error {
	return &Error{Status: 400, Code: "invalid", Message: fmt.Sprintf(format, args...)}
}
func Conflict(message string) error { return &Error{Status: 409, Code: "conflict", Message: message} }
func Missing(message string) error  { return &Error{Status: 404, Code: "missing", Message: message} }
func Gone(message string) error     { return &Error{Status: 410, Code: "gone", Message: message} }
func Busy(message string) error     { return &Error{Status: 429, Code: "capacity", Message: message} }
func RevisionConflict(expected, current int, details []ConflictDetail) error {
	return &Error{
		Status:  409,
		Code:    "conflict",
		Message: fmt.Sprintf("scene changed since revision %d; %d conflict(s) prevent merging", expected, len(details)),
		Details: map[string]any{"expectedRevision": expected, "currentRevision": current, "conflicts": details},
	}
}
