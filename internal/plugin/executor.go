// Non-stream and stream execution resolve the catalog snapshot, acquire a
// trusted account lease BEFORE upstream I/O, execute over plugin-owned HTTP/1,
// and translate the three supported protocols back to the client format.
// CPA selects AuthID; the pool alone supplies its authoritative credential.

package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/sjson"

	"commandcode-cpa-plugin/internal/adapter/chatcompletions"
	"commandcode-cpa-plugin/internal/adapter/messages"
	"commandcode-cpa-plugin/internal/adapter/responses"
	"commandcode-cpa-plugin/internal/adapter/shared"
	"commandcode-cpa-plugin/internal/catalog"
	"commandcode-cpa-plugin/internal/config"
	"commandcode-cpa-plugin/internal/errclass"
	"commandcode-cpa-plugin/internal/pool"
)

// executorRequest mirrors rpcExecutorRequest: the SDK embeds
// pluginapi.ExecutorRequest untagged, so its fields marshal under Go field
// names ("Model", "SourceFormat", "OriginalRequest", "Stream"). StreamID is
// the DOWNSTREAM host-allocated id for executor.execute_stream emissions;
// ids returned by DoStream are UPSTREAM and never interchangeable (§4).
type executorRequest struct {
	pluginapi.ExecutorRequest
	StreamID string `json:"stream_id,omitempty"`
}

// resolvedExecution holds the configuration/catalog snapshot and the trusted
// credential from the acquired pool lease, never an RPC-supplied API key.
type resolvedExecution struct {
	cfg   config.Config
	rec   catalog.ModelRecord
	key   string
	lease *pool.Lease
}

// acquireExecution is the admission boundary BEFORE any upstream I/O. The
// private header is the only request identity: RPC metadata is not a substitute.
// Add and shutdown's closed transition share m.mu so shutdown cannot outrun a
// newly registered producer or its host callbacks.
func (m *Manager) acquireExecution(req *executorRequest) (*resolvedExecution, []byte) {
	if req.AuthProvider != ProviderID {
		return nil, classEnvelope(&errclass.Error{Class: errclass.ClassAuth, Message: "selected auth provider is not commandcode"})
	}
	req.Headers = req.Headers.Clone()
	var ids []string
	for name, values := range req.Headers {
		if strings.EqualFold(name, requestIDHeader) {
			ids = append(ids, values...)
			delete(req.Headers, name)
		}
	}
	if len(ids) != 1 || strings.TrimSpace(ids[0]) == "" || strings.TrimSpace(ids[0]) != ids[0] {
		return nil, ErrEnvelope("invalid_request", "missing or ambiguous pool request identity")
	}
	m.mu.RLock()
	if m.execClosing.Load() || m.pool == nil {
		m.mu.RUnlock()
		return nil, ErrEnvelope("pool_unavailable", "execution pool is unavailable")
	}
	cfg, mgr, p := m.cfg, m.mgr, m.pool
	m.executionWG.Add(1)
	if m.bridge != nil {
		m.bridge.inFlight.Add(1)
	}
	m.mu.RUnlock()
	var rec catalog.ModelRecord
	var found bool
	if mgr != nil && req.Model != "" {
		rec, found = mgr.Lookup(req.Model)
	}
	if !found {
		m.executionDone()
		return nil, classEnvelope(&errclass.Error{Class: errclass.ClassInvalidModel, Message: "model not in routable catalog", StatusCode: http.StatusNotFound})
	}
	lease, err := p.Acquire(req.AuthID, ids[0], req.Model)
	if err != nil {
		m.executionDone()
		return nil, poolAdmissionEnvelope(err)
	}
	if lease.Credential.AuthID != req.AuthID || !lease.Credential.Enabled || strings.TrimSpace(lease.Credential.APIKey) == "" {
		lease.Cancel()
		lease.Settle("invalid_execution_credential")
		m.executionDone()
		return nil, classEnvelope(&errclass.Error{Class: errclass.ClassAuth, Message: "selected auth has no trusted pool credential"})
	}
	return &resolvedExecution{cfg: cfg, rec: rec, key: lease.Credential.APIKey, lease: lease}, nil
}

