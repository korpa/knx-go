package knx

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/vapourismo/knx-go/knx/cemi"
	"github.com/vapourismo/knx-go/knx/proto"
)

// TunnelConfig allows you to configure the client's behavior.
type TunnelConfig struct {
	// ResendInterval is how long to wait for a response, until the request is resend. A interval
	// <= 0 can't be used. The default value will be used instead.
	ResendInterval time.Duration

	// HeartbeatInterval specifies the time which has to elapse without any incoming communication,
	// until a heartbeat is triggered. A delay <= 0 will result in the use of a default value.
	HeartbeatInterval time.Duration

	// ResponseTimeout specifies how long to wait for a response. A timeout <= 0 will not be
	// accepted. Instead, the default value will be used.
	ResponseTimeout time.Duration
}

// Default configuration elements
var (
	defaultResendInterval    = 500 * time.Millisecond
	defaultHeartbeatInterval = 10 * time.Second
	defaultResponseTimeout   = 10 * time.Second

	DefaultTunnelConfig = TunnelConfig{
		defaultResendInterval,
		defaultHeartbeatInterval,
		defaultResponseTimeout,
	}
)

// checkTunnelConfig makes sure that the configuration is actually usable.
func checkTunnelConfig(config TunnelConfig) TunnelConfig {
	if config.ResendInterval <= 0 {
		config.ResendInterval = defaultResendInterval
	}

	if config.HeartbeatInterval <= 0 {
		config.HeartbeatInterval = defaultHeartbeatInterval
	}

	if config.ResponseTimeout <= 0 {
		config.ResponseTimeout = defaultResponseTimeout
	}

	return config
}

// tunnelConn is a handle for a tunnel connection.
type tunnelConn struct {
	sock      Socket
	config    TunnelConfig
	channel   uint8
	control   proto.HostInfo
	seqMu     sync.Mutex
	seqNumber uint8
	ack       chan *proto.TunnelRes
	inbound   chan cemi.Message
}

// requestConn repeatedly sends a connection request through the socket until the provided context gets
// canceled, or a response is received. A response that renders the gateway as busy will not stop
// requestConn.
func (conn *tunnelConn) requestConn(ctx context.Context) (err error) {
	req := &proto.ConnReq{Layer: proto.TunnelLayerData}

	// Send the initial request.
	err = conn.sock.Send(req)
	if err != nil {
		return
	}

	// Create a resend timer.
	ticker := time.NewTicker(conn.config.ResendInterval)
	defer ticker.Stop()

	// Cycle until a request gets a response.
	for {
		select {
		// Termination has been requested.
		case <-ctx.Done():
			return ctx.Err()

		// Resend timer triggered.
		case <-ticker.C:
			err = conn.sock.Send(req)
			if err != nil {
				return
			}

		// A message has been received or the channel has been closed.
		case msg, open := <-conn.sock.Inbound():
			if !open {
				return errors.New("Socket's inbound channel has been closed")
			}

			// We're only interested in connection responses.
			if res, ok := msg.(*proto.ConnRes); ok {
				switch res.Status {
				// Conection has been established.
				case proto.ConnResOk:
					conn.channel = res.Channel
					conn.control = res.Control

					conn.seqMu.Lock()
					defer conn.seqMu.Unlock()
					conn.seqNumber = 0

					return nil

				// The gateway is busy, but we don't stop yet.
				case proto.ConnResBusy:
					continue

				// Connection request has been denied.
				default:
					return res.Status
				}
			}
		}
	}
}

// requestConnState periodically sends a connection state request to the gateway until it has
// received a response or the context is done.
func (conn *tunnelConn) requestConnState(
	ctx context.Context,
	heartbeat <-chan proto.ConnState,
) (proto.ConnState, error) {
	req := &proto.ConnStateReq{Channel: conn.channel, Status: 0, Control: proto.HostInfo{}}

	// Send first connection state request
	err := conn.sock.Send(req)
	if err != nil {
		return proto.ConnStateInactive, err
	}

	// Start the resend timer.
	ticker := time.NewTicker(conn.config.ResendInterval)
	defer ticker.Stop()

	for {
		select {
		// Termination has been requested.
		case <-ctx.Done():
			return proto.ConnStateInactive, ctx.Err()

		// Resend timer fired.
		case <-ticker.C:
			err := conn.sock.Send(req)
			if err != nil {
				return proto.ConnStateInactive, err
			}

		// Received a connection state response.
		case res, open := <-heartbeat:
			if !open {
				return proto.ConnStateInactive, errors.New("Connection server has terminated")
			}

			return res, nil
		}
	}
}

// requestDisc sends a disconnect request to the gateway.
func (conn *tunnelConn) requestDisc() error {
	return conn.sock.Send(&proto.DiscReq{
		Channel: conn.channel,
		Status:  0,
		Control: conn.control,
	})
}

