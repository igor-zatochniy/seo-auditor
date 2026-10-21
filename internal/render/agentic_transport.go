package render

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

const maxAgenticAXMessageBytes = 4 << 20
const maxAgenticAXTotalBytes = 8 << 20
const maxAgenticAXMessages = 64
const agenticAXTimeout = 5 * time.Second

var errAgenticAXLimit = errors.New("accessibility CDP response exceeds budget")

// AX uses its own connection: closing an oversized response must not cancel
// the main rendering session or let chromedp buffer that response first.
func loadBoundedAXTree(ctx context.Context, wsURL string, targetID target.ID) (agenticAXTree, error) {
	ctx, cancel := context.WithTimeout(ctx, agenticAXTimeout)
	defer cancel()
	dialer := ws.Dialer{Host: "localhost", Timeout: agenticAXTimeout}
	conn, buffered, _, err := dialer.Dial(ctx, wsURL)
	if err != nil {
		return agenticAXTree{}, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return agenticAXTree{}, err
	}
	client := agenticCDPConn{conn: conn, source: conn, remainingBytes: maxAgenticAXTotalBytes, remainingMessages: maxAgenticAXMessages}
	if buffered != nil {
		client.source = buffered
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err := client.execute("Target.attachToTarget", "", map[string]any{"targetId": targetID, "flatten": true}, &attached); err != nil {
		return agenticAXTree{}, err
	}
	if attached.SessionID == "" {
		return agenticAXTree{}, errors.New("missing accessibility session")
	}
	if err := client.execute("Accessibility.enable", attached.SessionID, nil, nil); err != nil {
		return agenticAXTree{}, err
	}
	var tree agenticAXTree
	err = client.execute("Accessibility.getFullAXTree", attached.SessionID, nil, &tree)
	return tree, err
}

type agenticCDPConn struct {
	conn              net.Conn
	source            io.Reader
	nextID            int64
	remainingBytes    int64
	remainingMessages int
}

func (c *agenticCDPConn) execute(method, session string, params, result any) error {
	c.nextID++
	request, err := json.Marshal(struct {
		ID        int64  `json:"id"`
		Method    string `json:"method"`
		SessionID string `json:"sessionId,omitempty"`
		Params    any    `json:"params,omitempty"`
	}{c.nextID, method, session, params})
	if err != nil {
		return err
	}
	if err := wsutil.WriteClientText(c.conn, request); err != nil {
		return err
	}
	for c.remainingMessages > 0 && c.remainingBytes > 0 {
		c.remainingMessages--
		reader := wsutil.Reader{Source: c.source, State: ws.StateClientSide, MaxFrameSize: maxAgenticAXMessageBytes}
		header, err := reader.NextFrame()
		if errors.Is(err, wsutil.ErrFrameTooLarge) {
			return errAgenticAXLimit
		}
		if err != nil {
			return err
		}
		if header.OpCode != ws.OpText {
			return errors.New("unexpected accessibility websocket frame")
		}
		limit := min(int64(maxAgenticAXMessageBytes), c.remainingBytes)
		body, err := io.ReadAll(io.LimitReader(&reader, limit+1))
		if errors.Is(err, wsutil.ErrFrameTooLarge) || int64(len(body)) > limit {
			return errAgenticAXLimit
		}
		if err != nil {
			return err
		}
		c.remainingBytes -= int64(len(body))
		var response struct {
			ID        int64           `json:"id"`
			SessionID string          `json:"sessionId"`
			Result    json.RawMessage `json:"result"`
			Error     *struct {
				Code int `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(body, &response); err != nil {
			return err
		}
		if response.ID != c.nextID || response.SessionID != session {
			continue
		}
		if response.Error != nil {
			return fmt.Errorf("accessibility CDP error %d", response.Error.Code)
		}
		if result != nil {
			return json.Unmarshal(response.Result, result)
		}
		return nil
	}
	return errAgenticAXLimit
}
