package tui

import (
	"fmt"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/event"
)

func TestBridgePushNeverBlocksAndDrainKeepsOrder(t *testing.T) {
	b := newBridge()
	const writers, each = 4, 100
	var wg sync.WaitGroup
	for w := range writers {
		wg.Go(func() {
			for i := range each {
				b.push(event.Notice{Text: strconv.Itoa(w) + ":" + strconv.Itoa(i)})
			}
		})
	}
	wg.Wait()

	msg := b.wait()()

	evs, ok := msg.(eventsMsg)
	require.True(t, ok)
	require.Len(t, evs, writers*each)
	next := make([]int, writers)
	for _, ev := range evs {
		var w, i int
		_, err := fmt.Sscanf(ev.(event.Notice).Text, "%d:%d", &w, &i)
		require.NoError(t, err)
		assert.Equal(t, next[w], i)
		next[w]++
	}
}

func TestBridgeWaitEndsOnClose(t *testing.T) {
	b := newBridge()
	b.close()
	assert.Nil(t, b.wait()())
}
