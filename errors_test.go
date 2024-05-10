// Contains tests for handling of errors
package caching_test

import (
	"caching"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"net/http"
	"testing"
)

// Test503FromBackendIsNotVclBackendError tests that a 503 response from the backend
// is not treated as a VCL backend error and thus vcl_backend_error is not called.
func Test503FromBackendIsNotVclBackendError(t *testing.T) {
	t.Parallel()
	var backendRequests int

	// start a test server
	testServerPort, testServer := startTestServer(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Response", r.Header.Get("X-Request"))
		w.WriteHeader(http.StatusServiceUnavailable)
		backendRequests++
	})
	defer testServer.Close()

	// start varnish container with a custom VCL
	port, stopFunc, err := caching.StartVarnishInDocker(caching.VarnishConfig{
		BackendPort: testServerPort,
		Vcl: `
sub vcl_backend_error {
    set beresp.body = "ERROR: " + beresp.status + " " + beresp.reason;
    return (deliver);
}
`,
	})
	require.NoError(t, err)
	defer stopFunc()
	waitForHealthy(t, port)

	// send request
	assert.Equal(t, mkResp(http.StatusServiceUnavailable, "foo", withBody("")), mkReq(t, port, "foo", withStoreBody()))

	// expect one backend request
	assert.Equal(t, 1, backendRequests)
}

// TestNoConnectionToBackendIsVclBackendError tests that a connection error to the backend
// is treated as a VCL backend error and thus vcl_backend_error is called.
func TestNoConnectionToBackendIsVclBackendError(t *testing.T) {
	t.Parallel()

	// start a test server
	testServerPort, testServer := startTestServer(func(w http.ResponseWriter, r *http.Request) {
		assert.Fail(t, "should not be called")
	})
	defer testServer.Close()

	// start varnish container with a custom VCL
	port, stopFunc, err := caching.StartVarnishInDocker(caching.VarnishConfig{
		BackendPort: testServerPort,
		Vcl: `
sub vcl_backend_error {
    set beresp.body = "ERROR: " + beresp.status + " " + beresp.reason;
    return (deliver);
}
`,
	})
	require.NoError(t, err)
	defer stopFunc()
	waitForHealthy(t, port)

	// stop the backend
	testServer.Close()

	// send request
	assert.Equal(t, mkResp(http.StatusServiceUnavailable, "", withBody("ERROR: 503 Backend fetch failed")), mkReq(t, port, "foo", withStoreBody()))
}
