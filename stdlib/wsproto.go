package stdlib

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
)

// The WebSocket protocol, RFC 6455, as much of it as a message needs.
//
// It is here rather than behind a dependency because it is small: a frame is a
// header of at most fourteen bytes and a payload, and the handshake is one HTTP
// request with a hash in the answer.

const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

// wsMagic is the string the standard says to append to the client's key before
// hashing it, which is how each end proves it is speaking this protocol and not
// answering a request it did not understand.
const wsMagic = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

func wsAccept(key string) string {
	sum := sha1.Sum([]byte(key + wsMagic))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// wsConn is one open socket.
//
// Reads happen on whichever goroutine is doing the reading, one at a time;
// writes are serialised, because a frame that is interleaved with another is
// not a frame at all.
type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
	// mask says whether what this end sends must be masked, which is what the
	// standard asks of a client and forbids of a server.
	mask bool
	// limit bounds one message, so that a peer cannot ask for all the memory
	// there is by announcing a large one.
	limit int

	wmu    sync.Mutex
	closed bool
}

func newWSConn(conn net.Conn, br *bufio.Reader, mask bool, limit int) *wsConn {
	if br == nil {
		br = bufio.NewReader(conn)
	}
	if limit <= 0 {
		limit = 16 << 20
	}
	return &wsConn{conn: conn, br: br, mask: mask, limit: limit}
}

// readMessage returns the next message, joining the frames a fragmented one
// arrives in and answering the control frames that arrive in between.
func (c *wsConn) readMessage() (byte, []byte, error) {
	var kind byte
	var payload []byte
	for {
		fin, op, data, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}
		switch op {
		case opPing:
			if err := c.writeMessage(opPong, data); err != nil {
				return 0, nil, err
			}
			continue
		case opPong:
			continue
		case opClose:
			return opClose, data, nil
		case opText, opBinary:
			kind = op
			payload = data
		case opContinuation:
			if kind == 0 {
				return 0, nil, errors.New("a continuation frame began a message")
			}
			payload = append(payload, data...)
		default:
			return 0, nil, fmt.Errorf("unknown frame type %d", op)
		}
		if len(payload) > c.limit {
			return 0, nil, errors.New("the message is larger than this runtime will read")
		}
		if fin {
			return kind, payload, nil
		}
	}
}

// readFrame reads one frame, unmasking it if it was masked.
func (c *wsConn) readFrame() (bool, byte, []byte, error) {
	var head [2]byte
	if _, err := io.ReadFull(c.br, head[:]); err != nil {
		return false, 0, nil, err
	}
	fin := head[0]&0x80 != 0
	op := head[0] & 0x0f
	masked := head[1]&0x80 != 0
	length := int64(head[1] & 0x7f)

	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = int64(binary.BigEndian.Uint64(ext[:]))
	}
	if length < 0 || length > int64(c.limit) {
		return false, 0, nil, errors.New("the frame is larger than this runtime will read")
	}

	var key [4]byte
	if masked {
		if _, err := io.ReadFull(c.br, key[:]); err != nil {
			return false, 0, nil, err
		}
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(c.br, data); err != nil {
		return false, 0, nil, err
	}
	if masked {
		for i := range data {
			data[i] ^= key[i%4]
		}
	}
	return fin, op, data, nil
}

// writeMessage writes one whole message as a single frame.
func (c *wsConn) writeMessage(op byte, data []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closed {
		return errors.New("the socket is closed")
	}

	head := make([]byte, 0, 14)
	head = append(head, 0x80|op)
	maskBit := byte(0)
	if c.mask {
		maskBit = 0x80
	}
	switch n := len(data); {
	case n < 126:
		head = append(head, maskBit|byte(n))
	case n <= 0xffff:
		head = append(head, maskBit|126, byte(n>>8), byte(n))
	default:
		head = append(head, maskBit|127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		head = append(head, ext[:]...)
	}

	body := data
	if c.mask {
		var key [4]byte
		if _, err := rand.Read(key[:]); err != nil {
			return err
		}
		head = append(head, key[:]...)
		// The payload is copied before it is masked: what was handed in
		// belongs to the caller.
		body = make([]byte, len(data))
		for i := range data {
			body[i] = data[i] ^ key[i%4]
		}
	}
	if _, err := c.conn.Write(head); err != nil {
		return err
	}
	if len(body) > 0 {
		if _, err := c.conn.Write(body); err != nil {
			return err
		}
	}
	return nil
}

// sendClose says goodbye with a code and a reason, which is what lets the other
// end tell a finished conversation from a dropped connection.
func (c *wsConn) sendClose(code int, reason string) error {
	if code == 0 {
		code = 1000
	}
	payload := make([]byte, 2, 2+len(reason))
	binary.BigEndian.PutUint16(payload, uint16(code))
	payload = append(payload, reason...)
	return c.writeMessage(opClose, payload)
}

func (c *wsConn) close() {
	c.wmu.Lock()
	already := c.closed
	c.closed = true
	c.wmu.Unlock()
	if !already {
		c.conn.Close()
	}
}

// closeInfo reads the code and reason out of a close frame's payload.
func closeInfo(payload []byte) (int, string) {
	if len(payload) < 2 {
		return 1005, ""
	}
	return int(binary.BigEndian.Uint16(payload)), string(payload[2:])
}

// wsKey is a fresh client key, which is not a secret: it is there so that a
// cache or a proxy cannot answer the handshake from something it remembered.
func wsKey() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b[:]), nil
}
