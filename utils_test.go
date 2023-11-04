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

type response struct {
	statusCode   int
	xResponse    string
	body         string
	cacheControl string
	xCache       string
	cacheStatus  string
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

func reqR(t *testing.T, port, xRequest string) response {
	return req(t, port, "/", http.MethodGet, 0, xRequest, "", "", "", "", false)
}

func reqPR(t *testing.T, port, path, xRequest string) response {
	return req(t, port, path, http.MethodGet, 0, xRequest, "", "", "", "", false)
}

func reqMR(t *testing.T, port, method, xRequest string) response {
	return req(t, port, "/", method, 0, xRequest, "", "", "", "", false)
}

func reqRCC(t *testing.T, port, xRequest, cacheControl string) response {
	return req(t, port, "/", http.MethodGet, 0, xRequest, cacheControl, "", "", "", false)
}

func reqR_B(t *testing.T, port, xRequest string) response {
	return req(t, port, "/", http.MethodGet, 0, xRequest, "", "", "", "", true)
}

func reqRA(t *testing.T, port, xRequest, authorization string) response {
	return req(t, port, "/", http.MethodGet, 0, xRequest, "", authorization, "", "", false)
}

func reqRC(t *testing.T, port, xRequest, cookie string) response {
	return req(t, port, "/", http.MethodGet, 0, xRequest, "", "", cookie, "", false)
}

func reqSR(t *testing.T, port string, status int, xRequest string) response {
	return req(t, port, "/", http.MethodGet, status, xRequest, "", "", "", "", false)
}

func reqRINM(t *testing.T, port, xRequest, ifNoneMatch string) response {
	return req(t, port, "/", http.MethodGet, 0, xRequest, "", "", "", ifNoneMatch, true)
}

func req(t *testing.T, port, path, method string, status int, xRequest, cacheControl, authorization, cookie, ifNoneMatch string, storeBody bool) response {
	httpClient := http.Client{}
	req, err := http.NewRequest(method, "http://localhost:"+port+path, nil)
	if status != 0 {
		req.Header.Set("X-Status-Code", strconv.Itoa(status))
	}
	if xRequest != "" {
		req.Header.Set("X-Request", xRequest)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	if cacheControl != "" {
		req.Header.Set("Cache-Control", cacheControl)
	}
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	assert.NoError(t, err)
	resp, err := httpClient.Do(req)
	assert.NoError(t, err)
	body := ""
	if storeBody {
		body = readBody(t, resp)
	}
	return response{
		statusCode:   resp.StatusCode,
		xResponse:    resp.Header.Get("X-Response"),
		body:         body,
		cacheControl: resp.Header.Get("Cache-Control"),
		xCache:       resp.Header.Get("X-Cache"),
		cacheStatus:  resp.Header.Get("Cache-Status"),
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
