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
)

const (
	headerRequestID = "X-Request-ID"
)

func usage() {
	_, cmd := path.Split(os.Args[0])
	fmt.Fprintf(os.Stderr, "Usage: %s [-requests=n] [-connect-timeout=n] url [url...]\n", cmd)
}

func main() {
	flag.Usage = usage
	flag.Parse()

	if len(flag.Args()) == 0 {
		fmt.Fprintf(os.Stderr, "No URLs provided\n")
		return
	}

	urls := make([]string, 0, len(flag.Args()))

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

	st := &sessionTransport{m: map[string]*session{}}
	uuidList := []string{}

	bc := &byteCounter{}
	client := &http.Client{Transport: st}

	wg := &sync.WaitGroup{}
	ch := make(chan *session, int(*numWorkers))

	spinupInterval := time.Duration(*spinupPeriod) * time.Millisecond / time.Duration(*numWorkers)
	for i := 0; i < int(*numWorkers); i++ {
		wg.Add(1)
		go func(factor int) {
			defer wg.Done()
			delay := time.Duration(factor) * spinupInterval
			for s := range ch {
				if delay != 0 {
					time.Sleep(delay)
					delay = 0
				}
				s.initiated = time.Now()
				s.connections = make([]*timedConnection, 0, len(urls))
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
				s.completed = time.Now()
			}
		}(i)
	}

	ticker := time.NewTicker(1 * time.Second) // 100 * time.Millisecond)
	go func() {
		for range ticker.C {
			rp, tp, rb, tb := bc.Sample()
			fmt.Fprintln(os.Stderr, "Packets sent:", tp, "received", rp, "Bytes sent:", tb, "bytes received:", rb)
		}
	}()

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
	close(ch)
	ticker.Stop()
	wg.Wait()

	// Summary headers
	fmt.Printf("Session\tStartTime")
	for i := range urls {
		fmt.Printf("\tC%[1]dConnSetup\tC%[1]dReqSent\tC%[1]dRespStarted\tC%[1]dRespCompleted", i+1)
	}
	fmt.Printf("\tDuration\n")

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

type session struct {
	uuid        string
	rt          http.RoundTripper
	bc          *byteCounter
	generated   time.Time
	initiated   time.Time
	completed   time.Time
	connections []*timedConnection
}

func newSession(uuid string, bc *byteCounter) *session {
	s := &session{uuid: uuid, bc: bc, generated: time.Now()}
	s.rt = &http.Transport{Dial: s.Dial}
	return s
}

func (s *session) Dial(network, addr string) (net.Conn, error) {
	tc := &timedConnection{started: time.Now()}
	conn, err := (&net.Dialer{Timeout: time.Duration(*connectTimeout) * time.Millisecond}).Dial(network, addr)
	tc.established = time.Now()
	tc.bc = s.bc
	tc.Conn = conn
	s.connections = append(s.connections, tc)
	return tc, err
}

type sessionTransport struct {
	m  map[string]*session
	mu sync.Mutex
}

func (st *sessionTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	uuid := req.Header.Get(headerRequestID)
	st.mu.Lock()
	sess := st.m[uuid]
	st.mu.Unlock()
	res, err := sess.rt.RoundTrip(req)
	return res, err
}

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

type byteCounter struct {
	m  sync.Mutex
	rp int
	tp int
	rb int
	tb int
}

func (bc *byteCounter) Sample() (int, int, int, int) {
	bc.m.Lock()
	rp, tp, rb, tb := bc.rp, bc.tp, bc.rb, bc.tb
	bc.rp, bc.tp, bc.rb, bc.tb = 0, 0, 0, 0
	bc.m.Unlock()
	return rp, tp, rb, tb
}
