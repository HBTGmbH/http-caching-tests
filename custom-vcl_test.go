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
	assert.Equal(t, mkResp(http.StatusOK, "foo"), mkReq(t, port, "foo", withXStatusCode(http.StatusOK)))

	// wait half a second
	time.Sleep(500 * time.Millisecond)

	// send another request and expect the previous cached return
	assert.Equal(t, mkResp(http.StatusOK, "foo"), mkReq(t, port, "bar"))

	// wait for 600 ms to let the cached response expire and enter grace period
	time.Sleep(600 * time.Millisecond)

	// send a request which will trigger a background/asynchronous revalidation
	// request and result in a 500 response. We still get the 200 cached response here.
	assert.Equal(t, mkResp(http.StatusOK, "foo"), mkReq(t, port, "baz", withXStatusCode(http.StatusInternalServerError)))

	// wait a bit for Varnish to finish the revalidation request. Normally, if we hadn't
	// modified vcl_backend_response, this would now abandon the 200 cached response and
	// later requests with a 500 response would also return 500.
	// But not this time. See next request.
	time.Sleep(50 * time.Millisecond)

	// Do another request which will also respond with the cached 200 response.
	// This is because we abandoned the background request and still have a cached 200 response.
	// Note that this request here will _also_ trigger a background revalidation request whose
	// 500 response will then also be abandoned.
	assert.Equal(t, mkResp(http.StatusOK, "foo"), mkReq(t, port, "boo", withXStatusCode(http.StatusInternalServerError)))

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
	assert.Equal(t, mkResp(http.StatusInternalServerError, "foo"), mkReq(t, port, "foo"))

	// wait a tiny bit
	time.Sleep(100 * time.Millisecond)

	// send another request and expect the previous cached return
	assert.Equal(t, mkResp(http.StatusInternalServerError, "foo"), mkReq(t, port, "bar"))

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
	assert.Equal(t, mkResp(http.StatusInternalServerError, "foo"), mkReq(t, port, "foo"))

	// wait a tiny bit
	time.Sleep(100 * time.Millisecond)

	// send another request and expect a new backend request because of Hit-For-Miss
	assert.Equal(t, mkResp(http.StatusInternalServerError, "bar"), mkReq(t, port, "bar"))

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

	assert.Equal(t, mkResp(http.StatusOK, "", withResponseCacheControl("s-maxage=10")), mkReq(t, port, "s-maxage=10, stale-while-revalidate", withPath("/1")))
	assert.Equal(t, mkResp(http.StatusOK, "", withResponseCacheControl("public, s-maxage=10")), mkReq(t, port, "public, s-maxage=10, stale-while-revalidate", withPath("/2")))
	assert.Equal(t, mkResp(http.StatusOK, "", withResponseCacheControl("s-maxage=10, public")), mkReq(t, port, "s-maxage=10, stale-while-revalidate, public", withPath("/3")))
	assert.Equal(t, mkResp(http.StatusOK, "", withResponseCacheControl("stale-while-revalidate=10, public")), mkReq(t, port, "stale-while-revalidate=10, public", withPath("/4")))
	assert.Equal(t, mkResp(http.StatusOK, "", withResponseCacheControl("stale-while-revalidate=10")), mkReq(t, port, "stale-while-revalidate=10", withPath("/5")))
	assert.Equal(t, mkResp(http.StatusOK, "", withResponseCacheControl("stale-while-revalidate = 10")), mkReq(t, port, "stale-while-revalidate = 10", withPath("/6")))
	assert.Equal(t, mkResp(http.StatusOK, "", withResponseCacheControl("")), mkReq(t, port, "stale-while-revalidate", withPath("/7")))
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
	assert.Equal(t, mkResp(http.StatusOK, "foo", withAcceptRanges("")), mkReq(t, port, "foo"))

	// wait a bit (for no reason, really)
	time.Sleep(100 * time.Millisecond)

	// send another request and expect a new backend request because
	assert.Equal(t, mkResp(http.StatusOK, "foo", withAcceptRanges("")), mkReq(t, port, "foo"))

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
	assert.Equal(t, mkResp(http.StatusOK, "foo", withResponseCacheControl("")), mkReq(t, port, "foo"))

	// wait for the response to become stale but still within grace
	time.Sleep(200 * time.Millisecond)

	// send another request and expect a cached response and an asynchronous backend request
	assert.Equal(t, mkResp(http.StatusOK, "foo", withResponseCacheControl("")), mkReq(t, port, "bar"))

	// wait to get outside of grace, which should only have been 1s
	time.Sleep(1200 * time.Millisecond)

	// send another request and expect a synchronous backend request
	assert.Equal(t, mkResp(http.StatusOK, "buzz", withResponseCacheControl("")), mkReq(t, port, "buzz"))

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
	assert.Equal(t, mkResp(http.StatusOK, "foo", withResponseCacheControl("max-age=1, stale-while-revalidate=10")), mkReq(t, port, "foo"))

	// wait for the response to become stale but still within grace
	time.Sleep(1200 * time.Millisecond)

	// send another request and expect a cached response and an asynchronous backend request
	assert.Equal(t, mkResp(http.StatusOK, "foo", withResponseCacheControl("max-age=1, stale-while-revalidate=10")), mkReq(t, port, "bar"))

	// wait to get outside of grace, which should only have been 1s
	time.Sleep(2200 * time.Millisecond)

	// send another request and expect a synchronous backend request
	assert.Equal(t, mkResp(http.StatusOK, "buzz", withResponseCacheControl("max-age=1, stale-while-revalidate=10")), mkReq(t, port, "buzz"))

	// expect three backend requests
	assert.Equal(t, 3, backendRequests)
}

