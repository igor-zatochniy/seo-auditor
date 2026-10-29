package render

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto"
	"github.com/chromedp/chromedp"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

func cdpPipe(t *testing.T, send func(net.Conn)) *boundedCDPConn {
	t.Helper()
	client, server := net.Pipe()
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer server.Close()
		send(server)
	}()
	c := &boundedCDPConn{conn: client, source: client, remainingBytes: maxCDPTotalBytes, remainingMessages: maxCDPMessages}
	t.Cleanup(func() {
		_ = c.Close()
		<-done
	})
	return c
}

func TestMainCDPRejectsOversizedHeaderBeforeBody(t *testing.T) {
	c := cdpPipe(t, func(conn net.Conn) {
		_ = ws.WriteHeader(conn, ws.Header{Fin: true, OpCode: ws.OpText, Length: 64 << 20})
		_, _ = io.Copy(io.Discard, conn)
	})
	limited := false
	c.onLimit = func() { limited = true }
	start := time.Now()
	var msg cdproto.Message
	if err := c.Read(context.Background(), &msg); !errors.Is(err, errCDPBudget) || !limited || time.Since(start) > time.Second {
		t.Fatalf("oversized header was not rejected before receiving its body: %v", err)
	}
	if msg.Method != "" || len(msg.Params) != 0 {
		t.Fatal("oversized message reached JSON decoding")
	}
}

func TestMainCDPRejectsFragmentedOversizedMessage(t *testing.T) {
	c := cdpPipe(t, func(conn net.Conn) {
		chunk := []byte(strings.Repeat(" ", maxCDPMessageBytes/2))
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
	var msg cdproto.Message
	if err := c.Read(context.Background(), &msg); !errors.Is(err, errCDPBudget) {
		t.Fatalf("fragmentation bypassed the per-message budget: %v", err)
	}
}

func TestMainCDPEnforcesAggregateBudget(t *testing.T) {
	for _, tc := range []struct {
		name     string
		bytes    int64
		messages int
	}{
		{"bytes", 55, maxCDPMessages},
		{"messages", maxCDPTotalBytes, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := cdpPipe(t, func(conn net.Conn) {
				for {
					if err := wsutil.WriteServerText(conn, []byte(`{"id":1,"result":{}}`)); err != nil {
						return
					}
				}
			})
			c.remainingBytes, c.remainingMessages = tc.bytes, tc.messages
			for range 4 {
				var msg cdproto.Message
				err := c.Read(context.Background(), &msg)
				if errors.Is(err, errCDPBudget) {
					return
				}
				if err != nil || msg.ID != 1 {
					t.Fatalf("valid CDP message failed before budget exhaustion: %v", err)
				}
			}
			t.Fatal("aggregate budget was not enforced")
		})
	}
}

func TestMainCDPDecodesFragmentedReply(t *testing.T) {
	c := cdpPipe(t, func(conn net.Conn) {
		for i, body := range []string{`{"id":7,`, `"result":{}}`} {
			opcode := ws.OpContinuation
			if i == 0 {
				opcode = ws.OpText
			}
			_ = ws.WriteFrame(conn, ws.Frame{Header: ws.Header{Fin: i == 1, OpCode: opcode, Length: int64(len(body))}, Payload: []byte(body)})
		}
	})
	var msg cdproto.Message
	if err := c.Read(context.Background(), &msg); err != nil || msg.ID != 7 || string(msg.Result) != "{}" {
		t.Fatalf("valid fragmented response changed: %+v %v", msg, err)
	}
}

func TestMainCDPTransportHookAndCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, _, err := ws.UpgradeHTTP(r, w)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, _ = io.Copy(io.Discard, conn)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var called atomic.Bool
	b, err := chromedp.NewBrowser(ctx, strings.Replace(server.URL, "http:", "ws:", 1), chromedp.WithTransportDialer(func(dialCtx context.Context, u string) (chromedp.Transport, error) {
		called.Store(true)
		return dialBoundedCDP(dialCtx, ctx, u, nil)
	}))
	if err != nil || !called.Load() {
		t.Fatalf("chromedp did not use the bounded transport: %v", err)
	}
	cancel()
	select {
	case <-b.LostConnection:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not interrupt a blocked CDP read")
	}
}
