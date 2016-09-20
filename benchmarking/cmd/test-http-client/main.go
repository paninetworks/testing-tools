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
	"flag"
	"fmt"
	_ "github.com/cgilmour/maxopen"
	"github.com/cgilmour/uuid"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"path"
	"runtime"
	"sync"
	"time"
)

var (
	requests       = flag.Uint("requests", 1, "Number of requests to send before completion.")
	connectTimeout = flag.Uint("connect-timeout", 1000, "Number of milliseconds to permit connection attempts before timing out.")
	spinupPeriod   = flag.Uint("spinup-period", 500, "Duration to slowly spin up connections (avoiding initial spike).")
	numWorkers     = flag.Uint("workers", uint(runtime.NumCPU()), "Number of worker goroutines for client connections.")
	startupDelay   = flag.Uint("startup-delay", uint(0), "Number of milliseconds to delay before starting the requests.")
)

const (
	// This is used to add a UUID to the HTTP requests for a session.
	headerRequestID = "X-Request-ID"
)

func usage() {
	_, cmd := path.Split(os.Args[0])
	fmt.Fprintf(os.Stderr, "Usage: %s [-requests=n] [-connect-timeout=n] [-spinup-period=n] [-workers=n] [-startup-delay=n] url [url...]\n", cmd)
}

func main() {
	// Set up usage function and parse command-line arguments
	flag.Usage = usage
	flag.Parse()

	// Expect a list of URLs as remaining arguments
	if len(flag.Args()) == 0 {
		fmt.Fprintf(os.Stderr, "No URLs provided\n")
		return
	}

	urls := make([]string, 0, len(flag.Args()))

	// Validate the remaining arguments, check that they're parseable URLs
	for _, param := range flag.Args() {
		if param == "" {
			fmt.Fprintf(os.Stderr, "Empty parameter provided, expected URL\n")
			flag.Usage()
			return
		}
		// Check that we can build requests using the provided URL
		_, err := http.NewRequest("GET", param, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Cannot use parameter '%s' as URL: %s\n", param, err)
			return
		}
		urls = append(urls, param)
	}

	// The sessionTransport is used by http.Client to capture per-session timing information.
	// The uuidList is for ordering these sessions when reporting the timing information
	st := &sessionTransport{m: map[string]*session{}}
	uuidList := []string{}

	// Initialize a byteCounter for overall connection activity
	bc := &byteCounter{}
	client := &http.Client{Transport: st}

	// WaitGroup and channel for worker goroutines
	wg := &sync.WaitGroup{}
	ch := make(chan *session, int(*numWorkers))

	// To avoid hammering endpoints immediately, the worker goroutines are delayed from starting by
	// the spinupInterval multiplied by worker number.
	spinupInterval := time.Duration(*spinupPeriod) * time.Millisecond / time.Duration(*numWorkers)
	for i := 0; i < int(*numWorkers); i++ {
		wg.Add(1)
		go func(factor int) {
			defer wg.Done()
			delay := time.Duration(factor) * spinupInterval
			for s := range ch {
				if delay != 0 {
					// Delay from starting on first request only.
					time.Sleep(delay)
					delay = 0
				}
				// Capture start timestamp for this session
				s.initiated = time.Now()
				s.connections = make([]*timedConnection, 0, len(urls))
				// Send requests to each target URL.
				for _, url := range urls {
					req, err := http.NewRequest("GET", url, nil)
					if err != nil {
						fmt.Fprintf(os.Stderr, "Error creating request to '%s': %s\n", url, err)
						continue
					}
					req.Close = true
					req.Header.Set(headerRequestID, s.uuid)
					resp, err := client.Do(req)
					if err != nil {
						fmt.Fprintf(os.Stderr, "Error execuing request to '%s': %s\n", url, err)
						continue
					}
					io.Copy(ioutil.Discard, resp.Body)
					resp.Body.Close()
				}
				// Capture end timestamp for this session
				s.completed = time.Now()
			}
		}(i)
	}

	// Delay overall startup.
	if *startupDelay > 0 {
		fmt.Fprintf(os.Stderr, "Delaying start for %d milliseconds", *startupDelay)
		time.Sleep(time.Duration(*startupDelay) * time.Millisecond)
	}

	// Output some information while requests are being run and data is being captured.
	// Gives an indication of the amount of traffic that's occuring.
	ticker := time.NewTicker(1 * time.Second) // 100 * time.Millisecond)
	go func() {
		for range ticker.C {
			rp, tp, rb, tb := bc.Sample()
			// TODO: Improve the accuracy of this. It's Write()'s and Read()'s, not Packets.
			fmt.Fprintln(os.Stderr, "Packets sent:", tp, "received", rp, "Bytes sent:", tb, "bytes received:", rb)
		}
	}()

	// Start submitting work to the queue.
	for i := uint(0); i < *requests; i++ {
		u, err := uuid.New4()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error creating UUID: %s\n", err)
			continue
		}
		s := newSession(u, bc)
		st.mu.Lock()
		st.m[u] = s
		st.mu.Unlock()
		uuidList = append(uuidList, u)
		ch <- s
	}
	// Close the channel when all work is submitted.
	close(ch)
	// Stop reporting information
	ticker.Stop()
	// Wait for goroutines to finish.
	wg.Wait()

	// Summary headers
	fmt.Printf("Session\tStartTime")
	for i := range urls {
		fmt.Printf("\tC%[1]dConnSetup\tC%[1]dReqSent\tC%[1]dRespStarted\tC%[1]dRespCompleted", i+1)
	}
	fmt.Printf("\tDuration\n")

	// Output the data collected for all the sessions.
	// This should be redirected to a file for large numbers of requests.
	for _, v := range uuidList {
		s := st.m[v]

		fmt.Printf("%s\t%d", s.uuid, s.initiated.UnixNano())
		for _, c := range s.connections {
			connSetup := c.established.Sub(c.started)
			if connSetup < 0 {
				connSetup = 0
			}
			reqSent := c.firstWrite.ts.Sub(c.established)
			if reqSent < 0 {
				reqSent = 0
			}
			respStarted := c.firstRead.ts.Sub(c.established)
			if respStarted < 0 {
				respStarted = 0
			}
			respCompleted := c.closed.ts.Sub(c.established)
			if respCompleted < 0 {
				respCompleted = 0
			}
			fmt.Printf("\t%d\t%d\t%d\t%d", connSetup, reqSent, respStarted, respCompleted)
		}
		dur := s.completed.Sub(s.initiated)
		if dur < 0 {
			dur = 0
		}
		fmt.Printf("\t%d\n", dur)

	}
}

