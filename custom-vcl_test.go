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

	// send a request which will trigger a background/asynchronous revalidation
	// request and result in a 500 response. We still get the 200 cached response here.
	assert.Equal(t, resp(http.StatusOK, "foo"), reqSR(t, port, http.StatusInternalServerError, "baz"))

	// wait a bit for Varnish to finish the revalidation request. Normally, if we hadn't
	// modified vcl_backend_response, this would now abandon the 200 cached response and
	// later requests with a 500 response would also return 500.
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

// TestTtlOn5xxBackendResponseIsNotAutomaticallyHitForMiss will implement and test the idea of
// https://info.varnish-software.com/blog/hit-for-miss-and-why-a-null-ttl-is-bad-for-you
// and check that setting a TTL and grace for 5xx responses will not automatically mark
// beresp.uncacheable as true, in which case the response becomes a Hit-For-Miss object.
func TestTtlOn5xxBackendResponseIsNotAutomaticallyHitForMiss(t *testing.T) {
	t.Parallel()
	var backendRequests int

	// start a test server
	testServerPort, testServer := startTestServer(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Response", r.Header.Get("X-Request"))
		w.WriteHeader(http.StatusInternalServerError)
		backendRequests++
	})
	defer testServer.Close()

	// start varnish container with a custom VCL
	port, stopFunc, err := caching.StartVarnishInDocker(caching.VarnishConfig{
		BackendPort: testServerPort,
		Vcl: `
sub vcl_backend_response {
  if (beresp.status == 500 || (beresp.status >= 502 && beresp.status <= 504)) {
    set beresp.ttl = 1s;
    set beresp.grace = 10s;
  }
}`,
	})
	require.NoError(t, err)
	defer stopFunc()
	waitForHealthy(t, port)

	// send request which will become a 500 response
	assert.Equal(t, resp(http.StatusInternalServerError, "foo"), reqR(t, port, "foo"))

	// wait a tiny bit
	time.Sleep(100 * time.Millisecond)

	// send another request and expect the previous cached return
	assert.Equal(t, resp(http.StatusInternalServerError, "foo"), reqR(t, port, "bar"))

	// expect one backend request
	// If beresp.uncacheable would have been set to true, we would have gotten a Hit-For-Miss object
	// and thus _two_ backend requests. This test was to verify that this is _not_ the case and
	// we only got one backend request.
	assert.Equal(t, 1, backendRequests)
}

// TestExplicitMarkingAsUncacheableOn5xxBackendResponseIsHitForMiss checks that when we _do_
// explicitly set beresp.uncacheable to true, we get a Hit-For-Miss object.
// This is somewhat the inverse of the TestTtlOn5xxBackendResponseIsNotAutomaticallyHitForMiss above.
func TestExplicitMarkingAsUncacheableOn5xxBackendResponseIsHitForMiss(t *testing.T) {
	t.Parallel()
	var backendRequests int

	// start a test server
	testServerPort, testServer := startTestServer(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Response", r.Header.Get("X-Request"))
		w.WriteHeader(http.StatusInternalServerError)
		backendRequests++
	})
	defer testServer.Close()

	// start varnish container with a custom VCL
	port, stopFunc, err := caching.StartVarnishInDocker(caching.VarnishConfig{
		BackendPort: testServerPort,
		Vcl: `
sub vcl_backend_response {
  if (beresp.status == 500 || (beresp.status >= 502 && beresp.status <= 504)) {
    set beresp.ttl = 1s;
    set beresp.grace = 10s;
    set beresp.uncacheable = true;
  }
}`,
	})
	require.NoError(t, err)
	defer stopFunc()
	waitForHealthy(t, port)

	// send request which will become a 500 response
	assert.Equal(t, resp(http.StatusInternalServerError, "foo"), reqR(t, port, "foo"))

	// wait a tiny bit
	time.Sleep(100 * time.Millisecond)

	// send another request and expect a new backend request because of Hit-For-Miss
	assert.Equal(t, resp(http.StatusInternalServerError, "bar"), reqR(t, port, "bar"))

	// expect two backend requests
	assert.Equal(t, 2, backendRequests)
}

