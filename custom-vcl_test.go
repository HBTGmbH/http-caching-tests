// Contains tests for behaviour using custom VCL implementations
package caching_test

import (
	"caching"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// TestAbandon5xxResponseInGracePeriodWhen2xxCached will implement and test the idea of
// https://blog.markvincze.com/how-to-gracefully-fall-back-to-cache-on-5xx-responses-with-varnish/
// The idea here is to abandon a background-fetched 5xx response when we still have a cached 2xx response
// within the grace period.
func TestAbandon5xxResponseInGracePeriodWhen2xxCached(t *testing.T) {
	t.Parallel()
	var backendRequests int

	// start a test server
	testServerPort, testServer := startTestServer(func(w http.ResponseWriter, r *http.Request) {
		xStatusCode, err := strconv.Atoi(r.Header.Get("X-Status-Code"))
		w.Header().Set("X-Response", r.Header.Get("X-Request"))
		assert.NoError(t, err)
		w.WriteHeader(xStatusCode)
		backendRequests++
	})
	defer testServer.Close()

	// start varnish container with a custom VCL
	port, stopFunc, err := caching.StartVarnishInDocker(caching.VarnishConfig{
		BackendPort:  testServerPort,
		DefaultTtl:   "1s",
		DefaultGrace: "5s",
		Vcl: `
sub vcl_backend_response {
  if (beresp.status == 500 || (beresp.status >= 502 && beresp.status <= 504)) {
    if (bereq.is_bgfetch) {
      return (abandon);
    }
  }
}`,
	})
	require.NoError(t, err)
	defer stopFunc()
	waitForHealthy(t, port)

	// send request with a 200 response, which will be cached
	assert.Equal(t, resp(http.StatusOK, "foo"), reqSR(t, port, http.StatusOK, "foo"))

	// wait half a second
	time.Sleep(500 * time.Millisecond)

	// send another request and expect the previous cached return
	assert.Equal(t, resp(http.StatusOK, "foo"), reqR(t, port, "bar"))

	// wait for 600 ms to let the cached response expire and enter grace period
	time.Sleep(600 * time.Millisecond)

	// send a request which will trigger a background/asynchronous revalication
	// request and result in a 500 response. We still get the 200 cached response here.
	assert.Equal(t, resp(http.StatusOK, "foo"), reqSR(t, port, http.StatusInternalServerError, "baz"))

	// wait a bit for Varnish to finish the revalidation request. Normally, if we hadn't
	// modified vcl_backend_response, this would now abandon the 200 cached response and
	// later requsts with a 500 response would also return 500.
	// But not this time. See next request.
	time.Sleep(50 * time.Millisecond)

	// Do another request which will also respond with the cached 200 response.
	// This is because we abandoned the background request and still have a cached 200 response.
	// Note that this request here will _also_ trigger a background revalidation request whose
	// 500 response will then also be abandoned.
	assert.Equal(t, resp(http.StatusOK, "foo"), reqSR(t, port, http.StatusInternalServerError, "boo"))

	// wait a bit for Varnish to finish the revalidation request.
	time.Sleep(50 * time.Millisecond)

	// expect three backend requests
	// 1. initial request with 200 response
	// 2. background request with 500 response (which was abandoned)
	// 3. next background request with 500 response (which was also abandoned)
	assert.Equal(t, 3, backendRequests)
}