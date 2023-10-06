// Contains utility functions for tests
package caching_test

import (
	"caching"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

type response struct {
	statusCode int
	xResponse  string
}

func resp(statusCode int, xResponse string) response {
	return response{
		statusCode: statusCode,
		xResponse:  xResponse,
	}
}

func reqR(t *testing.T, port string, xRequest string) response {
	return reqSR(t, port, http.StatusOK, xRequest)
}

func reqSR(t *testing.T, port string, status int, xRequest string) response {
	httpClient := http.Client{}
	req, err := http.NewRequest("GET", "http://localhost:"+port+"/", nil)
	req.Header.Set("X-Status-Code", strconv.Itoa(status))
	req.Header.Set("X-Request", xRequest)
	assert.NoError(t, err)
	resp, err := httpClient.Do(req)
	assert.NoError(t, err)
	return response{
		statusCode: resp.StatusCode,
		xResponse:  resp.Header.Get("X-Response"),
	}
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
		req, err := http.NewRequest("GET", "http://localhost:"+port+"/health", nil)
		require.NoError(t, err)
		resp, err := httpClient.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
