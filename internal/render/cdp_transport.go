package render

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/chromedp/cdproto"
	"github.com/chromedp/chromedp"
	jsonv2 "github.com/go-json-experiment/json"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

const maxCDPMessageBytes = 4 << 20
const maxCDPTotalBytes = 8 << 20
const maxCDPMessages = 8192

var errCDPBudget = errors.New("перевищено ліміт основного CDP transport; rendering зупинено")

// Each rendering/performance pass has its own lifetime budget. Check frame
// headers and fragmented messages before chromedp can buffer or decode them.
type boundedCDPConn struct {
	conn              net.Conn
	source            io.Reader
	remainingBytes    int64
	remainingMessages int
	onLimit           func()
	stop              func() bool
	writeMu           sync.Mutex
}

func dialBoundedCDP(dialCtx, lifetime context.Context, endpoint string, onLimit func()) (chromedp.Transport, error) {
	dialer := ws.Dialer{Host: "localhost", Timeout: 5 * time.Second}
	conn, buffered, _, err := dialer.Dial(dialCtx, endpoint)
	if err != nil {
		return nil, err
	}
	c := &boundedCDPConn{
		conn: conn, source: conn, remainingBytes: maxCDPTotalBytes,
		remainingMessages: maxCDPMessages, onLimit: onLimit,
	}
	if buffered != nil {
		c.source = buffered
	}
	c.stop = context.AfterFunc(lifetime, func() { _ = conn.Close() })
	return c, nil
}

func (c *boundedCDPConn) Close() error {
	if c.stop != nil {
		c.stop()
	}
	return c.conn.Close()
}

func (c *boundedCDPConn) limitError() error {
	if c.onLimit != nil {
		c.onLimit()
	}
	_ = c.Close()
	return errCDPBudget
}

func (c *boundedCDPConn) Read(ctx context.Context, msg *cdproto.Message) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if c.remainingBytes <= 0 || c.remainingMessages <= 0 {
			return c.limitError()
		}
		c.remainingMessages--
		limit := min(int64(maxCDPMessageBytes), c.remainingBytes)
		reader := wsutil.Reader{Source: c.source, State: ws.StateClientSide, MaxFrameSize: limit}
		header, err := reader.NextFrame()
		if errors.Is(err, wsutil.ErrFrameTooLarge) {
			return c.limitError()
		}
		if err != nil {
			return err
		}
		if header.OpCode == ws.OpClose {
			return io.EOF
		}
		if header.OpCode != ws.OpText && header.OpCode != ws.OpPing && header.OpCode != ws.OpPong {
			return errors.New("unexpected main CDP WebSocket frame")
		}
		body, err := io.ReadAll(io.LimitReader(&reader, limit+1))
		if errors.Is(err, wsutil.ErrFrameTooLarge) || int64(len(body)) > limit {
			return c.limitError()
		}
		if err != nil {
			return err
		}
		c.remainingBytes -= int64(len(body))
		if header.OpCode == ws.OpPing {
			c.writeMu.Lock()
			err = wsutil.WriteClientMessage(c.conn, ws.OpPong, body)
			c.writeMu.Unlock()
			if err != nil {
				return err
			}
			continue
		}
		if header.OpCode == ws.OpPong {
			continue
		}
		return jsonv2.Unmarshal(body, msg, chromedp.DefaultUnmarshalOptions)
	}
}

func (c *boundedCDPConn) Write(ctx context.Context, msg *cdproto.Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	body, err := jsonv2.Marshal(msg, chromedp.DefaultMarshalOptions)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return wsutil.WriteClientText(c.conn, body)
}