// TestRemoveIllegalStaleWhileRevalidateWithoutValue tests a custom implementation of
// vcl_backend_response which removes any stale-while-revalidate directive without a duration from the
// Cache-Control header, which would be illegal according to RFC 5861.
// See: https://datatracker.ietf.org/doc/html/rfc5861#section-3
func TestRemoveIllegalStaleWhileRevalidateWithoutValue(t *testing.T) {
	t.Parallel()

	// start a test server
	testServerPort, testServer := startTestServer(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", r.Header.Get("X-Request"))
		w.WriteHeader(http.StatusOK)
	})
	defer testServer.Close()

	// start varnish container with a custom VCL
	port, stopFunc, err := caching.StartVarnishInDocker(caching.VarnishConfig{
		BackendPort: testServerPort,
		Vcl: `
sub vcl_backend_response {
  set beresp.http.Cache-Control = regsub(beresp.http.Cache-Control, "(,\s+)?stale-while-revalidate(?!\s*=\s*)", "");
}`,
	})
	require.NoError(t, err)
	defer stopFunc()
	waitForHealthy(t, port)

	assert.Equal(t, respCC(http.StatusOK, "", "s-maxage=10"), reqPR(t, port, "/1", "s-maxage=10, stale-while-revalidate"))
	assert.Equal(t, respCC(http.StatusOK, "", "public, s-maxage=10"), reqPR(t, port, "/2", "public, s-maxage=10, stale-while-revalidate"))
	assert.Equal(t, respCC(http.StatusOK, "", "s-maxage=10, public"), reqPR(t, port, "/3", "s-maxage=10, stale-while-revalidate, public"))
	assert.Equal(t, respCC(http.StatusOK, "", "stale-while-revalidate=10, public"), reqPR(t, port, "/4", "stale-while-revalidate=10, public"))
	assert.Equal(t, respCC(http.StatusOK, "", "stale-while-revalidate=10"), reqPR(t, port, "/5", "stale-while-revalidate=10"))
	assert.Equal(t, respCC(http.StatusOK, "", "stale-while-revalidate = 10"), reqPR(t, port, "/6", "stale-while-revalidate = 10"))
	assert.Equal(t, respCC(http.StatusOK, "", ""), reqPR(t, port, "/7", "stale-while-revalidate"))
}

// TestReturnPassInVclRecvBypassesTheCache tests that returning pass in vcl_recv bypasses the cache.
func TestReturnPassInVclRecvBypassesTheCache(t *testing.T) {
	t.Parallel()
	var backendRequests int

	// start a test server
	testServerPort, testServer := startTestServer(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Response", r.Header.Get("X-Request"))
		w.WriteHeader(http.StatusOK)
		backendRequests++
	})
	defer testServer.Close()

	// start varnish container with a custom VCL
	port, stopFunc, err := caching.StartVarnishInDocker(caching.VarnishConfig{
		BackendPort:  testServerPort,
		DefaultTtl:   "1s",
		DefaultGrace: "10s",
		Vcl: `
sub vcl_recv {
  if (req.http.X-Request) {
    return (pass);
  }
}`,
	})
	require.NoError(t, err)
	defer stopFunc()
	waitForHealthy(t, port)

	// send first request which will be passed through to the backend
	assert.Equal(t, resp(http.StatusOK, "foo"), reqR(t, port, "foo"))

	// wait a bit (for no reason, really)
	time.Sleep(100 * time.Millisecond)

	// send another request and expect a new backend request because
	assert.Equal(t, resp(http.StatusOK, "foo"), reqR(t, port, "foo"))

	// expect two backend requests
	assert.Equal(t, 2, backendRequests)
}

