package caching

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
)

func newServer(handler http.Handler) *httptest.Server {
	server := &httptest.Server{
		Listener: newListener(),
		Config: &http.Server{
			Handler: handler,
		},
	}
	server.Start()
	return server
}

func newListener() net.Listener {
	l, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		panic(err)
	}
	return l
}

func StartTestServer(handler func(w http.ResponseWriter, r *http.Request)) (string, *httptest.Server) {
	srv := newServer(http.HandlerFunc(handler))
	// determine port
	hostNameAndPort := srv.URL[len("http://"):]
	indexOfPort := strings.LastIndex(hostNameAndPort, ":")
	port := hostNameAndPort[indexOfPort+1:]
	return port, srv
}
