package main

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/coder/websocket"
)

func main() {
	url := os.Args[1]
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	// Development only: the gateway presents a self-signed certificate here.
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
	c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		fmt.Println("dial:", err)
		os.Exit(1)
	}
	defer c.CloseNow()
	c.SetReadLimit(32 << 20)

	var rects, pixels, msgs int
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		typ, data, err := c.Read(ctx)
		if err != nil {
			break
		}
		if typ != websocket.MessageBinary {
			continue
		}
		msgs++
		for at := 0; at+12 <= len(data); {
			kind := data[at]
			w := int(binary.LittleEndian.Uint16(data[at+6 : at+8]))
			h := int(binary.LittleEndian.Uint16(data[at+8 : at+10]))
			switch kind {
			case 1:
				n := w * h * 4
				if at+12+n > len(data) {
					fmt.Println("TRUNCATED FRAME")
					os.Exit(1)
				}
				rects++
				pixels += w * h
				at += 12 + n
			case 3:
				fmt.Printf("ready       %dx%d\n", w, h)
				at += 12
			case 4:
				fmt.Println("closed")
				at += 12
			default:
				at += 12
			}
		}
		if rects > 0 && msgs > 2 {
			break
		}
	}
	fmt.Printf("messages    %d\nrectangles  %d\npixels      %d\n", msgs, rects, pixels)
	if rects == 0 {
		fmt.Println("NO GRAPHICS")
		os.Exit(1)
	}
	fmt.Println("OK")
}
