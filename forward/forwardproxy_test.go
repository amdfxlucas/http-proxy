// Copyright 2024 ETH Zurich
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package forward_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"golang.org/x/net/http2"

	"github.com/scionproto-contrib/http-proxy/forward"
	"github.com/scionproto-contrib/http-proxy/forward/utils"
)

/*
Test naming: Test{Secure?}Proxy{Method}{Auth}{Policy}
GET/CONNECT -- get gets, connect connects and gets
Auth/NoAuth
Empty/WithPolicy -- tries different credentials
*/
var (
	testResources         = []string{"/", "/image.png"}
	testHTTPProxyVersions = []string{"HTTP/1.1", "HTTP/2.0"}
	// we do not target HTTP2, because the go client does not support
	// using a proxy with HTTP2.
	// https://github.com/golang/go/issues/26479
	testHTTPTargetVersions = []string{"HTTP/1.1"}
	httpVersionToALPN      = map[string]string{
		"HTTP/1.1": "http/1.1",
		"HTTP/2.0": "h2",
	}
)

var (
	credentialsEmpty                = ""
	credentialsIncorrect            = "Basic c3RoOjEyMzQ1"                                         // sth:12345
	credentialsCorrectNoPolicy      = "Basic cG9saWN5Og=="                                         // policy:
	credentialsCorrectPolicyInvalid = "Basic cG9saWN5OmNhZGR5LXNjaW9uLWZvcndhcmQtcHJveHk9Ymx1Yg==" // policy:caddy-scion-forward-proxy=blub
)

var (
	statusCodeProxyAuthReq    = http.StatusProxyAuthRequired
	responseOK                = http.StatusOK
	responseProxyAuthRequired = []byte("required to pass valid proxy authorization header\n")
)

func TestSecureProxyGETNoAuth(t *testing.T) {
	const useTLS = true
	for _, httpProxyVer := range testHTTPProxyVersions {
		for _, resource := range testResources {
			t.Run(fmt.Sprintf("GET %s over %s", resource, httpProxyVer), func(t *testing.T) {
				response, err := getViaProxy(insecureTestTarget.addr, resource, secureForwardProxy.addr, httpProxyVer, credentialsEmpty, useTLS)
				require.NoError(t, err)
				assert.NoError(t, responseExpected(response, statusCodeProxyAuthReq, responseProxyAuthRequired))
			})
		}
	}
}

func TestSecureProxyGETAuthNoPolicy(t *testing.T) {
	const useTLS = true
	for _, httpProxyVer := range testHTTPProxyVersions {
		for _, resource := range testResources {
			t.Run(fmt.Sprintf("GET %s over %s", resource, httpProxyVer), func(t *testing.T) {
				response, err := getViaProxy(insecureTestTarget.addr, resource, secureForwardProxy.addr, httpProxyVer, credentialsCorrectNoPolicy, useTLS)
				require.NoError(t, err)
				assert.NoError(t, responseExpected(response, responseOK, insecureTestTarget.contents[resource]))
			})
		}
	}
}

func TestSecureProxyGETIncorrectAuth(t *testing.T) {
	const useTLS = true
	for _, httpProxyVer := range testHTTPProxyVersions {
		for _, resource := range testResources {
			t.Run(fmt.Sprintf("GET %s over %s", resource, httpProxyVer), func(t *testing.T) {
				response, err := getViaProxy(insecureTestTarget.addr, resource, secureForwardProxy.addr, httpProxyVer, credentialsIncorrect, useTLS)
				require.NoError(t, err)
				assert.NoError(t, responseExpected(response, statusCodeProxyAuthReq, responseProxyAuthRequired))
			})
		}
	}
}

func TestConnectIncorrectAuth(t *testing.T) {
	const useTLS = true
	for _, httpProxyVer := range testHTTPProxyVersions {
		for _, httpTargetVer := range testHTTPTargetVersions {
			for _, resource := range testResources {
				t.Run(fmt.Sprintf("CONNECT %s over %s and %s", resource, httpProxyVer, httpTargetVer), func(t *testing.T) {
					response, err := connectAndGetViaProxy(insecureTestTarget.addr, resource, secureForwardProxy.addr, httpTargetVer, credentialsIncorrect, httpProxyVer, useTLS)
					require.NoError(t, err)
					assert.NoError(t, responseExpected(response, statusCodeProxyAuthReq, responseProxyAuthRequired))
				})
			}
		}
	}
}

