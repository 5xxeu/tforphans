package model

// ResponseError marks a response that could not be decoded or validated.
// Its message is unchanged so callers can keep their usual error reporting.
type ResponseError struct{ Err error }

func (e *ResponseError) Error() string { return e.Err.Error() }
func (e *ResponseError) Unwrap() error { return e.Err }
