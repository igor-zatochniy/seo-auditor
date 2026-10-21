package render

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

func axTestEndpoint(t *testing.T, reply func(net.Conn, int64, string)) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "localhost" {
			t.Errorf("unexpected CDP Host: %s", r.Host)
		}
		conn, _, _, err := ws.UpgradeHTTP(r, w)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		for {
			body, err := wsutil.ReadClientText(conn)
			if err != nil {
				return
			}
			var request struct {
				ID        int64  `json:"id"`
				Method    string `json:"method"`
				SessionID string `json:"sessionId"`
				Params    struct {
					TargetID target.ID `json:"targetId"`
					Flatten  bool      `json:"flatten"`
				} `json:"params"`
			}
			if err := json.Unmarshal(body, &request); err != nil {
				t.Error(err)
				return
			}
			result := any(map[string]any{})
			switch request.Method {
			case "Target.attachToTarget":
				if request.Params.TargetID != "fixture" || !request.Params.Flatten {
					t.Errorf("incorrect target attachment: %+v", request)
				}
				result = map[string]any{"sessionId": "ax-session"}
			case "Accessibility.enable":
			case "Accessibility.getFullAXTree":
				reply(conn, request.ID, request.SessionID)
				return
			default:
				t.Errorf("unexpected CDP command: %s", request.Method)
				return
			}
			response, _ := json.Marshal(map[string]any{"id": request.ID, "sessionId": request.SessionID, "result": result})
			if err := wsutil.WriteServerText(conn, response); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return strings.Replace(server.URL, "http://", "ws://", 1)
}

func TestBoundedAXTreeDecodesOnlyDedicatedSession(t *testing.T) {
	endpoint := axTestEndpoint(t, func(conn net.Conn, id int64, session string) {
		_ = wsutil.WriteServerText(conn, []byte(`{"method":"Accessibility.loadComplete","params":{}}`))
		foreign, _ := json.Marshal(map[string]any{"id": id, "sessionId": "foreign-session", "result": map[string]any{"nodes": "must not decode"}})
		_ = wsutil.WriteServerText(conn, foreign)
		body, _ := json.Marshal(map[string]any{"id": id, "sessionId": session, "result": map[string]any{"nodes": []any{
			map[string]any{"role": map[string]string{"value": "main"}, "name": map[string]string{"value": ""}},
		}}})
		_ = wsutil.WriteServerText(conn, body)
	})
	tree, err := loadBoundedAXTree(context.Background(), endpoint, "fixture")
	if err != nil || len(tree.Nodes) != 1 || axString(tree.Nodes[0].Role) != "main" {
		t.Fatalf("lost bounded AX result: %+v %v", tree, err)
	}
}

func TestBoundedAXTreeRejectsOversizedFrameBeforePayload(t *testing.T) {
	endpoint := axTestEndpoint(t, func(conn net.Conn, _ int64, _ string) {
		// Send only a header, never the 159 MB body. The reader must reject
		// immediately instead of waiting for, allocating, or decoding it.
		_ = ws.WriteHeader(conn, ws.Header{Fin: true, OpCode: ws.OpText, Length: 159281301})
		_, _ = io.Copy(io.Discard, conn)
	})
	start := time.Now()
	_, err := loadBoundedAXTree(context.Background(), endpoint, "fixture")
	if !errors.Is(err, errAgenticAXLimit) || time.Since(start) >= agenticAXTimeout {
		t.Fatalf("oversized AX frame was not rejected before its body: %v", err)
	}
}

func TestBoundedAXTreeLimitsFragmentedMessage(t *testing.T) {
	endpoint := axTestEndpoint(t, func(conn net.Conn, _ int64, _ string) {
		chunk := []byte(strings.Repeat("x", maxAgenticAXMessageBytes/2))
		for i := range 3 {
			opcode := ws.OpContinuation
			if i == 0 {
				opcode = ws.OpText
			}
			if err := ws.WriteFrame(conn, ws.Frame{Header: ws.Header{Fin: i == 2, OpCode: opcode, Length: int64(len(chunk))}, Payload: chunk}); err != nil {
				return
			}
		}
	})
	_, err := loadBoundedAXTree(context.Background(), endpoint, "fixture")
	if !errors.Is(err, errAgenticAXLimit) {
		t.Fatalf("fragmentation bypassed message budget: %v", err)
	}
}

func TestBoundedAXTreeHonorsCancellation(t *testing.T) {
	endpoint := axTestEndpoint(t, func(conn net.Conn, _ int64, _ string) { _, _ = io.Copy(io.Discard, conn) })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := loadBoundedAXTree(ctx, endpoint, "fixture"); err == nil || time.Since(start) > time.Second {
		t.Fatalf("AX read ignored cancellation: %v", err)
	}
}

func TestAgenticCDPLimitsTotalMessagesAndBytes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		bytes    int64
		messages int
	}{
		{"message_count", maxAgenticAXTotalBytes, 2},
		{"total_bytes", 40, maxAgenticAXMessages},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()
			_ = client.SetDeadline(time.Now().Add(time.Second))
			done := make(chan struct{})
			go func() {
				defer close(done)
				if _, err := wsutil.ReadClientText(server); err != nil {
					return
				}
				for {
					if err := wsutil.WriteServerText(server, []byte(`{"method":"AX.event"}`)); err != nil {
						return
					}
				}
			}()
			c := agenticCDPConn{conn: client, source: client, remainingBytes: tc.bytes, remainingMessages: tc.messages}
			err := c.execute("Accessibility.getFullAXTree", "ax-session", nil, nil)
			_ = client.Close()
			<-done
			if !errors.Is(err, errAgenticAXLimit) {
				t.Fatalf("ignored aggregate budget: %v", err)
			}
		})
	}
}
