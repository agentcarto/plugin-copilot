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
	// ParserVersion=7: user events now carry the normalized Prompt field
	// (agent-specific pseudo-prompt vocabulary moved out of core).
	// ParserVersion=8: tool calls carry ToolArg and unknown-CWD sessions are
	// flagged InferCWD for the host's cross-plugin backfill.
	return plugin.Descriptor{Type: "copilot-vc", DisplayName: "GitHub Copilot Chat (VS Code)", ParserVersion: "8", Capabilities: domain.Capabilities{Scan: true, Conversation: true}}
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

// toolCallInfo is one entry of result.metadata.toolCallRounds: the function
// name and its JSON arguments.
type toolCallInfo struct{ name, args string }

// roundsByID indexes result.metadata.toolCallRounds by tool-call id, keeping
// the call order for the calls that never surface as a response part.
func roundsByID(req map[string]any) (map[string]toolCallInfo, []string) {
	out := map[string]toolCallInfo{}
	var order []string
	md := common.Map(common.Map(req["result"])["metadata"])
	for _, rnd := range common.Slice(md["toolCallRounds"]) {
		for _, tc := range common.Slice(common.Map(rnd)["toolCalls"]) {
			m := common.Map(tc)
			id := common.String(m["id"])
			out[id] = toolCallInfo{name: common.String(m["name"]), args: common.Text(m["arguments"])}
			order = append(order, id)
		}
	}
	return out, order
}

// resultsByID extracts result.metadata.toolCallResults as plain text per call id.
func resultsByID(req map[string]any) map[string]string {
	out := map[string]string{}
	md := common.Map(common.Map(req["result"])["metadata"])
	for id, v := range common.Map(md["toolCallResults"]) {
		if s := strings.TrimSpace(renderTreeText(v)); s != "" {
			out[id] = s
		}
	}
	return out
}

// renderTreeText flattens VS Code's rendered-result tree (content[].value.node.
// children[].text, nested) into plain text. Only the ordered container fields
// are walked — a full map walk would visit keys in random order.
func renderTreeText(v any) string {
	var b strings.Builder
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if t, ok := x["text"].(string); ok {
				if lb, _ := x["lineBreakBefore"].(bool); lb && b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(t)
			}
			for _, k := range []string{"content", "value", "node", "children"} {
				walk(x[k])
			}
		case []any:
			for _, it := range x {
				walk(it)
			}
		}
	}
	walk(v)
	return b.String()
}

// requestEvents flattens one request into a user event followed by the response
// parts in their recorded order. Every event is stamped with the request
// timestamp (VS Code records one timestamp per request rather than per message).
func requestEvents(req map[string]any) []domain.Event {
	ts := msToTime(req["timestamp"])
	var out []domain.Event
	if ut := requestUserText(req); ut != "" {
		out = append(out, userEvent(ut, ts, "message"))
	}
	return append(out, responseEvents(req, ts)...)
}

