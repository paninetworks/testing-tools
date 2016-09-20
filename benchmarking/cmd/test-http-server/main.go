// Copyright (c) 2016 Pani Networks
// All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
// License for the specific language governing permissions and limitations
// under the License.

package main

import (
	"bytes"
	"crypto/rand"
	"flag"
	"fmt"
	_ "github.com/cgilmour/maxopen"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"time"
)

var (
	port         = flag.Uint("port", 8080, "Port to listen for HTTP requests")
	endpoint     = flag.String("endpoint", "/endpoint", "Name of the endpoint that HTTP requests can be sent to")
	responseSize = flag.Uint("response-size", 512, "Number of bytes of random data to send in HTTP responses")
)

func main() {
	flag.Parse()

	// Generate 'responseSize' bytes of random data.
	// This can be slow for large response sizes, delaying startup.
	randomData := make([]byte, *responseSize)
	if _, err := rand.Read(randomData); err != nil {
		fmt.Fprintf(os.Stderr, "Initialization error: Failed to read %d bytes of random data: %s", *responseSize, err)
		return
	}

	// a simple HandleFunc that responds to a request with a Content-Length (to prevent chunk encoding),
	// an OK status and the contents of randomData.
	http.HandleFunc(*endpoint, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", *responseSize))
		w.WriteHeader(http.StatusOK)
		io.Copy(w, bytes.NewReader(randomData))
	})

	// Initialize a server
	server := http.Server{Addr: fmt.Sprintf(":%d", *port)}
	server.SetKeepAlivesEnabled(false)
	errCh := make(chan struct{})

	// Counters for reporting server activity at regular intervals.
	bc := &byteCounter{}
	cc := &connCounter{}

	// try to start the server.
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

	// check every 100ms if there's something to report.
	go func() {
		tickCh := time.Tick(100 * time.Millisecond)
		idle := false
		idleTime := time.Time{}
		for range tickCh {
			rx, tx := bc.Sample()
			n := cc.Sample()
			if rx == 0 && tx == 0 && n == 0 {
				// transition to idle state
				if !idle {
					idleTime = time.Now()
				}
				idle = true
				continue
			}
			if idle {
				// transition from idle state
				fmt.Println("Idle for", time.Now().Sub(idleTime))
				idle = false
			}
			fmt.Println("connections:", n, "bytes sent:", tx, "bytes received:", rx)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, os.Kill)

	// stop on a ^C or an error while initializing or running the server.
	select {
	case <-sigCh:
	case <-errCh:
	}
}


// this is a net.Listener that captures information for connections that arrive on the server.
type timedListener struct {
	net.Listener
	bc *byteCounter
	cc *connCounter
}

func (tl *timedListener) Accept() (net.Conn, error) {
	conn, err := tl.Listener.Accept()
	// increase connection count for this sample
	tl.cc.m.Lock()
	tl.cc.n++
	tl.cc.m.Unlock()
	if err != nil {
		fmt.Println("Accept() error was", err)
	}
	// wrap the connection, so it can capture additional byteCounter information
	return &timedConn{Conn: conn, openTS: time.Now(), bc: tl.bc}, err
}

type connCounter struct {
	m sync.Mutex
	n int
}

// Sample() reports and resets the number of connections since it was last called.
func (c *connCounter) Sample() int {
	c.m.Lock()
	n := c.n
	c.n = 0
	c.m.Unlock()
	return n
}

// a timedConnection captures when the connection was opened/closed, and increases counters during
// Read() and Write() calls.
type timedConn struct {
	net.Conn
	openTS  time.Time
	closeTS time.Time
	bc      *byteCounter
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

// The byteCounter captures bytes read and written for the timedConn it is related to.
type byteCounter struct {
	m  sync.Mutex
	rx int
	tx int
}

// Sample() reports and resets the number of bytes read and written since it was last called
func (bc *byteCounter) Sample() (int, int) {
	bc.m.Lock()
	rx, tx := bc.rx, bc.tx
	bc.rx, bc.tx = 0, 0
	bc.m.Unlock()
	return rx, tx
}
