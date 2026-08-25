package smb

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/handlers"
	"github.com/marmos91/dittofs/internal/adapter/smb/session"
)

// newBenchConnInfo mirrors newTestConnInfo without a *testing.T.
func newBenchConnInfo(conn net.Conn) *ConnInfo {
	mgr := session.NewDefaultManager()
	return &ConnInfo{
		Conn:           conn,
		Handler:        handlers.NewHandlerWithSessionManager(mgr),
		SessionManager: mgr,
		WriteMu:        &LockedWriter{},
		WriteTimeout:   2 * time.Second,
		SequenceWindow: NewSequenceWindowForConnection(mgr),
	}
}

// benchConn is a server-side conn whose peer is drained, so a response write
// costs what it costs on a real socket and nothing more.
func benchConn(b *testing.B) (net.Conn, func()) {
	b.Helper()
	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(io.Discard, clientConn)
	}()
	return serverConn, func() {
		_ = serverConn.Close()
		_ = clientConn.Close()
		<-done
	}
}

// What ordering costs a pipelining client on the dispatch path, measured on
// ECHO so the number is the ordering overhead rather than a handler's work.
// Each iteration dispatches `depth` requests concurrently, as a client with
// that many credits outstanding would.
func BenchmarkPipelinedDispatch(b *testing.B) {
	for _, depth := range []int{1, 8, 32} {
		for _, ordered := range []bool{false, true} {
			name := "unordered"
			if ordered {
				name = "ordered"
			}
			b.Run(fmt.Sprintf("%s/depth=%d", name, depth), func(b *testing.B) {
				conn, cleanup := benchConn(b)
				defer cleanup()
				ci := newBenchConnInfo(conn)
				if ordered {
					ci.RequestOrder = NewRequestOrder()
				}
				ctx := context.Background()

				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					var wg sync.WaitGroup
					for j := range depth {
						tok := ci.RequestOrder.Begin()
						wg.Add(1)
						go func(msgID uint64) {
							defer wg.Done()
							defer tok.Release()
							_ = ProcessSingleRequest(WithOrderToken(ctx, tok),
								echoHeader(msgID), echoBody(), nil, ci, false, nil)
						}(uint64(i*depth + j + 1))
					}
					wg.Wait()
				}
				b.ReportMetric(float64(depth), "requests/iter")
			})
		}
	}
}

// Handlers must keep running concurrently: only the moment a response is
// written is ordered. A slow request at the head of the pipeline must not add
// its latency to each of the requests behind it.
func TestRequestOrder_KeepsHandlersConcurrent(t *testing.T) {
	const depth = 32
	const handlerLatency = 100 * time.Millisecond

	o := NewRequestOrder()
	tokens := make([]*OrderToken, depth)
	for i := range tokens {
		tokens[i] = o.Begin()
	}

	start := time.Now()
	var wg sync.WaitGroup
	for _, tok := range tokens {
		wg.Add(1)
		go func(tok *OrderToken) {
			defer wg.Done()
			defer tok.Release()
			time.Sleep(handlerLatency) // the handler's own work
			tok.WaitTurn(context.Background())
		}(tok)
	}
	wg.Wait()

	// Serialized execution would cost depth*handlerLatency; concurrent
	// execution with ordered emission costs one handlerLatency plus wakeups.
	if elapsed := time.Since(start); elapsed > 4*handlerLatency {
		t.Fatalf("%d concurrent handlers took %v: ordering is serializing execution, not just emission", depth, elapsed)
	}
}
