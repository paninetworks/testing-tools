package main

import (
	"fmt"
	"flag"
	"net"
	"net/http"
	"io"
	"bytes"
	"crypto/rand"
	"os"
	"os/signal"
	"sync"
	"time"
	_ "github.com/cgilmour/maxopen"
)

var (
	port = flag.Uint("port", 8080, "Port to listen for HTTP requests")
	endpoint = flag.String("endpoint", "/endpoint", "Name of the endpoint that HTTP requests can be sent to")
	responseSize = flag.Uint("response-size", 512, "Number of bytes of random data to send in HTTP responses")
)

func main() {
	flag.Parse()

	randomData := make([]byte, *responseSize)
	if _, err := rand.Read(randomData); err != nil {
		fmt.Fprintf(os.Stderr, "Initialization error: Failed to read %d bytes of random data: %s", *responseSize, err)
		return
	}

	http.HandleFunc(*endpoint, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", *responseSize))
		w.WriteHeader(http.StatusOK)
		io.Copy(w, bytes.NewReader(randomData))
	})

	server := http.Server{Addr: fmt.Sprintf(":%d", *port)}
	server.SetKeepAlivesEnabled(false)
	errCh := make(chan struct{})
	bc := &byteCounter{}
	cc := &connCounter{}
	go func() {
		defer close(errCh)
		ln, err := net.Listen("tcp", server.Addr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error from net.Listen: %s\n", err)
			return
		}
		err = server.Serve(&timedListener{Listener: ln, bc: bc, cc: cc})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error from server.Serve: %s\n", err)
			return
		}
	}()
	go func() {
		tickCh := time.Tick(100 * time.Millisecond)
		idle := false
		idleTime := time.Time{}
		for range tickCh {
			rx, tx := bc.Sample()
			n := cc.Sample()
			if rx == 0 && tx == 0 && n == 0 {
				if ! idle {
					idleTime = time.Now()
				}
				idle = true
				continue
			}
			if idle {
				fmt.Println("Idle for", time.Now().Sub(idleTime))
				idle = false
			}
			fmt.Println("connections:", n, "bytes sent:", tx, "bytes received:", rx)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, os.Kill)

	select {
	case <-sigCh:
	case <-errCh:
	}
}

type timedListener struct {
	net.Listener
	bc *byteCounter
	cc *connCounter
}

func (tl *timedListener) Accept() (net.Conn, error) {
	conn, err := tl.Listener.Accept()
	tl.cc.m.Lock()
	tl.cc.n++
	tl.cc.m.Unlock()
	if err != nil {
		fmt.Println("Accept() error was", err)
	}
	return &timedConn{Conn: conn, openTS: time.Now(), bc: tl.bc}, err
}

type connCounter struct {
	m sync.Mutex
	n int
}

func (c *connCounter) Sample() int {
	c.m.Lock()
	n := c.n
	c.n = 0
	c.m.Unlock()
	return n
}

type timedConn struct {
	net.Conn
	openTS time.Time
	closeTS time.Time
	bc *byteCounter
}

func (tc *timedConn) Read(b []byte) (int, error) {
	n, err := tc.Conn.Read(b)
	tc.bc.m.Lock()
	tc.bc.rx += n
	tc.bc.m.Unlock()
	return n, err
}

func (tc *timedConn) Write(b []byte) (int, error) {
	n, err := tc.Conn.Write(b)
	tc.bc.m.Lock()
	tc.bc.tx += n
	tc.bc.m.Unlock()
	return n, err
}

func (tc timedConn) Close() error {
	tc.closeTS = time.Now()
	// fmt.Printf("Opened: %s, Closed: %s, Delta: %s\n", tc.openTS, tc.closeTS, tc.closeTS.Sub(tc.openTS))
	return tc.Conn.Close()
}

type byteCounter struct {
	m sync.Mutex
	rx int
	tx int
}

func (bc *byteCounter) Sample() (int, int) {
	bc.m.Lock()
	rx, tx := bc.rx, bc.tx
	bc.rx, bc.tx = 0, 0
	bc.m.Unlock()
	return rx, tx
}
