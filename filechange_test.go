package copilot

import (
	"testing"
	"time"

	"github.com/agentcarto/core/domain"
)

func TestTextEditEventCarriesChanges(t *testing.T) {
	m := map[string]any{
		"uri":   map[string]any{"fsPath": "/repo/a.go"},
		"edits": []any{[]any{map[string]any{"text": "one\ntwo\n"}}},
	}
	e := textEditEvent(m, time.Time{})
	if e == nil || e.Kind != domain.EventFileChange {
		t.Fatalf("event=%+v", e)
	}
	if len(e.Changes) != 1 {
		t.Fatalf("changes=%+v", e.Changes)
	}
	fc := e.Changes[0]
	if fc.Path != "/repo/a.go" || fc.Op != "update" || fc.Added != 2 || fc.Removed != 0 {
		t.Fatalf("fc=%+v", fc)
	}
	if fc.Diff != "+one\n+two" {
		t.Fatalf("diff=%q", fc.Diff)
	}
}
