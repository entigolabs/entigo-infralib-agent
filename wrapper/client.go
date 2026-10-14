package wrapper

import (
	"context"
	"log/slog"

	"github.com/entigolabs/entigo-infralib-agent/gen/wrapper/v1alpha1"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/stream"
	"google.golang.org/grpc"
)

var pingRequest = &v1alpha1.StreamLogsRequest{
	Payload: &v1alpha1.StreamLogsRequest_Ping{Ping: &v1alpha1.Ping{}},
}

type backendClient struct {
	codec  *codec
	client *stream.Client[v1alpha1.StreamLogsRequest, v1alpha1.StreamLogsResponse]
}

func newBackendClient(api *model.NotificationApi, insecureGRPC bool) (*backendClient, error) {
	c := &codec{}
	client, err := stream.New(api, insecureGRPC, "wrapper", openStream, c)
	if err != nil {
		return nil, err
	}
	return &backendClient{codec: c, client: client}, nil
}

func openStream(ctx context.Context, cc grpc.ClientConnInterface) (stream.BidiStream[v1alpha1.StreamLogsRequest, v1alpha1.StreamLogsResponse], error) {
	return v1alpha1.NewWrapperServiceClient(cc).StreamLogs(ctx)
}

func (b *backendClient) Connect(h HandshakeData) error {
	b.codec.handshake = h
	return b.client.Connect()
}

func (b *backendClient) SendLog(line string) error {
	b.client.SendLog(line)
	return nil
}

func (b *backendClient) SendPlan(summary *v1alpha1.PlanSummary) error {
	b.client.Send(&v1alpha1.StreamLogsRequest{
		Payload: &v1alpha1.StreamLogsRequest_PlanSummary{PlanSummary: summary},
	})
	return nil
}

func (b *backendClient) Disconnect(ctx context.Context, exitCode int, execErr error) error {
	return b.client.Disconnect(ctx, exitCode, execErr)
}

type codec struct {
	handshake HandshakeData
}

func (c *codec) Handshake() *v1alpha1.StreamLogsRequest {
	return &v1alpha1.StreamLogsRequest{
		Payload: &v1alpha1.StreamLogsRequest_Handshake{
			Handshake: &v1alpha1.Handshake{
				CampaignId:    c.handshake.CampaignId,
				Step:          c.handshake.Step,
				Command:       c.handshake.Command,
				StepType:      c.handshake.StepType,
				PipelineIndex: c.handshake.PipelineIndex,
			},
		},
	}
}

func (c *codec) LogBatch(lines []string) *v1alpha1.StreamLogsRequest {
	return &v1alpha1.StreamLogsRequest{
		Payload: &v1alpha1.StreamLogsRequest_LogBatch{LogBatch: &v1alpha1.LogBatch{Lines: lines}},
	}
}

func (c *codec) Ping() *v1alpha1.StreamLogsRequest {
	return pingRequest
}

func (c *codec) Complete(exitCode int, errMsg string) *v1alpha1.StreamLogsRequest {
	return &v1alpha1.StreamLogsRequest{
		Payload: &v1alpha1.StreamLogsRequest_Complete{
			Complete: &v1alpha1.ExecutionComplete{ExitCode: int32(exitCode), Error: errMsg},
		},
	}
}

func (c *codec) IsAck(resp *v1alpha1.StreamLogsResponse) bool {
	return resp.GetHandshakeAck() != nil
}

func (c *codec) Handle(resp *v1alpha1.StreamLogsResponse) {
	if complete := resp.GetComplete(); complete != nil {
		slog.Debug("Server reported stream complete", "total_received", complete.GetTotalReceived())
	}
}