func TestGETAuthPolicyInvalid(t *testing.T) {
	const useTLS = true
	for _, httpProxyVer := range testHTTPProxyVersions {
		for _, resource := range testResources {
			t.Run(fmt.Sprintf("GET %s over %s", resource, httpProxyVer), func(t *testing.T) {
				response, err := getViaProxy(insecureTestTarget.addr, resource, secureForwardProxy.addr, httpProxyVer, credentialsCorrectPolicyInvalid, useTLS)
				require.NoError(t, err)
				assert.NoError(t, responseExpected(response, responseOK, insecureTestTarget.contents[resource]))
			})
		}
	}
}

func TestSecureProxyGETSelf(t *testing.T) {
	const useTLS = true
	for _, httpProxyVer := range testHTTPProxyVersions {
		for _, resource := range testResources {
			t.Run(fmt.Sprintf("GET %s over %s", resource, httpProxyVer), func(t *testing.T) {
				response, err := getViaProxy(secureForwardProxy.addr, resource, secureForwardProxy.addr, httpProxyVer, credentialsEmpty, useTLS)
				require.NoError(t, err)
				assert.NoError(t, responseExpected(response, responseOK, secureForwardProxy.contents[resource]))
			})
		}
	}
}

func TestConnectNoAuth(t *testing.T) {
	const useTLS = true
	for _, httpProxyVer := range testHTTPProxyVersions {
		for _, httpTargetVer := range testHTTPTargetVersions {
			for _, resource := range testResources {
				t.Run(fmt.Sprintf("CONNECT %s over %s and %s", resource, httpProxyVer, httpTargetVer), func(t *testing.T) {
					response, err := connectAndGetViaProxy(insecureTestTarget.addr, resource, secureForwardProxy.addr, httpTargetVer, credentialsEmpty, httpProxyVer, useTLS)
					require.NoError(t, err)
					assert.NoError(t, responseExpected(response, statusCodeProxyAuthReq, responseProxyAuthRequired))
				})
			}
		}
	}
}

func TestConnectAuthNoPolicy(t *testing.T) {
	const useTLS = true
	for _, httpProxyVer := range testHTTPProxyVersions {
		for _, httpTargetVer := range testHTTPTargetVersions {
			for _, resource := range testResources {
				t.Run(fmt.Sprintf("CONNECT %s over %s and %s", resource, httpProxyVer, httpTargetVer), func(t *testing.T) {
					response, err := connectAndGetViaProxy(insecureTestTarget.addr, resource, secureForwardProxy.addr, httpTargetVer, credentialsCorrectNoPolicy, httpProxyVer, useTLS)
					require.NoError(t, err)
					assert.NoError(t, responseExpected(response, responseOK, insecureTestTarget.contents[resource]))
				})
			}
		}
	}
}

func TestConnectAuthPolicyInvalid(t *testing.T) {
	const useTLS = true
	for _, httpProxyVer := range testHTTPProxyVersions {
		for _, httpTargetVer := range testHTTPTargetVersions {
			for _, resource := range testResources {
				t.Run(fmt.Sprintf("CONNECT %s over %s and %s", resource, httpProxyVer, httpTargetVer), func(t *testing.T) {
					response, err := connectAndGetViaProxy(insecureTestTarget.addr, resource, secureForwardProxy.addr, httpTargetVer, credentialsCorrectPolicyInvalid, httpProxyVer, useTLS)
					require.NoError(t, err)
					assert.NoError(t, responseExpected(response, responseOK, insecureTestTarget.contents[resource]))
				})
			}
		}
	}
}

func TestAPISetPolicy(t *testing.T) {
	const useTLS = true
	for _, httpProxyVer := range testHTTPTargetVersions {
		t.Run(fmt.Sprintf("Set Policy over %s", httpProxyVer), func(t *testing.T) {
			proxyConn, err := dial(secureForwardProxy.addr, httpProxyVer, useTLS)
			require.NoError(t, err)

			req, err := http.NewRequest(http.MethodPut, "http://"+secureForwardProxy.addr+"/policy", bytes.NewBuffer([]byte(`["+ 42", "-"]`)))
			require.NoError(t, err)

			tp := &http.Transport{
				Dial: func(network, addr string) (net.Conn, error) {
					return proxyConn, nil
				},
			}

			response, err := tp.RoundTrip(req)
			require.NoError(t, err)
			assert.NoError(t, responseExpected(response, responseOK, nil))
		})
	}
}

