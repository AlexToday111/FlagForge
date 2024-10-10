package streaming

import (
	"github.com/flagforge/flagforge/pkg/types"
	"sync"
	"testing"
)

func TestHubIsolationCoalescingAndClose(t *testing.T) {
	h := New()
	ch, closeA := h.Subscribe("a")
	other, closeB := h.Subscribe("b")
	defer closeB()
	for i := int64(1); i <= 100; i++ {
		h.Publish(types.Event{EnvironmentID: "a", Version: i})
	}
	h.Publish(types.Event{EnvironmentID: "a", Version: 50})
	if e := <-ch; e.Version != 100 {
		t.Fatal(e)
	}
	select {
	case <-other:
		t.Fatal("cross-environment leak")
	default:
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				h.Publish(types.Event{EnvironmentID: "a"})
				closeA()
			}
		}()
	}
	wg.Wait()
	if len(h.subscribers["a"]) != 0 {
		t.Fatal("subscription leak")
	}
}
