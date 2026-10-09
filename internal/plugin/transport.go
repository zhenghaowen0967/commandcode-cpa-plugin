package plugin

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"commandcode-cpa-plugin/internal/config"
	"commandcode-cpa-plugin/internal/errclass"
)

// executionTransport is owned by ONE attempt. Neither a host callback ACK nor a
// context deadline proves upstream I/O has stopped. Cleanup seals dial admission,
// closes the actual response body and sockets, and joins every admitted dial and
// socket operation before the executor may settle its lease.
//
// A separate HTTP/1 transport deliberately trades pooling for an unambiguous
// cleanup boundary. In particular net/http detaches its dial context from the
// request: our dialer uses the attempt context instead and rejects late sockets.
type executionTransport struct {
	ctx    context.Context
	cancel context.CancelFunc
	http   *http.Transport
	dial   func(context.Context, string, string) (net.Conn, error)

	mu             sync.Mutex
	sealed         bool
	conns          []*executionConn
	body           *executionBody
	requestBody    *executionRequestBody
	closeFailed    bool
	dials          sync.WaitGroup
	watchDone      chan struct{}
	cleanupFailure func()
}

var errExecutionProxyConfig = errors.New("upstream proxy configuration unavailable")

func newExecutionTransport(parent context.Context, timeout time.Duration, proxyURL string, cleanupFailure ...func()) (*executionTransport, error) {
	proxy, err := config.ProxyFunc(proxyURL)
	if err != nil {
		return nil, errExecutionProxyConfig
	}
	dial := (&net.Dialer{Timeout: timeout, KeepAlive: -1}).DialContext

	ctx, cancel := context.WithTimeout(parent, timeout)
	t := &executionTransport{
		ctx: ctx, cancel: cancel,
		dial:      dial,
		watchDone: make(chan struct{}),
	}
	if len(cleanupFailure) > 0 {
		t.cleanupFailure = cleanupFailure[0]
	}
	t.http = &http.Transport{
		Proxy:                  proxy,
		DialContext:            t.dialContext,
		DisableKeepAlives:      true,
		ForceAttemptHTTP2:      false,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}},
		TLSNextProto:           map[string]func(string, *tls.Conn) http.RoundTripper{},
		TLSHandshakeTimeout:    timeout,
		MaxResponseHeaderBytes: 1 << 20,
	}
	go func() {
		defer close(t.watchDone)
		<-ctx.Done()
		t.closeIO()
	}()
	return t, nil
}

func (t *executionTransport) dialContext(_ context.Context, network, address string) (net.Conn, error) {
	t.mu.Lock()
	if t.sealed || t.ctx.Err() != nil {
		t.mu.Unlock()
		return nil, context.Canceled
	}
	t.dials.Add(1)
	t.mu.Unlock()
	defer t.dials.Done()

	conn, err := t.dial(t.ctx, network, address)
	if conn == nil {
		return nil, err
	}
	tracked := &executionConn{Conn: conn, ctx: t.ctx}
	t.mu.Lock()
	// Record even a rejected late connection: failure to close it must retain
	// the lease, and finish must wait for its actual socket operations too.
	t.conns = append(t.conns, tracked)
	late := t.sealed || t.ctx.Err() != nil || err != nil
	t.mu.Unlock()
	if late {
		if tracked.Close() != nil {
			t.noteCloseFailure()
		}
		if err != nil {
			return nil, err
		}
		return nil, context.Canceled
	}
	return tracked, nil
}

// roundTrip never follows redirects and never replays a model body: it invokes
// Transport.RoundTrip directly with a non-replayable request body (GetBody nil).
func (t *executionTransport) roundTrip(url string, headers http.Header, payload []byte) (*http.Response, error) {
	requestBody := &executionRequestBody{executionBody: &executionBody{ReadCloser: io.NopCloser(bytes.NewReader(payload))}, producerDone: make(chan struct{})}
	t.mu.Lock()
	t.requestBody = requestBody
	late := t.sealed || t.ctx.Err() != nil
	t.mu.Unlock()
	if late {
		_ = requestBody.Close()
		return nil, context.Canceled
	}
	req, err := http.NewRequestWithContext(t.ctx, http.MethodPost, url, requestBody)
	if err != nil {
		_ = requestBody.Close()
		return nil, err
	}
	req.ContentLength = int64(len(payload))
	req.Header = headers.Clone()
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	req.Header.Set("User-Agent", "") // preserve upstreamAuthHeaders' no-client-identity contract
	req.Close = true
	resp, err := t.http.RoundTrip(req)
	if resp != nil && resp.Body != nil {
		body := &executionBody{ReadCloser: resp.Body}
		resp.Body = body
		t.mu.Lock()
		t.body = body
		late := t.sealed || t.ctx.Err() != nil
		t.mu.Unlock()
		if late && body.Close() != nil {
			t.noteCloseFailure()
		}
	}
	return resp, err
}

