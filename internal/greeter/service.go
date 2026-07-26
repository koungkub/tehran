// Package greeter is the greeter bounded context.
//
// It is split so that domain logic stays independent of any transport:
//   - service.go holds the transport-agnostic domain logic (this file).
//   - rpc.go adapts the Service to the generated ConnectRPC interface.
//
// A future queue/stream adapter (e.g. the consumer command) would live in its
// own file here and call the same Service, so the domain logic is written once.
package greeter

import (
	"context"
	"log/slog"
)

// Service is the greeter domain logic. It has no knowledge of transports
// (ConnectRPC, message consumers, …); adapters translate to and from it.
type Service struct {
	log *slog.Logger
}

// NewService builds the greeter domain service.
func NewService(log *slog.Logger) *Service {
	return &Service{log: log}
}

// Greet returns a greeting for name, defaulting to "world" when empty.
func (s *Service) Greet(_ context.Context, name string) (string, error) {
	if name == "" {
		name = "world"
	}
	return "Hello, " + name + "!", nil
}