func poolAdmissionEnvelope(err error) []byte {
	// Never stringify pool errors or arbitrary account metadata at the wire
	// boundary. All retry decisions are made before any upstream I/O.
	switch {
	case errors.Is(err, pool.ErrUnknownCredential), errors.Is(err, pool.ErrDisabled):
		return classEnvelope(&errclass.Error{Class: errclass.ClassAuth, Message: "selected auth has no eligible trusted pool credential", StatusCode: http.StatusUnauthorized})
	case errors.Is(err, pool.ErrAtCapacity):
		return classEnvelope(&errclass.Error{Class: errclass.ClassRateLimit, Message: "selected account is at hard concurrency capacity", StatusCode: http.StatusTooManyRequests, Retryable: true})
	case errors.Is(err, pool.ErrAborted):
		return classEnvelope(&errclass.Error{Class: errclass.ClassNetwork, Message: "request has already completed or been canceled", StatusCode: http.StatusRequestTimeout})
	default:
		return classEnvelope(&errclass.Error{Class: errclass.ClassUpstream, Message: "execution pool is unavailable or quota is unverified", StatusCode: http.StatusServiceUnavailable, Retryable: true})
	}
}

func (m *Manager) executionDone() {
	if m.bridge != nil {
		m.bridge.inFlight.Done()
	}
	m.executionWG.Done()
}

func (m *Manager) finishExecution(res *resolvedExecution, transport *executionTransport, reason string) {
	defer m.executionDone()
	res.lease.Cancel()
	if transport != nil && !transport.finish() {
		if transport.cleanupFailure == nil {
			res.lease.Record("upstream_cleanup_unconfirmed")
		}
		return // fail closed: never release a possibly live upstream attempt
	}
	res.lease.Settle(reason)
}

// handleExecute implements executor.execute (non-stream). Stream-flagged
// requests are routed to the stream path instead of rejected.
func (m *Manager) handleExecute(request []byte) ([]byte, error) {
	var req executorRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed executor request body"), nil
	}
	if req.Stream {
		return m.executeStream(req)
	}
	res, failEnv := m.acquireExecution(&req)
	if res == nil {
		return failEnv, nil
	}
	var transport *executionTransport
	reason := "execution_validation_failed"
	defer func() { m.finishExecution(res, transport, reason) }()
	sessionID, eErr := resolveCommandCodeSessionID(req)
	if eErr != nil {
		return requestValidationEnvelope(eErr), nil
	}
	upstreamBody, eErr := buildUpstreamRequest(res.rec.Protocol, res.rec.UpstreamID, req.SourceFormat, req.OriginalRequest, res.rec.Thinking)
	if eErr != nil {
		return requestValidationEnvelope(eErr), nil
	}

	var transportErr error
	transport, transportErr = newExecutionTransport(res.lease.Context, res.cfg.RequestTimeout, res.cfg.ProxyURL, func() { res.lease.Record("upstream_cleanup_unconfirmed") })
	if transportErr != nil {
		return classEnvelope(executionNetworkError(res.lease.Context, transportErr)), nil
	}
	reason = "upstream_network_failure"
	resp, err := transport.roundTrip(catalog.JoinUpstreamURL(res.cfg.BaseURL, res.rec.EndpointPath), upstreamAuthHeaders(res.rec.Protocol, res.key, sessionID), upstreamBody)
	if err != nil {
		return classEnvelope(executionNetworkError(transport.ctx, err)), nil
	}
	body, eErr := readExecutionBody(transport, resp.Body, res.cfg.MaxResponseBytes)
	if eErr != nil {
		return classEnvelope(eErr), nil
	}
	if resp.StatusCode >= 300 {
		reason = "upstream_status_failure"
		return classEnvelope(shared.UpstreamStatusError(resp.StatusCode, body)), nil
	}
	converted, eErr := convertNonStream(res.rec.Protocol, req.SourceFormat, resp.StatusCode, body)
	if eErr != nil {
		reason = "upstream_translation_failure"
		return classEnvelope(eErr), nil
	}
	reason = "upstream_complete"
	return okEnvelope(pluginapi.ExecutorResponse{Payload: converted, Headers: resp.Header}), nil
}

// Bound allocation BEFORE parsing (including upstream error responses).
func readExecutionBody(transport *executionTransport, body io.Reader, limit int64) ([]byte, *errclass.Error) {
	if limit <= 0 {
		return nil, errclass.Translation("invalid max-response-bytes")
	}
	readLimit := limit
	if readLimit < math.MaxInt64 {
		readLimit++
	}
	payload, err := io.ReadAll(io.LimitReader(body, readLimit))
	if int64(len(payload)) > limit {
		return nil, errclass.Translation("response exceeds max-response-bytes")
	}
	if err != nil {
		return nil, executionNetworkError(transport.ctx, err)
	}
	return payload, nil
}