func TestAPIGetPath(t *testing.T) {
	// test
}

func TestAPIResolveHost(t *testing.T) {
	// test
}

func TestAPIResolveURL(t *testing.T) {
	// test
}

func BenchmarkSecureProxyGETAuthNoPolicy(b *testing.B) {
	const useTLS = true
	resource := "/"
	httpProxyVer := "HTTP/2.0"

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, err := getViaProxy(insecureTestTarget.addr, resource, secureForwardProxy.addr, httpProxyVer, credentialsCorrectNoPolicy, useTLS)
		require.NoError(b, err)
	}
}

func BenchmarkConnectAuthNoPolicy(b *testing.B) {
	const useTLS = true
	resource := "/"
	httpTargetVer := "HTTP/1.1"
	httpProxyVer := "HTTP/2.0"

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, err := connectAndGetViaProxy(insecureTestTarget.addr, resource, secureForwardProxy.addr, httpTargetVer, credentialsCorrectNoPolicy, httpProxyVer, useTLS)
		require.NoError(b, err)
	}
}

func newHttp2Conn(c net.Conn, pipedReqBody *io.PipeWriter, respBody io.ReadCloser) net.Conn {
	return &http2Conn{Conn: c, in: pipedReqBody, out: respBody}
}

type http2Conn struct {
	net.Conn
	in  *io.PipeWriter
	out io.ReadCloser
}

func (h *http2Conn) Read(p []byte) (n int, err error) {
	return h.out.Read(p)
}

func (h *http2Conn) Write(p []byte) (n int, err error) {
	return h.in.Write(p)
}

func (h *http2Conn) Close() error {
	inErr := h.in.Close()
	outErr := h.out.Close()

	if inErr != nil {
		return inErr
	}
	return outErr
}

func (h *http2Conn) CloseConn() error {
	return h.Conn.Close()
}

func (h *http2Conn) CloseWrite() error {
	return h.in.Close()
}

func (h *http2Conn) CloseRead() error {
	return h.out.Close()
}

func dial(proxyAddr, httpProxyVer string, useTLS bool) (net.Conn, error) {
	if useTLS {
		return tls.Dial("tcp", proxyAddr, &tls.Config{
			InsecureSkipVerify: true,
			NextProtos:         []string{httpVersionToALPN[httpProxyVer]},
		})
	}
	return net.Dial("tcp", proxyAddr)
}

func getViaProxy(targetHost, resource, proxyAddr, httpProxyVer, proxyCredentials string, useTLS bool) (*http.Response, error) {
	proxyConn, err := dial(proxyAddr, httpProxyVer, useTLS)
	if err != nil {
		return nil, err
	}
	return getResourceViaProxyConn(proxyConn, targetHost, resource, proxyCredentials, httpProxyVer)
}

// if connect is not successful - that response is returned, otherwise the requested resource
func connectAndGetViaProxy(targetHost, resource, proxyAddr, httpTargetVer, proxyCredentials, httpProxyVer string, useTLS bool) (*http.Response, error) {
	proxyConn, err := dial(proxyAddr, httpProxyVer, useTLS)
	if err != nil {
		return nil, err
	}
	// do not defer close here, because we want to close the connection only after the response has been read

	req := &http.Request{Header: make(http.Header)}
	if len(proxyCredentials) > 0 {
		req.Header.Set("Proxy-Authorization", proxyCredentials)
	}
	req.Host = targetHost
	req.URL, err = url.Parse("https://" + req.Host + "/")
	if err != nil {
		return nil, err
	}
	req.RequestURI = req.Host
	req.Method = "CONNECT"
	req.Proto = httpProxyVer

	var resp *http.Response
	switch httpProxyVer {
	case "HTTP/2.0":
		req.ProtoMajor = 2
		req.ProtoMinor = 0
		pr, pw := io.Pipe()
		req.Body = io.NopCloser(pr)
		t := http2.Transport{}
		clientConn, err := t.NewClientConn(proxyConn)
		if err != nil {
			return nil, err
		}
		resp, err = clientConn.RoundTrip(req)
		if err != nil {
			return resp, err
		}
		proxyConn = newHttp2Conn(proxyConn, pw, resp.Body)
	case "HTTP/1.1":
		req.ProtoMajor = 1
		req.ProtoMinor = 1
		_ = req.Write(proxyConn) // we don't care about the error here
		resp, err = http.ReadResponse(bufio.NewReader(proxyConn), req)
		if err != nil {
			return resp, err
		}
	default:
		return nil, fmt.Errorf("unsupported proxy version: %s", httpProxyVer)
	}

	if resp.StatusCode != http.StatusOK {
		return resp, nil
	}

	return getResourceViaProxyConn(proxyConn, targetHost, resource, proxyCredentials, httpTargetVer)
}

