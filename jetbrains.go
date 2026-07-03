package copilot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/agentcarto/core/common"
	"github.com/agentcarto/core/domain"
	"github.com/agentcarto/core/plugin"
	"github.com/agentcarto/core/scan"
	"gopkg.in/yaml.v3"
)

// JetBrains GitHub Copilot Chat. Reports agent_type "copilot-jb". Sessions are
// stored as ~/.copilot/jb/<id>/partition-*.jsonl.

type JetBrainsOptions struct {
	Dirs []string `yaml:"dirs"`
}

type JetBrainsFactory struct{}

func (JetBrainsFactory) Descriptor() plugin.Descriptor {
	// ParserVersion=3: user events now carry the normalized Prompt field
	// (agent-specific pseudo-prompt vocabulary moved out of core).
	// ParserVersion=4: tool calls carry ToolArg and unknown-CWD sessions are
	// flagged InferCWD for the host's cross-plugin backfill.
	return plugin.Descriptor{Type: "copilot-jb", DisplayName: "GitHub Copilot Chat (JetBrains)", ParserVersion: "4", Capabilities: domain.Capabilities{Scan: true, Conversation: true}}
}

func (JetBrainsFactory) New(id string, n *yaml.Node) (any, error) {
	o := JetBrainsOptions{}
	if e := common.DecodeOptions(n, &o); e != nil {
		return nil, e
	}
	return newJetBrainsPlugin(id, o), nil
}

func newJetBrainsPlugin(id string, o JetBrainsOptions) *jetbrainsPlugin {
	if len(o.Dirs) == 0 {
		o.Dirs = []string{"~/.copilot/jb"}
	}
	for i := range o.Dirs {
		o.Dirs[i] = common.ExpandHome(o.Dirs[i])
	}
	return &jetbrainsPlugin{id, o}
}

type jetbrainsPlugin struct {
	id string
	o  JetBrainsOptions
}

func (p *jetbrainsPlugin) Scan(ctx context.Context, in plugin.ScanInput) (plugin.ScanOutput, error) {
	cache := scan.New(in.Warm, in.Dead, JetBrainsFactory{}.Descriptor().ParserVersion)
	var out []domain.Session
	for _, root := range p.o.Dirs {
		ss, err := p.scanDir(ctx, root, cache)
		if err != nil {
			return plugin.ScanOutput{}, err
		}
		out = append(out, ss...)
	}
	return plugin.ScanOutput{Sessions: out, Dead: cache.DeadOut()}, nil
}

func (p *jetbrainsPlugin) scanDir(ctx context.Context, root string, cache *scan.Cache) ([]domain.Session, error) {
	ds, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []domain.Session
	for _, d := range ds {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if !d.IsDir() {
			continue
		}
		dir := filepath.Join(root, d.Name())
		// A session directory must contain at least partition-1.jsonl.
		if _, err := os.Stat(filepath.Join(dir, "partition-1.jsonl")); err != nil {
			continue
		}
		if s, ok := scanEntry(cache, dir, func() (domain.Session, bool) {
			return p.buildSession(ctx, dir, d.Name())
		}); ok {
			out = append(out, s)
		}
	}
	// Sessions that could not resolve their own CWD borrow it from a session
	// that ran around the same time.
	backfillUnknownCWD(out, 6*time.Hour)
	return out, nil
}

// buildSession parses a single JetBrains session directory into a Session,
// reporting false when it contains no usable events.
func (p *jetbrainsPlugin) buildSession(ctx context.Context, dir, id string) (domain.Session, bool) {
	ev, started, cwd := parseJetBrainsCopilotWithMeta(ctx, dir)
	if len(ev) == 0 {
		return domain.Session{}, false
	}
	if cwd == "" {
		cwd = "(unknown)"
	}
	updated := common.MaxMTime(dir)
	if started.IsZero() {
		started = updated
	}
	return domain.Session{
		PluginID:  p.id,
		AgentType: "copilot-jb",
		SessionID: id,
		CWD:       cwd,
		// Copilot logs often carry no workspace path; let the host infer one
		// from a temporally-near session (a cross-plugin heuristic) unless the
		// intra-plugin backfill below resolves it first.
		InferCWD:  cwd == "(unknown)",
		StartedAt: started,
		UpdatedAt: updated,
		Title:     common.Title(ev, "(no title)"),
		SourceRef: domain.SessionRef{Source: dir},
		LastKind:  common.LastMeaningful(ev),
	}, true
}

func (p *jetbrainsPlugin) LoadConversation(ctx context.Context, r domain.SessionRef) (*domain.Conversation, error) {
	ev := parseJetBrainsCopilot(ctx, r.Source)
	c := common.Linear(ev)
	return &c, nil
}

