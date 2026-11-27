package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"time"

	"browsercompute.local/runtime/internal/engine"
	"browsercompute.local/runtime/internal/protocol"
	"browsercompute.local/runtime/internal/workload"
	"github.com/coder/websocket"
)

func (s *Server) websocket(w http.ResponseWriter, r *http.Request) {
	if values := r.Header.Values("Origin"); len(values) != 1 || values[0] != origin(r) {
		problem(w, 403, "origin rejected")
		return
	}
	select {
	case s.connections <- struct{}{}:
		defer func() { <-s.connections }()
	default:
		problem(w, 429, "connection limit")
		return
	}
	select {
	case s.unauthenticated <- struct{}{}:
	default:
		problem(w, 429, "handshake limit")
		return
	}
	handshake := true
	defer func() {
		if handshake {
			<-s.unauthenticated
		}
	}()
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		problem(w, 503, "coordinator unavailable")
		return
	}
	s.sockets.Add(1)
	s.mu.Unlock()
	defer s.sockets.Done()

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	auth, stop := context.WithTimeout(ctx, 5*time.Second)
	frame, err := readFrame(auth, conn)
	stop()
	if err != nil || frame.Type != "hello" {
		return
	}
	var hello protocol.Hello
	if protocol.Decode(frame.Payload, &hello) != nil || hello.Slots < 1 || hello.Slots > 4 ||
		len(hello.Kernels) > 16 || !slices.Contains(hello.Kernels, workload.Kernel) ||
		len(hello.Token) > 512 || !match(s.worker, hello.Token) {
		_ = sendFrame(ctx, conn, protocol.Out{V: protocol.Version, Type: "error", RequestID: frame.RequestID, Payload: diagnostic("invalid hello or worker credential")})
		return
	}
	session, err := s.engine.Join(s.ctx, hello.Slots, frame.RequestID)
	if err != nil {
		_ = sendFrame(ctx, conn, protocol.Out{V: protocol.Version, Type: "error", RequestID: frame.RequestID, Payload: diagnostic("worker capacity unavailable")})
		return
	}
	<-s.unauthenticated
	handshake = false
	defer s.engine.Update(s.ctx, session.ID, engine.Update{Type: "leave"})

	writerDone := make(chan struct{})
	diagnostics := make(chan protocol.Out, 1)
	go func() {
		defer close(writerDone)
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case <-session.Done:
				return
			default:
			}
			var msg protocol.Out
			select {
			case <-ctx.Done():
				return
			case <-session.Done:
				return
			case msg = <-session.Out:
			case msg = <-diagnostics:
				_ = sendFrame(ctx, conn, msg)
				return
			}
			if sendFrame(ctx, conn, msg) != nil {
				return
			}
		}
	}()
	defer func() { cancel(); <-writerDone }()
	for {
		frame, err := readFrame(ctx, conn)
		if err != nil {
			return
		}
		u, err := update(frame)
		if err != nil {
			diagnostics <- protocol.Out{V: protocol.Version, Type: "error", RequestID: frame.RequestID, Payload: diagnostic("invalid worker message")}
			<-writerDone
			return
		}
		if s.engine.Update(ctx, session.ID, u) != nil || u.Type == "leave" {
			return
		}
	}
}

func diagnostic(message string) map[string]string { return map[string]string{"message": message} }

func readFrame(ctx context.Context, c *websocket.Conn) (protocol.Frame, error) {
	typ, data, err := c.Read(ctx)
	var f protocol.Frame
	if err != nil {
		return f, err
	}
	if typ != websocket.MessageText || protocol.Decode(data, &f) != nil || f.V != protocol.Version || len(f.RequestID) > 64 {
		return f, errors.New("invalid frame")
	}
	return f, nil
}

func sendFrame(ctx context.Context, c *websocket.Conn, msg protocol.Out) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return c.Write(ctx, websocket.MessageText, data)
}

func update(f protocol.Frame) (engine.Update, error) {
	u := engine.Update{Type: f.Type, RequestID: f.RequestID}
	switch f.Type {
	case "heartbeat", "pause", "resume", "leave":
		if protocol.Empty(f.Payload) {
			return u, nil
		}
	case "ready":
		var p protocol.Ready
		if protocol.Decode(f.Payload, &p) == nil && p.Slots != nil && *p.Slots >= 0 {
			u.Slots = *p.Slots
			return u, nil
		}
	case "result":
		var p protocol.Result
		if protocol.Decode(f.Payload, &p) == nil && p.Lease.Valid() && p.Data != nil {
			u.Lease = p.Lease
			if len(*p.Data) <= base64.StdEncoding.EncodedLen(2*64*64) {
				if b, err := base64.StdEncoding.Strict().DecodeString(*p.Data); err == nil {
					u.Data = b
				}
			}
			return u, nil
		}
	case "task_error":
		var p protocol.TaskError
		if protocol.Decode(f.Payload, &p) == nil && p.Lease.Valid() && len(p.Code) > 0 && len(p.Code) <= 32 && p.Message != nil && len(*p.Message) <= 256 {
			u.Lease = p.Lease
			return u, nil
		}
	}
	return u, errors.New("invalid worker payload")
}