// TestSettingReqGraceInVclRecvIsUpperCapForBerespGraceInVclBackendResponse tests that setting
// req.grace in vcl_recv is the upper cap for any possible beresp.grace in vcl_backend_response.
// This means that vcl_recv can control the maximum grace period regardless of what the backend
// sends or what is being overwritten in vcl_backend_response.
func TestSettingReqGraceInVclRecvIsUpperCapForBerespGraceInVclBackendResponse(t *testing.T) {
	t.Parallel()
	var backendRequests int

	// start a test server
	testServerPort, testServer := startTestServer(func(w http.ResponseWriter, r *http.Request) {
		backendRequests++
		w.Header().Set("X-Response", r.Header.Get("X-Request"))
		w.WriteHeader(http.StatusOK)
	})
	defer testServer.Close()

	// start varnish container with a custom VCL
	port, stopFunc, err := caching.StartVarnishInDocker(caching.VarnishConfig{
		BackendPort: testServerPort,
		Vcl: `
sub vcl_recv {
  set req.grace = 1s;
}
sub vcl_backend_response {
  set beresp.ttl = 100ms;
  set beresp.grace = 10s;
}`,
	})
	require.NoError(t, err)
	defer stopFunc()
	waitForHealthy(t, port)

	// send first request which should get a grace of only 1s
	assert.Equal(t, respCC(http.StatusOK, "foo", ""), reqR(t, port, "foo"))

	// wait for the response to become stale but still within grace
	time.Sleep(200 * time.Millisecond)

	// send another request and expect a cached response and an asynchronous backend request
	assert.Equal(t, respCC(http.StatusOK, "foo", ""), reqR(t, port, "bar"))

	// wait to get outside of grace, which should only have been 1s
	time.Sleep(1200 * time.Millisecond)

	// send another request and expect a synchronous backend request
	assert.Equal(t, respCC(http.StatusOK, "buzz", ""), reqR(t, port, "buzz"))

	// expect three backend requests
	assert.Equal(t, 3, backendRequests)
}

// TestSettingReqGraceInVclRecvIsUpperCapForSwrOfBackendResponse tests that setting
// req.grace in vcl_recv is the upper cap for any possible stale-while-revalidate in the
// Cache-Control header of the backend response.
func TestSettingReqGraceInVclRecvIsUpperCapForSwrOfBackendResponse(t *testing.T) {
	t.Parallel()
	var backendRequests int

	// start a test server
	testServerPort, testServer := startTestServer(func(w http.ResponseWriter, r *http.Request) {
		backendRequests++
		w.Header().Set("Cache-Control", "max-age=1, stale-while-revalidate=10")
		w.Header().Set("X-Response", r.Header.Get("X-Request"))
		w.WriteHeader(http.StatusOK)
	})
	defer testServer.Close()

	// start varnish container with a custom VCL
	port, stopFunc, err := caching.StartVarnishInDocker(caching.VarnishConfig{
		BackendPort: testServerPort,
		Vcl: `
sub vcl_recv {
  set req.grace = 1s;
}`,
	})
	require.NoError(t, err)
	defer stopFunc()
	waitForHealthy(t, port)

	// send first request which should get a grace of only 1s
	assert.Equal(t, respCC(http.StatusOK, "foo", "max-age=1, stale-while-revalidate=10"), reqR(t, port, "foo"))

	// wait for the response to become stale but still within grace
	time.Sleep(1200 * time.Millisecond)

	// send another request and expect a cached response and an asynchronous backend request
	assert.Equal(t, respCC(http.StatusOK, "foo", "max-age=1, stale-while-revalidate=10"), reqR(t, port, "bar"))

	// wait to get outside of grace, which should only have been 1s
	time.Sleep(2200 * time.Millisecond)

	// send another request and expect a synchronous backend request
	assert.Equal(t, respCC(http.StatusOK, "buzz", "max-age=1, stale-while-revalidate=10"), reqR(t, port, "buzz"))

	// expect three backend requests
	assert.Equal(t, 3, backendRequests)
}