func (t *executionTransport) noteCloseFailure() {
	t.mu.Lock()
	first := !t.closeFailed
	t.closeFailed = true
	t.mu.Unlock()
	if first && t.cleanupFailure != nil {
		t.cleanupFailure()
	}
}

func (t *executionTransport) closeIO() {
	t.mu.Lock()
	t.sealed = true // no WaitGroup.Add may race the subsequent Wait
	conns := append([]*executionConn(nil), t.conns...)
	body := t.body
	t.mu.Unlock()
	// Close sockets first: Body.Close may itself wait for a blocked read. Do
	// not rely on Body.Close or a host-side close ACK to have closed the socket.
	for _, conn := range conns {
		if conn.Close() != nil {
			t.noteCloseFailure()
		}
	}
	if body != nil && body.Close() != nil {
		t.noteCloseFailure()
	}
	t.http.CloseIdleConnections()
}

// finish is called only after RoundTrip and the consuming producer returned.
// Cancellation is a request to stop, not permission to release a slot. There is
// intentionally no TTL/grace-based release here. Unconfirmed close retains the
// slot permanently (until operator recovery), with a fixed diagnostic reason.
func (t *executionTransport) finish() bool {
	t.cancel()
	t.closeIO()
	<-t.watchDone
	t.dials.Wait() // includes a dial that ignores cancellation and returns late
	t.closeIO()    // includes late connections/body not in the first snapshot
	t.mu.Lock()
	conns := append([]*executionConn(nil), t.conns...)
	body := t.body
	requestBody := t.requestBody
	t.mu.Unlock()
	if requestBody != nil {
		// net/http closes the request body when its writing producer ends;
		// RoundTrip returning alone does not guarantee that producer finished.
		<-requestBody.producerDone
		requestBody.reads.Wait()
	}
	for _, conn := range conns {
		conn.ops.Wait()
	}
	if body != nil {
		body.reads.Wait()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return !t.closeFailed
}

// executionConn prevents net/http's late writeLoop/dial from sending a model
// body after cancellation, and counts operations already inside the socket.
type executionConn struct {
	net.Conn
	ctx       context.Context
	mu        sync.Mutex
	closed    bool
	ops       sync.WaitGroup
	closeOnce sync.Once
	closeErr  error
}

func (c *executionConn) operation() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.ctx.Err() != nil {
		return false
	}
	c.ops.Add(1)
	return true
}

func (c *executionConn) Read(p []byte) (int, error) {
	if !c.operation() {
		return 0, net.ErrClosed
	}
	defer c.ops.Done()
	return c.Conn.Read(p)
}

func (c *executionConn) Write(p []byte) (int, error) {
	if !c.operation() {
		return 0, net.ErrClosed
	}
	defer c.ops.Done()
	return c.Conn.Write(p)
}

func (c *executionConn) Close() error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		c.closeErr = c.Conn.Close()
	})
	return c.closeErr
}

type executionBody struct {
	io.ReadCloser
	mu        sync.Mutex
	closed    bool
	reads     sync.WaitGroup
	closeOnce sync.Once
	closeErr  error
}

func (b *executionBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return 0, net.ErrClosed
	}
	b.reads.Add(1)
	b.mu.Unlock()
	defer b.reads.Done()
	return b.ReadCloser.Read(p)
}

func (b *executionBody) Close() error {
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closed = true
		b.mu.Unlock()
		b.closeErr = b.ReadCloser.Close()
	})
	return b.closeErr
}

// Only the transport's request-writing producer calls Close. Cancellation must
// not fake this acknowledgment by closing the request body itself.
type executionRequestBody struct {
	*executionBody
	producerDone chan struct{}
	producerOnce sync.Once
}

func (b *executionRequestBody) Close() error {
	err := b.executionBody.Close()
	b.producerOnce.Do(func() { close(b.producerDone) })
	return err
}

// Do not stringify network errors: net/url and proxy/TLS errors can carry URLs,
// userinfo, opaque keys or arbitrary body text. Only fixed class labels escape.
func executionNetworkError(ctx context.Context, err error) *errclass.Error {
	message := "upstream network failure"
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		message = "upstream request timeout"
	} else if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		message = "upstream request canceled"
	}
	return &errclass.Error{Class: errclass.ClassNetwork, Message: message, Retryable: true}
}
