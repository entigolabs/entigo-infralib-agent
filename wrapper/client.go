package wrapper

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/entigolabs/entigo-infralib-agent/gen/wrapper/v1alpha1"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	pingInterval     = 4 * time.Minute
	initialBackoff   = 1 * time.Second
	maxBackoff       = 30 * time.Second
	logBufferSize    = 256
	windDownTimeout  = 5 * time.Second
	handshakeTimeout = 10 * time.Second
)

var pingRequest = &v1alpha1.StreamLogsRequest{
	Payload: &v1alpha1.StreamLogsRequest_Ping{Ping: &v1alpha1.Ping{}},
}

type wrapperStream = grpc.BidiStreamingClient[v1alpha1.StreamLogsRequest, v1alpha1.StreamLogsResponse]

type backendClient struct {
	ctx        context.Context
	cancel     context.CancelFunc
	client     v1alpha1.WrapperServiceClient
	conn       *grpc.ClientConn
	pingTime   time.Duration
	pingTicker *time.Ticker

	handshake HandshakeData

	logs        chan string
	planSummary chan *v1alpha1.PlanSummary
	done        chan struct{}
	finished    chan error

	// pendingLog / pendingPlan hold the single item that has been consumed
	// from its channel but not yet successfully Sent. Set before Send, cleared
	// after success; on a broken stream they survive into the next epoch and
	// get replayed on the freshly-opened stream so reconnect doesn't drop them.
	// Single-item (not a slice) because runEpoch consumes one item per select.
	pendingLog  *string
	pendingPlan *v1alpha1.PlanSummary

	closeOnce sync.Once

	exitCode int
	execErr  error
}

func newBackendClient(_ context.Context, api *model.WrapperApi) (*backendClient, error) {
	host, pathPrefix, err := parseTarget(api.URL)
	if err != nil {
		return nil, err
	}
	// Internal ctx is detached from the caller's ctx so that SIGINT cancelling
	// the wrapper's ctx doesn't tear down the gRPC stream mid-flight — Disconnect
	// still needs a live stream to deliver ExecutionComplete.
	internalCtx, cancel := context.WithCancel(context.Background())
	ts, err := util.GetTokenSource(internalCtx, api.OAuth)
	if err != nil {
		cancel()
		return nil, err
	}

	interceptors := []grpc.StreamClientInterceptor{
		NewAuthInterceptor(ts, api.Headers).StreamClient(),
	}
	if pathPrefix != "" {
		interceptors = append(interceptors, pathPrefixInterceptor(pathPrefix))
	}
	dialOpts := []grpc.DialOption{
		grpc.WithChainStreamInterceptor(interceptors...),
	}
	if api.Insecure {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	} else {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{})))
	}

	conn, err := grpc.NewClient(host, dialOpts...)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create grpc client: %v", err)
	}

	return &backendClient{
		ctx:         internalCtx,
		cancel:      cancel,
		client:      v1alpha1.NewWrapperServiceClient(conn),
		conn:        conn,
		pingTime:    pingInterval,
		pingTicker:  time.NewTicker(pingInterval),
		logs:        make(chan string, logBufferSize),
		planSummary: make(chan *v1alpha1.PlanSummary, 1),
		done:        make(chan struct{}),
		finished:    make(chan error, 1),
	}, nil
}

func (g *backendClient) Connect(h HandshakeData) error {
	g.handshake = h
	stream, err := g.openStream()
	if err != nil {
		g.cancel()
		_ = g.conn.Close()
		return err
	}
	go g.supervise(stream)
	return nil
}

func (g *backendClient) SendLog(line string) error {
	select {
	case g.logs <- line:
	default:
		slog.Debug("wrapper log buffer full, dropping line")
	}
	return nil
}

// Buffer size 1 — there's only ever one plan summary per execution.
func (g *backendClient) SendPlan(summary *v1alpha1.PlanSummary) error {
	select {
	case g.planSummary <- summary:
	default:
		slog.Warn("wrapper plan summary buffer full, dropping")
	}
	return nil
}