func getResourceViaProxyConn(proxyConn net.Conn, targetHost, resource, proxyCredentials, httpTargetVer string) (*http.Response, error) {
	var err error

	req := &http.Request{Header: make(http.Header)}
	if len(proxyCredentials) > 0 {
		req.Header.Set("Proxy-Authorization", proxyCredentials)
	}
	req.Host = targetHost
	req.URL, err = url.Parse("http://" + targetHost + resource)
	if err != nil {
		return nil, err
	}
	req.RequestURI = req.Host + resource
	req.Method = "GET"
	req.Proto = httpTargetVer

	switch httpTargetVer {
	case "HTTP/2.0":
		req.ProtoMajor = 2
		req.ProtoMinor = 0
		t := http2.Transport{AllowHTTP: true}
		clientConn, err := t.NewClientConn(proxyConn)
		if err != nil {
			return nil, err
		}
		return clientConn.RoundTrip(req)
	case "HTTP/1.1":
		req.ProtoMajor = 1
		req.ProtoMinor = 1
		t := http.Transport{
			Dial: func(network, addr string) (net.Conn, error) {
				return proxyConn, nil
			},
		}
		return t.RoundTrip(req)
	default:
		return nil, fmt.Errorf("unsupported HTTP version: %s", httpTargetVer)
	}
}

// If response is expected: returns nil.
func responseExpected(resp *http.Response, expectedStatusCode int, expectedResponse []byte) error {
	if expectedStatusCode != resp.StatusCode {
		return fmt.Errorf("expected response status code %d, got %d", expectedStatusCode, resp.StatusCode)
	}

	response, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("error reading response body: %v", err)
	}

	if !bytes.Equal(expectedResponse, response) {
		return fmt.Errorf("expected response: '%s', got: '%s'", expectedResponse, response)
	}

	return nil
}

type testServer struct {
	addr     string
	tls      bool
	root     string
	contents map[string][]byte
	proxy    *forward.CoreProxy
}

var (
	secureForwardProxy   testServer
	insecureForwardProxy testServer
	secureTestTarget     testServer
	insecureTestTarget   testServer
)

const (
	apiPathPrefix  = ""
	apiPolicyPath  = apiPathPrefix + "/policy"
	apiPathUsage   = apiPathPrefix + "/path-usage"
	apiResolveURL  = apiPathPrefix + "/redirect"
	apiResolveHost = apiPathPrefix + "/resolve"
)

func (s *testServer) interceptConnect(connect, next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodConnect || req.Host != s.addr {
			connect.ServeHTTP(w, req)
			return
		}
		next.ServeHTTP(w, req)
	}
}

func (s *testServer) start() {
	mux := http.NewServeMux()
	mux.HandleFunc(apiPolicyPath, func(w http.ResponseWriter, r *http.Request) {
		if err := s.proxy.HandlePolicyPath(w, r); err != nil {
			returnCode, err := unwrapError(err)
			http.Error(w, err.Error(), returnCode)
		}
	})
	mux.HandleFunc(apiPathUsage, func(w http.ResponseWriter, r *http.Request) {
		if err := s.proxy.HandlePathUsage(w, r); err != nil {
			returnCode, err := unwrapError(err)
			http.Error(w, err.Error(), returnCode)
		}
	})
	mux.HandleFunc(apiResolveURL, func(w http.ResponseWriter, r *http.Request) {
		if err := s.proxy.HandleResolveURL(w, r); err != nil {
			returnCode, err := unwrapError(err)
			http.Error(w, err.Error(), returnCode)
		}
	})
	mux.HandleFunc(apiResolveHost, func(w http.ResponseWriter, r *http.Request) {
		if err := s.proxy.HandleResolveHost(w, r); err != nil {
			returnCode, err := unwrapError(err)
			http.Error(w, err.Error(), returnCode)
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/" {
			path = "/index.html"
		}
		content, ok := s.contents[path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(content)
	})

	server := &http.Server{
		Addr: s.addr,
		Handler: s.interceptConnect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := s.proxy.HandleTunnelRequest(w, r); err != nil {
				returnCode, err := unwrapError(err)
				fmt.Println("Error:", err)
				http.Error(w, err.Error(), returnCode)
			}
		}), mux),
	}

	go func() {
		var err error
		if s.tls {
			err = server.ListenAndServeTLS(s.root+"/cert.pem", s.root+"/key.pem")
		} else {
			err = server.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			panic(err)
		}
	}()
}

