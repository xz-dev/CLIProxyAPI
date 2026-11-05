package usage

import (
	"context"
	"runtime"
	"testing"
	"time"
	"weak"
)

type retentionCtxKey struct{}

type signalPlugin struct{ got chan struct{} }

func (p signalPlugin) HandleUsage(context.Context, Record) { p.got <- struct{}{} }

// A delivered record's context (in production it carries the request's *gin.Context and the
// executor values, with copies of the request body) must not stay reachable from the queue.
func TestManagerDropsDeliveredRecordContext(t *testing.T) {
	m := NewManager(0)
	defer m.Stop()
	p := signalPlugin{got: make(chan struct{}, 1)}
	m.Register(p)

	big := make([]byte, 8<<20)
	wp := weak.Make(&big[0])
	m.Publish(context.WithValue(context.Background(), retentionCtxKey{}, big), Record{Model: "m"})
	big = nil
	<-p.got

	for i := 0; i < 200; i++ {
		runtime.GC()
		if wp.Value() == nil {
			return
		}
		time.Sleep(5 * time.Millisecond) // let the worker return from dispatch
	}
	t.Fatal("delivered record's context is still reachable from the usage queue")
}