func (g *backendClient) Disconnect(ctx context.Context, exitCode int, execErr error) error {
	g.exitCode = exitCode
	g.execErr = execErr
	g.closeOnce.Do(func() { close(g.done) })
	defer func() {
		g.cancel()
		_ = g.conn.Close()
	}()
	select {
	case err := <-g.finished:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *backendClient) supervise(initialStream wrapperStream) {
	defer g.pingTicker.Stop()
	stream := initialStream
	g.pingTicker.Reset(g.pingTime)

	for {
		recvErrCh := make(chan error, 1)
		recvDone := make(chan struct{})
		go g.runReceive(stream, recvErrCh, recvDone)

		epochErr := g.runEpoch(stream, recvErrCh)
		if epochErr == nil {
			err := g.sendComplete(stream)
			_ = stream.CloseSend()
			select {
			case <-recvDone:
			case <-time.After(windDownTimeout):
				slog.Warn("wrapper recv goroutine wind-down timed out")
			}
			g.finished <- err
			return
		}

		slog.Warn("wrapper stream broken, reconnecting", "err", epochErr)
		<-recvDone

		newStream, ok := g.reconnect()
		if !ok {
			return
		}
		stream = newStream
		g.pingTicker.Reset(g.pingTime)
	}
}

func (g *backendClient) reconnect() (wrapperStream, bool) {
	backoffDur := initialBackoff
	for {
		select {
		case <-g.done:
			g.finished <- nil
			return nil, false
		case <-time.After(backoffDur):
		}
		backoffDur = min(backoffDur*2, maxBackoff)

		stream, err := g.openStream()
		if err != nil {
			slog.Error("wrapper reconnect failed", "err", err)
			continue
		}
		return stream, true
	}
}

func (g *backendClient) runEpoch(stream wrapperStream, recvErrCh <-chan error) error {
	if err := g.flushPending(stream); err != nil {
		return err
	}
	for {
		select {
		case line := <-g.logs:
			if err := g.sendLogLine(stream, line); err != nil {
				return err
			}
		case summary := <-g.planSummary:
			if err := g.sendPlanSummary(stream, summary); err != nil {
				return err
			}
		case <-g.pingTicker.C:
			slog.Debug("Sending ping request to server")
			if err := stream.Send(pingRequest); err != nil {
				return err
			}
		case err := <-recvErrCh:
			return err
		case <-g.done:
			return g.drainPending(stream)
		}
	}
}

// drainPending flushes anything Disconnect's caller pushed before signalling
// done. Callers can rely on "anything sent before Disconnect arrives at the
// server before ExecutionComplete" — important for the plan summary, which
// would otherwise race with the disconnect signal in select.
func (g *backendClient) drainPending(stream wrapperStream) error {
	for {
		select {
		case line := <-g.logs:
			if err := g.sendLogLine(stream, line); err != nil {
				return err
			}
		case summary := <-g.planSummary:
			if err := g.sendPlanSummary(stream, summary); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

func (g *backendClient) sendLogLine(stream wrapperStream, line string) error {
	g.pendingLog = &line
	if err := stream.Send(&v1alpha1.StreamLogsRequest{
		Payload: &v1alpha1.StreamLogsRequest_LogLine{
			LogLine: &v1alpha1.LogLine{Line: line},
		},
	}); err != nil {
		return err
	}
	g.pendingLog = nil
	g.pingTicker.Reset(g.pingTime)
	return nil
}

func (g *backendClient) sendPlanSummary(stream wrapperStream, summary *v1alpha1.PlanSummary) error {
	g.pendingPlan = summary
	if err := stream.Send(&v1alpha1.StreamLogsRequest{
		Payload: &v1alpha1.StreamLogsRequest_PlanSummary{
			PlanSummary: summary,
		},
	}); err != nil {
		return err
	}
	g.pendingPlan = nil
	g.pingTicker.Reset(g.pingTime)
	return nil
}

func (g *backendClient) flushPending(stream wrapperStream) error {
	if g.pendingLog != nil {
		if err := g.sendLogLine(stream, *g.pendingLog); err != nil {
			return err
		}
	}
	if g.pendingPlan != nil {
		if err := g.sendPlanSummary(stream, g.pendingPlan); err != nil {
			return err
		}
	}
	return nil
}

func (g *backendClient) runReceive(stream wrapperStream, recvErrCh chan<- error, recvDone chan<- struct{}) {
	defer close(recvDone)
	for {
		resp, err := stream.Recv()
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				slog.Info("Stream listener stopped due to context cancellation.")
			} else if errors.Is(err, io.EOF) {
				slog.Info("Server closed the stream gracefully.")
			} else {
				slog.Error("Stream listener stopped with an unexpected error", "error", err)
			}
			recvErrCh <- err
			return
		}
		if c := resp.GetComplete(); c != nil {
			slog.Debug("Server reported stream complete", "total_received", c.GetTotalReceived())
		}
	}
}

func (g *backendClient) openStream() (wrapperStream, error) {
	slog.Info("Opening wrapper log stream...")
	streamCtx, streamCancel := context.WithCancel(g.ctx)
	stream, err := g.client.StreamLogs(streamCtx)
	if err != nil {
		streamCancel()
		return nil, fmt.Errorf("failed to open log stream: %v", err)
	}

	// Cancel streamCtx if the handshake exchange stalls, so the stuck Send/Recv
	// returns instead of blocking forever. Stopped on success below.
	handshakeTimer := time.AfterFunc(handshakeTimeout, streamCancel)
	defer handshakeTimer.Stop()

	hs := &v1alpha1.StreamLogsRequest{
		Payload: &v1alpha1.StreamLogsRequest_Handshake{
			Handshake: &v1alpha1.Handshake{
				CampaignId: g.handshake.CampaignId,
				Step:       g.handshake.Step,
				Command:    g.handshake.Command,
				StepType:   g.handshake.StepType,
			},
		},
	}
	if err := stream.Send(hs); err != nil {
		streamCancel()
		return nil, fmt.Errorf("failed to send handshake: %v", err)
	}
	resp, err := stream.Recv()
	if err != nil {
		streamCancel()
		return nil, fmt.Errorf("failed to read handshake ack: %v", err)
	}
	if resp.GetHandshakeAck() == nil {
		streamCancel()
		return nil, fmt.Errorf("expected handshake ack, got %T", resp.GetPayload())
	}
	slog.Info("Successfully connected to backend.")
	return stream, nil
}

func (g *backendClient) sendComplete(stream wrapperStream) error {
	complete := &v1alpha1.StreamLogsRequest{
		Payload: &v1alpha1.StreamLogsRequest_Complete{
			Complete: &v1alpha1.ExecutionComplete{
				ExitCode: int32(g.exitCode),
				Error:    errString(g.execErr),
			},
		},
	}
	if err := stream.Send(complete); err != nil {
		return fmt.Errorf("send complete: %v", err)
	}
	return nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func parseTarget(raw string) (host, pathPrefix string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", errors.New("wrapper api url is empty")
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return "", "", fmt.Errorf("invalid wrapper api url %q: %v", raw, err)
		}
		if u.Host == "" {
			return "", "", fmt.Errorf("wrapper api url %q has no host", raw)
		}
		return u.Host, strings.TrimRight(u.Path, "/"), nil
	}
	if i := strings.Index(raw, "/"); i >= 0 {
		return raw[:i], strings.TrimRight(raw[i:], "/"), nil
	}
	return raw, "", nil
}

func pathPrefixInterceptor(prefix string) grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		return streamer(ctx, desc, cc, prefix+method, opts...)
	}
}