func buildUpstreamRequest(route catalog.Route, upstreamModel, sourceFormat string, sourceBody []byte, ts *pluginapi.ThinkingSupport) ([]byte, *errclass.Error) {
	switch route {
	case catalog.RouteChatCompletions:
		return chatcompletions.BuildRequest(upstreamModel, sourceFormat, sourceBody, ts)
	case catalog.RouteMessages:
		return messages.BuildRequest(upstreamModel, sourceFormat, sourceBody, ts)
	case catalog.RouteResponses:
		return responses.BuildRequest(upstreamModel, sourceFormat, sourceBody, ts)
	}
	return nil, errclass.Translation("unsupported route")
}

func upstreamAuthHeaders(route catalog.Route, key, sessionID string) http.Header {
	var h http.Header
	if route == catalog.RouteMessages {
		h = messages.AuthHeaders(key)
	} else {
		// Chat Completions and Responses endpoints are OpenAI-style bearer.
		h = chatcompletions.AuthHeaders(key)
	}
	h.Set("x-commandcode-session", sessionID)
	return h
}

const emptyCommandCodeSessionID = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// resolveCommandCodeSessionID applies the FR-012/AC-H precedence: CPA's canonical
// identity, an explicit inbound session header, then the existing content hash.
func resolveCommandCodeSessionID(req executorRequest) (string, *errclass.Error) {
	if sid, ok := req.Metadata["canonical_session_id"].(string); ok && sid != "" {
		return sid, nil
	}
	for _, name := range []string{
		"X-Session-Affinity",
		"X-Commandcode-Session",
		"X-Session-Id",
		"X-Claude-Code-Session-Id",
		"Session-Id",
	} {
		if sid := req.Headers.Get(name); sid != "" {
			return sid, nil
		}
	}
	return deriveCommandCodeSessionID(req.SourceFormat, req.OriginalRequest)
}

// deriveCommandCodeSessionID hashes the model-visible content of the initial user
// turn before translation (FR-012/AC-H). Metadata is excluded;
// valid requests without a user turn use the fixed empty-input digest.
func deriveCommandCodeSessionID(sourceFormat string, originalRequest []byte) (string, *errclass.Error) {
	var content strings.Builder
	appendParts := func(raw json.RawMessage, target string) *errclass.Error {
		parts, eErr := shared.DecodeStringOrParts(raw, target)
		if eErr != nil {
			return eErr
		}
		for _, part := range parts {
			if part.ImageURL != "" {
				content.WriteString(part.ImageURL)
			} else {
				content.WriteString(part.Text)
			}
		}
		return nil
	}

	switch sourceFormat {
	case "openai":
		var req shared.ChatCompletionsRequest
		if err := json.Unmarshal(originalRequest, &req); err != nil {
			return "", errclass.Translation("malformed openai request JSON: " + err.Error())
		}
		started := false
		for _, msg := range req.Messages {
			if !started {
				if msg.Role == "system" || msg.Role == "developer" {
					continue
				}
				if msg.Role != "user" {
					break
				}
				started = true
			} else if msg.Role != "user" {
				break
			}
			if eErr := appendParts(msg.Content, "/v1/chat/completions"); eErr != nil {
				return "", eErr
			}
		}
	case "claude":
		req, eErr := shared.DecodeClaudeMessages(originalRequest)
		if eErr != nil {
			return "", eErr
		}
		started := false
		for _, msg := range req.Messages {
			if !started {
				if msg.Role != "user" {
					continue
				}
				started = true
			} else if msg.Role != "user" {
				break
			}
			if msg.Content != "" {
				content.WriteString(msg.Content)
			}
			for _, block := range msg.Blocks {
				switch block.Kind {
				case "text":
					content.WriteString(block.Text)
				case "image":
					content.WriteString(block.URL)
				case "tool_result":
					text, eErr := shared.ToolResultText(block.Result, req.Tools, "tool messages carry text only")
					if eErr != nil {
						return "", eErr
					}
					content.WriteString(text)
				}
			}
		}
	case "openai-response":
		var req shared.ResponsesRequest
		if err := json.Unmarshal(originalRequest, &req); err != nil {
			return "", errclass.Translation("malformed openai-response request JSON: " + err.Error())
		}
		items, eErr := req.DecodeInputItems()
		if eErr != nil {
			return "", eErr
		}
		started := false
		for _, item := range items {
			isUserMessage := item.Role == "user" && (item.Type == "message" || item.Type == "")
			if !started {
				if !isUserMessage {
					continue
				}
				started = true
			} else if !isUserMessage {
				break
			}
			if eErr := appendParts(item.Content, "/v1/responses"); eErr != nil {
				return "", eErr
			}
		}
	default:
		return "", shared.UnsupportedFormat(sourceFormat, "CommandCode session derivation")
	}

	digest := sha256.Sum256([]byte(content.String()))
	return hex.EncodeToString(digest[:]), nil
}