// responseEvents walks the response parts in order, buffering markdown
// fragments (which carry their own whitespace, so they are concatenated as-is)
// and flushing the buffer as one assistant event whenever a non-markdown part
// interrupts the flow. Tool calls take their arguments from toolCallRounds and
// their result from toolCallResults. The serialized invocation parts use their
// own UUIDs, unrelated to the rounds/results ids, but the Nth invocation part
// is the Nth rounds call (verified 275/275 on real data), so the join is
// positional; rounds entries beyond the surfaced parts are appended at the end
// so no call is lost.
func responseEvents(req map[string]any, ts time.Time) []domain.Event {
	calls, order := roundsByID(req)
	results := resultsByID(req)
	next := 0
	var out []domain.Event
	var md strings.Builder
	flush := func() {
		s := strings.TrimSpace(md.String())
		md.Reset()
		// A lone code fence is the leftover of a block whose body was recorded
		// as a textEditGroup part rather than markdown; drop the empty shell.
		if s == "" || (strings.HasPrefix(s, "```") && !strings.Contains(s, "\n")) {
			return
		}
		out = append(out, domain.Event{Kind: domain.EventAssistant, Text: s, Timestamp: ts, RawType: "message"})
	}
	emitCall := func(id, name, text string) {
		if name == "" {
			name = "tool"
		}
		out = append(out, domain.Event{Kind: domain.EventToolCall, Text: text, Timestamp: ts, ToolName: name, RawType: "tool_call", ToolArg: toolArg(text)})
		if r := results[id]; r != "" {
			out = append(out, domain.Event{Kind: domain.EventToolResult, Text: r, Timestamp: ts, RawType: "tool_result"})
		}
	}
	parts, isList := req["response"].([]any)
	if !isList {
		// Old flat layout: the response is a single markdown string.
		md.WriteString(common.String(req["response"]))
	}
	for _, p := range parts {
		m := common.Map(p)
		if m == nil {
			continue
		}
		switch common.String(m["kind"]) {
		case "":
			md.WriteString(common.String(m["value"]))
		case "inlineReference":
			// A file reference splitting the markdown mid-sentence; put its
			// name back into the flow.
			md.WriteString(inlineRefName(m))
		case "thinking":
			flush()
			// Plaintext reasoning only; an empty value with an opaque id is the
			// encrypted form.
			if t := strings.TrimSpace(common.Text(m["value"])); t != "" {
				out = append(out, domain.Event{Kind: domain.EventReasoning, Text: t, Timestamp: ts, RawType: "thinking"})
			}
		case "toolInvocationSerialized":
			flush()
			id := ""
			if next < len(order) {
				id = order[next]
				next++
			}
			name := common.String(m["toolId"])
			if name == "" {
				name = calls[id].name
			}
			text := calls[id].args
			if text == "" {
				// Terminal runs keep their command in toolSpecificData rather
				// than the (empty) invocation message.
				text = common.String(common.Map(common.Map(m["toolSpecificData"])["commandLine"])["original"])
			}
			if text == "" {
				text = markdownString(m["invocationMessage"])
			}
			emitCall(id, name, text)
		case "textEditGroup":
			flush()
			if e := textEditEvent(m, ts); e != nil {
				out = append(out, *e)
			}
		case "questionCarousel":
			flush()
			if t := questionText(m); t != "" {
				out = append(out, domain.Event{Kind: domain.EventAssistant, Text: t, Timestamp: ts, RawType: "question"})
			}
		case "subagent":
			flush()
			text := strings.TrimSpace(strings.TrimSpace(common.String(m["description"])) + "\n\n" + common.String(m["prompt"]))
			out = append(out, domain.Event{Kind: domain.EventToolCall, Text: text, Timestamp: ts, ToolName: "subagent", RawType: "subagent"})
		case "confirmation", "warning":
			flush()
			body := markdownString(m["message"])
			if body == "" {
				body = markdownString(m["content"])
			}
			if t := strings.TrimSpace(strings.TrimSpace(common.String(m["title"])) + "\n" + body); t != "" {
				out = append(out, domain.Event{Kind: domain.EventSystem, Text: t, Timestamp: ts, RawType: common.String(m["kind"])})
			}
		}
	}
	flush()
	for _, id := range order[next:] {
		emitCall(id, calls[id].name, calls[id].args)
	}
	return out
}

// markdownString unwraps VS Code's markdown-string values, which appear both as
// plain strings and as {"value": ...} objects.
func markdownString(v any) string {
	if s := common.String(v); s != "" {
		return s
	}
	return common.String(common.Map(v)["value"])
}

// inlineRefName returns the display name of an inlineReference part, falling
// back to the referenced file's basename ("inlineReference" holds either the
// uri object directly or a wrapper with a "uri" field).
func inlineRefName(m map[string]any) string {
	if n := common.String(m["name"]); n != "" {
		return n
	}
	ref := common.Map(m["inlineReference"])
	uri := common.Map(ref["uri"])
	if uri == nil {
		uri = ref
	}
	p := common.String(uri["path"])
	if p == "" {
		p = common.String(uri["fsPath"])
	}
	return filepath.Base(p)
}

