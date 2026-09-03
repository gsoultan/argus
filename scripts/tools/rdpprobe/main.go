package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/gsoultan/argus/internal/rdp"
)

func cr(cookie string, protocols uint32) []byte {
	v := []byte("Cookie: mstshash=" + cookie + "\r\n")
	neg := make([]byte, 8)
	neg[0] = 0x01
	binary.LittleEndian.PutUint16(neg[2:4], 8)
	binary.LittleEndian.PutUint32(neg[4:8], protocols)
	v = append(v, neg...)
	x := make([]byte, 7+len(v))
	x[0] = byte(len(x) - 1)
	x[1] = 0xE0
	copy(x[7:], v)
	out := make([]byte, 4+len(x))
	out[0] = 3
	binary.BigEndian.PutUint16(out[2:4], uint16(len(out)))
	copy(out[4:], x)
	return out
}

func main() {
	addr, cookie, proto := os.Args[1], os.Args[2], uint32(rdp.ProtocolSSL)
	if len(os.Args) > 3 && os.Args[3] == "weak" {
		proto = rdp.ProtocolRDP
	}
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		fmt.Println("dial:", err)
		os.Exit(1)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(8 * time.Second))
	if _, err := c.Write(cr(cookie, proto)); err != nil {
		fmt.Println("write:", err)
		os.Exit(1)
	}
	frame, err := rdp.ReadPDU(c)
	if err != nil {
		fmt.Printf("%-34s no reply (%v)\n", cookie, err)
		os.Exit(1)
	}
	p, failure, err := rdp.ParseConnectionConfirm(frame)
	switch {
	case err != nil:
		fmt.Printf("%-34s unparseable: %v\n", cookie, err)
	case failure != 0:
		fmt.Printf("%-34s REFUSED: %s\n", cookie, rdp.FailureName(failure))
	default:
		fmt.Printf("%-34s ACCEPTED over %s\n", cookie, rdp.ProtocolName(p))
	}
}
