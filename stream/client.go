package stream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/entigolabs/entigo-infralib-agent/model"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	pingInterval      = 4 * time.Minute
	initialBackoff    = 1 * time.Second
	maxBackoff        = 30 * time.Second
	logBufferSize     = 4096
	messageBufferSize = 64
	maxBatchSize      = 1024
	windDownTimeout   = 5 * time.Second
	handshakeTimeout  = 10 * time.Second
)

// Codec builds the request messages of a concrete log stream RPC and handles
// its responses.
type Codec[Req, Resp any] interface {
	Handshake() *Req
	LogBatch(lines []string) *Req
	Ping() *Req
	Complete(exitCode int, errMsg string) *Req
	IsAck(resp *Resp) bool
	// Handle is called from the receive goroutine for every response after the
	// handshake ack. Must not block.
	Handle(resp *Resp)
}

type BidiStream[Req, Resp any] = grpc.BidiStreamingClient[Req, Resp]

type Opener[Req, Resp any] func(ctx context.Context, cc grpc.ClientConnInterface) (BidiStream[Req, Resp], error)

// Client forwards log lines and one-off messages to the backend over a
// bidirectional stream, reconnecting with backoff when the stream breaks.
type Client[Req, Resp any] struct {
	ctx        context.Context
	cancel     context.CancelFunc
	conn       *grpc.ClientConn
	name       string
	open       Opener[Req, Resp]
	codec      Codec[Req, Resp]
	pingTime   time.Duration
	pingTicker *time.Ticker

	logs     chan string
	messages chan *Req
	done     chan struct{}
	finished chan error

	// pendingLogs / pendingMsg hold what was consumed from its channel but not
	// yet successfully Sent. Set before Send, cleared after success; on a broken
	// stream they survive into the next epoch and get replayed on the
	// freshly-opened stream so reconnect doesn't drop them.
	pendingLogs []string
	pendingMsg  *Req

	droppedLogs atomic.Uint64

	closeOnce sync.Once

	exitCode int
	execErr  error
}

func New[Req, Resp any](api *model.NotificationApi, insecure bool, name string, open Opener[Req, Resp], codec Codec[Req, Resp]) (*Client[Req, Resp], error) {
	// Internal ctx is detached from the caller's ctx so that SIGINT cancelling
	// the caller doesn't tear down the stream mid-flight — Disconnect still needs
	// a live stream to deliver the completion message.
	ctx, cancel := context.WithCancel(context.Background())
	conn, err := dial(ctx, api, insecure)
	if err != nil {
		cancel()
		return nil, err
	}
	return newClient(ctx, cancel, conn, name, open, codec), nil
}

func newClient[Req, Resp any](ctx context.Context, cancel context.CancelFunc, conn *grpc.ClientConn, name string, open Opener[Req, Resp], codec Codec[Req, Resp]) *Client[Req, Resp] {
	return &Client[Req, Resp]{
		ctx:        ctx,
		cancel:     cancel,
		conn:       conn,
		name:       name,
		open:       open,
		codec:      codec,
		pingTime:   pingInterval,
		pingTicker: time.NewTicker(pingInterval),
		logs:       make(chan string, logBufferSize),
		messages:   make(chan *Req, messageBufferSize),
		done:       make(chan struct{}),
		finished:   make(chan error, 1),
	}
}

// Connect opens the stream, sends the handshake, and starts the supervisor
// that owns reconnect logic. Must be called exactly once, instead of Start.
func (c *Client[Req, Resp]) Connect() error {
	stream, streamCancel, err := c.openStream()
	if err != nil {
		c.cancel()
		_ = c.conn.Close()
		return err
	}
	go c.supervise(stream, streamCancel)
	return nil
}

// Start opens the stream in the background, retrying with backoff. Logs and
// messages queued before the stream opens are delivered once it does. Must be
// called exactly once, instead of Connect.
func (c *Client[Req, Resp]) Start() {
	go c.supervise(nil, nil)
}

// SendLog queues a log line. Non-blocking, drops the line when the buffer is full.
func (c *Client[Req, Resp]) SendLog(line string) {
	select {
	case c.logs <- line:
	default:
		c.droppedLogs.Add(1)
	}
}

