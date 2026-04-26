// Package channels defines the adapter interface and the three concrete
// implementations: email (SMTP), Slack (webhook), in-app (DB insert).
package channels

import (
	"context"
	"errors"
	"fmt"
)

type Message struct {
	UserID        string
	ToEmail       string
	ToSlackURL    string
	Subject       string
	BodyText      string
	BodyHTML      string
	TemplateName  string
	NotificationID string
}

type Result struct {
	ProviderID string
}

// Adapter is the single interface every channel implements. Errors are
// classified via apperr so the worker knows whether to retry or DLQ.
type Adapter interface {
	Name() string
	Send(ctx context.Context, msg Message) (Result, error)
}

// Registry maps channel name -> Adapter. Kept simple on purpose; no
// plugin system, no reflection.
type Registry struct {
	byName map[string]Adapter
}

func NewRegistry(adapters ...Adapter) *Registry {
	r := &Registry{byName: make(map[string]Adapter, len(adapters))}
	for _, a := range adapters {
		r.byName[a.Name()] = a
	}
	return r
}

func (r *Registry) Get(name string) (Adapter, error) {
	a, ok := r.byName[name]
	if !ok {
		return nil, fmt.Errorf("unknown channel %q", name)
	}
	return a, nil
}

var ErrMissingDestination = errors.New("recipient address is missing for this channel")
