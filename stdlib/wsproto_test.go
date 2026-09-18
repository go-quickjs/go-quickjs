package stdlib

import (
	"bytes"
	"io"
	"net"
	"strings"
	"testing"
)

// pair returns two ends of a connection speaking the protocol at each other,
// the first masking as a client does.
func pair(t *testing.T) (*wsConn, *wsConn) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	return newWSConn(a, nil, true, 1<<20), newWSConn(b, nil, false, 1<<20)
}

func TestWSMessages(t *testing.T) {
	client, server := pair(t)

	for _, tc := range []struct {
		name string
		op   byte
		data []byte
	}{
		{"text", opText, []byte("hello")},
		{"empty", opText, nil},
		// The three lengths a frame header has: one byte, two, and eight.
		{"125 bytes", opBinary, bytes.Repeat([]byte("x"), 125)},
		{"126 bytes", opBinary, bytes.Repeat([]byte("y"), 126)},
		{"70k", opBinary, bytes.Repeat([]byte("z"), 70000)},
		{"not ascii", opText, []byte("héllo — 世界")},
	} {
		go func() {
			if err := client.writeMessage(tc.op, tc.data); err != nil {
				t.Errorf("%s: write: %v", tc.name, err)
			}
		}()
		op, got, err := server.readMessage()
		if err != nil {
			t.Fatalf("%s: read: %v", tc.name, err)
		}
		if op != tc.op || !bytes.Equal(got, tc.data) {
			t.Errorf("%s: got op %d, %d bytes; want op %d, %d bytes",
				tc.name, op, len(got), tc.op, len(tc.data))
		}
	}
}

// What a client sends is masked and what a server sends is not, which is what
// the standard asks of each end.
func TestWSMasking(t *testing.T) {
	client, server := pair(t)

	go client.writeMessage(opText, []byte("from the client"))
	fin, op, data, err := server.readFrame()
	if err != nil || !fin || op != opText || string(data) != "from the client" {
		t.Fatalf("frame = %v %d %q, %v", fin, op, data, err)
	}

	// Reading the raw bytes the other way shows the difference: a header with
	// the mask bit clear, and the payload in the clear behind it.
	const said = "from the server"
	var seen []byte
	done := make(chan struct{})
	go func() {
		defer close(done)
		head := make([]byte, 2)
		if _, err := io.ReadFull(client.conn, head); err != nil {
			return
		}
		body := make([]byte, len(said))
		if _, err := io.ReadFull(client.conn, body); err != nil {
			return
		}
		seen = append(head, body...)
	}()
	server.writeMessage(opText, []byte(said))
	<-done
	if len(seen) < 2 {
		t.Fatal("nothing arrived")
	}
	if seen[1]&0x80 != 0 {
		t.Error("the server masked a frame, which a server may not do")
	}
	if !strings.Contains(string(seen), said) {
		t.Errorf("the payload is not there in the clear: %q", seen)
	}
}

// A message that arrives in pieces is one message, and a ping in the middle of
// it is answered without disturbing it.
func TestWSFragmentsAndPings(t *testing.T) {
	client, server := pair(t)

	go func() {
		client.writeFrameFor(t, false, opText, []byte("one "))
		client.writeFrameFor(t, true, opPing, []byte("are you there"))
		client.writeFrameFor(t, false, opContinuation, []byte("two "))
		client.writeFrameFor(t, true, opContinuation, []byte("three"))
	}()

	// The server answers the ping while it is reading, so the pong is read
	// here before the message can be returned.
	go func() {
		for {
			fin, op, data, err := client.readFrame()
			if err != nil {
				return
			}
			if fin && op == opPong && string(data) == "are you there" {
				return
			}
		}
	}()

	op, got, err := server.readMessage()
	if err != nil {
		t.Fatal(err)
	}
	if op != opText || string(got) != "one two three" {
		t.Errorf("message = %d %q", op, got)
	}
}

// A close frame carries a code and a reason, which is how a finished
// conversation is told from a dropped connection.
func TestWSClose(t *testing.T) {
	client, server := pair(t)
	go client.sendClose(4000, "that is all")
	op, payload, err := server.readMessage()
	if err != nil {
		t.Fatal(err)
	}
	if op != opClose {
		t.Fatalf("op = %d", op)
	}
	if code, reason := closeInfo(payload); code != 4000 || reason != "that is all" {
		t.Errorf("close = %d %q", code, reason)
	}
	// A close frame with nothing in it means nobody said why.
	if code, reason := closeInfo(nil); code != 1005 || reason != "" {
		t.Errorf("empty close = %d %q", code, reason)
	}
}

// A peer that announces more than the limit is refused rather than believed.
func TestWSLimit(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	client := newWSConn(a, nil, true, 8)
	server := newWSConn(b, nil, false, 8)

	go client.writeMessage(opBinary, bytes.Repeat([]byte("x"), 64))
	if _, _, err := server.readMessage(); err == nil {
		t.Error("a message over the limit was read")
	}
}

// The handshake answer is the hash the standard specifies, which is how each
// end knows the other understood the request.
func TestWSAccept(t *testing.T) {
	// The example from RFC 6455.
	if got := wsAccept("dGhlIHNhbXBsZSBub25jZQ=="); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Errorf("accept = %q", got)
	}
	key, err := wsKey()
	if err != nil || len(key) != 24 {
		t.Errorf("key = %q, %v", key, err)
	}
	if again, _ := wsKey(); again == key {
		t.Error("two keys came out the same")
	}
}

// writeFrameFor writes one frame of a message, for the tests that need the
// pieces rather than the whole.
func (c *wsConn) writeFrameFor(t *testing.T, fin bool, op byte, data []byte) {
	t.Helper()
	head := []byte{op, 0x80 | byte(len(data))}
	if fin {
		head[0] |= 0x80
	}
	key := []byte{1, 2, 3, 4}
	body := make([]byte, len(data))
	for i := range data {
		body[i] = data[i] ^ key[i%4]
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if _, err := c.conn.Write(append(append(head, key...), body...)); err != nil {
		t.Errorf("write: %v", err)
	}
}