// TestSettingReqTtlInVclRecvIsNoUpperCapForBerespTtlInVclBackendResponse tests that setting
// req.ttl in vcl_recv is NOT CONSIDERED AN upper cap for any possible beresp.ttl in vcl_backend_response.
func TestSettingReqTtlInVclRecvIsNoUpperCapForBerespTtlInVclBackendResponse(t *testing.T) {
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
  set req.ttl = 0s;
}
sub vcl_backend_response {
  set beresp.ttl = 10s;
}`,
	})
	require.NoError(t, err)
	defer stopFunc()
	waitForHealthy(t, port)

	// send first request which should get a TTL of 10s
	assert.Equal(t, mkResp(http.StatusOK, "foo", withResponseCacheControl("")), mkReq(t, port, "foo"))

	// wait a bit
	time.Sleep(100 * time.Millisecond)

	// send another request and expect the cached response (because req.ttl is NO upper cap for beresp.ttl)
	assert.Equal(t, mkResp(http.StatusOK, "foo", withResponseCacheControl("")), mkReq(t, port, "bar"))

	// expect one backend request
	assert.Equal(t, 1, backendRequests)
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
	assert.Equal(t, mkResp(http.StatusOK, "foo", withResponseCacheControl("stale-while-revalidate=1")), mkReq(t, port, "foo"))

	// wait a bit but still within grace
	time.Sleep(200 * time.Millisecond)

	// send another request and expect a cached response and an asynchronous backend request
	assert.Equal(t, mkResp(http.StatusOK, "foo", withResponseCacheControl("stale-while-revalidate=1")), mkReq(t, port, "bar"))

	// wait to get outside of grace
	time.Sleep(1100 * time.Millisecond)

	// send another request and expect a synchronous backend request
	assert.Equal(t, mkResp(http.StatusOK, "buzz", withResponseCacheControl("stale-while-revalidate=1")), mkReq(t, port, "buzz"))

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
	assert.Equal(t, mkResp(http.StatusOK, "foo", withResponseCacheControl("private, stale-while-revalidate=1")), mkReq(t, port, "foo"))

	// wait a bit
	time.Sleep(200 * time.Millisecond)

	// send another request and expect a new synchronous backend request
	assert.Equal(t, mkResp(http.StatusOK, "bar", withResponseCacheControl("private, stale-while-revalidate=1")), mkReq(t, port, "bar"))

	// wait to get outside of supposed grace period
	time.Sleep(1100 * time.Millisecond)

	// send another request and also expect a synchronous backend request
	assert.Equal(t, mkResp(http.StatusOK, "buzz", withResponseCacheControl("private, stale-while-revalidate=1")), mkReq(t, port, "buzz"))

	// expect three backend requests
	assert.Equal(t, 3, backendRequests)
}

// TestRetainOnlyNeededCookies tests that removing specific cookies works with the code shown under
// https://www.varnish-software.com/developers/tutorials/removing-cookies-varnish/#only-keep-required-cookies
func TestRetainOnlyNeededCookies(t *testing.T) {
	t.Parallel()
	var backendRequests int

	// start a test server
	testServerPort, testServer := startTestServer(func(w http.ResponseWriter, r *http.Request) {
		backendRequests++
		assert.Equal(t, r.Header.Get("X-Request"), r.Header.Get("Cookie"))
		w.WriteHeader(http.StatusOK)
	})
	defer testServer.Close()

	// start varnish container with a custom VCL
	port, stopFunc, err := caching.StartVarnishInDocker(caching.VarnishConfig{
		BackendPort: testServerPort,
		Vcl: `
