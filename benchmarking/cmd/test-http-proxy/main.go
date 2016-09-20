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
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"time"
)

var (
	port           = flag.Uint("port", 8080, "Port to listen for HTTP requests")
	endpoint       = flag.String("endpoint", "/endpoint", "Name of the endpoint that HTTP requests can be sent to")
	target         = flag.String("target", "", "Target for the HTTP requests. Must be a valid URL.")
	connectTimeout = flag.Uint("connect-timeout", 1000, "Number of milliseconds to permit connection attempts before timing out.")
)

func main() {
	flag.Parse()

	if *target == "" {
		fmt.Fprintf(os.Stderr, "No target URL provided\n")
		flag.Usage()
		return
	}
	// Check that we can build requests using the provided URL
	_, err := http.NewRequest("GET", *target, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating request with '%s': %s\n", *target, err)
		return
	}

	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		fmt.Fprintf(os.Stderr, "Error asserting http.DefaultTransport\n")
		return
	}
	cbc := &byteCounter{}
	dialer := timedDialer{
		dial: (&net.Dialer{Timeout: time.Duration(*connectTimeout) * time.Millisecond}).Dial,
		bc:   cbc,
	}
	defaultTransport.Dial = dialer.Dial
	client := &http.Client{Transport: newTimedRoundTripper(http.DefaultTransport)}

	numWorkers := int(*connectTimeout) / 10
	if numWorkers < runtime.NumCPU()*8 {
		numWorkers = runtime.NumCPU() * 8
	}
	wg := &sync.WaitGroup{}
	ch := make(chan wrappedHandler, numWorkers)
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for handler := range ch {
				outReq, err := http.NewRequest("GET", *target, nil)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Error creating request with '%s': %s\n", *target, err)
					close(handler.done)
					continue
				}
				outReq.Close = true
				if id := handler.req.Header.Get("X-Request-ID"); id != "" {
					outReq.Header.Set("X-Request-ID", id)
				}
				resp, err := client.Do(outReq)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Error from request to target '%s': %s\n", *target, err)
					close(handler.done)
					continue
				}
				body, err := ioutil.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil {
					handler.res.WriteHeader(http.StatusBadGateway)
					close(handler.done)
					continue
				}
				handler.res.WriteHeader(http.StatusOK)
				handler.res.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
				io.Copy(handler.res, bytes.NewReader(body))
				close(handler.done)
			}
		}()
	}

	http.HandleFunc(*endpoint, func(w http.ResponseWriter, r *http.Request) {
		doneCh := make(chan struct{})
		before := time.Now()
		ch <- wrappedHandler{req: r, res: w, done: doneCh}
		after := time.Now()
		if after.Sub(before) > 1*time.Second {
			fmt.Println("Request delayed by", after.Sub(before))
		}
		for range doneCh {
		}
	})

	server := http.Server{Addr: fmt.Sprintf(":%d", *port)}
	server.SetKeepAlivesEnabled(false)
	errCh := make(chan struct{})
	bc := &byteCounter{}
	go func() {
		defer close(errCh)
		ln, err := net.Listen("tcp", server.Addr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error from net.Listen: %s\n", err)
			return
		}
		err = server.Serve(&timedListener{Listener: ln, bc: bc})
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
			if rx == 0 && tx == 0 {
				if !idle {
					idleTime = time.Now()
				}
				idle = true
				continue
			}
			if idle {
				fmt.Println("Idle for", time.Now().Sub(idleTime))
				idle = false
			}
			fmt.Println("Bytes sent:", tx, "bytes received:", rx)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, os.Kill)

	select {
	case <-sigCh:
	case <-errCh:
	}
}

type wrappedHandler struct {
	req  *http.Request
	res  http.ResponseWriter
	done chan<- struct{}
}

type timedRoundTripper struct {
	rt http.RoundTripper
}

func newTimedRoundTripper(rt http.RoundTripper) http.RoundTripper {
	return timedRoundTripper{rt: rt}
}

func (t timedRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// fmt.Println("ReqTS:", time.Now())
	res, err := t.rt.RoundTrip(req)
	// fmt.Println("ResTS:", time.Now())
	return res, err
}

type timedDialer struct {
	dial func(network, addr string) (net.Conn, error)
	bc   *byteCounter
}

func (td timedDialer) Dial(network, addr string) (net.Conn, error) {
	// fmt.Println("DialStart:", time.Now())
	conn, err := td.dial(network, addr)
	// connTime := time.Now()
	// fmt.Println("DialEnd:  ", connTime)
	return &timedConn{Conn: conn, openTS: time.Now(), bc: td.bc}, err
}

type timedListener struct {
	net.Listener
	bc *byteCounter
}

func (tl *timedListener) Accept() (net.Conn, error) {
	conn, err := tl.Listener.Accept()
	return &timedConn{Conn: conn, openTS: time.Now(), bc: tl.bc}, err
}

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

type byteCounter struct {
	m  sync.Mutex
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
