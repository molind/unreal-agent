package responsesapi

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/unreallabsai/unreal-agent/harness/llm"
)

// Auto falls back only when the HTTP upgrade is explicitly unsupported, before
// generation. Authentication, quota, policy and context errors never fall back.
type Transport string

const (
	TransportHTTP      Transport = "http"
	TransportAuto      Transport = "auto"
	TransportWebSocket Transport = "websocket"
)

type websocketSession struct {
	gate         chan struct{}
	ctx          context.Context
	cancel       context.CancelFunc
	closeOnce    sync.Once
	client       *http.Client
	conn         *websocket.Conn
	incoming     chan websocketFrame
	readerCancel context.CancelFunc
	readerDone   chan struct{}
	disabled     bool
	key          string
	properties   []byte
	prefix       []jsontext.Value
	previousID   string
}

func newWebsocketSession() *websocketSession {
	ctx, cancel := context.WithCancel(context.Background())
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	return &websocketSession{ctx: ctx, cancel: cancel, gate: gate, client: &http.Client{
		Transport:     http.DefaultTransport.(*http.Transport).Clone(),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}
func (w *websocketSession) reset() {
	if w.conn != nil {
		w.readerCancel()
		_ = w.conn.CloseNow()
		<-w.readerDone
		w.conn = nil
		w.incoming = nil
	}
	w.key, w.previousID = "", ""
	w.prefix = nil
	w.properties = nil
}
func (w *websocketSession) close() {
	w.closeOnce.Do(func() {
		w.cancel()
		<-w.gate
		defer func() { w.gate <- struct{}{} }()
		w.reset()
		w.client.CloseIdleConnections()
	})
}

func (a *adapter) respondWebsocket(ctx context.Context, body []byte, key string) (llm.Response, error) {
	w := a.websocket
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(w.ctx, cancel)
	defer stop()
	select {
	case <-ctx.Done():
		return llm.Response{}, ctx.Err()
	case <-w.gate:
	}
	var trace *Exchange
	defer func() {
		w.gate <- struct{}{}
		if trace != nil && a.trace != nil {
			a.trace(*trace)
		}
	}()
	if err := ctx.Err(); err != nil {
		return llm.Response{}, err
	}
	if w.disabled {
		return a.respondHTTP(ctx, body, key, "websocket unavailable")
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(body, &fields); err != nil {
		return llm.Response{}, err
	}
	input := make([]jsontext.Value, 0)
	if raw := fields["input"]; len(raw) != 0 {
		if err := json.Unmarshal(raw, &input); err != nil {
			return llm.Response{}, err
		}
	}
	delete(fields, "input")
	properties, err := json.Marshal(fields, json.Deterministic(true))
	if err != nil {
		return llm.Response{}, err
	}
	w.drainIdle()
	incremental := w.conn != nil && key != "" && key == w.key && w.previousID != "" && bytes.Equal(properties, w.properties) && len(input) >= len(w.prefix)
	if incremental {
		for i := range w.prefix {
			if !bytes.Equal(input[i], w.prefix[i]) {
				incremental = false
				break
			}
		}
	}
	// Changed settings/history/session and compaction all invalidate continuation.
	if w.conn != nil && !incremental {
		w.reset()
	}
	for recovery := 0; recovery < 2; recovery++ {
		if err := ctx.Err(); err != nil {
			w.reset()
			return llm.Response{}, err
		}
		reused := w.conn != nil
		if w.conn == nil {
			headers := http.Header{}
			for name, values := range a.headers {
				headers[name] = append([]string(nil), values...)
			}
			headers.Set("OpenAI-Beta", "responses_websockets=2026-02-06")
			if a.cacheKeyPlacement.Header != "" && key != "" {
				headers.Set(a.cacheKeyPlacement.Header, key)
			}
			dialCtx, dialCancel := context.WithTimeout(ctx, 30*time.Second)
			conn, response, dialErr := websocket.Dial(dialCtx, a.endpoint, &websocket.DialOptions{HTTPClient: w.client, HTTPHeader: headers})
			dialCancel()
			if dialErr != nil {
				if ctx.Err() != nil {
					return llm.Response{}, ctx.Err()
				}
				if response != nil {
					data, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
					_ = response.Body.Close()
					status := response.StatusCode
					if a.transport == TransportAuto && (status == 404 || status == 405 || status == 426 || status == 501) {
						w.disabled = true
						w.reset()
						return a.respondHTTP(ctx, body, key, "websocket unsupported")
					}
					return llm.Response{}, providerError(status, data)
				}
				return llm.Response{}, fmt.Errorf("connect responses websocket: %w", dialErr)
			}
			conn.SetReadLimit(maxSSEFrameBytes)
			w.startReader(conn)
			incremental = false
		}
		payload := make(map[string]jsontext.Value, len(fields)+3)
		for name, value := range fields {
			payload[name] = value
		}
		sent := input
		if incremental {
			sent = input[len(w.prefix):]
			payload["previous_response_id"], _ = json.Marshal(w.previousID)
		}
		payload["type"] = jsontext.Value(`"response.create"`)
		payload["input"], err = json.Marshal(sent)
		if err != nil {
			w.reset()
			return llm.Response{}, err
		}
		wire, err := json.Marshal(payload, json.Deterministic(true))
		if err != nil {
			w.reset()
			return llm.Response{}, err
		}
		responseBody, exchangeErr := w.exchange(ctx, wire)
		if exchangeErr != nil {
			var apiErr *APIError
			// Missing references / expired connections are explicit non-generation
			// failures. Retry full on a new connection ONCE, never blindly loop.
			var failure *websocketFailure
			recoverable := errors.As(exchangeErr, &failure) && failure.beforeGeneration && errors.As(exchangeErr, &apiErr) && (apiErr.Code == "previous_response_not_found" || apiErr.Code == "websocket_connection_limit_reached")
			w.reset()
			incremental = false
			if recoverable && recovery == 0 && ctx.Err() == nil {
				continue
			}
			return llm.Response{}, exchangeErr
		}
		if err := ctx.Err(); err != nil {
			w.reset()
			return llm.Response{}, err
		}
		response, err := decodeResponse(responseBody)
		if err != nil {
			w.reset()
			return llm.Response{}, err
		}
		if response.Failure != nil {
			w.reset()
			return llm.Response{}, &APIError{Code: response.Failure.Code, Message: response.Failure.Message}
		}
		response.Transport = &llm.TransportUsage{Mode: "websocket", Incremental: incremental, Reused: reused, SentInputItems: len(sent), TotalInputItems: len(input), RequestBytes: len(wire)}
		if a.trace != nil {
			trace = &Exchange{RequestBody: wire, StatusCode: 101, ResponseBody: responseBody}
		}
		if response.ID != "" && response.Stop == llm.StopComplete && key != "" {
			converted, err := requestInput(response.Output)
			if err != nil {
				w.reset()
				return llm.Response{}, err
			}
			encoded, err := json.Marshal(converted, json.Deterministic(true))
			if err != nil {
				w.reset()
				return llm.Response{}, err
			}
			var output []jsontext.Value
			if err := json.Unmarshal(encoded, &output); err != nil {
				w.reset()
				return llm.Response{}, err
			}
			w.prefix = append(append([]jsontext.Value(nil), input...), output...)
			w.previousID, w.key, w.properties = response.ID, key, properties
		} else {
			w.reset()
		}
		return response, nil
	}
	return llm.Response{}, errors.New("websocket continuation recovery exhausted")
}

func (w *websocketSession) exchange(ctx context.Context, body []byte) ([]byte, error) {
	if err := w.conn.Write(ctx, websocket.MessageText, body); err != nil {
		return nil, err
	}
	var state responseState
	received := 0
	begun := false
	for {
		idle, cancel := context.WithTimeout(ctx, modelResponseIdleTimeout)
		var frame websocketFrame
		select {
		case frame = <-w.incoming:
		case <-idle.Done():
			cancel()
			return nil, idle.Err()
		}
		cancel()
		kind, data, err := frame.kind, frame.data, frame.err
		if err != nil {
			return nil, err
		}
		if kind != websocket.MessageText {
			return nil, errors.New("responses websocket returned a non-text message")
		}
		received += len(data)
		if received > maxSSEFrameBytes {
			return nil, errors.New("responses websocket response exceeds size limit")
		}
		var event struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(data, &event)
		if strings.HasPrefix(event.Type, "response.") && event.Type != "response.failed" {
			begun = true
		}
		if err := state.observe(data); err != nil {
			return nil, &websocketFailure{error: err, beforeGeneration: !begun}
		}
		if state.err != nil {
			return nil, &websocketFailure{error: state.err, beforeGeneration: !begun}
		}
		if state.terminal {
			if state.failure != nil {
				return nil, &websocketFailure{error: state.failure, beforeGeneration: !begun}
			}
			return state.unwrap()
		}
	}
}

// DefaultTransport uses WebSockets automatically only for the known first-party
// endpoints. Custom/compatibility servers remain HTTP unless explicitly enabled.
func DefaultTransport(baseURL string, configured Transport) Transport {
	if configured != "" {
		return configured
	}
	if strings.TrimRight(baseURL, "/") == "https://api.openai.com/v1" || strings.TrimRight(baseURL, "/") == "https://chatgpt.com/backend-api/codex" {
		return TransportAuto
	}
	return "" // Legacy/custom endpoint: plain HTTP, no capability assumption.
}

type websocketFrame struct {
	kind websocket.MessageType
	data []byte
	err  error
}
type websocketFailure struct {
	error
	beforeGeneration bool
}

func (e *websocketFailure) Unwrap() error { return e.error }

// Keep a reader alive between model requests so server pings are answered while
// local tools run. It is bounded and joined on reset/cancel/close.
func (w *websocketSession) startReader(conn *websocket.Conn) {
	ctx, cancel := context.WithCancel(w.ctx)
	frames := make(chan websocketFrame, 1)
	done := make(chan struct{})
	w.conn, w.incoming, w.readerCancel, w.readerDone = conn, frames, cancel, done
	go func() {
		defer close(done)
		for {
			kind, data, err := conn.Read(ctx)
			select {
			case frames <- websocketFrame{kind, data, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
}
func (w *websocketSession) drainIdle() {
	if w.conn == nil {
		return
	}
	for {
		select {
		case frame := <-w.incoming:
			var event struct {
				Type string `json:"type"`
			}
			err := json.Unmarshal(frame.data, &event)
			// A closed/expired idle connection can be replaced before sending any work.
			// Unsolicited response frames must never satisfy a new request.
			if frame.err != nil || err != nil || strings.HasPrefix(event.Type, "response.") || event.Type == "error" {
				w.reset()
				return
			}
		default:
			return
		}
	}
}