// TestSetBerespTtlToTinyValueAllowsForStaleWhileRevalidate tests that setting beresp.ttl to even a tiny
// duration allows for stale-while-revalidate to work, because then Varnish will actually keep the cached
// object around for the grace period allowing for asynchronous backend revalidation requests.
func TestSetBerespTtlToTinyValueAllowsForStaleWhileRevalidate(t *testing.T) {
	t.Parallel()
	var backendRequests int

	// start a test server
	testServerPort, testServer := startTestServer(func(w http.ResponseWriter, r *http.Request) {
		backendRequests++
		w.Header().Set("Cache-Control", "stale-while-revalidate=1")
		w.Header().Set("X-Response", r.Header.Get("X-Request"))
		w.WriteHeader(http.StatusOK)
	})
	defer testServer.Close()

	// start varnish container with a custom VCL
	port, stopFunc, err := caching.StartVarnishInDocker(caching.VarnishConfig{
		BackendPort: testServerPort,
		Vcl: `
sub vcl_backend_response {
  if (beresp.ttl == 0s && beresp.http.Cache-Control ~ "stale-while-revalidate" && beresp.http.Cache-Control !~ "private|no-store|no-cache") {
    # If the backend response specifies a zero TTL but also has a stale-while-revalidate
    # directive, the desired behaviour should be to make the response stale immediately
    # and revalidate it in the background for the specified swr/grace duration.
    # But since Varnish will not cache a TTL=0 at all (not even for grace), we need to set
    # a small TTL to make the response being cached (even if for the swr/grace duration only).
    set beresp.ttl = 1ms;
  }
}`,
	})
	require.NoError(t, err)
	defer stopFunc()
	waitForHealthy(t, port)

	// send first request should get a grace of 1s
	assert.Equal(t, respCC(http.StatusOK, "foo", "stale-while-revalidate=1"), reqR(t, port, "foo"))

	// wait a bit but still within grace
	time.Sleep(200 * time.Millisecond)

	// send another request and expect a cached response and an asynchronous backend request
	assert.Equal(t, respCC(http.StatusOK, "foo", "stale-while-revalidate=1"), reqR(t, port, "bar"))

	// wait to get outside of grace
	time.Sleep(1100 * time.Millisecond)

	// send another request and expect a synchronous backend request
	assert.Equal(t, respCC(http.StatusOK, "buzz", "stale-while-revalidate=1"), reqR(t, port, "buzz"))

	// expect three backend requests
	assert.Equal(t, 3, backendRequests)
}

// TestDoNotSetBerespTtlWhenCacheControlPrivate tests that we do not set beresp.ttl to a tiny value
// when the Cache-Control header contains a private directive.
func TestDoNotSetBerespTtlWhenCacheControlPrivate(t *testing.T) {
	t.Parallel()
	var backendRequests int

	// start a test server
	testServerPort, testServer := startTestServer(func(w http.ResponseWriter, r *http.Request) {
		backendRequests++
		w.Header().Set("Cache-Control", "private, stale-while-revalidate=1")
		w.Header().Set("X-Response", r.Header.Get("X-Request"))
		w.WriteHeader(http.StatusOK)
	})
	defer testServer.Close()

	// start varnish container with a custom VCL
	port, stopFunc, err := caching.StartVarnishInDocker(caching.VarnishConfig{
		BackendPort: testServerPort,
		Vcl: `
sub vcl_backend_response {
  if (beresp.ttl == 0s && beresp.http.Cache-Control ~ "stale-while-revalidate" && beresp.http.Cache-Control !~ "private|no-store|no-cache") {
    # If the backend response specifies a zero TTL but also has a stale-while-revalidate
    # directive, the desired behaviour should be to make the response stale immediately
    # and revalidate it in the background for the specified swr/grace duration.
    # But since Varnish will not cache a TTL=0 at all (not even for grace), we need to set
    # a small TTL to make the response being cached (even if for the swr/grace duration only).
    set beresp.ttl = 1ms;
  }
}`,
	})
	require.NoError(t, err)
	defer stopFunc()
	waitForHealthy(t, port)

	// send first request
	assert.Equal(t, respCC(http.StatusOK, "foo", "private, stale-while-revalidate=1"), reqR(t, port, "foo"))

	// wait a bit
	time.Sleep(200 * time.Millisecond)

	// send another request and expect a new synchronous backend request
	assert.Equal(t, respCC(http.StatusOK, "bar", "private, stale-while-revalidate=1"), reqR(t, port, "bar"))

	// wait to get outside of supposed grace period
	time.Sleep(1100 * time.Millisecond)

	// send another request and also expect a synchronous backend request
	assert.Equal(t, respCC(http.StatusOK, "buzz", "private, stale-while-revalidate=1"), reqR(t, port, "buzz"))

	// expect three backend requests
	assert.Equal(t, 3, backendRequests)
}