// Send queues a one-off message that is replayed after a reconnect and
// delivered before the completion message. Non-blocking.
func (c *Client[Req, Resp]) Send(msg *Req) {
	select {
	case c.messages <- msg:
	default:
		slog.Warn("Stream message buffer full, dropping", "stream", c.name)
	}
}

// Disconnect delivers the completion message on the current stream and shuts
// down. The ctx bounds how long to wait for the supervisor to wind down.
// Releases the underlying connection regardless of outcome.
func (c *Client[Req, Resp]) Disconnect(ctx context.Context, exitCode int, execErr error) error {
	if dropped := c.droppedLogs.Load(); dropped > 0 {
		slog.Warn("Stream dropped log lines due to backpressure", "stream", c.name, "count", dropped)
	}
	c.exitCode = exitCode
	c.execErr = execErr
	c.closeOnce.Do(func() { close(c.done) })
	defer func() {
		c.cancel()
		_ = c.conn.Close()
	}()
	select {
	case err := <-c.finished:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client[Req, Resp]) supervise(stream BidiStream[Req, Resp], streamCancel context.CancelFunc) {
	defer c.pingTicker.Stop()
	var ok bool
	if stream == nil {
		if stream, streamCancel, ok = c.reconnect(0); !ok {
			return
		}
	}
	c.pingTicker.Reset(c.pingTime)

	for {
		recvErrCh := make(chan error, 1)
		recvDone := make(chan struct{})
		go c.runReceive(stream, recvErrCh, recvDone)

		epochErr := c.runEpoch(stream, recvErrCh)
		if epochErr == nil {
			err := c.sendComplete(stream)
			_ = stream.CloseSend()
			// A successful Send only means the message was buffered; the server closing
			// the stream cleanly is what confirms it received the completion.
			select {
			case <-recvDone:
				if recvErr := <-recvErrCh; err == nil && !errors.Is(recvErr, io.EOF) {
					err = fmt.Errorf("completion not confirmed by the server: %w", recvErr)
				}
			case <-time.After(windDownTimeout):
				if err == nil {
					err = fmt.Errorf("completion not confirmed by the server within %s", windDownTimeout)
				}
			}
			streamCancel()
			c.finished <- err
			return
		}

		slog.Warn("Stream broken, reconnecting", "stream", c.name, "err", epochErr)
		// Cancelling unblocks Recv if the server keeps a stream open that can no longer be sent on.
		streamCancel()
		<-recvDone

		if stream, streamCancel, ok = c.reconnect(initialBackoff); !ok {
			return
		}
		c.pingTicker.Reset(c.pingTime)
	}
}

// reconnect opens a new stream, retrying with backoff. After Disconnect it makes
// one last attempt so pending messages and the completion can still be delivered.
// Returns false after reporting the outcome to finished.
func (c *Client[Req, Resp]) reconnect(delay time.Duration) (BidiStream[Req, Resp], context.CancelFunc, bool) {
	backoffDur := initialBackoff
	for {
		select {
		case <-c.done:
			stream, streamCancel, err := c.openStream()
			if err != nil {
				c.finished <- fmt.Errorf("stream unavailable, completion not delivered: %w", err)
				return nil, nil, false
			}
			return stream, streamCancel, true
		case <-time.After(delay):
		}
		delay = backoffDur
		backoffDur = min(backoffDur*2, maxBackoff)

		stream, streamCancel, err := c.openStream()
		if err == nil {
			return stream, streamCancel, true
		}
		if code := status.Code(err); code == codes.Unimplemented || code == codes.InvalidArgument {
			slog.Error("Stream rejected by backend, won't retry", "stream", c.name, "err", err)
			c.finished <- err
			return nil, nil, false
		}
		slog.Error("Stream connect failed", "stream", c.name, "err", err)
	}
}

func (c *Client[Req, Resp]) runEpoch(stream BidiStream[Req, Resp], recvErrCh <-chan error) error {
	if err := c.flushPending(stream); err != nil {
		return err
	}
	for {
		select {
		case line := <-c.logs:
			if err := c.sendLogBatch(stream, c.drainLogBatch(line)); err != nil {
				return err
			}
		case msg := <-c.messages:
			if err := c.sendMessage(stream, msg); err != nil {
				return err
			}
		case <-c.pingTicker.C:
			if err := stream.Send(c.codec.Ping()); err != nil {
				return err
			}
		case err := <-recvErrCh:
			return err
		case <-c.done:
			return c.drainPending(stream)
		}
	}
}

// drainPending flushes anything Disconnect's caller pushed before signalling
// done. Callers can rely on "anything sent before Disconnect arrives at the
// server before the completion message".
func (c *Client[Req, Resp]) drainPending(stream BidiStream[Req, Resp]) error {
	for {
		select {
		case line := <-c.logs:
			if err := c.sendLogBatch(stream, c.drainLogBatch(line)); err != nil {
				return err
			}
		case msg := <-c.messages:
			if err := c.sendMessage(stream, msg); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

// drainLogBatch pulls everything currently sitting in c.logs into a batch
// starting with the line that already woke the select. Caps at maxBatchSize
// to bound message size; if more remain, the next select iteration picks them
// up.
func (c *Client[Req, Resp]) drainLogBatch(first string) []string {
	batch := make([]string, 0, maxBatchSize)
	batch = append(batch, first)
	for len(batch) < maxBatchSize {
		select {
		case more := <-c.logs:
			batch = append(batch, more)
		default:
			return batch
		}
	}
	return batch
}

func (c *Client[Req, Resp]) sendLogBatch(stream BidiStream[Req, Resp], lines []string) error {
	c.pendingLogs = lines
	if err := stream.Send(c.codec.LogBatch(lines)); err != nil {
		return err
	}
	c.pendingLogs = nil
	c.pingTicker.Reset(c.pingTime)
	return nil
}

func (c *Client[Req, Resp]) sendMessage(stream BidiStream[Req, Resp], msg *Req) error {
	c.pendingMsg = msg
	if err := stream.Send(msg); err != nil {
		return err
	}
	c.pendingMsg = nil
	c.pingTicker.Reset(c.pingTime)
	return nil
}

func (c *Client[Req, Resp]) flushPending(stream BidiStream[Req, Resp]) error {
	if len(c.pendingLogs) > 0 {
		if err := c.sendLogBatch(stream, c.pendingLogs); err != nil {
			return err
		}
	}
	if c.pendingMsg != nil {
		if err := c.sendMessage(stream, c.pendingMsg); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client[Req, Resp]) runReceive(stream BidiStream[Req, Resp], recvErrCh chan<- error, recvDone chan<- struct{}) {
	defer close(recvDone)
	for {
		resp, err := stream.Recv()
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				slog.Info("Stream listener stopped due to context cancellation.", "stream", c.name)
			} else if errors.Is(err, io.EOF) {
				slog.Debug("Server closed the stream gracefully.", "stream", c.name)
			} else {
				slog.Error("Stream listener stopped with an unexpected error", "stream", c.name, "error", err)
			}
			recvErrCh <- err
			return
		}
		c.codec.Handle(resp)
	}
}

func (c *Client[Req, Resp]) openStream() (BidiStream[Req, Resp], context.CancelFunc, error) {
	slog.Debug("Opening log stream...", "stream", c.name)
	streamCtx, streamCancel := context.WithCancel(c.ctx)
	stream, err := c.open(streamCtx, c.conn)
	if err != nil {
		streamCancel()
		return nil, nil, fmt.Errorf("failed to open log stream: %w", err)
	}

	// Cancel streamCtx if the handshake exchange stalls, so the stuck Send/Recv
	// returns instead of blocking forever. Stopped on success below.
	handshakeTimer := time.AfterFunc(handshakeTimeout, streamCancel)
	defer handshakeTimer.Stop()

	if err := stream.Send(c.codec.Handshake()); err != nil {
		streamCancel()
		return nil, nil, fmt.Errorf("failed to send handshake: %w", err)
	}
	resp, err := stream.Recv()
	if err != nil {
		streamCancel()
		return nil, nil, fmt.Errorf("failed to read handshake ack: %w", err)
	}
	if !c.codec.IsAck(resp) {
		streamCancel()
		return nil, nil, fmt.Errorf("expected handshake ack, got %T", resp)
	}
	slog.Info("Successfully connected to backend.", "stream", c.name)
	return stream, streamCancel, nil
}

func (c *Client[Req, Resp]) sendComplete(stream BidiStream[Req, Resp]) error {
	errMsg := ""
	if c.execErr != nil {
		errMsg = c.execErr.Error()
	}
	if err := stream.Send(c.codec.Complete(c.exitCode, errMsg)); err != nil {
		return fmt.Errorf("send complete: %v", err)
	}
	return nil
}