// textEditEvent renders a textEditGroup (an applied edit) as a file-change
// event in apply_patch form. The log only records the inserted text — the
// replaced content is gone — so the hunk carries "+" lines only.
func textEditEvent(m map[string]any, ts time.Time) *domain.Event {
	uri := common.Map(m["uri"])
	path := common.String(uri["fsPath"])
	if path == "" {
		path = common.String(uri["path"])
	}
	if path == "" {
		return nil
	}
	var b strings.Builder
	b.WriteString("*** Begin Patch\n*** Update File: " + path)
	for _, group := range common.Slice(m["edits"]) {
		for _, ed := range common.Slice(group) {
			text := common.String(common.Map(ed)["text"])
			if text == "" {
				continue
			}
			for _, ln := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
				b.WriteString("\n+" + ln)
			}
		}
	}
	b.WriteString("\n*** End Patch")
	return &domain.Event{Kind: domain.EventFileChange, Text: b.String(), Timestamp: ts, RawType: "textEditGroup"}
}

// questionText renders a questionCarousel (the agent asking the user to pick
// options) as markdown: each question followed by its option labels.
func questionText(m map[string]any) string {
	var lines []string
	for _, q := range common.Slice(m["questions"]) {
		qm := common.Map(q)
		t := markdownString(qm["message"])
		if t == "" {
			t = common.String(qm["title"])
		}
		if t != "" {
			lines = append(lines, t)
		}
		for _, o := range common.Slice(qm["options"]) {
			if l := common.String(common.Map(o)["label"]); l != "" {
				lines = append(lines, "- "+l)
			}
		}
	}
	return strings.Join(lines, "\n")
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

// rebuildJSONL replays the streaming .jsonl session format into the final root
// object. The file is an operation log: kind 0 carries the initial state
// snapshot, kind 1 sets the value at path "k", and kind 2 appends "v"'s items
// to the array at path "k". Replaying the ops yields the exact same layout the
// flat .json files use.
func rebuildJSONL(b []byte) map[string]any {
	root := map[string]any{}
	for _, line := range strings.Split(string(b), "\n") {
		var ln struct {
			Kind float64 `json:"kind"`
			K    []any   `json:"k"`
			V    any     `json:"v"`
		}
		if json.Unmarshal([]byte(line), &ln) != nil {
			continue
		}
		switch {
		case ln.Kind == 0:
			if m := common.Map(ln.V); len(m) > 0 {
				root = m
			}
		case len(ln.K) > 0:
			if m, ok := applyOp(root, ln.K, ln.V, ln.Kind == 2).(map[string]any); ok {
				root = m
			}
		}
	}
	return root
}

// applyOp applies one .jsonl operation to the container c at path: a set
// (replacing the value) or an append (extending the array with v's items).
// Missing intermediate containers are created; out-of-range indexes pad with
// nil. The (possibly re-allocated) container is returned.
func applyOp(c any, path []any, v any, appendOp bool) any {
	if len(path) == 0 {
		if appendOp {
			return append(common.Slice(c), common.Slice(v)...)
		}
		return v
	}
	switch key := path[0].(type) {
	case string:
		m, ok := c.(map[string]any)
		if !ok {
			m = map[string]any{}
		}
		m[key] = applyOp(m[key], path[1:], v, appendOp)
		return m
	case float64:
		s, _ := c.([]any)
		for len(s) <= int(key) {
			s = append(s, nil)
		}
		s[int(key)] = applyOp(s[int(key)], path[1:], v, appendOp)
		return s
	}
	return c
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
	cwd := resolveSessionCWD(ws, root)
	return domain.Session{
		PluginID:  p.id,
		AgentType: "copilot-vc",
		SessionID: strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)),
		CWD:       cwd,
		// Copilot logs often carry no workspace path at all; let the host infer
		// one from a temporally-near session (a cross-plugin heuristic).
		InferCWD:  cwd == "(unknown)",
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
