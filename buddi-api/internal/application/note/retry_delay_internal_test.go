package note

import (
	"testing"
	"time"
)

// The first retry has to be quick because the usual cause is an embedding model
// still loading, and a minute of unsearchable text for a five-second warm-up is a
// poor trade. Later attempts back off because the cause there is a runtime that is
// not coming back on its own.
func TestIndexRetryDelayGrowsAndIsCapped(t *testing.T) {
	if got := indexRetryDelay(1); got != time.Minute {
		t.Errorf("indexRetryDelay(1) = %v, want %v", got, time.Minute)
	}

	if got := indexRetryDelay(2); got != 2*time.Minute {
		t.Errorf("indexRetryDelay(2) = %v, want %v", got, 2*time.Minute)
	}

	// Capped so a long-lived deployment does not schedule a retry a year out.
	for _, attempt := range []int{5, 6, 9, 40} {
		if got := indexRetryDelay(attempt); got > 30*time.Minute {
			t.Errorf("indexRetryDelay(%d) = %v, want it capped at %v", attempt, got, 30*time.Minute)
		}
	}
}