// convertNonStream routes one upstream response to its adapter's uniform
// translator: every adapter owns status classification (>=400 → §7
// classified errors), native passthrough, and cross-format conversion for
// all client formats.
func convertNonStream(route catalog.Route, sourceFormat string, status int, body []byte) ([]byte, *errclass.Error) {
	switch route {
	case catalog.RouteChatCompletions:
		return chatcompletions.ConvertNonStreamResponse(sourceFormat, status, body)
	case catalog.RouteMessages:
		return messages.ConvertNonStreamResponse(sourceFormat, status, body)
	case catalog.RouteResponses:
		return responses.ConvertNonStreamResponse(sourceFormat, status, body)
	}
	return nil, errclass.Translation("unsupported route")
}

func requestValidationEnvelope(e *errclass.Error) []byte {
	if e != nil && e.StatusCode == 0 && (e.Class == errclass.ClassTranslation || e.Class == errclass.ClassUnsupported) {
		requestErr := *e
		requestErr.StatusCode = http.StatusBadRequest
		requestErr.Retryable = false
		return classEnvelope(&requestErr)
	}
	return classEnvelope(e)
}

// classEnvelope renders a classified failure as the wire error envelope;
// ToEnvelopeError redacts and propagates Retryable/HTTPStatus host-side.
func classEnvelope(e *errclass.Error) []byte {
	wire := errclass.ToEnvelopeError(e)
	out, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: &wire})
	return out
}

// streamConverter is the common shape of the three adapters' stream
// converters: feed one upstream SSE chunk, get translated client events.
type streamConverter interface {
	Feed(chunk []byte) (events [][]byte, done bool, eErr *errclass.Error)
}

// Compile-time proof the close-without-terminal Flush seam (F5) picks up
// both converters that can hold a deferred terminal.
var (
	_ interface{ Flush() [][]byte } = (*chatcompletions.StreamConverter)(nil)
	_ interface{ Flush() [][]byte } = (*messages.StreamConverter)(nil)
)

func newStreamConverter(route catalog.Route, sourceFormat string) streamConverter {
	switch route {
	case catalog.RouteMessages:
		return messages.NewStreamConverter(sourceFormat)
	case catalog.RouteResponses:
		return responses.NewStreamConverter(sourceFormat)
	}
	return chatcompletions.NewStreamConverter(sourceFormat)
}

// handleExecuteStream implements executor.execute_stream (FR-006, §7).
// It decodes once and delegates to executeStream so a stream-flagged
// request arriving via executor.execute is never parsed twice.
func (m *Manager) handleExecuteStream(request []byte) ([]byte, error) {
	var req executorRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed executor request body"), nil
	}
	return m.executeStream(req)
}

