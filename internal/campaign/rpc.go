package campaign

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	campaignv1 "github.com/koungkub/isfahan/gen/go/proto/campaign/v1"
	"github.com/koungkub/isfahan/gen/go/proto/campaign/v1/campaignv1connect"
)

// This file is the only one in the package that depends on the transport, on the
// generated types, or on how a domain error becomes a status code. Two handlers
// over one Service: the proto splits campaigns and events into two services
// because they are two resources, while the rules that relate them are one
// domain's.

// Handler adapts Service to the generated CampaignService interface.
type Handler struct {
	svc *Service
}

var _ campaignv1connect.CampaignServiceHandler = (*Handler)(nil)

// NewHandler wraps svc in the ConnectRPC adapter for campaigns.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// Register mounts the campaign Connect handler on mux and returns the gRPC service
// names it serves, satisfying connectrpc.Module structurally — this package never
// imports the server, so the server never names this domain.
func (h *Handler) Register(mux *http.ServeMux, opts ...connect.HandlerOption) []string {
	mux.Handle(campaignv1connect.NewCampaignServiceHandler(h, opts...))
	return []string{campaignv1connect.CampaignServiceName}
}

// CreateCampaign implements the generated handler by delegating to the domain
// Service.
func (h *Handler) CreateCampaign(
	ctx context.Context,
	req *connect.Request[campaignv1.CreateCampaignRequest],
) (*connect.Response[campaignv1.CreateCampaignResponse], error) {
	msg := req.Msg
	c, err := h.svc.CreateCampaign(ctx, CreateCampaignInput{
		Name:        msg.GetName(),
		Description: msg.GetDescription(),
		Status:      statusFromProto(msg.GetStatus()),
		StartAt:     timeFromProto(msg.GetStartAt()),
		EndAt:       timeFromProto(msg.GetEndAt()),
	})
	if err != nil {
		return nil, h.connectError(ctx, err)
	}
	return connect.NewResponse(&campaignv1.CreateCampaignResponse{
		Campaign: campaignToProto(c),
	}), nil
}

// GetCampaign implements the generated handler.
func (h *Handler) GetCampaign(
	ctx context.Context,
	req *connect.Request[campaignv1.GetCampaignRequest],
) (*connect.Response[campaignv1.GetCampaignResponse], error) {
	c, err := h.svc.GetCampaign(ctx, req.Msg.GetId())
	if err != nil {
		return nil, h.connectError(ctx, err)
	}
	return connect.NewResponse(&campaignv1.GetCampaignResponse{
		Campaign: campaignToProto(c),
	}), nil
}

// ListCampaigns implements the generated handler, returning one keyset page.
func (h *Handler) ListCampaigns(
	ctx context.Context,
	req *connect.Request[campaignv1.ListCampaignsRequest],
) (*connect.Response[campaignv1.ListCampaignsResponse], error) {
	items, next, err := h.svc.ListCampaigns(ctx, int(req.Msg.GetPageSize()), req.Msg.GetPageToken())
	if err != nil {
		return nil, h.connectError(ctx, err)
	}
	out := make([]*campaignv1.Campaign, 0, len(items))
	for i := range items {
		out = append(out, campaignToProto(&items[i]))
	}
	return connect.NewResponse(&campaignv1.ListCampaignsResponse{
		Campaigns:     out,
		NextPageToken: next,
	}), nil
}

// UpdateCampaign implements the generated handler. Only the fields the request
// carries are applied; see the note on presence below.
func (h *Handler) UpdateCampaign(
	ctx context.Context,
	req *connect.Request[campaignv1.UpdateCampaignRequest],
) (*connect.Response[campaignv1.UpdateCampaignResponse], error) {
	msg := req.Msg
	in := UpdateCampaignInput{
		ID:           msg.GetId(),
		Name:         msg.Name,
		Description:  msg.Description,
		StartAt:      timeFromProto(msg.GetStartAt()),
		EndAt:        timeFromProto(msg.GetEndAt()),
		ClearStartAt: msg.GetClearStartAt(),
		ClearEndAt:   msg.GetClearEndAt(),
	}
	// Presence, not value: an explicit CAMPAIGN_STATUS_UNSPECIFIED is a bad
	// argument, while an absent field means "leave the status alone". Only the
	// optional wrapper can tell those apart.
	if msg.Status != nil {
		s := statusFromProto(msg.GetStatus())
		in.Status = &s
	}
	c, err := h.svc.UpdateCampaign(ctx, in)
	if err != nil {
		return nil, h.connectError(ctx, err)
	}
	return connect.NewResponse(&campaignv1.UpdateCampaignResponse{
		Campaign: campaignToProto(c),
	}), nil
}