// requestTunnel sends a tunnel request to the gateway and waits for an appropriate acknowledgement.
func (conn *tunnelConn) requestTunnel(
	ctx context.Context,
	data cemi.Message,
) error {
	// Sequence numbers cannot be reused, therefore we must protect against that.
	conn.seqMu.Lock()
	defer conn.seqMu.Unlock()

	req := &proto.TunnelReq{
		Channel:   conn.channel,
		SeqNumber: conn.seqNumber,
		Payload:   data,
	}

	// Send initial request.
	err := conn.sock.Send(req)
	if err != nil {
		return err
	}

	// Start the resend timer.
	ticker := time.NewTicker(conn.config.ResendInterval)
	defer ticker.Stop()

	for {
		select {
		// Termination has been requested.
		case <-ctx.Done():
			return ctx.Err()

		// Resend timer fired.
		case <-ticker.C:
			err := conn.sock.Send(req)
			if err != nil {
				return err
			}

		// Received a tunnel response.
		case res, open := <-conn.ack:
			if !open {
				return errors.New("Connection server has terminated")
			}

			// Ignore mismatching sequence numbers.
			if res.SeqNumber != conn.seqNumber {
				continue
			}

			// Gateway has received the request, therefore we can increase on our side.
			conn.seqNumber++

			// Check if the response confirms the tunnel request.
			if res.Status == 0 {
				return nil
			}

			return fmt.Errorf("Tunnel request has been rejected with status %#x", res.Status)
		}
	}
}

// performHeartbeat uses requestState to determine if the gateway is still alive.
func (conn *tunnelConn) performHeartbeat(
	ctx context.Context,
	heartbeat <-chan proto.ConnState,
	timeout chan<- struct{},
) {
	// Setup a child context which will time out with the given heartbeat timeout.
	childCtx, cancel := context.WithTimeout(ctx, conn.config.ResponseTimeout)
	defer cancel()

	// Request the connction state.
	state, err := conn.requestConnState(childCtx, heartbeat)
	if err != nil || state != proto.ConnStateNormal {
		if err != nil {
			log(conn, "conn", "Error while requesting connection state: %v", err)
		} else {
			log(conn, "conn", "Bad connection state: %v", state)
		}

		// Write to timeout as an indication that the heartbeat has failed.
		select {
		case <-ctx.Done():
		case timeout <- struct{}{}:
		}
	}
}

// handleDiscReq validates the request.
func (conn *tunnelConn) handleDiscReq(
	ctx context.Context,
	req *proto.DiscReq,
) error {
	// Validate the request channel.
	if req.Channel != conn.channel {
		return errors.New("Invalid communication channel in disconnect request")
	}

	// We don't need to check if this errors or not. It doesn't matter.
	conn.sock.Send(&proto.DiscRes{Channel: req.Channel, Status: 0})

	return nil
}

// handleDiscRes validates the response.
func (conn *tunnelConn) handleDiscRes(
	ctx context.Context,
	res *proto.DiscRes,
) error {
	// Validate the response channel.
	if res.Channel != conn.channel {
		return errors.New("Invalid communication channel in disconnect response")
	}

	return nil
}

// handleTunnelReq validates the request, pushes the data to the client and acknowledges the
// request for the gateway.
func (conn *tunnelConn) handleTunnelReq(
	ctx context.Context,
	req *proto.TunnelReq,
	seqNumber *uint8,
) error {
	// Validate the request channel.
	if req.Channel != conn.channel {
		return errors.New("Invalid communication channel in tunnel request")
	}

	expected := *seqNumber

	// Is the sequence number what we expected?
	if req.SeqNumber == expected {
		*seqNumber++

		// Send tunnel data to the client.
		go func() {
			select {
			case <-ctx.Done():
			case conn.inbound <- req.Payload:
			}
		}()
	} else if req.SeqNumber != expected-1 {
		// The sequence number is out of the range which we would have to acknowledge.
		return errors.New("Out of sequence tunnel acknowledgement")
	}

	// Send the acknowledgement.
	return conn.sock.Send(&proto.TunnelRes{
		Channel:   conn.channel,
		SeqNumber: req.SeqNumber,
		Status:    0,
	})
}

// handleTunnelRes validates the response and relays it to a sender that is awaiting an
// acknowledgement.
func (conn *tunnelConn) handleTunnelRes(
	ctx context.Context,
	res *proto.TunnelRes,
) error {
	// Validate the request channel.
	if res.Channel != conn.channel {
		return errors.New("Invalid communication channel in connection state response")
	}

	// Send to client.
	go func() {
		select {
		case <-ctx.Done():
		case <-time.After(conn.config.ResendInterval):
		case conn.ack <- res:
		}
	}()

	return nil
}

// handleConnStateRes validates the response and sends it to the heartbeat routine, if
// there is a waiting one.
func (conn *tunnelConn) handleConnStateRes(
	ctx context.Context,
	res *proto.ConnStateRes,
	heartbeat chan<- proto.ConnState,
) error {
	// Validate the request channel.
	if res.Channel != conn.channel {
		return errors.New("Invalid communication channel in connection state response")
	}

	// Send connection state to the heartbeat goroutine.
	go func() {
		select {
		case <-ctx.Done():
		case <-time.After(conn.config.ResendInterval):
		case heartbeat <- res.Status:
		}
	}()

	return nil
}

