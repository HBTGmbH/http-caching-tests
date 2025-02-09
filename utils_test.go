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
	path           string
	method         string
	xStatusCode    int
	xRequest       string
	cacheControl   string
	authorization  string
	cookie         string
	ifNoneMatch    string
	acceptEncoding string
	storeBody      bool
	origin         string
	range_         string
}

type response struct {
	statusCode               int
	xResponse                string
	body                     string
	cacheControl             string
	xCache                   string
	cacheStatus              string
	contentRange             string
	acceptRanges             string
	contentType              string
	transferEncoding         string
	contentEncoding          string
	contentLength            int
	accessControlAllowOrigin string
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

func mkResp(statusCode int, xResponse string, modifiers ...func(*response)) response {
	r := response{
		statusCode: statusCode,
		xResponse:  xResponse,
	}
	if statusCode == http.StatusOK || statusCode == http.StatusNotModified {
		// Varnish always responds with Accept-Ranges: bytes for 200 or 304 responses
		r.acceptRanges = "bytes"
	}
	for _, m := range modifiers {
		m(&r)
	}
	return r
}

func withCacheStatus(cacheStatus string) func(*response) {
	return func(r *response) {
		r.cacheStatus = cacheStatus
	}
}

func withAcceptRanges(acceptRanges string) func(*response) {
	return func(r *response) {
		r.acceptRanges = acceptRanges
	}
}

func withContentType(contentType string) func(*response) {
	return func(r *response) {
		r.contentType = contentType
	}
}

func withBody(body string) func(*response) {
	return func(r *response) {
		r.body = body
	}
}

func withResponseCacheControl(cacheControl string) func(*response) {
	return func(r *response) {
		r.cacheControl = cacheControl
	}
}

func withXCache(xCache string) func(*response) {
	return func(r *response) {
		r.xCache = xCache
	}
}

func withContentLength(contentLength int) func(*response) {
	return func(r *response) {
		r.contentLength = contentLength
	}
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

func withOrigin(origin string) func(*request) {
	return func(r *request) {
		r.origin = origin
	}
}

func withStoreBody() func(*request) {
	return func(r *request) {
		r.storeBody = true
	}
}

func withAcceptEncoding(acceptEncoding string) func(*request) {
	return func(r *request) {
		r.acceptEncoding = acceptEncoding
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
	httpClient := http.Client{
		Transport: &http.Transport{
			DisableCompression: true,
		},
	}
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
	if r.origin != "" {
		req.Header.Set("Origin", r.origin)
	}
	if r.ifNoneMatch != "" {
		req.Header.Set("If-None-Match", r.ifNoneMatch)
	}
	if r.range_ != "" {
		req.Header.Set("Range", r.range_)
	}
	if r.acceptEncoding != "" {
		req.Header.Set("Accept-Encoding", r.acceptEncoding)
	}
	assert.NoError(t, err)
	resp, err := httpClient.Do(req)
	assert.NoError(t, err)
	body := ""
	if r.storeBody {
		body = readBody(t, resp)
	}
	transferEncoding := ""
	if len(resp.TransferEncoding) > 0 {
		transferEncoding = resp.TransferEncoding[0]
	}
	return response{
		statusCode:               resp.StatusCode,
		xResponse:                resp.Header.Get("X-Response"),
		body:                     body,
		cacheControl:             resp.Header.Get("Cache-Control"),
		xCache:                   resp.Header.Get("X-Cache"),
		cacheStatus:              resp.Header.Get("Cache-Status"),
		contentRange:             resp.Header.Get("Content-Range"),
		contentType:              resp.Header.Get("Content-Type"),
		contentEncoding:          resp.Header.Get("Content-Encoding"),
		contentLength:            int(resp.ContentLength),
		transferEncoding:         transferEncoding,
		acceptRanges:             resp.Header.Get("Accept-Ranges"),
		accessControlAllowOrigin: resp.Header.Get("Access-Control-Allow-Origin"),
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