func (s *testServer) loadContents() {
	s.contents = make(map[string][]byte)
	files := []string{"/index.html", "/image.png"}
	for _, file := range files {
		content, err := os.ReadFile(s.root + file)
		if err != nil {
			panic(err)
		}
		s.contents[file] = content
	}
	s.contents["/"] = s.contents["/index.html"]
	s.contents[""] = s.contents["/index.html"]
}

func TestMain(m *testing.M) {
	// Initialize logger
	logger := zap.NewNop()

	// Initialize test servers
	secureForwardProxy = testServer{
		addr:  "127.0.42.1:8200",
		root:  "./test/forwardproxy",
		tls:   true,
		proxy: forward.NewCoreProxy(logger, 10*time.Second, 10*time.Second, 10*time.Second, 10*time.Second, false),
	}
	insecureForwardProxy = testServer{
		addr:  "127.0.42.1:8201",
		root:  "./test/forwardproxy",
		proxy: forward.NewCoreProxy(logger, 10*time.Second, 10*time.Second, 10*time.Second, 10*time.Second, false),
	}
	secureTestTarget = testServer{
		addr: "127.0.42.2:8300",
		root: "./test/target",
		tls:  true,
	}
	insecureTestTarget = testServer{
		addr: "127.0.42.2:8301",
		root: "./test/target",
	}

	// Initialize proxies
	if err := secureForwardProxy.proxy.Initialize(); err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := secureForwardProxy.proxy.Cleanup(); err != nil {
			log.Fatal(err)
		}
	}()
	if err := insecureForwardProxy.proxy.Initialize(); err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := insecureForwardProxy.proxy.Cleanup(); err != nil {
			log.Fatal(err)
		}
	}()

	// Load contents for test servers
	secureForwardProxy.loadContents()
	insecureForwardProxy.loadContents()
	secureTestTarget.loadContents()
	insecureTestTarget.loadContents()

	// Start the servers
	secureForwardProxy.start()
	insecureForwardProxy.start()
	secureTestTarget.start()
	insecureTestTarget.start()

	// Allow servers to start
	time.Sleep(1 * time.Second)

	// Run the tests
	code := m.Run()

	// Cleanup and exit
	os.Exit(code)
}

// This is a sanity check confirming that target servers actually directly serve what they are expected to.
// (And that they don't serve what they should not)
func TestTheTest(t *testing.T) {
	client := &http.Client{Transport: testTransport, Timeout: 2 * time.Second}

	// Request index
	resp, err := client.Get("http://" + insecureTestTarget.addr)
	require.NoError(t, err)
	assert.NoError(t, responseExpected(resp, responseOK, insecureTestTarget.contents[""]))

	// Request image
	resp, err = client.Get("http://" + insecureTestTarget.addr + "/image.png")
	require.NoError(t, err)
	assert.NoError(t, responseExpected(resp, responseOK, insecureTestTarget.contents["/image.png"]))

	// Request image, but expect index. Should fail
	resp, err = client.Get("http://" + insecureTestTarget.addr + "/image.png")
	require.NoError(t, err)
	assert.Error(t, responseExpected(resp, responseOK, insecureTestTarget.contents[""]))

	// Request index, but expect image. Should fail
	resp, err = client.Get("http://" + insecureTestTarget.addr)
	require.NoError(t, err)
	assert.Error(t, responseExpected(resp, responseOK, insecureTestTarget.contents["/image.png"]))

	// Request non-existing resource
	resp, err = client.Get("http://" + insecureTestTarget.addr + "/idontexist")
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "Expected: 404 StatusNotFound, got %d. Response: %#v\n", resp.StatusCode, resp)
}

var testTransport = &http.Transport{
	ResponseHeaderTimeout: 2 * time.Second,
	DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		return new(net.Dialer).DialContext(ctx, network, addr)
	},
	DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := new(net.Dialer).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return tls.Client(conn, &tls.Config{InsecureSkipVerify: true}), nil
	},
}

func unwrapError(err error) (int, error) {
	he := err.(*utils.HandlerError)
	return he.StatusCode, he.Err
}
