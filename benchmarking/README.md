# Benchmarking Tools

The benchmarking tools contained here were written for a very specific purpose: generating HTTP traffic to monitor the behavior and performance of the network and TCP sessions.

As such, they are not general-purpose HTTP test tools, and contain only a small number of options.
For that specific purpose though, these tools helped to observe the networking behavior with microscopic detail, and verify the end-to-end components under heavy load.

# Test HTTP Client

## Description
The Test HTTP Client is used to generate unique HTTP sessions as rapidly as possible.

A session is composed of one or more HTTP URLs. The client sends a HTTP GET request to that URL, and reads the response.
Each session has a unique UUID, and this is sent in the HTTP header as `X-Request-ID`.
During each session, details are kept for the following events:
* StartTime (UNIX Nanosecond timestamp)
* C1ConnSetup (duration for the TCP connection established)
* C1ReqSent (duration when the HTTP request started to be sent)
* C1RespStarted (duration when the HTTP response started to be sent)
* C1RespCompleted (duration when the HTTP connection was completed)
* repeated for C2, C3.. if applicable)
* Duration (duration from StartTime until final connection is completed)

These measurements are tracked in nanoseconds, and dumped to `stdout` when all sessions have been completed.

## Installation

This tool is written in Go, and can be installed using `go get`:
```bash
go get github.com/romana/benchmark-tools/test-http-client
```
After installation, it will be in `$GOPATH/bin/test-http-client`.

## Usage and Examples

Usage: `test-http-client [-requests=n] [-connect-timeout=n] [-spinup-period=n] [-workers=n] [-startup-delay=n] url [url...]`

At least one URL must be provided.

Options:
- `-requests`: Number of requests (sessions) to execute. (default: 1)
- `-connect-timeout`: Number of milliseconds to permit connection attempts before timing out. (default: 1000)
- `-spinup-period`: Duration to slowly spin up connections (avoiding initial spike). (default: 500)
- `-workers`: Number of worker goroutines for HTTP clients/connections. (default: number of CPUs/cores)
- `-startup-delay`: Number of milliseconds to delay before starting the requests. (default: 0)

Examples:
```bash
# Test a single HTTP request to a single URL
test-http-client http://example.com/path

# Test a single HTTP request to each URL in the list
test-http-client http://example.com/path1 http://example.com/path2 http://example.com/path3

# Test 10,000 HTTP requests to a single URL
test-http-client -requests=10000 http://example.com/path

# Ensure the requests are executed sequentially
test-http-client -requests=10000 -workers=1 http://example.com/path

# Emulate many (32) HTTP clients operating concurrently
test-http-client -requests=10000 -workers=32 http://example.com/path
```

# Test HTTP Server

## Description
The Test HTTP Server is used to respond to HTTP requests with a 200 OK, followed a fixed amount of random bytes to emulate the reply from an HTTP endpoint.

To ensure consistency between requests, the test server disabled HTTP Keep-Alives,preallocates the random data for response and forces a `Content-Length` HTTP Header to avoid (chunked transfer encoding)[https://en.wikipedia.org/wiki/Chunked_transfer_encoding].

While running, the test server will output some data about the activity - connections per second, and bytes read/written for the HTTP connections.
These are for informational purposes, not intended for analysis.

## Installation

This tool is written in Go, and can be installed using `go get`:
```bash
go get github.com/romana/benchmark-tools/test-http-server
```
After installation, it will be in `$GOPATH/bin/test-http-server`.

## Usage and Examples

Usage: `test-http-server [-port=n] [-endpoint=/path] [-response-size=n]

Options:
- `-port`: The port number to listen for HTTP connections. (default: 8080)
- `-endpoint`: The path that should be used by HTTP requests. (default: `/endpoint`)
- `-response-size`: Number of bytes of random data to send in the HTTP response. (default: 512)

Examples:
```bash
# Listen on Port 80 for HTTP Requests with default endpoint and response size
test-http-server -port=80

# Emulate a server hosting large images (10MB each)
test-http-server -response-size=10485760

# Emulate an authorization server that sends very small responses
test-http-server -port=1234 -endpoint=/auth -response-size=64

# Test HTTP Proxy

## Description
The Test HTTP Proxy takes requests on a specific HTTP Endpoint, and proxies it to a target endpoint.

This tool was intended for service chaining for benchmarking scenarios between the test client and test server.
It did not receive significant use, but does function reasonably for its purpose.

## Installation

This tool is written in Go, and can be installed using `go get`:
```bash
go get github.com/romana/benchmark-tools/test-http-proxy
```
After installation, it will be in `$GOPATH/bin/test-http-proxy`.

## Usage and Examples

Usage: `test-http-proxy [-port=n] [-endpoint=/path] [-response-size=n]

Options:
- `-port`: The port number to listen for HTTP connections. (default: 8080)
- `-endpoint`: The path that should be used by HTTP requests. (default: `/endpoint`)
- `-target`: The target for the proxied HTTP request. Must be a valid URL.
- `-connect-timeout`: Number of milliseconds to permit connection attempts before timing out. (default: 1000)

Examples:
```bash
# Proxy requests for http://proxy.host:8080/endpoint to http://backend.service/target
test-http-proxy -target http://backend.service/target

# Proxy requests for a custom endpoint on port 80 to a custom target
test-http-proxy -port 80 -endpoint /custom -target http://backend.service/target

