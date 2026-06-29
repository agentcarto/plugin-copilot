package copilot

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentcarto/core/common"
	"github.com/agentcarto/core/domain"
	"github.com/agentcarto/core/plugin"
	"github.com/agentcarto/core/scan"
	"gopkg.in/yaml.v3"
	_ "modernc.org/sqlite"
)

// VS Code GitHub Copilot Chat. Reports agent_type "copilot-vc". Sessions are
// stored as <User>/workspaceStorage/<hash>/chatSessions/<uuid>.{json,jsonl}
// alongside a state.vscdb index, plus standalone "empty window" sessions under
// globalStorage/emptyWindowChatSessions.

type VSCodeOptions struct {
	UserDirs []string `yaml:"user_dirs"`
}

type VSCodeFactory struct{}

func (VSCodeFactory) Descriptor() plugin.Descriptor {
	return plugin.Descriptor{Type: "copilot-vc", DisplayName: "GitHub Copilot Chat (VS Code)", ParserVersion: "4", Capabilities: domain.Capabilities{Scan: true, Conversation: true}}
}

func (VSCodeFactory) New(id string, n *yaml.Node) (any, error) {
	o := VSCodeOptions{}
	if e := common.DecodeOptions(n, &o); e != nil {
		return nil, e
	}
	return newVSCodePlugin(id, o), nil
}

func newVSCodePlugin(id string, o VSCodeOptions) *vscodePlugin {
	if len(o.UserDirs) == 0 {
		o.UserDirs = defaultVSCodeUserDirs()
	}
	for i := range o.UserDirs {
		o.UserDirs[i] = common.ExpandHome(o.UserDirs[i])
		if filepath.Base(o.UserDirs[i]) != "User" {
			o.UserDirs[i] = filepath.Join(o.UserDirs[i], "User")
		}
	}
	return &vscodePlugin{id, o}
}

type vscodePlugin struct {
	id string
	o  VSCodeOptions
}

// defaultVSCodeUserDirs returns the candidate VS Code "User" directories across
// operating systems. Candidates that do not exist are silently ignored by the
// later ReadDir calls, so listing all of them is harmless.
func defaultVSCodeUserDirs() []string {
	h, _ := os.UserHomeDir()
	out := []string{
		filepath.Join(h, ".config", "Code", "User"),
		filepath.Join(h, ".config", "Code - Insiders", "User"),
		filepath.Join(h, "Library", "Application Support", "Code", "User"),
		filepath.Join(h, "Library", "Application Support", "Code - Insiders", "User"),
		filepath.Join(h, ".vscode-server", "data", "User"),
		filepath.Join(h, ".vscode-server-insiders", "data", "User"),
	}
	if ad := os.Getenv("AppData"); ad != "" {
		out = append(out, filepath.Join(ad, "Code", "User"), filepath.Join(ad, "Code - Insiders", "User"))
	}
	// Reach the Windows-side VS Code from inside WSL (glob only expands to
	// directories that actually exist).
	for _, pat := range []string{"/mnt/*/Users/*/AppData/Roaming/Code/User", "/mnt/*/Users/*/AppData/Roaming/Code - Insiders/User"} {
		xs, _ := filepath.Glob(pat)
		out = append(out, xs...)
	}
	return out
}

// stripIDE removes <ide_*>…</ide_*> wrapper blocks that VS Code injects into the
// user message text, returning the trimmed remainder.
func stripIDE(s string) string {
	for {
		a := strings.Index(s, "<ide_")
		if a < 0 {
			break
		}
		b := strings.Index(s[a:], ">")
		if b < 0 {
			break
		}
		tag := s[a+1 : a+b]
		name := strings.Fields(tag)[0]
		end := strings.Index(s[a+b+1:], "</"+name+">")
		if end < 0 {
			break
		}
		s = s[:a] + s[a+b+1+end+len(name)+3:]
	}
	return strings.TrimSpace(s)
}