// DeleteCampaign implements the generated handler, reporting the events the
// cascade removed with the campaign.
func (h *Handler) DeleteCampaign(
	ctx context.Context,
	req *connect.Request[campaignv1.DeleteCampaignRequest],
) (*connect.Response[campaignv1.DeleteCampaignResponse], error) {
	deletedEvents, err := h.svc.DeleteCampaign(ctx, req.Msg.GetId())
	if err != nil {
		return nil, h.connectError(ctx, err)
	}
	return connect.NewResponse(&campaignv1.DeleteCampaignResponse{
		DeletedEvents: deletedEvents,
	}), nil
}

func (h *Handler) connectError(ctx context.Context, err error) error {
	return toConnectError(ctx, h.svc.log, err)
}

// EventHandler adapts Service to the generated EventService interface.
type EventHandler struct {
	svc *Service
}

var _ campaignv1connect.EventServiceHandler = (*EventHandler)(nil)

// NewEventHandler wraps svc in the ConnectRPC adapter for events.
func NewEventHandler(svc *Service) *EventHandler {
	return &EventHandler{svc: svc}
}

// Register mounts the event Connect handler on mux and returns the gRPC service
// names it serves.
func (h *EventHandler) Register(mux *http.ServeMux, opts ...connect.HandlerOption) []string {
	mux.Handle(campaignv1connect.NewEventServiceHandler(h, opts...))
	return []string{campaignv1connect.EventServiceName}
}

// CreateEvent implements the generated handler. A missing parent campaign is a
// failed precondition here rather than a not-found — see below.
func (h *EventHandler) CreateEvent(
	ctx context.Context,
	req *connect.Request[campaignv1.CreateEventRequest],
) (*connect.Response[campaignv1.CreateEventResponse], error) {
	msg := req.Msg
	payload, err := payloadFromProto(msg.GetPayload())
	if err != nil {
		return nil, h.connectError(ctx, err)
	}
	e, err := h.svc.CreateEvent(ctx, CreateEventInput{
		CampaignID: msg.GetCampaignId(),
		Name:       msg.GetName(),
		Type:       msg.GetType(),
		Payload:    payload,
		OccurredAt: timeFromProto(msg.GetOccurredAt()),
	})
	if err != nil {
		// A create against a missing campaign is a precondition failure, not a
		// not-found: the resource the caller addressed is the collection of
		// events, which exists. Mapped here rather than in toConnectError
		// because the same sentinel is a not-found on the read paths.
		if errors.Is(err, ErrCampaignNotFound) {
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		}
		return nil, h.connectError(ctx, err)
	}
	out, err := eventToProto(e)
	if err != nil {
		return nil, h.connectError(ctx, err)
	}
	return connect.NewResponse(&campaignv1.CreateEventResponse{Event: out}), nil
}

// GetEvent implements the generated handler.
func (h *EventHandler) GetEvent(
	ctx context.Context,
	req *connect.Request[campaignv1.GetEventRequest],
) (*connect.Response[campaignv1.GetEventResponse], error) {
	e, err := h.svc.GetEvent(ctx, req.Msg.GetId())
	if err != nil {
		return nil, h.connectError(ctx, err)
	}
	out, err := eventToProto(e)
	if err != nil {
		return nil, h.connectError(ctx, err)
	}
	return connect.NewResponse(&campaignv1.GetEventResponse{Event: out}), nil
}

