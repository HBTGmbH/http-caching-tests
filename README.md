Execute the tests via `go test -v ./...` from the root directory of this project.

# How it works

Each test case will start Varnish as a Docker container and will start a simple httptest Go HTTP Server as the backend
for Varnish. The test case will then send requests with various headers and verify the response depending on those
headers and the configuration of Varnish and the backend HTTP test server.