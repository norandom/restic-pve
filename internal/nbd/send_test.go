package nbd

import (
	"net"
	"testing"
	"time"
)

// The client has no write command: send refuses WRITE (1), WRITE_ZEROES (6) and every other
// type but read, disc, trim and block status, before anything reaches the connection.
func TestSendRefusesWrites(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	c := &Client{conn: a}
	got := make(chan int, 1)
	go func() {
		b.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		n, _ := b.Read(make([]byte, 64))
		got <- n
	}()
	for _, cmd := range []uint16{1, 3, 5, 6, 8, 0xffff} {
		if _, err := c.send(cmd, 0, 4096); err == nil {
			t.Fatalf("command %d sent", cmd)
		}
	}
	if n := <-got; n != 0 {
		t.Fatalf("%d bytes reached the connection", n)
	}
}
