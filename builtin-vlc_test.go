// Contains tests for default behaviour using Varnish's built-in VCL implementation
package caching_test

import (
	"caching"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"net/http"
	"strconv"
	"sync"
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
	testServerPort, testServer := startTestServer(func(w http.ResponseWriter, r *http.Request) {
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
	waitForHealthy(t, port)

	// send request
	assert.Equal(t, "foo", mkReq(t, port, "foo").xResponse)

	// wait half a second
	time.Sleep(500 * time.Millisecond)

	// send another request and expect the previous cached return
	assert.Equal(t, "foo", mkReq(t, port, "bar").xResponse)

	// wait for 600 ms
	time.Sleep(600 * time.Millisecond)

	// send another request and expect no cached return
	assert.Equal(t, "baz", mkReq(t, port, "baz").xResponse)

	// expect two backend requests
	assert.Equal(t, 2, backendRequests)
}

// TestCachingOf404 tests that Varnish will cache a 404 response from the backend by default.
// For simplicity, we will use the default TTL without Cache-Control header.
func TestCachingOf404(t *testing.T) {
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

	// start varnish container
	port, stopFunc, err := caching.StartVarnishInDocker(caching.VarnishConfig{
		BackendPort: testServerPort,
		DefaultTtl:  "1s",
	})
	require.NoError(t, err)
	defer stopFunc()
	waitForHealthy(t, port)

	// send request and expect the backend to respond with 404
	assert.Equal(t, resp(http.StatusNotFound, "foo"), mkReq(t, port, "foo", withXStatusCode(http.StatusNotFound)))

	// wait half a second
	time.Sleep(500 * time.Millisecond)

	// send another request which the backend would respond with 200 but expect the previous cached 404 response
	assert.Equal(t, resp(http.StatusNotFound, "foo"), mkReq(t, port, "bar", withXStatusCode(http.StatusOK)))

	// expect one backend request
	assert.Equal(t, 1, backendRequests)
}

// TestNoCachingOfPost tests that Varnish will not cache a POST request by default.
func TestNoCachingOfPost(t *testing.T) {
	t.Parallel()
	var backendRequests int

	// start a test server
	testServerPort, testServer := startTestServer(func(w http.ResponseWriter, r *http.Request) {
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
	waitForHealthy(t, port)

	// send a POST request (which should not get cached)
	assert.Equal(t, resp(http.StatusOK, "foo"), mkReq(t, port, "foo", withMethod(http.MethodPost)))

	// wait half a second
	time.Sleep(500 * time.Millisecond)

	// send another request and expect an uncached response
	assert.Equal(t, resp(http.StatusOK, "bar"), mkReq(t, port, "bar", withMethod(http.MethodPost)))

	// expect two backend requests
	assert.Equal(t, 2, backendRequests)
}

// TestNoCachingOf500ErrorOnFirstRequest tests that Varnish will not cache an initial 500 error
// response from the backend when Varnish did not yet have a non 5xx response in its cache.
// The scenario here is: Varnish starts up and the backend responds with 500. In that case, Varnish
// will not start to cache these 500 errors, but request each time from the backend.
func TestNoCachingOf500ErrorOnFirstRequest(t *testing.T) {
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

	// start varnish container
	port, stopFunc, err := caching.StartVarnishInDocker(caching.VarnishConfig{
		BackendPort: testServerPort,
		DefaultTtl:  "1s",
	})
	require.NoError(t, err)
	defer stopFunc()
	waitForHealthy(t, port)

	// send request resulting in 500
	assert.Equal(t, resp(http.StatusInternalServerError, "1"), mkReq(t, port, "1", withXStatusCode(http.StatusInternalServerError)))

	// wait half a second
	time.Sleep(500 * time.Millisecond)

	// send another request and expect the previous cached error
	assert.Equal(t, resp(http.StatusOK, "2"), mkReq(t, port, "2", withXStatusCode(http.StatusOK)))

	// expect two backend requests (because the first one wasn't cached)
	assert.Equal(t, 2, backendRequests)
}

// TestNoCachingOf500ErrorInGracePeriodAfter200Request tests that Varnish will not cache a 500 error
// response from the backend even if Varnish had a 200 response in its cache within the grace period.
func TestNoCachingOf500ErrorInGracePeriodAfter200Request(t *testing.T) {
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

	// start varnish container
	port, stopFunc, err := caching.StartVarnishInDocker(caching.VarnishConfig{
		BackendPort:  testServerPort,
		DefaultTtl:   "1s",
		DefaultGrace: "5s",
	})
	require.NoError(t, err)
	defer stopFunc()
	waitForHealthy(t, port)

	// send request resulting in 200
	assert.Equal(t, resp(http.StatusOK, "1"), mkReq(t, port, "1", withXStatusCode(http.StatusOK)))

	// wait 1.1 seconds to let the response expire
	time.Sleep(1100 * time.Millisecond)

	// send another request which would result in 500 but still expect the previous cached 200 response
	// because we are still in the grace period and within that Varnish will perform background revalidation
	// asynchronous to the client's request.
	assert.Equal(t, resp(http.StatusOK, "1"), mkReq(t, port, "2", withXStatusCode(http.StatusInternalServerError)))

	// wait a bit for Varnish to revalidate the cached response. After this, Varnish will have
	// abandoned the cached 200 response and will also not have cached the 500 response resulting
	// in subsequent requests to always hit the backend.
	time.Sleep(50 * time.Millisecond)

	// send another request which will result in a backend fetch returning 500.
	assert.Equal(t, resp(http.StatusInternalServerError, "3"), mkReq(t, port, "3", withXStatusCode(http.StatusInternalServerError)))

	// send yet another request which will also result in a backend fetch returning 500
	// indicating that the previous response has not been cached.
	assert.Equal(t, resp(http.StatusInternalServerError, "4"), mkReq(t, port, "4", withXStatusCode(http.StatusInternalServerError)))

	// expect four backend requests
	assert.Equal(t, 4, backendRequests)
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
	testServerPort, testServer := startTestServer(func(w http.ResponseWriter, r *http.Request) {
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
	waitForHealthy(t, port)

	// send request
	assert.Equal(t, "foo", mkReq(t, port, "foo").xResponse)

	// send another request
	assert.Equal(t, "bar", mkReq(t, port, "bar").xResponse)

	// expect two backend requests
	assert.Equal(t, 2, backendRequests)
}

// TestCacheControlMaxAge1 tests that Varnish will respond with a cached item when the backend
// responds with a "Cache-Control: max-age=1" header, and the cache item has not yet expired.
func TestCacheControlMaxAge1(t *testing.T) {
	t.Parallel()
	var backendRequests int

	// start a test server
	testServerPort, testServer := startTestServer(func(w http.ResponseWriter, r *http.Request) {
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
	waitForHealthy(t, port)

	// send request to varnish
	assert.Equal(t, "1", mkReq(t, port, "1").xResponse)

	// send another request and expect to receive a cached response
	assert.Equal(t, "1", mkReq(t, port, "2").xResponse)

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
	testServerPort, testServer := startTestServer(func(w http.ResponseWriter, r *http.Request) {
		xRequest := r.Header.Get("X-Request")
		if xRequest == "2" {
			time.Sleep(500 * time.Millisecond)
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
	waitForHealthy(t, port)

	// send request to varnish
	assert.Equal(t, "1", mkReq(t, port, "1").xResponse)

	// sleep for 1.1 seconds to make the cached response stale
	time.Sleep(1100 * time.Millisecond)

	// send another request and expect to receive a cached response
	time1 := time.Now()
	assert.Equal(t, "1", mkReq(t, port, "2").xResponse)
	time2 := time.Now()
	// expect the response to have come back very fast
	assert.Less(t, time2.Sub(time1), 100*time.Millisecond)

	// sleep for 600 ms to let Varnish revalidate the cached response
	time.Sleep(600 * time.Millisecond)

	// send yet another request and expect to receive the second cached response
	assert.Equal(t, "2", mkReq(t, port, "3").xResponse)

	// expect two backend requests
	assert.Equal(t, 2, backendRequests)
}

// TestStaleWhileRevalidateWithoutTtlOrExpiresAndZeroDefaultTtl tests that Varnish will not cache a response
// (not even for the grace period) when the backend responded with a "Cache-Control: stale-while-revalidate"
// header but the response did not have a "Cache-Control: max-age" or "Expires" and the default TTL is 0.
func TestStaleWhileRevalidateWithoutTtlOrExpiresAndZeroDefaultTtl(t *testing.T) {
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
	})
	require.NoError(t, err)
	defer stopFunc()
	waitForHealthy(t, port)

	// send first request which should get a grace of only 1s
	assert.Equal(t, respCC(http.StatusOK, "foo", "stale-while-revalidate=1"), mkReq(t, port, "foo"))

	// with a non-existing max-age/TTL/Expires or 0, the behaviour of Varnish is to not cache the response
	// at all, also not for the grace period. So, every request will essentially be a pass.
	time.Sleep(500 * time.Millisecond)

	// send another request and expect a new synchronous backend request
	assert.Equal(t, respCC(http.StatusOK, "bar", "stale-while-revalidate=1"), mkReq(t, port, "bar"))

	// expect two backend requests
	assert.Equal(t, 2, backendRequests)
}

// TestHitForMissAndNoRequestCoalescingWhenNoStore tests that Varnish will not serialize multiple requests when
// the first response marks the response as uncacheable due to "Cache-Control: no-store".
// This is tested by sending N requests in parallel, where the first request will take about 1 second to
// respond, and the remaining N-1 requests will then respond in parallel because Varnish will
// create a "hit-for-miss" cache item and store that for 120s by default.
// See: https://github.com/varnishcache/varnish-cache/blob/master/bin/varnishd/builtin.vcl#L248-L252
func TestHitForMissAndNoRequestCoalescingWhenNoStore(t *testing.T) {
	t.Parallel()
	var backendRequests int
	sleepTime := 1 * time.Second

	// start a test server
	testServerPort, testServer := startTestServer(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(sleepTime)

		// The below will trigger Varnish's vcl_beresp_hitmiss logic
		// see: https://github.com/varnishcache/varnish-cache/blob/master/bin/varnishd/builtin.vcl#L248-L252
		w.Header().Set("Cache-Control", "no-store")

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
	waitForHealthy(t, port)

	const N = 10

	// send N requests in parallel
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		var i = i
		go func() {
			// and assert that each request (with each individual X-Request header)
			// gets a response with its corresponding individual X-Response header
			assert.Equal(t, strconv.Itoa(i), mkReq(t, port, strconv.Itoa(i)).xResponse)
			wg.Done()
		}()
	}

	// expect N responses, but NOT all of them serialized!
	time1 := time.Now()
	wg.Wait()
	time2 := time.Now()

	// expect all but the first response to have come back in parallel.
	// What will happen is: The first request will take sleepTime to respond,
	// then Varnish will create the hit-for-miss cache item and start off
	// the following N-1 requests in parallel, which will all take sleepTime
	// together to respond.
	// Therefore, the whole test case is completed after about 2 * sleepTime.
	assert.Less(t, time2.Sub(time1), 2*sleepTime+100*time.Millisecond)
	assert.Greater(t, time2.Sub(time1), 2*sleepTime-100*time.Millisecond)

	// expect N backend requests
	assert.Equal(t, N, backendRequests)
}

// TestNoCachingWhenRequestHasAuthorizationHeader tests that Varnish will not cache a response
// when the request has an "Authorization" header.
func TestNoCachingWhenRequestHasAuthorizationHeader(t *testing.T) {
	t.Parallel()
	var backendRequests int

	// start a test server
	testServerPort, testServer := startTestServer(func(w http.ResponseWriter, r *http.Request) {
		// test for the Authorization header to have the correct value
		xRequest := r.Header.Get("X-Request")
		if xRequest == "foo" {
			assert.Equal(t, "Test 12345", r.Header.Get("Authorization"))
		} else if xRequest == "bar" {
			assert.Equal(t, "Test 67890", r.Header.Get("Authorization"))
		}
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
	waitForHealthy(t, port)

	// send request with Authorization header
	assert.Equal(t, "foo", mkReq(t, port, "foo", withAuthorization("Test 12345")).xResponse)

	// wait a bit
	time.Sleep(50 * time.Millisecond)

	// send another request and expect uncached response
	assert.Equal(t, "bar", mkReq(t, port, "bar", withAuthorization("Test 67890")).xResponse)

	// expect two backend requests
	assert.Equal(t, 2, backendRequests)
}

// TestNoCachingWhenRequestHasCookieHeader tests that Varnish will not cache a response
// when the request has an "Cookie" header.
func TestNoCachingWhenRequestHasCookieHeader(t *testing.T) {
	t.Parallel()
	var backendRequests int

	// start a test server
	testServerPort, testServer := startTestServer(func(w http.ResponseWriter, r *http.Request) {
		// test for the Authorization header to have the correct value
		xRequest := r.Header.Get("X-Request")
		if xRequest == "foo" {
			assert.Equal(t, "test=12345", r.Header.Get("Cookie"))
		} else if xRequest == "bar" {
			assert.Equal(t, "test=67890", r.Header.Get("Cookie"))
		}
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
	waitForHealthy(t, port)

	// send request with Authorization header
	assert.Equal(t, "foo", mkReq(t, port, "foo", withCookie("test=12345")).xResponse)

	// wait a bit
	time.Sleep(50 * time.Millisecond)

	// send another request and expect uncached response
	assert.Equal(t, "bar", mkReq(t, port, "bar", withCookie("test=67890")).xResponse)

	// expect two backend requests
	assert.Equal(t, 2, backendRequests)
}

// TestBackendRespondsWith304WhenUnconditionalRequest tests what Varnish will do
// when the backend responds with 304 for an unconditional request, which is considered
// illegal by the HTTP 1.1 spec.
func TestBackendRespondsWith304WhenUnconditionalRequest(t *testing.T) {
	t.Parallel()
	var backendRequests int

	// start a test server
	testServerPort, testServer := startTestServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
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
	waitForHealthy(t, port)

	// send request which will be answered with 304 by the backend
	// but Varnish will return 503 to the client, because the backend
	// responding with a 304 for an unconditional request is an error.
	assert.Equal(t, resp(http.StatusServiceUnavailable, ""), mkReq(t, port, "foo"))

	// expect one backend request
	assert.Equal(t, 1, backendRequests)
}

// TestConditionalRequestWhenRevalidatingWithEtag tests that Varnish will perform a conditional request
// when revalidating a cached response that had an Etag validator and that Varnish is able to understand a 304 response
// (without a body) while still retaining the body of the first cached response.
func TestConditionalRequestWhenRevalidatingWithEtag(t *testing.T) {
	t.Parallel()
	var backendRequests int

	// start a test server
	testServerPort, testServer := startTestServer(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Response", r.Header.Get("X-Request"))
		w.Header().Set("Etag", "1234")
		if backendRequests == 0 {
			// check that the first request won't be conditional
			assert.Equal(t, "", r.Header.Get("If-None-Match"))
			w.WriteHeader(http.StatusOK)
			// and respond with a body
			_, err := w.Write([]byte("foo"))
			assert.NoError(t, err)
		} else if backendRequests == 1 {
			// the second request should be conditional and should include the If-None-Match header
			// with the ETag value from the previous response
			assert.Equal(t, "1234", r.Header.Get("If-None-Match"))
			// here, we will respond with 304, which Varnish translates to 200 for the client
			w.WriteHeader(http.StatusNotModified)
		} else {
			assert.Fail(t, "unexpected backend request")
		}
		backendRequests++
	})
	defer testServer.Close()

	// start varnish container
	port, stopFunc, err := caching.StartVarnishInDocker(caching.VarnishConfig{
		BackendPort: testServerPort,
		DefaultTtl:  "1s",
		DefaultKeep: "5s",
	})
	require.NoError(t, err)
	defer stopFunc()
	waitForHealthy(t, port)

	// send the first request which will be answered with 200 by the backend
	// and cached for 1 second. The response will have an Etag header to
	// enable conditional revalidation later.
	assert.Equal(t, respB(http.StatusOK, "1", "foo"), mkReq(t, port, "1", withStoreBody()))

	// wait a bit for the response to become stale and enter the "keep" interval
	// in which Varnish will still keep the cached object around but only for synchronous revalidation
	time.Sleep(1100 * time.Millisecond)

	// send the second request which will be answered with 304 by the backend
	// and Varnish will respond with 200 to the client, still with the response body
	// of the first response (which now became fresh again) and the headers of
	// the revalidation request's response from the backend.
	// Note that in this case we use "keep" instead of "grace" here to let Varnish
	// revalidate synchronously.
	assert.Equal(t, respB(http.StatusOK, "2", "foo"), mkReq(t, port, "2", withStoreBody()))

	// wait a tiny bit to see if we have the response still cached
	time.Sleep(200 * time.Millisecond)

	// send the third request which will be answered directly from the cache
	// because the once stale response became fresh again after the second request,
	// which successfully revalidated the cached object.
	assert.Equal(t, respB(http.StatusOK, "2", "foo"), mkReq(t, port, "3", withStoreBody()))

	// expect two backend requests
	assert.Equal(t, 2, backendRequests)
}

// TestConditionalRequestWhenRevalidatingWithLastModified tests that Varnish will perform a conditional request
// when revalidating a cached response that had a Last-Modified validator and that Varnish is able to understand
// a 304 response (without a body) while still retaining the body of the first cached response.
func TestConditionalRequestWhenRevalidatingWithLastModified(t *testing.T) {
	t.Parallel()
	var backendRequests = 0

	lastModified := time.Now().Add(-2 * time.Hour).UTC().Format(http.TimeFormat)

	// start a test server
	testServerPort, testServer := startTestServer(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Response", r.Header.Get("X-Request"))
		w.Header().Set("Last-Modified", lastModified)
		if backendRequests == 0 {
			// check that the first request won't be conditional
			assert.Equal(t, "", r.Header.Get("If-Modified-Since"))
			w.WriteHeader(http.StatusOK)
			// and respond with a body
			_, err := w.Write([]byte("foo"))
			assert.NoError(t, err)
		} else if backendRequests == 1 {
			// the second request should be conditional and should include the If-Modified-Since header
			// with the Last-Modified value from the previous response
			assert.Equal(t, lastModified, r.Header.Get("If-Modified-Since"))
			// here, we will respond with 304, which Varnish translates to 200 for the client
			w.WriteHeader(http.StatusNotModified)
		} else {
			assert.Fail(t, "unexpected backend request")
		}
		backendRequests++
	})
	defer testServer.Close()

	// start varnish container
	port, stopFunc, err := caching.StartVarnishInDocker(caching.VarnishConfig{
		BackendPort: testServerPort,
		DefaultTtl:  "1s",
		DefaultKeep: "5s",
	})
	require.NoError(t, err)
	defer stopFunc()
	waitForHealthy(t, port)

	// send the first request which will be answered with 200 by the backend
	// and cached for 1 second. The response will have an Etag header to
	// enable conditional revalidation later.
	assert.Equal(t, respB(http.StatusOK, "1", "foo"), mkReq(t, port, "1", withStoreBody()))

	// wait a bit for the response to become stale and enter the "keep" interval
	// in which Varnish will still keep the cached object around but only for synchronous revalidation
	time.Sleep(1100 * time.Millisecond)

	// send the second request which will be answered with 304 by the backend
	// and Varnish will respond with 200 to the client, still with the response body
	// of the first response (which now became fresh again) and the headers of
	// the revalidation request's response from the backend.
	// Note that in this case we use "keep" instead of "grace" here to let Varnish
	// revalidate synchronously.
	assert.Equal(t, respB(http.StatusOK, "2", "foo"), mkReq(t, port, "2", withStoreBody()))

	// wait a tiny bit to see if we have the response still cached
	time.Sleep(200 * time.Millisecond)

	// send the third request which will be answered directly from the cache
	// because the once stale response became fresh again after the second request,
	// which successfully revalidated the cached object.
	assert.Equal(t, respB(http.StatusOK, "2", "foo"), mkReq(t, port, "3", withStoreBody()))

	// expect two backend requests
	assert.Equal(t, 2, backendRequests)
}

// TestMaxAge0AndNoCacheInRequest tests that Varnish will simply ignore the "Cache-Control: max-age=0, no-cache"
// header in the request and will not revalidate with the backend, by default.
func TestMaxAge0AndNoCacheInRequest(t *testing.T) {
	t.Parallel()
	var backendRequests int

	// start a test server
	testServerPort, testServer := startTestServer(func(w http.ResponseWriter, r *http.Request) {
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
	waitForHealthy(t, port)

	// send request
	assert.Equal(t, "foo", mkReq(t, port, "foo").xResponse)

	// wait a bit
	time.Sleep(100 * time.Millisecond)

	// send another request with "Cache-Control: max-age=0, no-cache" and expect the previous cached return
	// because by default Varnish cannot be forced to revalidate with the backend based on the client's
	// request headers.
	assert.Equal(t, "foo", mkReq(t, port, "bar", withCacheControl("max-age=0, no-cache")).xResponse)

	// expect one backend request
	assert.Equal(t, 1, backendRequests)
}

// TestClientConditionalRequestWithEtag tests that Varnish will understand a client's conditional request
// and will respond with a cached item (only headers) when the client sends an "If-None-Match"
// that matches the cached item's Etag validator.
func TestClientConditionalRequestWithEtag(t *testing.T) {
	t.Parallel()
	var backendRequests int

	// start a test server
	testServerPort, testServer := startTestServer(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Response", r.Header.Get("X-Request"))
		w.Header().Set("Etag", "12345")
		w.WriteHeader(http.StatusOK)
		_, err := w.Write([]byte("foo"))
		assert.NoError(t, err)
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
	waitForHealthy(t, port)

	// send request
	assert.Equal(t, "foo", mkReq(t, port, "foo").xResponse)

	// wait a bit
	time.Sleep(100 * time.Millisecond)

	// send another request with "If-None-Match: 12345" header and expect the previous cached return
	// together with a 304 response code and no body.
	assert.Equal(t, respB(http.StatusNotModified, "foo", ""), mkReq(t, port, "bar", withIfNoneMatch("12345")))

	// expect one backend request
	assert.Equal(t, 1, backendRequests)
}

// TestStaleWhileRevalidateWithoutDurationWhenZeroDefaultGrace tests what Varnish will do when the backend responds with
// "Cache-Control: stale-while-revalidate" without a duration and a zero default grace period.
// Currently, apparently Varnish will interpret this as stale-while-revalidate=0.
// See also: https://datatracker.ietf.org/doc/html/rfc5861#section-3
// According to this specification, the syntax for "stale-while-revalidate" is always
// "stale-while-revalidate=<seconds>", so it is actually not allowed to omit the duration.
func TestStaleWhileRevalidateWithoutDurationWhenZeroDefaultGrace(t *testing.T) {
	t.Parallel()
	var backendRequests int

	// start a test server
	testServerPort, testServer := startTestServer(func(w http.ResponseWriter, r *http.Request) {
		backendRequests++
		time.Sleep(500 * time.Millisecond)
		w.Header().Set("X-Response", r.Header.Get("X-Request"))
		w.Header().Set("Cache-Control", "s-maxage=1, stale-while-revalidate")
		w.WriteHeader(http.StatusOK)
	})
	defer testServer.Close()

	// start varnish container
	port, stopFunc, err := caching.StartVarnishInDocker(caching.VarnishConfig{
		BackendPort: testServerPort,
	})
	require.NoError(t, err)
	defer stopFunc()
	waitForHealthy(t, port)

	// send request
	time1 := time.Now()
	assert.Equal(t, "foo", mkReq(t, port, "foo").xResponse)
	time2 := time.Now()
	assert.Greater(t, time2.Sub(time1), 400*time.Millisecond)

	// wait a bit for the response to become stale
	time.Sleep(1100 * time.Millisecond)

	// send another request which should also hit the backend synchronously
	time1 = time.Now()
	assert.Equal(t, "bar", mkReq(t, port, "bar").xResponse)
	time2 = time.Now()
	assert.Greater(t, time2.Sub(time1), 400*time.Millisecond)

	// expect two backend requests
	assert.Equal(t, 2, backendRequests)
}

// TestStaleWhileRevalidateWithoutDurationWhenNonZeroDefaultGrace tests what Varnish will do when the backend responds
// with "Cache-Control: stale-while-revalidate" without a duration when there is a non-zero default grace period.
// This is similar to TestStaleWhileRevalidateWithoutDurationWhenZeroDefaultGrace above just that
// we define a default grace period here.
func TestStaleWhileRevalidateWithoutDurationWhenNonZeroDefaultGrace(t *testing.T) {
	t.Parallel()
	var backendRequests int

	// start a test server
	testServerPort, testServer := startTestServer(func(w http.ResponseWriter, r *http.Request) {
		backendRequests++
		time.Sleep(500 * time.Millisecond)
		w.Header().Set("X-Response", r.Header.Get("X-Request"))
		w.Header().Set("Cache-Control", "s-maxage=1, stale-while-revalidate")
		w.WriteHeader(http.StatusOK)
	})
	defer testServer.Close()

	// start varnish container
	port, stopFunc, err := caching.StartVarnishInDocker(caching.VarnishConfig{
		BackendPort:  testServerPort,
		DefaultGrace: "10s",
	})
	require.NoError(t, err)
	defer stopFunc()
	waitForHealthy(t, port)

	// send request
	time1 := time.Now()
	assert.Equal(t, "foo", mkReq(t, port, "foo").xResponse)
	time2 := time.Now()
	assert.Greater(t, time2.Sub(time1), 400*time.Millisecond)

	// wait a bit for the response to become stale
	time.Sleep(1100 * time.Millisecond)

	// send another request which should respond immediately with the previously cached response
	// and trigger an asynchronous revalidation due to the non-zero default grace which will be applied here.
	time1 = time.Now()
	assert.Equal(t, "foo", mkReq(t, port, "bar").xResponse)
	time2 = time.Now()
	assert.Less(t, time2.Sub(time1), 100*time.Millisecond)

	// wait a bit for the asynchronous revalidation to complete
	time.Sleep(100 * time.Millisecond)

	// expect two backend requests
	assert.Equal(t, 2, backendRequests)
}

// TestStaleWhileRevalidateZeroDoesNotMeanDefaultGrace tests that the default grace period will not apply
// when the backend responds with "Cache-Control: stale-while-revalidate=0".
// The default grace period should only apply if Varnish could not figure out the grace period
// from the response headers, e.g. when Cache-Control contains no or contains an _invalid_
// stale-while-revalidate (without a duration).
func TestStaleWhileRevalidateZeroDoesNotMeanDefaultGrace(t *testing.T) {
	t.Parallel()
	var backendRequests int

	// start a test server
	testServerPort, testServer := startTestServer(func(w http.ResponseWriter, r *http.Request) {
		backendRequests++
		time.Sleep(500 * time.Millisecond)
		w.Header().Set("X-Response", r.Header.Get("X-Request"))
		w.Header().Set("Cache-Control", "max-age=1, stale-while-revalidate=0")
		w.WriteHeader(http.StatusOK)
	})
	defer testServer.Close()

	// start varnish container
	port, stopFunc, err := caching.StartVarnishInDocker(caching.VarnishConfig{
		BackendPort:  testServerPort,
		DefaultGrace: "10s",
	})
	require.NoError(t, err)
	defer stopFunc()
	waitForHealthy(t, port)

	// send request
	time1 := time.Now()
	assert.Equal(t, "foo", mkReq(t, port, "foo").xResponse)
	time2 := time.Now()
	assert.Greater(t, time2.Sub(time1), 400*time.Millisecond)

	// wait a bit for the response to become stale
	time.Sleep(1100 * time.Millisecond)

	// send another request which should also hit the backend synchronously
	time1 = time.Now()
	assert.Equal(t, "bar", mkReq(t, port, "bar").xResponse)
	time2 = time.Now()
	assert.Greater(t, time2.Sub(time1), 400*time.Millisecond)

	// expect two backend requests
	assert.Equal(t, 2, backendRequests)
}

// TestStaleWhileRevalidateWithZeroDuration tests that Varnish will do a non-ranged request to the backend when
// the client sends a ranged request (i.e. with request header "Range").
func TestRangeRequestIsAlwaysNonRangedForBackend(t *testing.T) {
	t.Parallel()
	var backendRequests int

	// start a test server
	testServerPort, testServer := startTestServer(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("Range"))
		w.Header().Set("X-Response", r.Header.Get("X-Request"))
		w.Header().Set("Cache-Control", "max-age=100")
		w.WriteHeader(http.StatusOK)
		backendRequests++
		_, _ = w.Write([]byte("foo"))
	})
	defer testServer.Close()

	// start varnish container
	port, stopFunc, err := caching.StartVarnishInDocker(caching.VarnishConfig{
		BackendPort: testServerPort,
	})
	require.NoError(t, err)
	defer stopFunc()
	waitForHealthy(t, port)

	// send first range request to varnish and expect an Accept-Ranges header with "bytes"
	assert.Equal(t, "bytes", mkReq(t, port, "1").acceptRanges)

	// send a range request and expect to receive a cached response
	assert.Equal(t, "1", mkReq(t, port, "2", withRange("bytes=0..")).xResponse)

	// expect one backend request
	assert.Equal(t, 1, backendRequests)
}