// ListEvents implements the generated handler, returning one keyset page of a
// single campaign's events.
func (h *EventHandler) ListEvents(
	ctx context.Context,
	req *connect.Request[campaignv1.ListEventsRequest],
) (*connect.Response[campaignv1.ListEventsResponse], error) {
	items, next, err := h.svc.ListEvents(ctx,
		req.Msg.GetCampaignId(), int(req.Msg.GetPageSize()), req.Msg.GetPageToken())
	if err != nil {
		return nil, h.connectError(ctx, err)
	}
	out := make([]*campaignv1.Event, 0, len(items))
	for i := range items {
		e, convErr := eventToProto(&items[i])
		if convErr != nil {
			return nil, h.connectError(ctx, convErr)
		}
		out = append(out, e)
	}
	return connect.NewResponse(&campaignv1.ListEventsResponse{
		Events:        out,
		NextPageToken: next,
	}), nil
}

// UpdateEvent implements the generated handler. There is no way to reparent an
// event through it.
func (h *EventHandler) UpdateEvent(
	ctx context.Context,
	req *connect.Request[campaignv1.UpdateEventRequest],
) (*connect.Response[campaignv1.UpdateEventResponse], error) {
	msg := req.Msg
	payload, err := payloadFromProto(msg.GetPayload())
	if err != nil {
		return nil, h.connectError(ctx, err)
	}
	e, err := h.svc.UpdateEvent(ctx, UpdateEventInput{
		ID:         msg.GetId(),
		Name:       msg.Name,
		Type:       msg.Type,
		Payload:    payload,
		OccurredAt: timeFromProto(msg.GetOccurredAt()),
	})
	if err != nil {
		return nil, h.connectError(ctx, err)
	}
	out, err := eventToProto(e)
	if err != nil {
		return nil, h.connectError(ctx, err)
	}
	return connect.NewResponse(&campaignv1.UpdateEventResponse{Event: out}), nil
}

// DeleteEvent implements the generated handler.
func (h *EventHandler) DeleteEvent(
	ctx context.Context,
	req *connect.Request[campaignv1.DeleteEventRequest],
) (*connect.Response[campaignv1.DeleteEventResponse], error) {
	if err := h.svc.DeleteEvent(ctx, req.Msg.GetId()); err != nil {
		return nil, h.connectError(ctx, err)
	}
	return connect.NewResponse(&campaignv1.DeleteEventResponse{}), nil
}

func (h *EventHandler) connectError(ctx context.Context, err error) error {
	return toConnectError(ctx, h.svc.log, err)
}

// toConnectError maps a domain error to a status code, in one place so that no
// handler invents its own.
//
// Anything unrecognised is CodeInternal carrying a fixed message: an error from
// the database can quote a statement, a column, or the row that collided, and
// none of that belongs in a reply to a caller. It is logged here instead, where
// it joins the request's trace.
func toConnectError(ctx context.Context, log *zerolog.Logger, err error) error {
	switch {
	case errors.Is(err, ErrInvalidArgument):
		return connect.NewError(connect.CodeInvalidArgument, err)
	// On the read paths, a missing parent and a missing row are both "not found";
	// CreateEvent overrides this with FailedPrecondition before getting here.
	case errors.Is(err, ErrCampaignNotFound), errors.Is(err, ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, context.Canceled):
		// The client hung up. Reported as canceled rather than internal so it does
		// not show up as this service's own failure.
		return connect.NewError(connect.CodeCanceled, err)
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, err)
	}
	log.Error().Ctx(ctx).Err(err).Msg("campaign request failed")
	return connect.NewError(connect.CodeInternal, errors.New("internal error"))
}

// campaignToProto converts an entity to its wire form.
func campaignToProto(c *Campaign) *campaignv1.Campaign {
	return &campaignv1.Campaign{
		Id:          c.ID,
		Name:        c.Name,
		Description: c.Description,
		Status:      statusToProto(c.Status),
		StartAt:     timeToProto(c.StartAt),
		EndAt:       timeToProto(c.EndAt),
		CreatedAt:   timestamppb.New(c.CreatedAt),
		UpdatedAt:   timestamppb.New(c.UpdatedAt),
	}
}

