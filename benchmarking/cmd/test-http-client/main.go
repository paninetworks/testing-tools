package main

import (
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"path"
	"runtime"
	"sync"
	"time"
	"github.com/cgilmour/uuid"
	_ "github.com/cgilmour/maxopen"
)

var (
	requests = flag.Uint("requests", 1, "Number of requests to send before completion.")
	connectTimeout = flag.Uint("connect-timeout", 1000, "Number of milliseconds to permit connection attempts before timing out.")
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

	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		fmt.Fprintf(os.Stderr, "Error asserting http.DefaultTransport\n")
		return
	}
	bc := &byteCounter{}
	dialer := timedDialer {
		dial: (&net.Dialer{Timeout: time.Duration(*connectTimeout) * time.Millisecond}).Dial,
		bc: bc,
	}
	defaultTransport.Dial = dialer.Dial
	client := &http.Client{Transport: newTimedRoundTripper(http.DefaultTransport)}

	numWorkers := int(*connectTimeout) / 10
	if numWorkers < runtime.NumCPU() * 8 {
		numWorkers = runtime.NumCPU() * 8
	}
	wg := &sync.WaitGroup{}
	ch := make(chan struct{}, numWorkers)
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range ch {
				u, err := uuid.New4()
				if err != nil {
					fmt.Fprintf(os.Stderr, "Error creating UUID: %s\n", err)
					continue
				}
				for _, url := range urls {
					req, err := http.NewRequest("GET", url, nil)
					if err != nil {
						fmt.Fprintf(os.Stderr, "Error creating request to '%s': %s\n", url, err)
						continue
					}
					req.Close = true
					req.Header.Set("X-Request-ID", u)
					resp, err := client.Do(req)
					if err != nil {
						fmt.Fprintf(os.Stderr, "Error execuing request to '%s': %s\n", url, err)
						continue
					}
					io.Copy(ioutil.Discard, resp.Body)
					resp.Body.Close()
				}
			}
		}()
	}

	go func() {
		tickCh := time.Tick(100 * time.Millisecond)
		for range tickCh {
			rx, tx := bc.Sample()
			fmt.Println("Bytes sent:", tx, "bytes received:", rx)
		}
	}()

	s := struct{}{}
	for i := uint(0);  i < *requests; i++ {
		before := time.Now()
		ch <- s
		after := time.Now()
		if after.Sub(before) > 1 * time.Second {
			fmt.Println("Request", i, "delay was", after.Sub(before))
		}
	}
	close(ch)
	submitFinished := time.Now()
	fmt.Println("Submitted", *requests, "requests, waiting for completion")
	wg.Wait()
	fmt.Println("Waited", time.Now().Sub(submitFinished))
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
	bc *byteCounter
}

func (td timedDialer) Dial(network, addr string) (net.Conn, error) {
	// fmt.Println("DialStart:", time.Now())
	conn, err := td.dial(network, addr)
	// connTime := time.Now()
	// fmt.Println("DialEnd:  ", connTime)
	return &timedConn{Conn: conn, openTS: time.Now(), bc: td.bc}, err
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

