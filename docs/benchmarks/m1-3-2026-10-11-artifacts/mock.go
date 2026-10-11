package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"
)

const fixedLatency = 15 * time.Millisecond
const streamGap = 10 * time.Millisecond

var responseBody = []byte(`{"id":"bench","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":8,"completion_tokens":2,"total_tokens":10}}`)
var streamParts = [][]byte{
	[]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"),
	[]byte("data: {\"choices\":[{\"delta\":{\"content\":\" world\"}}]}\n\n"),
	[]byte("data: [DONE]\n\n"),
}

type handler struct{}

func (handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
		return
	}
	_, _ = io.Copy(io.Discard, r.Body)
	_ = r.Body.Close()
	time.Sleep(fixedLatency)
	if r.Header.Get("Accept") == "text/event-stream" {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, part := range streamParts {
			_, _ = w.Write(part)
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(streamGap)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(responseBody)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(responseBody)
}
func main() {
	if len(os.Args) != 2 {
		panic("expected port")
	}
	addr := ":" + os.Args[1]
	server := &http.Server{Addr: addr, Handler: handler{}, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second}
	server.ConnState = func(conn net.Conn, state http.ConnState) {
		if tcp, ok := conn.(*net.TCPConn); ok && state == http.StateNew {
			_ = tcp.SetNoDelay(true)
		}
	}
	fmt.Printf("mock listening on %s\n", addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		panic(err)
	}
}
