package greeter

import (
	"context"
	"testing"

	"github.com/rs/zerolog"
)

// nopLogger keeps these tests off whatever zerolog's package-level logger
// happens to point at.
var nopLogger = zerolog.Nop()

func TestGreet(t *testing.T) {
	svc := NewService(&nopLogger)

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "with name", in: "tehran", want: "Hello, tehran!"},
		{name: "empty name defaults to world", in: "", want: "Hello, world!"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := svc.Greet(context.Background(), tt.in)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("greeting = %q, want %q", got, tt.want)
			}
		})
	}
}