// eventToProto converts an entity to its wire form. It can fail where
// campaignToProto cannot: the payload is opaque bytes on this side and a
// structpb.Struct on the other, so a document that is not a JSON object — a bare
// array or number, which the column accepts and Struct cannot hold — is reported
// rather than dropped.
func eventToProto(e *Event) (*campaignv1.Event, error) {
	payload, err := payloadToProto(e.Payload)
	if err != nil {
		return nil, err
	}
	return &campaignv1.Event{
		Id:         e.ID,
		CampaignId: e.CampaignID,
		Name:       e.Name,
		Type:       e.Type,
		Payload:    payload,
		OccurredAt: timestamppb.New(e.OccurredAt),
		CreatedAt:  timestamppb.New(e.CreatedAt),
		UpdatedAt:  timestamppb.New(e.UpdatedAt),
	}, nil
}

func statusToProto(s Status) campaignv1.CampaignStatus {
	switch s {
	case StatusDraft:
		return campaignv1.CampaignStatus_CAMPAIGN_STATUS_DRAFT
	case StatusActive:
		return campaignv1.CampaignStatus_CAMPAIGN_STATUS_ACTIVE
	case StatusPaused:
		return campaignv1.CampaignStatus_CAMPAIGN_STATUS_PAUSED
	case StatusEnded:
		return campaignv1.CampaignStatus_CAMPAIGN_STATUS_ENDED
	}
	return campaignv1.CampaignStatus_CAMPAIGN_STATUS_UNSPECIFIED
}

// statusFromProto maps the enum to a domain Status. UNSPECIFIED becomes the empty
// Status, which create reads as "default to draft" and update rejects — the
// distinction lives with those callers, not here.
func statusFromProto(s campaignv1.CampaignStatus) Status {
	switch s {
	case campaignv1.CampaignStatus_CAMPAIGN_STATUS_DRAFT:
		return StatusDraft
	case campaignv1.CampaignStatus_CAMPAIGN_STATUS_ACTIVE:
		return StatusActive
	case campaignv1.CampaignStatus_CAMPAIGN_STATUS_PAUSED:
		return StatusPaused
	case campaignv1.CampaignStatus_CAMPAIGN_STATUS_ENDED:
		return StatusEnded
	case campaignv1.CampaignStatus_CAMPAIGN_STATUS_UNSPECIFIED:
		return ""
	}
	return ""
}

// timeToProto renders an optional timestamp, keeping unset as unset rather than as
// the epoch.
func timeToProto(t *time.Time) *timestamppb.Timestamp {
	if t == nil {
		return nil
	}
	return timestamppb.New(*t)
}

// timeFromProto reads an optional timestamp. A nil message and one that fails
// validation both mean "not supplied": timestamppb carries seconds and nanos that
// need not be in range, and AsTime on an invalid one returns a date far outside
// anything a column will hold.
func timeFromProto(ts *timestamppb.Timestamp) *time.Time {
	if ts == nil || !ts.IsValid() {
		return nil
	}
	t := ts.AsTime().UTC()
	return &t
}

// errInvalidPayload classes a payload conversion failure as a bad argument, which
// is what it is on the write paths. A stored document that cannot be rendered back
// could only have been written by something other than this service — the column
// accepts any JSON, while Struct holds only an object — and it reaches the caller
// as an invalid argument rather than as an internal error for that reason.
func errInvalidPayload(err error) error {
	return fmt.Errorf("%w: payload: %w", ErrInvalidArgument, err)
}

// payloadFromProto flattens a Struct into the bytes stored in the jsonb column. A
// nil Struct yields nil, which the service reads as "no payload supplied" — on
// update that means "leave it alone", so clearing a payload is done by sending an
// empty object rather than by omitting the field.
func payloadFromProto(s *structpb.Struct) (json.RawMessage, error) {
	if s == nil {
		return nil, nil
	}
	b, err := s.MarshalJSON()
	if err != nil {
		// Only reachable from a Struct holding a value protojson cannot render,
		// such as a NaN. It is the caller's input, so it is their error.
		return nil, errInvalidPayload(err)
	}
	return b, nil
}

// payloadToProto parses stored bytes back into a Struct. Absent or empty is nil
// rather than an empty object, so a payload that was never set does not come back
// looking like one that was set to {}.
func payloadToProto(p json.RawMessage) (*structpb.Struct, error) {
	if len(p) == 0 {
		return nil, nil
	}
	s := &structpb.Struct{}
	if err := s.UnmarshalJSON(p); err != nil {
		return nil, errInvalidPayload(err)
	}
	return s, nil
}