// backfillUnknownCWD fills in the working directory of sessions that could not
// resolve their own by copying it from the temporally closest session that did,
// provided the time gap is within maxGap.
func backfillUnknownCWD(sessions []domain.Session, maxGap time.Duration) {
	for i := range sessions {
		if sessions[i].CWD != "" && sessions[i].CWD != "(unknown)" {
			continue
		}
		t := sessionTime(sessions[i])
		if t.IsZero() {
			continue
		}
		bestCWD := ""
		bestGap := time.Duration(0)
		for _, other := range sessions {
			if other.CWD == "" || other.CWD == "(unknown)" {
				continue
			}
			ot := sessionTime(other)
			if ot.IsZero() {
				continue
			}
			gap := absDuration(t.Sub(ot))
			if gap > maxGap {
				continue
			}
			if bestCWD == "" || gap < bestGap {
				bestCWD = other.CWD
				bestGap = gap
			}
		}
		if bestCWD != "" {
			sessions[i].CWD = bestCWD
			sessions[i].InferCWD = false
		}
	}
}

func sessionTime(s domain.Session) time.Time {
	if !s.StartedAt.IsZero() {
		return s.StartedAt
	}
	return s.UpdatedAt
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// parseJetBrainsCopilot returns just the events of a session directory.
func parseJetBrainsCopilot(ctx context.Context, dir string) []domain.Event {
	ev, _ := parseJetBrainsCopilotWithStart(ctx, dir)
	return ev
}

// parseJetBrainsCopilotWithStart returns the events and start time of a session
// directory.
func parseJetBrainsCopilotWithStart(ctx context.Context, dir string) ([]domain.Event, time.Time) {
	ev, start, _ := parseJetBrainsCopilotWithMeta(ctx, dir)
	return ev, start
}

// parseJetBrainsCopilotWithMeta parses every partition-*.jsonl file in a session
// directory (in sorted order) into a flat event list, and additionally reports
// the session start time and the inferred working directory.
func parseJetBrainsCopilotWithMeta(ctx context.Context, dir string) ([]domain.Event, time.Time, string) {
	files, _ := filepath.Glob(filepath.Join(dir, "partition-*.jsonl"))
	sort.Strings(files)
	var events []domain.Event
	var start time.Time
	var paths []string
	for _, f := range files {
		_ = common.JSONLines(ctx, f, func(_ int, o map[string]any) error {
			ts := common.Time(common.String(o["timestamp"]))
			if start.IsZero() && !ts.IsZero() {
				start = ts
			}
			typ := common.String(o["type"])
			data := common.Map(o["data"])
			paths = append(paths, jetBrainsPathCandidates(typ, data)...)
			if typ == "partition.created" && start.IsZero() {
				start = msToTime(data["createdAt"])
			}
			events = append(events, jetBrainsRecordEvents(typ, data, ts)...)
			return nil
		})
	}
	return events, start, inferCWDFromPathCandidates(paths)
}

// jetBrainsRecordEvents converts a single decoded JSONL record into the events
// it represents (zero or more). Record types it does not produce events for
// return nil.
func jetBrainsRecordEvents(typ string, data map[string]any, ts time.Time) []domain.Event {
	switch typ {
	case "user.message":
		if text := strings.TrimSpace(common.String(data["content"])); text != "" {
			return []domain.Event{userEvent(text, ts, "user.message")}
		}
	case "assistant.message":
		var out []domain.Event
		if thinking := strings.TrimSpace(common.String(common.Map(data["thinking"])["text"])); thinking != "" {
			out = append(out, domain.Event{Kind: domain.EventReasoning, Text: thinking, Timestamp: ts, RawType: "assistant.thinking"})
		}
		text := strings.TrimSpace(common.String(data["content"]))
		if text == "" {
			text = strings.TrimSpace(common.String(data["text"]))
		}
		if text != "" {
			out = append(out, domain.Event{Kind: domain.EventAssistant, Text: text, Timestamp: ts, RawType: "assistant.message"})
		}
		return out
	case "tool.execution_start":
		args := common.Text(data["arguments"])
		return []domain.Event{{Kind: domain.EventToolCall, Text: args, Timestamp: ts, ToolName: common.String(data["toolName"]), RawType: "tool.execution_start", ToolArg: common.ToolArgFromJSON(args)}}
	case "tool.execution_complete":
		text := common.Text(data["result"])
		if text == "{}" || strings.TrimSpace(text) == "" {
			text = fmt.Sprint(data["status"])
		}
		return []domain.Event{{Kind: domain.EventToolResult, Text: text, Timestamp: ts, RawType: "tool.execution_complete"}}
	case "assistant.turn_end":
		return []domain.Event{{Kind: domain.EventTurnComplete, Timestamp: ts, RawType: "assistant.turn_end"}}
	}
	return nil
}

// jetBrainsPathCandidates extracts path-like strings from a record, used to
// infer the session's working directory.
func jetBrainsPathCandidates(typ string, data map[string]any) []string {
	var out []string
	switch typ {
	case "user.message_rendered":
		out = append(out, pathsFromText(common.String(data["renderedMessage"]))...)
	case "tool.execution_start":
		collectPathFields(data["arguments"], &out)
	case "tool.execution_complete":
		result := common.Map(data["result"])
		out = append(out, pathsFromText(common.String(result["progressMessage"]))...)
		for _, item := range common.Slice(result["result"]) {
			out = append(out, pathsFromText(common.String(common.Map(item)["value"]))...)
		}
	}
	return out
}
