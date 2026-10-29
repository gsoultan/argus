package rdp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

// Static and dynamic virtual channels, as far as the graphics pipeline needs
// them (MS-RDPBCGR 2.2.6, MS-RDPEDYC).
//
// The graphics pipeline does not travel on the global channel. It runs as a
// dynamic channel, which runs inside the static channel "drdynvc", which is
// carried in MCS like everything else. Only the one dynamic channel is ever
// accepted: clipboard, drives and the rest arrive as requests on the same
// static channel and are refused, for the reason clientNetworkData gives.

// ErrVirtualChannel reports virtual channel traffic this client cannot follow.
var ErrVirtualChannel = errors.New("malformed virtual channel data")

const (
	// drdynvcName is the static channel dynamic channels ride on.
	drdynvcName = "drdynvc"
	// drdynvcOptions: initialised, and the flags mstsc sets for it.
	drdynvcOptions uint32 = 0x80000000 | 0x40000000 | 0x00800000

	channelFlagFirst = 0x01
	channelFlagLast  = 0x02

	// channelChunk is the most a static channel PDU carries without the
	// server having said otherwise in its virtual channel capability.
	channelChunk = 1600

	dvcCreate      = 0x01
	dvcDataFirst   = 0x02
	dvcData        = 0x03
	dvcClose       = 0x04
	dvcCapability  = 0x05
	dvcFirstCompr  = 0x06
	dvcDataCompr   = 0x07
	dvcSoftSync    = 0x08
	dvcSoftSyncRes = 0x09

	// dvcVersion is the highest version offered. Version 3 adds compressed
	// data PDUs, in a second compression format this client does not read.
	dvcVersion = 2

	// dvcMaxMessage bounds one reassembled dynamic channel message.
	dvcMaxMessage = 64 << 20

	// statusRefused answers a channel this client will not open.
	statusRefused uint32 = 0xC0000001
)

// graphicsChannelName is the dynamic channel of the graphics pipeline.
const graphicsChannelName = "Microsoft::Windows::RDS::Graphics"

// staticChannel reassembles one static channel's chunks into messages.
type staticChannel struct {
	buf   []byte
	total int
}

// feed takes one channel PDU and returns a whole message once its last chunk
// has arrived.
func (s *staticChannel) feed(pdu []byte) ([]byte, error) {
	if len(pdu) < 8 {
		return nil, fmt.Errorf("%w: channel PDU header is truncated", ErrVirtualChannel)
	}
	total := int(binary.LittleEndian.Uint32(pdu[0:4]))
	flags := binary.LittleEndian.Uint32(pdu[4:8])
	data := pdu[8:]
	if total > dvcMaxMessage {
		return nil, fmt.Errorf("%w: a message claims %d bytes", ErrVirtualChannel, total)
	}
	if flags&channelFlagFirst != 0 {
		s.buf, s.total = s.buf[:0], total
	}
	if len(s.buf)+len(data) > s.total {
		return nil, fmt.Errorf("%w: chunks run past the %d bytes announced", ErrVirtualChannel, s.total)
	}
	s.buf = append(s.buf, data...)
	if flags&channelFlagLast == 0 {
		return nil, nil
	}
	if len(s.buf) != s.total {
		return nil, fmt.Errorf("%w: message ended at %d of %d bytes", ErrVirtualChannel, len(s.buf), s.total)
	}
	return s.buf, nil
}

// channelPDUs splits a message into static channel PDUs.
func channelPDUs(msg []byte) [][]byte {
	var out [][]byte
	for at := 0; at < len(msg) || at == 0; {
		n := min(channelChunk, len(msg)-at)
		var flags uint32
		if at == 0 {
			flags |= channelFlagFirst
		}
		if at+n == len(msg) {
			flags |= channelFlagLast
		}
		pdu := make([]byte, 8, 8+n)
		binary.LittleEndian.PutUint32(pdu[0:4], uint32(len(msg)))
		binary.LittleEndian.PutUint32(pdu[4:8], flags)
		out = append(out, append(pdu, msg[at:at+n]...))
		at += n
		if n == 0 {
			break
		}
	}
	return out
}

// dynamicChannels follows the dynamic channel protocol on drdynvc.
type dynamicChannels struct {
	// graphics is the graphics pipeline's channel id once it is open.
	graphics uint32
	open     bool
	pending  map[uint32]*bytes.Buffer
	expect   map[uint32]int
}

// dvcEvent is what one drdynvc message meant.
type dvcEvent struct {
	// reply goes back to the server on drdynvc, if not nil.
	reply []byte
	// opened is set when the graphics channel has just been accepted.
	opened bool
	// data is a whole message on the graphics channel.
	data []byte
}

