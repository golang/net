package websocket_test

import (
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"

	"golang.org/x/net/websocket"
)

// This example demonstrates how (*Server).Handshake works
func ExampleServer() {
	origin, _ := url.Parse("http://localhost")
	serv := testServer(websocket.Config{
		Origin: origin,
	})
	defer serv.Close()

	url := "ws" + strings.TrimPrefix(serv.URL, "http")
	ws, err := websocket.Dial(url, origin.Scheme, origin.String())
	if err != nil {
		log.Fatalln(err)
	}
	received, err := receive(ws, 512)
	if err != nil {
		log.Fatalln(err)
	}

	fmt.Println(received)
	// Output:
	// hello websocket
}

func testServer(cnf websocket.Config) *httptest.Server {
	return httptest.NewServer(websocket.Server{
		Config: cnf,
		Handshake: func(cnf *websocket.Config, r *http.Request) error {
			proto, origin := r.Header.Get("Sec-Websocket-Protocol"), r.Header.Get("Origin")
			if err := checkProtocol(proto, cnf.Protocol...); err != nil {
				return err
			}
			if origin != cnf.Origin.String() {
				return websocket.ErrBadWebSocketOrigin
			}

			return nil
		},
		Handler: func(conn *websocket.Conn) {
			if _, err := conn.Write([]byte("hello websocket\n")); err != nil {
				log.Fatal(err)
			}
		},
	})
}

func checkProtocol(requested string, accepteds ...string) error {
	for _, accepted := range accepteds {
		if requested == accepted {
			return nil
		}
	}

	return websocket.ErrBadWebSocketProtocol
}

func receive(ws *websocket.Conn, max int) (string, error) {
	msg := make([]byte, max)
	n, err := ws.Read(msg)
	if err != nil {
		return "", err
	}

	return string(msg[:n]), nil
}
