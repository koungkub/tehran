package greeter

import (
	"context"
	"log/slog"
	"testing"
)

func TestGreet(t *testing.T) {
	svc := NewService(slog.New(slog.DiscardHandler))

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