// session represents a UUID, and a list of URLs that will be requested.
// Timing information is captured for each request, as well as the overall
// session initiated and completed timestamps.
type session struct {
	uuid        string
	rt          http.RoundTripper
	bc          *byteCounter
	generated   time.Time
	initiated   time.Time
	completed   time.Time
	connections []*timedConnection
}

// newSession returns a session with our injected RoundTripper.
func newSession(uuid string, bc *byteCounter) *session {
	s := &session{uuid: uuid, bc: bc, generated: time.Now()}
	s.rt = &http.Transport{Dial: s.Dial}
	return s
}

// To capture information about the connections made in each session,
// we provide a Dial method, collecting information around the real net.Dialer's Dial()
func (s *session) Dial(network, addr string) (net.Conn, error) {
	tc := &timedConnection{started: time.Now()}
	conn, err := (&net.Dialer{Timeout: time.Duration(*connectTimeout) * time.Millisecond}).Dial(network, addr)
	tc.established = time.Now()
	tc.bc = s.bc
	tc.Conn = conn
	s.connections = append(s.connections, tc)
	return tc, err
}

// sessionTransport is used to inject our session type and RoundTripper into http.Client interactions.
type sessionTransport struct {
	m  map[string]*session
	mu sync.Mutex
}

// The RoundTrip method is the entrypoint for http.Client. This looks up the session and invokes its individual RoundTrip method.
func (st *sessionTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	uuid := req.Header.Get(headerRequestID)
	st.mu.Lock()
	sess := st.m[uuid]
	st.mu.Unlock()
	res, err := sess.rt.RoundTrip(req)
	return res, err
}

// timedConnection is a net.Conn that collects timing information during Read(), Write() and Close() methods.
type timedConnection struct {
	net.Conn
	started     time.Time
	established time.Time
	firstRead   timedEvent
	firstWrite  timedEvent
	closed      timedEvent
	bc          *byteCounter
}

func (tc *timedConnection) Read(b []byte) (int, error) {
	tc.firstRead.Event()
	n, err := tc.Conn.Read(b)
	tc.bc.m.Lock()
	tc.bc.rp += 1
	tc.bc.rb += n
	tc.bc.m.Unlock()
	return n, err
}

func (tc *timedConnection) Write(b []byte) (int, error) {
	tc.firstWrite.Event()
	n, err := tc.Conn.Write(b)
	tc.bc.m.Lock()
	tc.bc.tp += 1
	tc.bc.tb += n
	tc.bc.m.Unlock()
	return n, err
}

func (tc *timedConnection) Close() error {
	tc.closed.Event()
	return tc.Conn.Close()
}

// a timedEvent captures a timestamp on first invocation of Event()
// regardless of how many times it is called.
type timedEvent struct {
	once     sync.Once
	occurred bool
	ts       time.Time
}

func (te *timedEvent) Event() {
	te.once.Do(
		func() {
			te.ts = time.Now()
			te.occurred = true
		},
	)
}

func (te *timedEvent) When() time.Time {
	return te.ts
}

func (te *timedEvent) Occurred() bool {
	return te.occurred
}

// a byteCounter holds the data captured by timedConnection Read() and Write() calls.
type byteCounter struct {
	m  sync.Mutex
	rp int
	tp int
	rb int
	tb int
}

// Sample() returns the current state of the byteCounter and resets the values
func (bc *byteCounter) Sample() (int, int, int, int) {
	bc.m.Lock()
	rp, tp, rb, tb := bc.rp, bc.tp, bc.rb, bc.tb
	bc.rp, bc.tp, bc.rb, bc.tb = 0, 0, 0, 0
	bc.m.Unlock()
	return rp, tp, rb, tb
}
