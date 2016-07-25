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

	sessionMap := map[string]*session{}
	uuidList := []string{}

	bc := &byteCounter{}
	client := &http.Client{Transport: &sessionTransport{m: sessionMap}}

	numWorkers := int(*connectTimeout) / 10
	if numWorkers < runtime.NumCPU()*8 {
		numWorkers = runtime.NumCPU() * 8
	}
	wg := &sync.WaitGroup{}
	ch := make(chan *session, numWorkers)
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for s := range ch {
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
		}()
	}

	go func() {
		tickCh := time.Tick(1 * time.Second) // 100 * time.Millisecond)
		for range tickCh {
			rp, tp, rb, tb := bc.Sample()
			fmt.Println("Packets sent:", tp, "received", rp, "Bytes sent:", tb, "bytes received:", rb)
		}
	}()

	for i := uint(0); i < *requests; i++ {
		u, err := uuid.New4()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error creating UUID: %s\n", err)
			continue
		}
		s := newSession(u, bc)
		sessionMap[u] = s
		uuidList = append(uuidList, u)
		ch <- s
	}
	close(ch)
	submitFinished := time.Now()
	fmt.Println("Submitted", *requests, "requests, waiting for completion")
	wg.Wait()
	fmt.Println("Waited", time.Now().Sub(submitFinished))
	for _, v := range uuidList {
		s := sessionMap[v]
		queTime := s.initiated.Sub(s.generated)
		duration := s.completed.Sub(s.initiated)
		c := s.connections[0]
		dialTime := c.established.Sub(c.started)
		xferTime := c.closed.ts.Sub(c.firstWrite.ts)
		fmt.Println(s.generated, queTime, duration, dialTime, xferTime)
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
	m map[string]*session
}

func (sr *sessionTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	uuid := req.Header.Get(headerRequestID)
	sess := sr.m[uuid]
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
