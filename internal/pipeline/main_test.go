package pipeline

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// TestMain gives the tests a throwaway home directory. Applying a config
// writes MakeMKV's settings.conf under $HOME/.MakeMKV, and a test config
// with a dummy key once replaced the real key of whoever ran the tests.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "media-ripper-test-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Setenv("HOME", home)
	// The fakes move files at once; the real wait covers NFS caching.
	heldWait, heldPoll = 2*time.Second, 20*time.Millisecond
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}