func requestUserText(req map[string]any) string {
	msg := common.Map(req["message"])
	text := common.String(msg["text"])
	if text == "" {
		text = common.Text(msg["parts"])
	}
	return stripIDE(text)
}

func responseText(req map[string]any) string {
	switch x := req["response"].(type) {
	case []any:
		var parts []string
		for _, p := range x {
			if v := common.String(common.Map(p)["value"]); strings.TrimSpace(v) != "" {
				parts = append(parts, v)
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	case string:
		return strings.TrimSpace(x)
	}
	return ""
}

// toolCallEvents extracts the tool-call events of a single request. They come
// from two shapes: .json files carry result.metadata.toolCallRounds, while
// .jsonl files inline kind="tool" entries in response[].
func toolCallEvents(req map[string]any) []domain.Event {
	var out []domain.Event
	rounds := common.Slice(common.Map(common.Map(req["result"])["metadata"])["toolCallRounds"])
	for _, rnd := range rounds {
		for _, tc := range common.Slice(common.Map(rnd)["toolCalls"]) {
			m := common.Map(tc)
			name := common.String(m["name"])
			if name == "" {
				name = common.String(m["id"])
			}
			if name == "" {
				name = "tool"
			}
			out = append(out, domain.Event{Kind: domain.EventToolCall, Text: common.Text(m["arguments"]), ToolName: name, RawType: "tool_call"})
		}
	}
	for _, p := range common.Slice(req["response"]) {
		m := common.Map(p)
		if kind := common.String(m["kind"]); strings.Contains(strings.ToLower(kind), "tool") {
			out = append(out, domain.Event{Kind: domain.EventToolCall, Text: common.String(m["value"]), ToolName: common.String(m["name"]), RawType: "tool_call"})
		}
	}
	return out
}

// requestEvents flattens one request into a linear user -> tool calls ->
// assistant event sequence. It does not insert turn-complete markers. Every
// event is stamped with the request timestamp (a millisecond epoch carried by
// both the .json and reassembled .jsonl request objects); VS Code records one
// timestamp per request rather than per message.
func requestEvents(req map[string]any) []domain.Event {
	ts := msToTime(req["timestamp"])
	var out []domain.Event
	if ut := requestUserText(req); ut != "" {
		out = append(out, domain.Event{Kind: domain.EventUser, Text: ut, Timestamp: ts, RawType: "message"})
	}
	for _, e := range toolCallEvents(req) {
		e.Timestamp = ts
		out = append(out, e)
	}
	if rt := responseText(req); rt != "" {
		out = append(out, domain.Event{Kind: domain.EventAssistant, Text: rt, Timestamp: ts, RawType: "message"})
	}
	return out
}

// copilotDefaultTitles holds the placeholder titles VS Code assigns to a chat
// before the user names it; these are treated as "no title". The Japanese
// entries are VS Code's localized "New Chat" placeholder and must match its
// output verbatim, so they are kept as-is rather than translated.
var copilotDefaultTitles = map[string]bool{
	"新しいチャット":   true,
	"新しい チャット":  true,
	"new chat":  true,
	"unknown":   true,
	"(unknown)": true,
}

func copilotDefaultTitle(s string) bool {
	s = strings.TrimSpace(s)
	return copilotDefaultTitles[s] || copilotDefaultTitles[strings.ToLower(s)]
}

// copilotModel returns the model name of the last request, preferring
// result.details and falling back to modelId.
func copilotModel(root map[string]any) string {
	reqs := common.Slice(root["requests"])
	for i := len(reqs) - 1; i >= 0; i-- {
		r := common.Map(reqs[i])
		if d := strings.TrimSpace(common.String(common.Map(r["result"])["details"])); d != "" {
			return d
		}
		if m := strings.TrimSpace(common.String(r["modelId"])); m != "" {
			return m
		}
	}
	return ""
}

// parseVSCodeSession reads a session file and returns its flattened events along
// with the decoded root object. .json files are plain JSON; .jsonl files are
// reassembled by rebuildJSONL.
func parseVSCodeSession(path string) ([]domain.Event, map[string]any) {
	b, e := os.ReadFile(path)
	if e != nil {
		return nil, nil
	}
	var root map[string]any
	if filepath.Ext(path) == ".json" {
		_ = json.Unmarshal(b, &root)
	} else {
		root = rebuildJSONL(b)
	}
	var ev []domain.Event
	for _, r := range common.Slice(root["requests"]) {
		ev = append(ev, requestEvents(common.Map(r))...)
	}
	return ev, root
}

// walkJSON invokes fn for every map node in a decoded JSON value (depth-first).
func walkJSON(v any, fn func(map[string]any)) {
	switch x := v.(type) {
	case map[string]any:
		fn(x)
		for _, v := range x {
			walkJSON(v, fn)
		}
	case []any:
		for _, v := range x {
			walkJSON(v, fn)
		}
	}
}

// jsonlBuilder reassembles the streaming .jsonl session format into a single
// root object equivalent to the flat .json layout. Records are grouped by
// requestId; duplicate response values are de-duplicated per request.
type jsonlBuilder struct {
	root  map[string]any
	reqs  []any
	byID  map[string]map[string]any
	seen  map[string]map[string]bool
	cur   map[string]any
	curID string
}

// consume folds a single decoded map node into the builder state.
func (jb *jsonlBuilder) consume(o map[string]any) {
	rid := common.String(o["requestId"])
	if rid != "" && len(common.Map(o["message"])) > 0 {
		if jb.byID[rid] == nil {
			jb.byID[rid] = map[string]any{"message": o["message"], "response": []any{}, "result": o["result"], "timestamp": o["timestamp"]}
			jb.reqs = append(jb.reqs, jb.byID[rid])
			jb.seen[rid] = map[string]bool{}
		}
		jb.cur = jb.byID[rid]
		jb.curID = rid
		return
	}
	if jb.cur == nil {
		return
	}
	if val := common.String(o["value"]); val != "" && (o["supportHtml"] != nil || o["baseUri"] != nil) {
		if !jb.seen[jb.curID][val] {
			jb.seen[jb.curID][val] = true
			jb.cur["response"] = append(common.Slice(jb.cur["response"]), map[string]any{"value": val})
		}
	} else if name := common.String(o["toolId"]); name != "" {
		jb.cur["response"] = append(common.Slice(jb.cur["response"]), map[string]any{"kind": "tool", "name": name, "value": common.Text(o["invocationMessage"])})
	}
}

func rebuildJSONL(b []byte) map[string]any {
	jb := &jsonlBuilder{
		root: map[string]any{},
		byID: map[string]map[string]any{},
		seen: map[string]map[string]bool{},
	}
	for _, line := range strings.Split(string(b), "\n") {
		var ln map[string]any
		if json.Unmarshal([]byte(line), &ln) != nil {
			continue
		}
		// kind==0 carries the header object; merge its fields into root.
		if n, ok := ln["kind"].(float64); ok && n == 0 {
			if m := common.Map(ln["v"]); len(m) > 0 {
				for k, v := range m {
					jb.root[k] = v
				}
			}
		}
		// A single-element "k" path names a top-level root key.
		if k := jsonlRootKey(ln["k"]); k != "" {
			jb.root[k] = ln["v"]
		}
		walkJSON(ln["v"], jb.consume)
	}
	jb.root["requests"] = jb.reqs
	return jb.root
}

func jsonlRootKey(v any) string {
	ks := common.Slice(v)
	if len(ks) != 1 {
		return ""
	}
	return common.String(ks[0])
}

// workspaceCWD reads workspace.json in a workspaceStorage directory and returns
// the normalized folder (or workspace) path it records.
func workspaceCWD(ws string) string {
	b, e := os.ReadFile(filepath.Join(ws, "workspace.json"))
	if e != nil {
		return ""
	}
	var v map[string]any
	_ = json.Unmarshal(b, &v)
	s := common.String(v["folder"])
	if s == "" {
		s = common.String(v["workspace"])
	}
	return NormalizeCWD(s)
}

// resolveSessionCWD determines a session's working directory, preferring the
// workspace.json folder and falling back to inference from path candidates
// found in the session content.
func resolveSessionCWD(ws string, root map[string]any) string {
	if ws != "" {
		if cwd := workspaceCWD(ws); cwd != "" && cwd != "(unknown)" {
			return cwd
		}
	}
	if cwd := inferCWDFromPathCandidates(copilotPathCandidates(root)); cwd != "" {
		return cwd
	}
	return "(unknown)"
}

// copilotPathCandidates gathers path-like strings from a decoded session: both
// path/uri-named fields and paths embedded in message/tool text bodies.
func copilotPathCandidates(v any) []string {
	var out []string
	collectPathFields(v, &out)
	walkJSON(v, func(o map[string]any) {
		for _, k := range []string{"content", "invocationMessage", "renderedMessage", "text", "value"} {
			out = append(out, pathsFromText(common.String(o[k]))...)
		}
	})
	return out
}

// isSessionFile reports whether a directory entry is a Copilot session file
// (a .json or .jsonl regular file).
func isSessionFile(f os.DirEntry) bool {
	if f.IsDir() {
		return false
	}
	ext := filepath.Ext(f.Name())
	return ext == ".json" || ext == ".jsonl"
}

func (p *vscodePlugin) Scan(ctx context.Context, in plugin.ScanInput) (plugin.ScanOutput, error) {
	cache := scan.New(in.Warm, in.Dead, VSCodeFactory{}.Descriptor().ParserVersion)
	var out []domain.Session
	for _, userDir := range p.o.UserDirs {
		ws, err := p.scanWorkspaceSessions(ctx, cache, userDir)
		out = append(out, ws...)
		if err != nil {
			return plugin.ScanOutput{Sessions: out, Dead: cache.DeadOut()}, err
		}
		ew, err := p.scanEmptyWindowSessions(ctx, cache, userDir)
		out = append(out, ew...)
		if err != nil {
			return plugin.ScanOutput{Sessions: out, Dead: cache.DeadOut()}, err
		}
	}
	return plugin.ScanOutput{Sessions: out, Dead: cache.DeadOut()}, nil
}

// scanWorkspaceSessions scans the per-workspace chat sessions under
// <userDir>/workspaceStorage/<hash>/chatSessions, using each workspace's
// state.vscdb index for titles and timestamps.
func (p *vscodePlugin) scanWorkspaceSessions(ctx context.Context, cache *scan.Cache, userDir string) ([]domain.Session, error) {
	wsroot := filepath.Join(userDir, "workspaceStorage")
	workspaces, _ := os.ReadDir(wsroot)
	var out []domain.Session
	for _, w := range workspaces {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if !w.IsDir() {
			continue
		}
		wsDir := filepath.Join(wsroot, w.Name())
		sessionsDir := filepath.Join(wsDir, "chatSessions")
		index := readIndex(wsDir)
		files, _ := os.ReadDir(sessionsDir)
		for _, f := range files {
			if !isSessionFile(f) {
				continue
			}
			path := filepath.Join(sessionsDir, f.Name())
			name := strings.TrimSuffix(f.Name(), filepath.Ext(f.Name()))
			entry := common.Map(index[name])
			if s, ok := scanEntry(cache, path, func() (domain.Session, bool) {
				return p.scanSessionFile(path, wsDir, entry)
			}); ok {
				out = append(out, s)
			}
		}
	}
	return out, nil
}

// scanEmptyWindowSessions scans the standalone sessions stored under
// <userDir>/globalStorage/emptyWindowChatSessions, which have no workspace and
// thus no index.
func (p *vscodePlugin) scanEmptyWindowSessions(ctx context.Context, cache *scan.Cache, userDir string) ([]domain.Session, error) {
	dir := filepath.Join(userDir, "globalStorage", "emptyWindowChatSessions")
	files, _ := os.ReadDir(dir)
	var out []domain.Session
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if !isSessionFile(f) {
			continue
		}
		path := filepath.Join(dir, f.Name())
		if s, ok := scanEntry(cache, path, func() (domain.Session, bool) {
			return p.scanSessionFile(path, "", nil)
		}); ok {
			out = append(out, s)
		}
	}
	return out, nil
}

func (p *vscodePlugin) scanSessionFile(path, ws string, entry map[string]any) (domain.Session, bool) {
	ev, root := parseVSCodeSession(path)
	if len(common.Slice(root["requests"])) == 0 {
		return domain.Session{}, false
	}
	title := sessionTitle(root, entry, ev)
	updated := sessionUpdatedAt(path, root, entry)
	started := sessionStartedAt(root, entry, updated)
	return domain.Session{
		PluginID:  p.id,
		AgentType: "copilot-vc",
		SessionID: strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)),
		CWD:       resolveSessionCWD(ws, root),
		StartedAt: started,
		UpdatedAt: updated,
		Title:     title,
		Model:     copilotModel(root),
		SourceRef: domain.SessionRef{Source: path},
	}, true
}