// executeStream runs the already-decoded stream execution: pre-first-byte errors
// (invalid request, unroutable model, upstream HTTP >=400) return immediately as
// error envelopes so CPA can failover pre-emission.
// On success (upstream HTTP 200 OK), the reading and emitting pump loop runs in
// a background goroutine and executeStream returns okEnvelope immediately so the
// host can start draining chunks to the downstream client without buffer deadlocks.
func (m *Manager) executeStream(req executorRequest) ([]byte, error) {
	res, failEnv := m.acquireExecution(&req)
	if res == nil {
		return failEnv, nil
	}
	var transport *executionTransport
	reason := "execution_validation_failed"
	transferred := false
	defer func() {
		if !transferred {
			m.finishExecution(res, transport, reason)
		}
	}()
	sessionID, eErr := resolveCommandCodeSessionID(req)
	if eErr != nil {
		return requestValidationEnvelope(eErr), nil
	}
	upstreamBody, eErr := buildUpstreamRequest(res.rec.Protocol, res.rec.UpstreamID, req.SourceFormat, req.OriginalRequest, res.rec.Thinking)
	if eErr != nil {
		return requestValidationEnvelope(eErr), nil
	}
	// The RPC streaming method is authoritative even when the inbound JSON
	// did not explicitly include stream:true.
	upstreamBody, err := sjson.SetBytes(upstreamBody, "stream", true)
	if err != nil {
		return classEnvelope(errclass.Translation("invalid upstream stream request")), nil
	}
	var transportErr error
	transport, transportErr = newExecutionTransport(res.lease.Context, res.cfg.RequestTimeout, res.cfg.ProxyURL, func() { res.lease.Record("upstream_cleanup_unconfirmed") })
	if transportErr != nil {
		return classEnvelope(executionNetworkError(res.lease.Context, transportErr)), nil
	}
	reason = "upstream_network_failure"
	resp, err := transport.roundTrip(catalog.JoinUpstreamURL(res.cfg.BaseURL, res.rec.EndpointPath), upstreamAuthHeaders(res.rec.Protocol, res.key, sessionID), upstreamBody)
	if err != nil {
		return classEnvelope(executionNetworkError(transport.ctx, err)), nil
	}
	if resp.StatusCode >= 300 {
		reason = "upstream_status_failure"
		body, eErr := readExecutionBody(transport, resp.Body, res.cfg.MaxResponseBytes)
		if eErr != nil {
			return classEnvelope(eErr), nil
		}
		return classEnvelope(shared.UpstreamStatusError(resp.StatusCode, body)), nil
	}
	// Ownership, including the lease and shutdown count, transfers to the
	// producer. Returning the opening envelope MUST NOT release admission.
	transferred = true
	go func() {
		reason := "upstream_stream_failure"
		defer func() {
			if recover() != nil {
				reason = "upstream_producer_panicked"
				transport.cancel()
				if m.bridge != nil {
					_ = m.bridge.StreamCloseDownstream(req.StreamID, "upstream producer failed")
				}
			}
			m.finishExecution(res, transport, reason)
		}()
		reason = m.pumpStream(req.StreamID, resp.Body, transport, res, req.SourceFormat)
	}()
	return okEnvelope(struct{}{}), nil
}

func (m *Manager) pumpStream(downID string, body io.Reader, transport *executionTransport, res *resolvedExecution, sourceFormat string) (reason string) {
	closeMessage := ""
	reason = "upstream_complete"
	defer func() {
		if recover() != nil {
			closeMessage = "upstream producer failed"
			reason = "upstream_producer_panicked"
		}
		// Cancel local upstream even if the emit failed or converter already
		// recognized a terminal event but upstream never actually sent EOF.
		transport.cancel()
		if m.bridge != nil {
			_ = m.bridge.StreamCloseDownstream(downID, closeMessage)
		}
	}()
	conv := newStreamConverter(res.rec.Protocol, sourceFormat)
	buf := make([]byte, 32*1024)
	var total int64
	convDone := false
	fail := func(message, why string) string {
		closeMessage = message
		return why
	}
	for {
		n, readErr := body.Read(buf)
		if transport.ctx.Err() != nil {
			return fail(executionNetworkError(transport.ctx, readErr).Message, "upstream_canceled")
		}
		if n < 0 || int64(n) > res.cfg.MaxResponseBytes-total {
			return fail("stream exceeded max-response-bytes", "upstream_response_limit")
		}
		total += int64(n)
		if n > 0 && !convDone {
			events, done, convErr := conv.Feed(buf[:n])
			if convErr != nil {
				return fail(errclass.Redact(convErr.Message), "upstream_translation_failure")
			}
			if m.emitAll(downID, events) != nil {
				return fail("downstream stream emission failed", "downstream_emit_failure")
			}
			convDone = done
			if convDone {
				// A terminal SSE marker is not proof of EOF. Returning stops
				// production; finish cancels, closes and joins actual I/O.
				return "upstream_complete"
			}
		}
		if errors.Is(readErr, io.EOF) {
			if !convDone {
				if flusher, ok := conv.(interface{ Flush() [][]byte }); ok {
					if m.emitAll(downID, flusher.Flush()) != nil {
						return fail("downstream stream emission failed", "downstream_emit_failure")
					}
				}
			}
			return "upstream_complete"
		}
		if readErr != nil {
			return fail(executionNetworkError(transport.ctx, readErr).Message, "upstream_read_failure")
		}
	}
}

// emitAll feeds converted events downstream in order. The bounded bridge
// callbacks remain independently counted by HostBridge after an emit timeout;
// shutdown must drain those orphaned callbacks even once upstream is settled.
func (m *Manager) emitAll(downStreamID string, events [][]byte) error {
	for _, evt := range events {
		if err := m.bridge.StreamEmit(downStreamID, evt); err != nil {
			return err
		}
	}
	return nil
}
