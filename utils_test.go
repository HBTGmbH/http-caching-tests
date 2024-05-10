// Contains utility functions for tests
package caching_test

import (
	"caching"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

type request struct {
	path          string
	method        string
	xStatusCode   int
	xRequest      string
	cacheControl  string
	authorization string
	cookie        string
	ifNoneMatch   string
	storeBody     bool
	range_        string
}

type response struct {
	statusCode   int
	xResponse    string
	body         string
	cacheControl string
	xCache       string
	cacheStatus  string
	contentRange string
	acceptRanges string
}

func resp(statusCode int, xResponse string) response {
	return response{
		statusCode: statusCode,
		xResponse:  xResponse,
	}
}

func respCC(statusCode int, xResponse string, cacheControl string) response {
	return response{
		statusCode:   statusCode,
		xResponse:    xResponse,
		cacheControl: cacheControl,
	}
}

func respB(statusCode int, xResponse, body string) response {
	return response{
		statusCode: statusCode,
		xResponse:  xResponse,
		body:       body,
	}
}

func mkReq(t *testing.T, port string, xRequest string, modifiers ...func(*request)) response {
	r := request{
		path:        "/",
		method:      http.MethodGet,
		xStatusCode: 200,
		xRequest:    xRequest,
	}
	for _, m := range modifiers {
		m(&r)
	}
	return req(t, port, r)
}

func withPath(path string) func(*request) {
	return func(r *request) {
		r.path = path
	}
}

func withMethod(method string) func(*request) {
	return func(r *request) {
		r.method = method
	}
}

func withCacheControl(cacheControl string) func(*request) {
	return func(r *request) {
		r.cacheControl = cacheControl
	}
}

func withStoreBody() func(*request) {
	return func(r *request) {
		r.storeBody = true
	}
}

func withAuthorization(authorization string) func(*request) {
	return func(r *request) {
		r.authorization = authorization
	}
}

func withCookie(cookie string) func(*request) {
	return func(r *request) {
		r.cookie = cookie
	}
}

func withXStatusCode(xStatusCode int) func(*request) {
	return func(r *request) {
		r.xStatusCode = xStatusCode
	}
}

func withIfNoneMatch(ifNoneMatch string) func(*request) {
	return func(r *request) {
		r.ifNoneMatch = ifNoneMatch
	}
}

func withRange(range_ string) func(*request) {
	return func(r *request) {
		r.range_ = range_
	}
}

func req(t *testing.T, port string, r request) response {
	httpClient := http.Client{}
	req, err := http.NewRequest(r.method, "http://localhost:"+port+r.path, nil)
	if r.xStatusCode != 0 {
		req.Header.Set("X-Status-Code", strconv.Itoa(r.xStatusCode))
	}
	if r.xRequest != "" {
		req.Header.Set("X-Request", r.xRequest)
	}
	if r.authorization != "" {
		req.Header.Set("Authorization", r.authorization)
	}
	if r.cookie != "" {
		req.Header.Set("Cookie", r.cookie)
	}
	if r.cacheControl != "" {
		req.Header.Set("Cache-Control", r.cacheControl)
	}
	if r.ifNoneMatch != "" {
		req.Header.Set("If-None-Match", r.ifNoneMatch)
	}
	if r.range_ != "" {
		req.Header.Set("Range", r.range_)
	}
	assert.NoError(t, err)
	resp, err := httpClient.Do(req)
	assert.NoError(t, err)
	body := ""
	if r.storeBody {
		body = readBody(t, resp)
	}
	return response{
		statusCode:   resp.StatusCode,
		xResponse:    resp.Header.Get("X-Response"),
		body:         body,
		cacheControl: resp.Header.Get("Cache-Control"),
		xCache:       resp.Header.Get("X-Cache"),
		cacheStatus:  resp.Header.Get("Cache-Status"),
		contentRange: resp.Header.Get("Content-Range"),
		acceptRanges: resp.Header.Get("Accept-Ranges"),
	}
}

func readBody(t *testing.T, resp *http.Response) string {
	body, err := io.ReadAll(resp.Body)
	assert.NoError(t, err)
	return string(body)
}

func startTestServer(handler http.HandlerFunc) (string, *httptest.Server) {
	return caching.StartTestServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusOK)
			return
		}
		handler(w, r)
	})
}

func waitForHealthy(t *testing.T, port string) {
	httpClient := http.Client{}
	for i := 0; i < 100; i++ {
		req, err := http.NewRequest(http.MethodGet, "http://localhost:"+port+"/health", nil)
		require.NoError(t, err)
		resp, err := httpClient.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