var (
	errHeartbeatFailed = errors.New("Heartbeat did not succeed")
	errInboundClosed   = errors.New("Socket's inbound channel is closed")
	errDisconnected    = errors.New("Gateway terminated the connection")
)

// process processes incoming packets.
func (conn *tunnelConn) process(ctx context.Context) error {
	heartbeat := make(chan proto.ConnState)
	defer close(heartbeat)

	timeout := make(chan struct{})

	var seqNumber uint8

	heartbeatInterval := time.NewTicker(conn.config.HeartbeatInterval)
	defer heartbeatInterval.Stop()

	for {
		select {
		// Termination has been requested.
		case <-ctx.Done():
			return ctx.Err()

		// Heartbeat worker signals a result.
		case <-timeout:
			return errHeartbeatFailed

		// Heartbeat check is due.
		case <-heartbeatInterval.C:
			go conn.performHeartbeat(ctx, heartbeat, timeout)

		// A message has been received or the channel is closed.
		case msg, open := <-conn.sock.Inbound():
			if !open {
				return errInboundClosed
			}

			// Determine what to do with the message.
			switch msg := msg.(type) {
			case *proto.DiscReq:
				err := conn.handleDiscReq(ctx, msg)
				if err == nil {
					return errDisconnected
				}

				log(conn, "conn", "Error while handling disconnect request %v: %v", msg, err)

			case *proto.DiscRes:
				err := conn.handleDiscRes(ctx, msg)
				if err == nil {
					return nil
				}

				log(conn, "conn", "Error while handling disconnect response %v: %v", msg, err)

			case *proto.TunnelReq:
				err := conn.handleTunnelReq(ctx, msg, &seqNumber)
				if err != nil {
					log(conn, "conn", "Error while handling tunnel request %v: %v", msg, err)
				}

			case *proto.TunnelRes:
				err := conn.handleTunnelRes(ctx, msg)
				if err != nil {
					log(conn, "conn", "Error while handling tunnel response %v: %v", msg, err)
				}

			case *proto.ConnStateRes:
				err := conn.handleConnStateRes(ctx, msg, heartbeat)
				if err != nil {
					log(
						conn, "conn",
						"Error while handling connection state response: %v", err,
					)
				}
			}
		}
	}
}

// serve serves the tunnel connection. It can sustain certain failures. This function will try to
// reconnect in case of a heartbeat failure or disconnect.
func (conn *tunnelConn) serve(ctx context.Context) (err error) {
	defer close(conn.ack)
	defer close(conn.inbound)

	for {
		err = conn.process(ctx)
		log(conn, "conn", "Server terminated with error: %v", err)

		// Check if we can try again.
		if err == errDisconnected || err == errHeartbeatFailed {
			log(conn, "conn", "Attempting reconnect")

			reconnCtx, cancelReconn := context.WithTimeout(ctx, conn.config.ResponseTimeout)
			reconnErr := conn.requestConn(reconnCtx)
			cancelReconn()

			if reconnErr == nil {
				log(conn, "conn", "Reconnect succeeded")
				continue
			}

			log(conn, "conn", "Reconnect failed: %v", reconnErr)
		}

		return
	}
}

// Tunnel represents the client endpoint in a connection with a gateway.
type Tunnel struct {
	tunnelConn

	ctx    context.Context
	cancel context.CancelFunc
}

// NewTunnel establishes a connection with a gateway. You can pass a zero initialized ClientConfig;
// the function will take care of filling in the default values.
func NewTunnel(gatewayAddr string, config TunnelConfig) (*Tunnel, error) {
	// Create socket which will be used for communication.
	sock, err := NewTunnelSocket(gatewayAddr)
	if err != nil {
		return nil, err
	}

	// Prepare a context for the inbound server.
	ctx, cancel := context.WithCancel(context.Background())

	// Initialize the Client structure.
	client := &Tunnel{
		tunnelConn: tunnelConn{
			sock:    sock,
			config:  checkTunnelConfig(config),
			ack:     make(chan *proto.TunnelRes),
			inbound: make(chan cemi.Message),
		},
		ctx:    ctx,
		cancel: cancel,
	}

	// Prepare a context, so that the connection request cannot run forever.
	connectCtx, cancelConnect := context.WithTimeout(ctx, client.config.ResponseTimeout)
	defer cancelConnect()

	// Connect to the gateway.
	err = client.requestConn(connectCtx)
	if err != nil {
		sock.Close()
		return nil, err
	}

	go client.serve(client.ctx)

	return client, nil
}

// Close will terminate the connection.
func (client *Tunnel) Close() {
	client.requestDisc()
	client.cancel()
	client.sock.Close()
}

// Inbound retrieves the channel which transmits incoming data.
func (client *Tunnel) Inbound() <-chan cemi.Message {
	return client.inbound
}

// Send relays a tunnel request to the gateway with the given contents.
func (client *Tunnel) Send(data cemi.Message) error {
	// Prepare a context, so that we won't wait forever for a tunnel response.
	ctx, cancel := context.WithTimeout(client.ctx, client.config.ResponseTimeout)
	defer cancel()

	// Send the tunnel reqest.
	err := client.requestTunnel(ctx, data)
	if err != nil {
		return err
	}

	return nil
}