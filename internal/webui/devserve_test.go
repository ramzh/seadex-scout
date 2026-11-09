//go:build devserve

package webui

import (
	"log/slog"
	"os"
	"testing"
)

// TestDevServe serves the dashboard over a copy of a saved state, with no
// sign-in, for looking at the page: DEVSERVE_STATE=<webui-state.json>
// go test -tags devserve -run TestDevServe ./internal/webui/
func TestDevServe(t *testing.T) {
	st := NewStore(os.Getenv("DEVSERVE_STATE"))
	if err := st.Load(); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(Options{Addr: "127.0.0.1:18080", Store: st, Log: slog.Default()})
	t.Fatal(srv.ListenAndServe())
}
