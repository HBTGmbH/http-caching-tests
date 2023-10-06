Execute the tests via `go test -v ./...` from the root directory of this project.

# How it works

Each test case will start Varnish as a Docker container and start a simple Go HTTP Server as the backend
for Varnish. The test case will then send requests and verify both the requests sent by Varnish to the test server
as well as the response received from Varnish.