# Remove all cookies that are not needed for the request,
# but would otherwise render the response uncacheable.
# See: https://www.varnish-software.com/developers/tutorials/removing-cookies-varnish/#only-keep-required-cookies
sub retain_only_needed_cookies {
  if (req.http.Cookie) {
    set req.http.Cookie = ";" + req.http.Cookie;
    set req.http.Cookie = regsuball(req.http.Cookie, "; +", ";");
    set req.http.Cookie = regsuball(req.http.Cookie, ";(__prerender_bypass|__n-p-d)=", "; \1=");
    set req.http.Cookie = regsuball(req.http.Cookie, ";[^ ][^;]*", "");
    set req.http.Cookie = regsuball(req.http.Cookie, "^[; ]+|[; ]+$", "");
    if (req.http.cookie ~ "^\s*$") {
      unset req.http.cookie;
    }
  }
}
sub vcl_recv {
  call retain_only_needed_cookies;
}`,
	})
	require.NoError(t, err)
	defer stopFunc()
	waitForHealthy(t, port)

	mkReq(t, port, "__prerender_bypass=1", withCookie("__prerender_bypass=1"))
	mkReq(t, port, "__n-p-d=1", withCookie("__n-p-d=1"))
	mkReq(t, port, "", withCookie(""))
	mkReq(t, port, "", withCookie("foo=bar"))
	mkReq(t, port, "__prerender_bypass=1", withCookie("foo=bar; __prerender_bypass=1"))
	mkReq(t, port, "__n-p-d=1", withCookie("foo=bar; __n-p-d=1"))
	mkReq(t, port, "__prerender_bypass=1", withCookie("__prerender_bypass=1; foo=bar"))
	mkReq(t, port, "__prerender_bypass=1", withCookie("a=b=3; __prerender_bypass=1; foo=bar=2"))
	mkReq(t, port, "", withCookie("a=b=3; foo=bar=2; c=3"))
}

// TestSetXCacheResponseHeaderOnHitOrMiss tests whether vcl_hit and vcl_miss are called appropriately
// and that we can transport information between vcl_hit/vcl_miss and vcl_deliver to indicate to the
// client/caller whether it was a cache hit or a miss.
func TestSetXCacheResponseHeaderOnHitOrMiss(t *testing.T) {
	t.Parallel()
	var backendRequests int

	// start a test server
	testServerPort, testServer := startTestServer(func(w http.ResponseWriter, r *http.Request) {
		backendRequests++
		w.Header().Set("Cache-Control", "max-age=1, stale-while-revalidate=1")
		w.Header().Set("X-Response", r.Header.Get("X-Request"))
		w.WriteHeader(http.StatusOK)
	})
	defer testServer.Close()

	// start varnish container with a custom VCL
	port, stopFunc, err := caching.StartVarnishInDocker(caching.VarnishConfig{
		BackendPort: testServerPort,
		Vcl: `