func (d *dynamicChannels) handle(msg []byte) (dvcEvent, error) {
	if len(msg) < 1 {
		return dvcEvent{}, fmt.Errorf("%w: empty dynamic channel PDU", ErrVirtualChannel)
	}
	cmd, sp, cbID := msg[0]>>4, int(msg[0]>>2&3), int(msg[0]&3)
	body := msg[1:]

	switch cmd {
	case dvcCapability:
		// The server's version, then for versions 2 and 3 its priority
		// charges, which only matter to a client that sends bulk data.
		if len(body) < 3 {
			return dvcEvent{}, fmt.Errorf("%w: capabilities request is truncated", ErrVirtualChannel)
		}
		version := min(int(binary.LittleEndian.Uint16(body[1:3])), dvcVersion)
		reply := []byte{dvcCapability << 4, 0, 0, 0}
		binary.LittleEndian.PutUint16(reply[2:4], uint16(version))
		return dvcEvent{reply: reply}, nil

	case dvcCreate:
		id, rest, err := dvcField(body, cbID)
		if err != nil {
			return dvcEvent{}, err
		}
		name, _, _ := bytes.Cut(rest, []byte{0})
		status := statusRefused
		accepted := string(name) == graphicsChannelName && !d.open
		if accepted {
			status = 0
			d.graphics, d.open = id, true
		}
		reply := append([]byte{dvcCreate<<4 | byte(cbID)}, dvcPut(id, cbID)...)
		reply = binary.LittleEndian.AppendUint32(reply, status)
		return dvcEvent{reply: reply, opened: accepted}, nil

	case dvcDataFirst, dvcData:
		id, rest, err := dvcField(body, cbID)
		if err != nil {
			return dvcEvent{}, err
		}
		if !d.open || id != d.graphics {
			return dvcEvent{}, nil
		}
		if d.pending == nil {
			d.pending, d.expect = map[uint32]*bytes.Buffer{}, map[uint32]int{}
		}
		if cmd == dvcDataFirst {
			total, data, err := dvcField(rest, sp)
			if err != nil {
				return dvcEvent{}, err
			}
			if total > dvcMaxMessage {
				return dvcEvent{}, fmt.Errorf("%w: a message claims %d bytes", ErrVirtualChannel, total)
			}
			d.pending[id] = bytes.NewBuffer(make([]byte, 0, total))
			d.expect[id] = int(total)
			rest = data
		}
		buf, inProgress := d.pending[id]
		if !inProgress {
			// A whole message in one PDU.
			return dvcEvent{data: append([]byte(nil), rest...)}, nil
		}
		if buf.Len()+len(rest) > d.expect[id] {
			return dvcEvent{}, fmt.Errorf("%w: data runs past the %d bytes announced",
				ErrVirtualChannel, d.expect[id])
		}
		buf.Write(rest)
		if buf.Len() < d.expect[id] {
			return dvcEvent{}, nil
		}
		delete(d.pending, id)
		return dvcEvent{data: buf.Bytes()}, nil

	case dvcClose:
		id, _, err := dvcField(body, cbID)
		if err != nil {
			return dvcEvent{}, err
		}
		if d.open && id == d.graphics {
			d.open = false
		}
		delete(d.pending, id)
		return dvcEvent{reply: append([]byte{dvcClose<<4 | byte(cbID)}, dvcPut(id, cbID)...)}, nil

	case dvcFirstCompr, dvcDataCompr:
		return dvcEvent{}, fmt.Errorf("%w: compressed dynamic channel data, which version %d does not allow",
			ErrVirtualChannel, dvcVersion)

	case dvcSoftSync, dvcSoftSyncRes:
		return dvcEvent{}, nil
	}
	return dvcEvent{}, nil
}

// dvcData builds data PDUs carrying msg on channel id.
func dvcMessages(id uint32, msg []byte) [][]byte {
	const room = channelChunk - 6 // header byte and a four-byte id
	if len(msg) <= room {
		return [][]byte{append(append([]byte{dvcData<<4 | 2}, dvcPut(id, 2)...), msg...)}
	}
	var out [][]byte
	first := append([]byte{dvcDataFirst<<4 | 2<<2 | 2}, dvcPut(id, 2)...)
	first = append(first, dvcPut(uint32(len(msg)), 2)...)
	n := room - 4
	out = append(out, append(first, msg[:n]...))
	for at := n; at < len(msg); at += room {
		end := min(at+room, len(msg))
		out = append(out, append(append([]byte{dvcData<<4 | 2}, dvcPut(id, 2)...), msg[at:end]...))
	}
	return out
}

// dvcField reads a one, two or four byte field as sized by a cbId or Sp code.
func dvcField(b []byte, size int) (uint32, []byte, error) {
	switch size {
	case 0:
		if len(b) >= 1 {
			return uint32(b[0]), b[1:], nil
		}
	case 1:
		if len(b) >= 2 {
			return uint32(binary.LittleEndian.Uint16(b)), b[2:], nil
		}
	case 2:
		if len(b) >= 4 {
			return binary.LittleEndian.Uint32(b), b[4:], nil
		}
	default:
		return 0, nil, fmt.Errorf("%w: field size code %d", ErrVirtualChannel, size)
	}
	return 0, nil, fmt.Errorf("%w: dynamic channel PDU is truncated", ErrVirtualChannel)
}

func dvcPut(v uint32, size int) []byte {
	switch size {
	case 0:
		return []byte{byte(v)}
	case 1:
		return binary.LittleEndian.AppendUint16(nil, uint16(v))
	}
	return binary.LittleEndian.AppendUint32(nil, v)
}
