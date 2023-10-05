package caching_test

import (
	"caching"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"net/http"
	"testing"
	"time"
)

// TestNoCacheControl tests that Varnish will use its default TTL when the backend response
// does not provide a "Cache-Control" header (or any other cache-related header) and the VCL
// does not alter the caching logic.
// This test configures the default TTL to be 1 second (with no grace period).
func TestNoCacheControl(t *testing.T) {
	t.Parallel()
	var backendRequests int

	// start a test server
	testServerPort, testServer := caching.StartTestServer(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Response", r.Header.Get("X-Request"))
		w.WriteHeader(http.StatusOK)
		backendRequests++
	})
	defer testServer.Close()

	// start varnish container
	port, stopFunc, err := caching.StartVarnishInDocker(caching.VarnishConfig{
		BackendPort: testServerPort,
		DefaultTtl:  "1s",
	})
	require.NoError(t, err)
	defer stopFunc()

	// send request
	assert.Equal(t, "foo", req(t, port, "foo"))

	// wait half a second
	time.Sleep(500 * time.Millisecond)

	// send another request and expect the previous cached return
	assert.Equal(t, "foo", req(t, port, "bar"))

	// wait for 600 ms
	time.Sleep(600 * time.Millisecond)

	// send another request and expect no cached return
	assert.Equal(t, "baz", req(t, port, "baz"))

	// expect two backend requests
	assert.Equal(t, 2, backendRequests)
}

// TestCacheControlNoCache tests that Varnish does not respond with a cached item
// when the backend response had a "Cache-Control: no-cache" header, which will force
// Varnish to revalidate with the backend on each request.
// However, since the backend response in this test does not provide any validator (e.g. ETag or Last-Modified),
// Varnish will simply call the backend on each request and not respond with a cached item.
// This is tested by sending two requests to a test server that echoes the request headers back in the response
// along with a "Cache-Control: no-cache" header.
// Note that "Cache-Control: no-cache" does not mean "do not cache", but rather
// "revalidate with the backend on each request".
func TestCacheControlNoCache(t *testing.T) {
	t.Parallel()
	var backendRequests int

	// start a test server
	testServerPort, testServer := caching.StartTestServer(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Response", r.Header.Get("X-Request"))
		w.WriteHeader(http.StatusOK)
		backendRequests++
	})
	defer testServer.Close()

	// start varnish container
	port, stopFunc, err := caching.StartVarnishInDocker(caching.VarnishConfig{
		BackendPort: testServerPort,
	})
	require.NoError(t, err)
	defer stopFunc()

	// send request
	assert.Equal(t, "foo", req(t, port, "foo"))

	// send another request
	assert.Equal(t, "bar", req(t, port, "bar"))

	// expect two backend requests
	assert.Equal(t, 2, backendRequests)
}

// TestCacheControlMaxAge1 tests that Varnish will respond with a cached item when the backend
// responds with a "Cache-Control: max-age=1" header, and the cache item has not yet expired.
func TestCacheControlMaxAge1(t *testing.T) {
	t.Parallel()
	var backendRequests int

	// start a test server
	testServerPort, testServer := caching.StartTestServer(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=1")
		w.Header().Set("X-Response", r.Header.Get("X-Request"))
		w.WriteHeader(http.StatusOK)
		backendRequests++
	})
	defer testServer.Close()

	// start varnish container
	port, stopFunc, err := caching.StartVarnishInDocker(caching.VarnishConfig{
		BackendPort: testServerPort,
	})
	require.NoError(t, err)
	defer stopFunc()

	// send request to varnish
	assert.Equal(t, "1", req(t, port, "1"))

	// send another request and expect to receive a cached response
	assert.Equal(t, "1", req(t, port, "2"))

	// expect one backend request
	assert.Equal(t, 1, backendRequests)
}

// TestStaleWhileRevalidate tests that Varnish will respond with a cached item when the TTL has expired,
// if the backend responded with a "Cache-Control: stale-while-revalidate" header giving the grace period
// in which Varnish will do a background fetch asynchronous to any client request.
func TestStaleWhileRevalidate(t *testing.T) {
	t.Parallel()
	var backendRequests int

	// start a test server
	testServerPort, testServer := caching.StartTestServer(func(w http.ResponseWriter, r *http.Request) {
		xRequest := r.Header.Get("X-Request")
		if xRequest == "2" {
			time.Sleep(2000 * time.Millisecond)
		}
		w.Header().Set("Cache-Control", "max-age=1, stale-while-revalidate=10")
		w.Header().Set("X-Response", r.Header.Get("X-Request"))
		w.WriteHeader(http.StatusOK)
		backendRequests++
	})
	defer testServer.Close()

	// start varnish container
	port, stopFunc, err := caching.StartVarnishInDocker(caching.VarnishConfig{
		BackendPort: testServerPort,
	})
	require.NoError(t, err)
	defer stopFunc()

	// send request to varnish
	assert.Equal(t, "1", req(t, port, "1"))

	// sleep for 1.1 seconds to make the cached response stale
	time.Sleep(1100 * time.Millisecond)

	// send another request and expect to receive a cached response
	time1 := time.Now()
	assert.Equal(t, "1", req(t, port, "2"))
	time2 := time.Now()
	// expect the response to have come back very fast
	assert.Less(t, time2.Sub(time1), 100*time.Millisecond)

	// sleep for 2.1 seconds to let Varnish revalidate the cached response
	time.Sleep(2100 * time.Millisecond)

	// send yet another request and expect to receive the second cached response
	assert.Equal(t, "2", req(t, port, "3"))

	// expect two backend requests
	assert.Equal(t, 2, backendRequests)
}

// ################ Test fixtures and helpers ################

func req(t *testing.T, port, xRequestHeader string) string {
	httpClient := http.Client{}
	req, err := http.NewRequest("GET", "http://localhost:"+port+"/", nil)
	req.Header.Set("X-Request", xRequestHeader)
	assert.NoError(t, err)
	resp, err := doRequestWithRetry(httpClient, req, 10)
	assert.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	return resp.Header.Get("X-Response")
}

func doRequestWithRetry(httpClient http.Client, req *http.Request, retries int) (*http.Response, error) {
	resp, err := httpClient.Do(req)
	if err != nil && retries > 0 {
		time.Sleep(100 * time.Millisecond)
		return doRequestWithRetry(httpClient, req, retries-1)
	}
	return resp, nil
}