sub vcl_hit {
  # mark the hit on the client request object
  # (we don't have resp here)
  set req.http.x-cache = "hit";
}
sub vcl_miss {
  # mark the miss on the client request object
  # (we don't have resp here)
  set req.http.x-cache = "miss";
}
sub vcl_deliver {
  # Transport the x-cache from req to resp
  set resp.http.x-cache = req.http.x-cache;
}
`,
	})
	require.NoError(t, err)
	defer stopFunc()
	waitForHealthy(t, port)

	// do the first request, which will be a miss
	assert.Equal(t, mkResp(http.StatusOK, "foo", withResponseCacheControl("max-age=1, stale-while-revalidate=1"), withXCache("miss")),
		mkReq(t, port, "foo"))

	// do the second request, which will be a hit due to being within TTL
	assert.Equal(t, mkResp(http.StatusOK, "foo", withResponseCacheControl("max-age=1, stale-while-revalidate=1"), withXCache("hit")),
		mkReq(t, port, "bar"))

	// wait for being out of TTL
	time.Sleep(1100 * time.Millisecond)

	// do the third request, which will still be considered a hit because within grace
	assert.Equal(t, mkResp(http.StatusOK, "foo", withResponseCacheControl("max-age=1, stale-while-revalidate=1"), withXCache("hit")),
		mkReq(t, port, "baz"))

	// wait a bit for background refresh
	time.Sleep(100 * time.Millisecond)

	// now, varnish will have refreshed the object in the background and it will again have a TTL of 1
	// so we must wait for being out of TTL and of grace
	time.Sleep(2100 * time.Millisecond)

	// do the fourth request, which will be a miss
	assert.Equal(t, mkResp(http.StatusOK, "foobarbaz", withResponseCacheControl("max-age=1, stale-while-revalidate=1"), withXCache("miss")),
		mkReq(t, port, "foobarbaz"))
}

func TestRfc9211CacheStatusImplementation(t *testing.T) {
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
		DefaultTtl:  "1s",
		Vcl: `
sub vcl_hit {
  set req.http.Cache-Status = "my-cache; hit";
}
sub vcl_miss {
  set req.http.Cache-Status = "my-cache; fwd=miss";
}
sub vcl_pass {
  if (req.method != "GET" && req.method != "HEAD") {
    set req.http.Cache-Status = "my-cache; fwd=method; detail=" + req.method;
  } else if (req.http.Authorization) {
    set req.http.Cache-Status = "my-cache; fwd=bypass; detail=AUTHORIZATION";
  } else if (req.http.Cookie) {
    set req.http.Cache-Status = "my-cache; fwd=bypass; detail=COOKIE";
  } else {
    set req.http.Cache-Status = "my-cache; fwd=bypass; detail=OTHER";
  }
  set req.http.x-cache = "pass";
}
sub vcl_deliver {
  set resp.http.Cache-Status = req.http.Cache-Status;
}
`,
	})
	require.NoError(t, err)
	defer stopFunc()
	waitForHealthy(t, port)

	// forward because of POST method
	assert.Equal(t, mkResp(http.StatusOK, "foo", withCacheStatus("my-cache; fwd=method; detail=POST"), withAcceptRanges("")),
		mkReq(t, port, "foo", withMethod(http.MethodPost)))

	// forward because of PUT method
	assert.Equal(t, mkResp(http.StatusOK, "bar", withCacheStatus("my-cache; fwd=method; detail=PUT"), withAcceptRanges("")),
		mkReq(t, port, "bar", withMethod(http.MethodPut)))

	// forward because of Authorization header
	assert.Equal(t, mkResp(http.StatusOK, "baz", withCacheStatus("my-cache; fwd=bypass; detail=AUTHORIZATION"), withAcceptRanges("")),
		mkReq(t, port, "baz", withAuthorization("Bearer Test")))

	// forward because of Cookie header
	assert.Equal(t, mkResp(http.StatusOK, "foobar", withCacheStatus("my-cache; fwd=bypass; detail=COOKIE"), withAcceptRanges("")),
		mkReq(t, port, "foobar", withCookie("myCookieValue=3")))

	// miss because no object in cache
	assert.Equal(t, mkResp(http.StatusOK, "foobaz", withCacheStatus("my-cache; fwd=miss")),
		mkReq(t, port, "foobaz"))

	// hit to cached object of previous request
	assert.Equal(t, mkResp(http.StatusOK, "foobaz", withCacheStatus("my-cache; hit")),
		mkReq(t, port, "barbaz"))
}

// TestDeliverInVclRecvMeansNonZeroObjTtlInVclDeliver tests that obj.ttl in vcl_deliver will
// be the TTL (determine from max-age) of the backend response when vcl_recv uses return(hash),
// which is the default.
// This test was added for a situation where we wanted to know the TTL that a backend response
// advertized in its Cache-Control header, in order to do different things conditionally based
// on obj.ttl in vcl_deliver.
// See also the next test below.
func TestReturnHashInVclRecvMeansNonZeroObjTtlInVclDeliver(t *testing.T) {
	t.Parallel()
	var backendRequests int

	// start a test server
	testServerPort, testServer := startTestServer(func(w http.ResponseWriter, r *http.Request) {
		backendRequests++
		w.Header().Set("Cache-Control", "max-age=10, stale-while-revalidate=30")
		w.WriteHeader(http.StatusOK)
	})
	defer testServer.Close()

	// start varnish container with a custom VCL
	port, stopFunc, err := caching.StartVarnishInDocker(caching.VarnishConfig{
		BackendPort: testServerPort,
		Vcl: `
