package greeter

import (
	"context"
	"net/http"

	"connectrpc.com/connect"

	greeterv1 "github.com/koungkub/tehran/gen/proto/greeter/v1"
	"github.com/koungkub/tehran/gen/proto/greeter/v1/greeterv1connect"
)

// Handler adapts the greeter Service to the generated ConnectRPC interface.
// It is the only greeter file that depends on the transport layer.
type Handler struct {
	svc *Service
}

var _ greeterv1connect.GreeterServiceHandler = (*Handler)(nil)

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

func (h *Handler) SayHello(
	ctx context.Context,
	req *connect.Request[greeterv1.SayHelloRequest],
) (*connect.Response[greeterv1.SayHelloResponse], error) {
	greeting, err := h.svc.Greet(ctx, req.Msg.GetName())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&greeterv1.SayHelloResponse{
		Greeting: greeting,
	}), nil
}

// Register mounts the greeter Connect handler on mux and returns the gRPC
// service names it serves. It satisfies server.Module, so the app wires
// greeter in without the server package depending on this one.
func (h *Handler) Register(mux *http.ServeMux, opts ...connect.HandlerOption) []string {
	mux.Handle(greeterv1connect.NewGreeterServiceHandler(h, opts...))
	return []string{greeterv1connect.GreeterServiceName}
}