// sessionTitle resolves a session title: the file's customTitle, then the
// index title, then the first real user message; placeholder titles are
// rejected at each step.
func sessionTitle(root, entry map[string]any, ev []domain.Event) string {
	title := strings.TrimSpace(common.String(root["customTitle"]))
	if copilotDefaultTitle(title) {
		title = ""
	}
	if title == "" {
		if it := strings.TrimSpace(common.String(entry["title"])); it != "" && !copilotDefaultTitle(it) {
			title = it
		}
	}
	if title == "" {
		title = common.Title(ev, "")
	}
	title = common.CleanTitle(title)
	if title == "" {
		title = "(no title)"
	}
	return title
}

// sessionUpdatedAt resolves the last-update time: index lastMessageDate, then
// file lastMessageDate, then the file mtime.
func sessionUpdatedAt(path string, root, entry map[string]any) time.Time {
	updated := msToTime(entry["lastMessageDate"])
	if updated.IsZero() {
		updated = msToTime(root["lastMessageDate"])
	}
	if updated.IsZero() {
		updated = common.FileTime(path)
	}
	return updated
}

// sessionStartedAt resolves the start time: index timing.created, then file
// creationDate, then the resolved update time.
func sessionStartedAt(root, entry map[string]any, updated time.Time) time.Time {
	started := msToTime(common.Map(entry["timing"])["created"])
	if started.IsZero() {
		started = msToTime(root["creationDate"])
	}
	if started.IsZero() {
		started = updated
	}
	return started
}

// readIndex loads the chat session index (titles, timestamps) from a
// workspace's state.vscdb, returning an empty map when it is absent or
// unreadable.
func readIndex(ws string) map[string]any {
	out := map[string]any{}
	path := filepath.Join(ws, "state.vscdb")
	if _, e := os.Stat(path); e != nil {
		return out
	}
	db, e := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	if e != nil {
		return out
	}
	defer db.Close()
	var raw string
	if db.QueryRow("SELECT value FROM ItemTable WHERE key='chat.ChatSessionStore.index'").Scan(&raw) != nil {
		return out
	}
	var root map[string]any
	if json.Unmarshal([]byte(raw), &root) == nil {
		if x := common.Map(root["entries"]); x != nil {
			return x
		}
	}
	return out
}

func (p *vscodePlugin) LoadConversation(_ context.Context, r domain.SessionRef) (*domain.Conversation, error) {
	ev, _ := parseVSCodeSession(r.Source)
	c := common.Linear(ev)
	return &c, nil
}