sub vcl_deliver {
  set resp.http.X-Response = obj.ttl;
}
`,
	})
	require.NoError(t, err)
	defer stopFunc()
	waitForHealthy(t, port)

	resp := mkReq(t, port, "")
	xResponseAsFloat, err := strconv.ParseFloat(resp.xResponse, 32)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.statusCode)
	assert.LessOrEqual(t, xResponseAsFloat, 10.0)
}

// TestReturnPassInVclRecvMeansZeroObjTtlInVclDeliver tests that obj.ttl in vcl_deliver will
// be zero regardless of the backend response when vcl_recv uses return(pass).
// This test was added for a situation where we wanted to know the TTL that a backend response
// advertized in its Cache-Control header, in order to do different things conditionally based
// on obj.ttl in vcl_deliver, but we did not actually _cache_ the response in Varnish - only
// repair/modify Cache-Control response headers. In this situation (with a pass), obj.ttl will
// always be zero and cannot be used to determine the TTL of the backend response.
func TestReturnPassInVclRecvMeansZeroObjTtlInVclDeliver(t *testing.T) {
	t.Parallel()
	var backendRequests int

	// start a test server
	testServerPort, testServer := startTestServer(func(w http.ResponseWriter, r *http.Request) {
		backendRequests++
		w.Header().Set("Cache-Control", "max-age=10, stale-while-revalidate=30")
		w.WriteHeader(http.StatusOK)
	})
	defer testServer.Close()

	// start varnish container with a custom VCL
	port, stopFunc, err := caching.StartVarnishInDocker(caching.VarnishConfig{
		BackendPort: testServerPort,
		Vcl: `
sub vcl_recv {
  return (pass);
}
sub vcl_deliver {
  set resp.http.X-Response = obj.ttl;
}
`,
	})
	require.NoError(t, err)
	defer stopFunc()
	waitForHealthy(t, port)

	resp := mkReq(t, port, "")
	xResponseAsFloat, err := strconv.ParseFloat(resp.xResponse, 32)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.statusCode)
	assert.LessOrEqual(t, xResponseAsFloat, 0.0)
}

// TestVaryOnOrigin checks that Varnish behaves properly when adding a custom Vary response header
// in vcl_backend_response, such that further requests will not be served from the cache when
// the request header value mentioned previously in Vary is different.
func TestVaryOnOrigin(t *testing.T) {
	t.Parallel()
	var backendRequests int

	// start a test server
	testServerPort, testServer := startTestServer(func(w http.ResponseWriter, r *http.Request) {
		backendRequests++
		w.Header().Set("Access-Control-Allow-Origin", r.Header.Get("Origin"))
		w.Header().Set("Cache-Control", "max-age=300, stale-while-revalidate=30")
		w.Header().Set("Vary", "Accept-Encoding")
		w.WriteHeader(http.StatusOK)
	})
	defer testServer.Close()

	// start varnish container with a custom VCL
	port, stopFunc, err := caching.StartVarnishInDocker(caching.VarnishConfig{
		BackendPort: testServerPort,
		Vcl: `
