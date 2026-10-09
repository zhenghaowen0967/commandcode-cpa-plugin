package plugin

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestExecutionTransport(t *testing.T, parent context.Context, timeout time.Duration) *executionTransport {
	t.Helper()
	transport, err := newExecutionTransport(parent, timeout, "")
	if err != nil {
		t.Fatalf("create execution transport: %v", err)
	}
	return transport
}

func TestExecutionTransportPolicyAndRedirect(t *testing.T) {
	var followed atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/target" {
			followed.Add(1)
			return
		}
		w.Header().Set("Location", "/target")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	transport := newTestExecutionTransport(t, context.Background(), time.Second)
	if !transport.http.DisableKeepAlives || transport.http.ForceAttemptHTTP2 || transport.http.Proxy == nil || len(transport.http.TLSNextProto) != 0 {
		t.Fatal("transport must disable pooling/H2 and retain environment proxy/TLS")
	}
	resp, err := transport.roundTrip(server.URL, make(http.Header), []byte(`{"model":"fake"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusTemporaryRedirect || followed.Load() != 0 {
		t.Fatal("redirect replayed model request")
	}
	if !transport.finish() {
		t.Fatal("cleanup unconfirmed")
	}
}

func TestExecutionTransportConfiguredHTTPProxy(t *testing.T) {
	var routed atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Scheme != "http" || r.URL.Host != "provider.invalid" || r.Host != "provider.invalid" {
			t.Errorf("unexpected proxy request method=%s url=%s host=%s", r.Method, r.URL.Redacted(), r.Host)
			http.Error(w, "unexpected proxy request", http.StatusBadRequest)
			return
		}
		routed.Add(1)
		_, _ = io.WriteString(w, "proxied")
	}))
	defer proxy.Close()

	transport, err := newExecutionTransport(context.Background(), time.Second, proxy.URL)
	if err != nil {
		t.Fatalf("create configured proxy transport: %v", err)
	}
	proxyURL, _ := url.Parse(proxy.URL)
	selectedProxy, proxyErr := transport.http.Proxy(&http.Request{URL: &url.URL{Scheme: "http", Host: "provider.invalid"}})
	if proxyErr != nil || selectedProxy == nil || selectedProxy.String() != proxyURL.String() {
		t.Fatalf("explicit proxy selection=%v err=%v, want configured proxy", selectedProxy, proxyErr)
	}
	resp, err := transport.roundTrip("http://provider.invalid/v1", nil, []byte("synthetic-model-body"))
	if err != nil {
		_ = transport.finish()
		t.Fatalf("proxied round trip failed: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "proxied" {
		_ = transport.finish()
		t.Fatalf("proxied response body=%q err=%v", body, err)
	}
	if !transport.finish() {
		t.Fatal("proxied execution cleanup unconfirmed")
	}
	if routed.Load() != 1 {
		t.Fatalf("proxy route count=%d, want one", routed.Load())
	}
}

func TestExecutionTransportConfiguredHTTPSProxy(t *testing.T) {
	var routed atomic.Int32
	proxy := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Scheme != "http" || r.URL.Host != "provider.invalid" || r.Host != "provider.invalid" {
			t.Errorf("unexpected HTTPS proxy request method=%s url=%s host=%s", r.Method, r.URL.Redacted(), r.Host)
			http.Error(w, "unexpected proxy request", http.StatusBadRequest)
			return
		}
		routed.Add(1)
		_, _ = io.WriteString(w, "proxied")
	}))
	defer proxy.Close()

	transport, err := newExecutionTransport(context.Background(), time.Second, proxy.URL)
	if err != nil {
		t.Fatalf("create configured HTTPS proxy transport: %v", err)
	}
	proxyClientTransport := proxy.Client().Transport.(*http.Transport)
	transport.http.TLSClientConfig.RootCAs = proxyClientTransport.TLSClientConfig.RootCAs
	resp, err := transport.roundTrip("http://provider.invalid/v1", nil, []byte("synthetic-model-body"))
	if err != nil {
		_ = transport.finish()
		t.Fatalf("HTTPS-proxied round trip failed: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "proxied" {
		_ = transport.finish()
		t.Fatalf("HTTPS-proxied response body=%q err=%v", body, err)
	}
	if !transport.finish() {
		t.Fatal("HTTPS-proxied execution cleanup unconfirmed")
	}
	if routed.Load() != 1 {
		t.Fatalf("HTTPS proxy route count=%d, want one", routed.Load())
	}
}

func TestExecutionTransportRejectsProxyConfigWithoutEchoingIt(t *testing.T) {
	secretURL := "ftp://user:synthetic-secret@proxy.invalid:21"
	if _, err := newExecutionTransport(context.Background(), time.Second, secretURL); err == nil || strings.Contains(err.Error(), "synthetic-secret") {
		t.Fatalf("proxy config error=%v, want fixed redacted failure", err)
	}
}

func TestExecutionTransportRealStreamCancel(t *testing.T) {
	canceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte("first"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(canceled)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	transport := newTestExecutionTransport(t, ctx, time.Second)
	resp, err := transport.roundTrip(server.URL, nil, []byte("fake-model-body"))
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatal(err)
	}
	cancel()
	if !transport.finish() {
		t.Fatal("real canceled socket did not close")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("server context not canceled")
	}
	if n, _ := resp.Body.Read(buf); n != 0 {
		t.Fatal("body remained readable after cleanup")
	}
}

// A dialer ignoring cancellation mimics net/http's detached/late dial. Finish
// must wait, reject the late socket, and never allow any model-body write.
func TestExecutionTransportLateDialJoinedBeforeCleanup(t *testing.T) {
	transport := newTestExecutionTransport(t, context.Background(), 30*time.Millisecond)
	entered, release := make(chan struct{}), make(chan struct{})
	conn := &testExecutionConn{}
	transport.dial = func(context.Context, string, string) (net.Conn, error) {
		close(entered)
		<-release
		return conn, nil
	}
	result := make(chan error, 1)
	go func() {
		_, err := transport.roundTrip("http://local.test/v1", nil, []byte("model-body"))
		result <- err
	}()
	<-entered
	select {
	case <-result:
	case <-time.After(time.Second):
		t.Fatal("RoundTrip did not cancel")
	}
	finished := make(chan bool, 1)
	go func() { finished <- transport.finish() }()
	select {
	case <-finished:
		t.Fatal("released while late dial still live")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case ok := <-finished:
		if !ok {
			t.Fatal("late close unconfirmed")
		}
	case <-time.After(time.Second):
		t.Fatal("late dial not joined")
	}
	if conn.writes.Load() != 0 || conn.closes.Load() != 1 {
		t.Fatalf("late conn writes=%d closes=%d", conn.writes.Load(), conn.closes.Load())
	}
}

func TestExecutionTransportTimeoutWaitsActualClose(t *testing.T) {
	transport := newTestExecutionTransport(t, context.Background(), 20*time.Millisecond)
	entered, release := make(chan struct{}), make(chan struct{})
	conn := &testExecutionConn{closeEntered: entered, closeRelease: release}
	tracked := &executionConn{Conn: conn, ctx: transport.ctx}
	transport.mu.Lock()
	transport.conns = append(transport.conns, tracked)
	transport.mu.Unlock()
	<-entered
	finished := make(chan bool, 1)
	go func() { finished <- transport.finish() }()
	select {
	case <-finished:
		t.Fatal("timeout released before actual socket close")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case ok := <-finished:
		if !ok {
			t.Fatal("close should succeed")
		}
	case <-time.After(time.Second):
		t.Fatal("close not joined")
	}
}

func TestExecutionTransportCloseFailureFailClosed(t *testing.T) {
	transport := newTestExecutionTransport(t, context.Background(), time.Second)
	conn := &testExecutionConn{closeErr: errors.New("secret-url-or-key-never-render")}
	transport.conns = append(transport.conns, &executionConn{Conn: conn, ctx: transport.ctx})
	if transport.finish() {
		t.Fatal("failed real close must not confirm settlement")
	}
}

func TestExecutionTransportBodyCloseJoined(t *testing.T) {
	transport := newTestExecutionTransport(t, context.Background(), time.Second)
	entered, release := make(chan struct{}), make(chan struct{})
	body := &testExecutionBody{entered: entered, release: release}
	transport.body = &executionBody{ReadCloser: body}
	finished := make(chan bool, 1)
	go func() { finished <- transport.finish() }()
	<-entered
	select {
	case <-finished:
		t.Fatal("body close not joined")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	select {
	case ok := <-finished:
		if !ok {
			t.Fatal("body cleanup failed")
		}
	case <-time.After(time.Second):
		t.Fatal("body cleanup stuck")
	}
}

func TestExecutionNetworkErrorNeverEchoesInput(t *testing.T) {
	secret := "https://user:key@example.test/prompt?key=opaque-body-text"
	err := executionNetworkError(context.Background(), errors.New(secret))
	if strings.Contains(err.Message, "example") || strings.Contains(err.Message, "key") || err.Message != "upstream network failure" {
		t.Fatalf("unsafe error: %s", err.Message)
	}
}

type testExecutionConn struct {
	writes       atomic.Int32
	closes       atomic.Int32
	closeErr     error
	closeEntered chan struct{}
	closeRelease chan struct{}
}

func (c *testExecutionConn) Read([]byte) (int, error)    { return 0, io.EOF }
func (c *testExecutionConn) Write(p []byte) (int, error) { c.writes.Add(1); return len(p), nil }
func (c *testExecutionConn) Close() error {
	c.closes.Add(1)
	if c.closeEntered != nil {
		close(c.closeEntered)
	}
	if c.closeRelease != nil {
		<-c.closeRelease
	}
	return c.closeErr
}
func (*testExecutionConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (*testExecutionConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (*testExecutionConn) SetDeadline(time.Time) error      { return nil }
func (*testExecutionConn) SetReadDeadline(time.Time) error  { return nil }
func (*testExecutionConn) SetWriteDeadline(time.Time) error { return nil }

type testExecutionBody struct {
	entered, release chan struct{}
	once             sync.Once
}

func (*testExecutionBody) Read([]byte) (int, error) { return 0, io.EOF }
func (b *testExecutionBody) Close() error {
	b.once.Do(func() { close(b.entered); <-b.release })
	return nil
}
