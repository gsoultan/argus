package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/coder/websocket"

	"github.com/gsoultan/argus/internal/rdp"
)

// capture keeps every display message for a while, as the console would
// have received it, for a browser to decode afterwards (web/scripts/gfx-check).
// Each message is written as its length, a uint32 little-endian, then its
// bytes.
//
// It moves the pointer about meanwhile, so the screen has something to send
// after its first picture.
func capture(ctx context.Context, c *websocket.Conn, send func(any), path string, d time.Duration) {
	f, err := os.Create(path)
	if err != nil {
		fail("%v", err)
	}
	defer f.Close()

	// Read on its own goroutine: cancelling a read closes the connection, so
	// the time limit is kept here instead.
	type message struct {
		typ  websocket.MessageType
		data []byte
		err  error
	}
	messages := make(chan message, 64)
	go func() {
		for {
			typ, data, err := c.Read(ctx)
			messages <- message{typ, data, err}
			if err != nil {
				return
			}
		}
	}()

	kinds := map[byte]int{}
	total := 0
	var timeUp <-chan time.Time
	for done := false; !done; {
		var msg message
		select {
		case <-timeUp:
			done = true
			continue
		case msg = <-messages:
		}
		typ, data, err := msg.typ, msg.data, msg.err
		if err != nil {
			fail("the desktop ended after %d bytes: %v", total, err)
		}
		if typ == websocket.MessageText {
			var m struct{ Type, Data, Session string }
			_ = json.Unmarshal(data, &m)
			switch m.Type {
			case "ready":
				fmt.Fprintf(os.Stderr, "session=%s\n", m.Session)
			case "error":
				fail("refused: %s", m.Data)
			}
			continue
		}
		var n [4]byte
		binary.LittleEndian.PutUint32(n[:], uint32(len(data)))
		if _, err := f.Write(append(n[:], data...)); err != nil {
			fail("%v", err)
		}
		total += len(data)
		for at := 0; at+rdp.FrameHeaderSize <= len(data); {
			kind := data[at]
			kinds[kind]++
			size := frameSize(data[at:])
			if size <= 0 {
				break
			}
			at += size
			if kind == rdp.FrameReady && timeUp == nil {
				timeUp = time.After(d)
				go wander(ctx, send, d)
			}
			if kind == rdp.FrameClosed {
				fail("the desktop closed while it was being captured")
			}
		}
	}
	fmt.Fprintf(os.Stderr, "captured %d bytes: %d graphics commands (%d in parts), %d bitmap frames\n",
		total, kinds[rdp.FrameGraphics], kinds[rdp.FrameGraphicsPart], kinds[rdp.FrameBitmap]+kinds[rdp.FrameEncoded])
	if kinds[rdp.FrameGraphics]+kinds[rdp.FrameGraphicsPart] == 0 {
		fail("no graphics pipeline commands arrived")
	}
	_ = c.Close(websocket.StatusNormalClosure, "done")
}

// frameSize is the length of the display frame at the start of b, or 0 if
// it cannot be told.
func frameSize(b []byte) int {
	switch b[0] {
	case rdp.FrameBitmap:
		w := int(binary.LittleEndian.Uint16(b[6:8]))
		h := int(binary.LittleEndian.Uint16(b[8:10]))
		return rdp.FrameHeaderSize + w*h*4
	case rdp.FrameEncoded:
		return rdp.FrameHeaderSize + (int(binary.LittleEndian.Uint16(b[10:12]))+3)&^3
	case rdp.FrameGraphics, rdp.FrameGraphicsPart:
		return rdp.FrameHeaderSize + int((binary.LittleEndian.Uint32(b[4:8])+3)&^3)
	}
	return rdp.FrameHeaderSize
}

// wander moves the pointer in a slow circle for d.
func wander(ctx context.Context, send func(any), d time.Duration) {
	end := time.Now().Add(d)
	for i := 0; time.Now().Before(end) && ctx.Err() == nil; i++ {
		send(map[string]any{"type": "mouse", "x": 300 + (i*37)%400, "y": 300 + (i*53)%300})
		time.Sleep(100 * time.Millisecond)
	}
}