sub vcl_backend_response {
  if (beresp.http.Access-Control-Allow-Origin || beresp.http.Access-Control-Allow-Credentials ||
      beresp.http.Access-Control-Expose-Headers || beresp.http.Access-Control-Max-Age ||
      beresp.http.Access-Control-Allow-Methods || beresp.http.Access-Control-Allow-Headers) {
    # If there wasn't a Vary header already, we must add it.
    if (!beresp.http.Vary) {
      set beresp.http.Vary = "Origin";
    } else if (beresp.http.Vary !~ "Origin") {
      set beresp.http.Vary = beresp.http.Vary + ", Origin";
    }
  }
}
`,
	})
	require.NoError(t, err)
	defer stopFunc()
	waitForHealthy(t, port)

	resp := mkReq(t, port, "", withOrigin("https://a"))
	assert.Equal(t, "https://a", resp.accessControlAllowOrigin)

	resp = mkReq(t, port, "", withOrigin("https://b"))
	assert.Equal(t, "https://b", resp.accessControlAllowOrigin)

	assert.Equal(t, 2, backendRequests)
}

// TestDoGzipWhenGetAndCacheableAndAcceptEncodingGzip checks that Varnish will
// do gzip compression when the backend did not do it when using beresp.do_gzip.
func TestDoGzipWhenGetAndCacheableAndAcceptEncodingGzip(t *testing.T) {
	t.Parallel()
	var backendRequests int

	// start a test server
	testServerPort, testServer := startTestServer(func(w http.ResponseWriter, r *http.Request) {
		oneThousandBytes := make([]byte, 1024)
		w.Header().Set("Cache-Control", "max-age=1")
		w.WriteHeader(http.StatusOK)
		w.Write(oneThousandBytes)
		backendRequests++
	})
	defer testServer.Close()

	// start varnish container with a custom VCL
	port, stopFunc, err := caching.StartVarnishInDocker(caching.VarnishConfig{
		BackendPort: testServerPort,
		Vcl: `
import std;
sub vcl_miss {
  set req.http.x-cache = "miss";
}
sub vcl_deliver {
  set resp.http.x-cache = req.http.x-cache;
}
sub vcl_backend_response {
  set beresp.do_gzip = true;
}`,
	})
	require.NoError(t, err)
	defer stopFunc()
	waitForHealthy(t, port)

	// send request which will become a 500 response
	resp := mkReq(t, port, "foo", withStoreBody(), withAcceptEncoding("gzip"))

	if resp.transferEncoding == "chunked" {
		assert.Equal(t, -1, resp.contentLength) // <- because of chunked encoding
	} else {
		assert.Equal(t, 29, resp.contentLength)
	}
	assert.Equal(t, "gzip", resp.contentEncoding)
	assert.Equal(t, "miss", resp.xCache)
	assert.Equal(t, 1, backendRequests)
	assert.Less(t, len(resp.body), 1024)
	assert.Greater(t, len(resp.body), 0)
}

// TestDontGzipWhenPassAndNoAcceptEncodingGzip checks that clients receive uncompressed responses
// when we use gzip towards the backend and the backend responding with a compressed response.
func TestDontGzipWhenPassAndNoAcceptEncodingGzip(t *testing.T) {
	t.Parallel()
	var backendRequests int

	// start a test server
	testServerPort, testServer := startTestServer(func(w http.ResponseWriter, r *http.Request) {
		oneThousandBytes := make([]byte, 1024)
		w.Header().Set("Cache-Control", "max-age=1")
		w.WriteHeader(http.StatusOK)
		w.Write(oneThousandBytes)
		backendRequests++
	})
	defer testServer.Close()

	// start varnish container with a custom VCL
	port, stopFunc, err := caching.StartVarnishInDocker(caching.VarnishConfig{
		BackendPort: testServerPort,
		Vcl: `
import std;
sub vcl_pass {
  set req.http.x-cache = "pass";
}
sub vcl_deliver {
  set resp.http.x-cache = req.http.x-cache;
}
sub vcl_backend_fetch {
  set bereq.http.Accept-Encoding = "gzip";  
}
sub vcl_backend_response {
  set beresp.do_gzip = true;
}`,
	})
	require.NoError(t, err)
	defer stopFunc()
	waitForHealthy(t, port)

	// send request which will become a 500 response
	resp := mkReq(t, port, "foo", withStoreBody(), withMethod(http.MethodPost))

	if resp.transferEncoding == "chunked" {
		assert.Equal(t, -1, resp.contentLength) // <- because of chunked encoding
	} else {
		assert.Equal(t, 1024, resp.contentLength)
	}
	assert.Equal(t, "", resp.contentEncoding)
	assert.Equal(t, "pass", resp.xCache)
	assert.Equal(t, 1, backendRequests)
	assert.Equal(t, len(resp.body), 1024)
}
