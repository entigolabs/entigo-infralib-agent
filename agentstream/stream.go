package agentstream

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/gen/wrapper/v1alpha1"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/stream"
	"google.golang.org/grpc"
)

var pingRequest = &v1alpha1.ConnectRequest{
	Payload: &v1alpha1.ConnectRequest_Ping{Ping: &v1alpha1.Ping{}},
}

type client = stream.Client[v1alpha1.ConnectRequest, v1alpha1.ConnectResponse]

// Stream forwards the agent's own logs to the backend and applies approval
// decisions the backend sends back.
type Stream struct {
	client *client
}

func New(api *model.NotificationApi, campaignId string, command common.Command, approvals *Approvals) (*Stream, error) {
	c := &codec{
		approvals: approvals,
		handshake: &v1alpha1.AgentHandshake{
			CampaignId:       campaignId,
			Command:          protoCommand(command),
			Version:          common.GetVersion(),
			HandlesApprovals: approvals != nil,
		},
	}
	streamClient, err := stream.New(api, api.Insecure, "agent", openStream, c)
	if err != nil {
		return nil, err
	}
	c.send = streamClient.Send
	return &Stream{client: streamClient}, nil
}

func openStream(ctx context.Context, cc grpc.ClientConnInterface) (stream.BidiStream[v1alpha1.ConnectRequest, v1alpha1.ConnectResponse], error) {
	return v1alpha1.NewInfralibAgentServiceClient(cc).Connect(ctx)
}

func (s *Stream) Start() {
	s.client.Start()
}

func (s *Stream) SendLog(line string) {
	s.client.SendLog(line)
}

func (s *Stream) Disconnect(ctx context.Context, exitCode int, err error) error {
	return s.client.Disconnect(ctx, exitCode, err)
}

func protoCommand(command common.Command) v1alpha1.AgentCommand {
	switch command {
	case common.RunCommand:
		return v1alpha1.AgentCommand_AGENT_COMMAND_RUN
	case common.UpdateCommand:
		return v1alpha1.AgentCommand_AGENT_COMMAND_UPDATE
	default:
		return v1alpha1.AgentCommand_AGENT_COMMAND_UNSPECIFIED
	}
}

type codec struct {
	send      func(*v1alpha1.ConnectRequest)
	approvals *Approvals
	handshake *v1alpha1.AgentHandshake
}

func (c *codec) Handshake() *v1alpha1.ConnectRequest {
	return &v1alpha1.ConnectRequest{
		Payload: &v1alpha1.ConnectRequest_Handshake{Handshake: c.handshake},
	}
}

func (c *codec) LogBatch(lines []string) *v1alpha1.ConnectRequest {
	return &v1alpha1.ConnectRequest{
		Payload: &v1alpha1.ConnectRequest_LogBatch{LogBatch: &v1alpha1.LogBatch{Lines: lines}},
	}
}

func (c *codec) Ping() *v1alpha1.ConnectRequest {
	return pingRequest
}

func (c *codec) Complete(exitCode int, errMsg string) *v1alpha1.ConnectRequest {
	return &v1alpha1.ConnectRequest{
		Payload: &v1alpha1.ConnectRequest_Complete{
			Complete: &v1alpha1.ExecutionComplete{ExitCode: int32(exitCode), Error: errMsg},
		},
	}
}

func (c *codec) IsAck(resp *v1alpha1.ConnectResponse) bool {
	return resp.GetHandshakeAck() != nil
}

func (c *codec) Handle(resp *v1alpha1.ConnectResponse) {
	switch payload := resp.GetPayload().(type) {
	case *v1alpha1.ConnectResponse_ApprovalRequest:
		go c.decide(payload.ApprovalRequest)
	case *v1alpha1.ConnectResponse_Complete:
		slog.Debug("Server reported stream complete", "total_received", payload.Complete.GetTotalReceived())
	}
}

func (c *codec) decide(req *v1alpha1.ApprovalRequest) {
	var result *v1alpha1.ApprovalResult
	if c.approvals == nil {
		result = &v1alpha1.ApprovalResult{
			ApprovalId: req.GetApprovalId(),
			Outcome:    v1alpha1.ApprovalOutcome_APPROVAL_OUTCOME_NOT_WAITING,
			Error:      "agent does not handle approvals",
		}
	} else {
		result = c.approvals.Decide(req)
	}
	if result.GetOutcome() != v1alpha1.ApprovalOutcome_APPROVAL_OUTCOME_SUCCESS {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Approval %s for step %s not applied: %s %s",
			req.GetApprovalId(), req.GetStep(), result.GetOutcome(), result.GetError())))
	}
	c.send(&v1alpha1.ConnectRequest{
		Payload: &v1alpha1.ConnectRequest_ApprovalResult{ApprovalResult: result},
	})
}
