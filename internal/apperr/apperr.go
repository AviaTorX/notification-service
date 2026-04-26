// Package apperr defines typed errors shared by the API, worker, and channel
// adapters. Two concerns matter across layers:
//
//  1. HTTP status mapping at the API boundary.
//  2. Retryable vs permanent classification at the worker boundary so Asynq
//     can decide whether to retry or move to the dead-letter (archived) set.
package apperr

import (
	"errors"
	"fmt"
)

type Kind int

const (
	KindInternal Kind = iota
	KindInvalid
	KindNotFound
	KindConflict
	KindUnauthorized
	KindRateLimited
	KindTransient // retryable at worker layer
	KindPermanent // non-retryable at worker layer
)

type Error struct {
	Kind    Kind
	Message string
	Err     error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Err)
	}
	return e.Message
}

func (e *Error) Unwrap() error { return e.Err }

func New(kind Kind, msg string) *Error { return &Error{Kind: kind, Message: msg} }
func Wrap(kind Kind, msg string, err error) *Error {
	return &Error{Kind: kind, Message: msg, Err: err}
}

func Invalid(msg string) *Error      { return New(KindInvalid, msg) }
func NotFound(msg string) *Error     { return New(KindNotFound, msg) }
func Conflict(msg string) *Error     { return New(KindConflict, msg) }
func Unauthorized(msg string) *Error { return New(KindUnauthorized, msg) }
func RateLimited(msg string) *Error  { return New(KindRateLimited, msg) }
func Transient(msg string, err error) *Error {
	return Wrap(KindTransient, msg, err)
}
func Permanent(msg string, err error) *Error {
	return Wrap(KindPermanent, msg, err)
}

func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return KindInternal
}

func IsRetryable(err error) bool {
	switch KindOf(err) {
	case KindPermanent, KindInvalid, KindNotFound, KindUnauthorized:
		return false
	default:
		return true
	}
}
