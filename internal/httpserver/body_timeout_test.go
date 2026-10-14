package httpserver

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// Exercise the server's real request-body deadline, including a complete JSON
// value whose declared body has not finished arriving. Neither failure may be
// mislabeled as malformed JSON or reach generation.
func TestJSONBodyReadTimeoutIsNotInvalidJSON(t *testing.T) {
	for _, decode := range []bool{false, true} {
		for _, prefix := range []string{"", `{"model":"m"}`} {
			t.Run(fmt.Sprintf("decode=%t/prefix=%d", decode, len(prefix)), func(t *testing.T) {
				var generations atomic.Int32
				server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if decode {
						var value map[string]any
						if !DecodeJSONBody(w, r, &value) {
							return
						}
					} else if _, err := ReadBoundedJSONBody(w, r); err != nil {
						return
					}
					generations.Add(1)
					w.WriteHeader(http.StatusNoContent)
				}))
				server.Config.ReadHeaderTimeout = time.Second
				server.Config.ReadTimeout = 100 * time.Millisecond
				server.Start()
				defer server.Close()
				conn, err := net.DialTimeout("tcp", server.Listener.Addr().String(), time.Second)
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
					t.Fatal(err)
				}
				// Withhold the final byte just as an incomplete upload would.
				_, err = fmt.Fprintf(conn, "POST /cline/v1/responses HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(prefix)+1, prefix)
				if err != nil {
					t.Fatal(err)
				}
				response, err := http.ReadResponse(bufio.NewReader(conn), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				body, err := io.ReadAll(response.Body)
				if err != nil {
					t.Fatal(err)
				}
				var envelope struct {
					Error struct {
						Code    string `json:"code"`
						Message string `json:"message"`
					} `json:"error"`
				}
				if err := json.Unmarshal(body, &envelope); err != nil {
					t.Fatal(err)
				}
				if response.StatusCode != http.StatusRequestTimeout || envelope.Error.Code != "timeout" || envelope.Error.Message != "request body read timed out" {
					t.Fatalf("timeout response = %d %s", response.StatusCode, body)
				}
				if generations.Load() != 0 {
					t.Fatal("incomplete upload reached generation")
				}
			})
		}
	}
}
