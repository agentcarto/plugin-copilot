package copilot

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentcarto/core/domain"
	"github.com/agentcarto/core/plugin"
)

func TestNormalizeCWD(t *testing.T) {
	cases := map[string]string{"file:///C:/repo/x": "C:\\repo\\x", "file:///home/u/x": "/home/u/x", "vscode-remote://ssh-remote+other/path": "other:/path"}
	for in, want := range cases {
		if got := NormalizeCWD(in); got != want {
			t.Errorf("%s: %q != %q", in, got, want)
		}
	}
}

func TestParseJetBrainsCopilot(t *testing.T) {
	dir := t.TempDir()
	data := `{"type":"partition.created","data":{"conversationId":"s1","partitionId":1,"source":"panel","createdAt":1710000000000},"id":"p1","timestamp":"2024-03-09T16:00:00Z","parentId":null}
{"type":"user.message","data":{"content":"fix this","turnId":"t1"},"id":"u1","timestamp":"2024-03-09T16:00:01Z","parentId":null}
{"type":"assistant.message","data":{"content":"done","messageId":"m1","text":"","iterationNumber":1},"id":"a1","timestamp":"2024-03-09T16:00:02Z","parentId":null}
{"type":"tool.execution_start","data":{"toolCallId":"tc1","toolName":"read_file","arguments":{"filePath":"main.go"}},"id":"t1","timestamp":"2024-03-09T16:00:03Z","parentId":null}
`
	if err := os.WriteFile(filepath.Join(dir, "partition-1.jsonl"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	ev, started := parseJetBrainsCopilotWithStart(context.Background(), dir)
	if len(ev) != 3 {
		t.Fatalf("events=%d %#v", len(ev), ev)
	}
	if started.IsZero() {
		t.Fatal("started time was not parsed")
	}
	if ev[0].Kind != domain.EventUser || ev[0].Text != "fix this" {
		t.Fatalf("bad user event: %#v", ev[0])
	}
	if ev[1].Kind != domain.EventAssistant || ev[1].Text != "done" {
		t.Fatalf("bad assistant event: %#v", ev[1])
	}
	if ev[2].Kind != domain.EventToolCall || ev[2].ToolName != "read_file" {
		t.Fatalf("bad tool event: %#v", ev[2])
	}
}

func TestParseJetBrainsCopilotRenderedAttachments(t *testing.T) {
	dir := t.TempDir()
	rendered := `<attachments>
<attachment id="relative.go">
Excerpt from relative.go, lines 1 to 2:
` + "```go\npackage main\n```" + `
</attachment>
<attachment id="active.go" filePath="/repo/a&amp;b/active.go">
User's active file for additional context:
` + "```go\nfunc main() {}\n```" + `
</attachment>
</attachments>`
	renderedJSON, err := json.Marshal(rendered)
	if err != nil {
		t.Fatal(err)
	}
	data := `{"type":"partition.created","data":{"conversationId":"s1","partitionId":1,"source":"panel","createdAt":1710000000000},"timestamp":"2024-03-09T16:00:00Z"}
{"type":"user.message","data":{"content":"review","turnId":"t1"},"timestamp":"2024-03-09T16:00:01Z"}
{"type":"user.message_rendered","data":{"turnId":"t1","renderedMessage":` + string(renderedJSON) + `},"timestamp":"2024-03-09T16:00:01Z"}
{"type":"assistant.message","data":{"content":"done","messageId":"m1"},"timestamp":"2024-03-09T16:00:02Z"}
`
	if err := os.WriteFile(filepath.Join(dir, "partition-1.jsonl"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}

	ev := parseJetBrainsCopilot(context.Background(), dir)
	if len(ev) != 4 {
		t.Fatalf("events=%d %#v", len(ev), ev)
	}
	if ev[1].Kind != domain.EventAttachment || ev[1].ToolArg != "relative.go" || !strings.Contains(ev[1].Text, "package main") {
		t.Fatalf("relative attachment=%#v", ev[1])
	}
	if ev[2].Kind != domain.EventAttachment || ev[2].ToolArg != "/repo/a&b/active.go" || !strings.Contains(ev[2].Text, "func main") {
		t.Fatalf("active attachment=%#v", ev[2])
	}
	if ev[1].TurnID != "t1" || ev[2].TurnID != "t1" {
		t.Fatalf("attachment turn ids=%q, %q", ev[1].TurnID, ev[2].TurnID)
	}
}

func TestScanJetBrainsCopilotIgnoresInlineSessions(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, "inline-session")
	if err := os.MkdirAll(sessionDir, 0700); err != nil {
		t.Fatal(err)
	}
	data := `{"type":"partition.created","data":{"conversationId":"inline-session","partitionId":1,"source":"inline","createdAt":1710000000000},"timestamp":"2024-03-09T16:00:00Z"}
{"type":"user.message","data":{"content":"test","turnId":"t1"},"timestamp":"2024-03-09T16:00:01Z"}
{"type":"assistant.message","data":{"content":"this must not be imported","messageId":"m1"},"timestamp":"2024-03-09T16:00:02Z"}
{"type":"assistant.turn_end","data":{"turnId":"t1","status":"success","turnStatus":"success"},"timestamp":"2024-03-09T16:00:03Z"}
`
	if err := os.WriteFile(filepath.Join(sessionDir, "partition-1.jsonl"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}

	p := jetbrainsPlugin{id: "copilot", o: JetBrainsOptions{Dirs: []string{root}}}
	res, err := p.Scan(context.Background(), plugin.ScanInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Sessions) != 0 {
		t.Fatalf("inline sessions were imported: %#v", res.Sessions)
	}
	if ev := parseJetBrainsCopilot(context.Background(), sessionDir); len(ev) != 0 {
		t.Fatalf("inline conversation events were loaded: %#v", ev)
	}
}

func TestScanJetBrainsCopilotInfersCWDFromRenderedAttachment(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "docker"), 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(repo, "docker", "Dockerfile")
	if err := os.WriteFile(target, []byte("FROM scratch\n"), 0600); err != nil {
		t.Fatal(err)
	}

	sessionDir := filepath.Join(root, "copilot", "s1")
	if err := os.MkdirAll(sessionDir, 0700); err != nil {
		t.Fatal(err)
	}
	rendered := `<attachment id="Dockerfile" filePath="` + target + `"></attachment>
Files:
- file://` + filepath.ToSlash(target)
	renderedJSON, err := json.Marshal(rendered)
	if err != nil {
		t.Fatal(err)
	}
	data := `{"type":"partition.created","data":{"conversationId":"s1","partitionId":1,"source":"panel","createdAt":1710000000000},"timestamp":"2024-03-09T16:00:00Z"}
{"type":"user.message","data":{"content":"review","turnId":"t1"},"timestamp":"2024-03-09T16:00:01Z"}
{"type":"user.message_rendered","data":{"turnId":"t1","renderedMessage":` + string(renderedJSON) + `},"timestamp":"2024-03-09T16:00:01Z"}
{"type":"assistant.message","data":{"content":"done","messageId":"m1"},"timestamp":"2024-03-09T16:00:02Z"}
`
	if err := os.WriteFile(filepath.Join(sessionDir, "partition-1.jsonl"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}

	p := jetbrainsPlugin{id: "copilot", o: JetBrainsOptions{Dirs: []string{filepath.Join(root, "copilot")}}}
	res, err := p.Scan(context.Background(), plugin.ScanInput{})
	ss := res.Sessions
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 1 {
		t.Fatalf("sessions=%d %#v", len(ss), ss)
	}
	if ss[0].CWD != repo {
		t.Fatalf("cwd=%q want %q", ss[0].CWD, repo)
	}
}

func TestScanVSCopilotIgnoresUnknownStoredTitle(t *testing.T) {
	root := t.TempDir()
	userDir := filepath.Join(root, "User")
	wsDir := filepath.Join(userDir, "workspaceStorage", "ws1")
	sessionDir := filepath.Join(wsDir, "chatSessions")
	if err := os.MkdirAll(sessionDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsDir, "workspace.json"), []byte(`{"folder":"file:///tmp/repo"}`), 0600); err != nil {
		t.Fatal(err)
	}
	data := `{
  "customTitle": "unknown",
  "creationDate": 1710000000000,
  "lastMessageDate": 1710000001000,
  "requests": [
    {
      "message": {"text": "explain the failing test"},
      "response": "Use the failure output as the starting point."
    }
  ]
}`
	if err := os.WriteFile(filepath.Join(sessionDir, "s1.json"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}

	p := vscodePlugin{id: "copilot", o: VSCodeOptions{UserDirs: []string{userDir}}}
	res, err := p.Scan(context.Background(), plugin.ScanInput{})
	ss := res.Sessions
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 1 {
		t.Fatalf("sessions=%d %#v", len(ss), ss)
	}
	if ss[0].Title != "explain the failing test" {
		t.Fatalf("title=%q", ss[0].Title)
	}
}

func TestScanVSCopilotInfersCWDFromToolPath(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(repo, "cmd", "main.go")
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("package main\n"), 0600); err != nil {
		t.Fatal(err)
	}

	userDir := filepath.Join(root, "User")
	sessionDir := filepath.Join(userDir, "workspaceStorage", "ws1", "chatSessions")
	if err := os.MkdirAll(sessionDir, 0700); err != nil {
		t.Fatal(err)
	}
	arg, err := json.Marshal(map[string]any{"filePath": target})
	if err != nil {
		t.Fatal(err)
	}
	data := `{
  "creationDate": 1710000000000,
  "lastMessageDate": 1710000001000,
  "requests": [
    {
      "message": {"text": "review the entrypoint"},
      "response": "done",
      "result": {
        "metadata": {
          "toolCallRounds": [
            {"toolCalls": [{"name": "read_file", "arguments": ` + string(arg) + `}]}
          ]
        }
      }
    }
  ]
}`
	if err := os.WriteFile(filepath.Join(sessionDir, "s1.json"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}

	p := vscodePlugin{id: "copilot", o: VSCodeOptions{UserDirs: []string{userDir}}}
	res, err := p.Scan(context.Background(), plugin.ScanInput{})
	ss := res.Sessions
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 1 {
		t.Fatalf("sessions=%d %#v", len(ss), ss)
	}
	if ss[0].CWD != repo {
		t.Fatalf("cwd=%q want %q", ss[0].CWD, repo)
	}
}

func TestScanVSCopilotFindsEmptyWindowSessions(t *testing.T) {
	root := t.TempDir()
	userDir := filepath.Join(root, "User")
	sessionDir := filepath.Join(userDir, "globalStorage", "emptyWindowChatSessions")
	if err := os.MkdirAll(sessionDir, 0700); err != nil {
		t.Fatal(err)
	}
	data := `{"kind":0,"v":{"version":3,"creationDate":1710000000000,"sessionId":"s1","requests":[]}}
{"kind":1,"k":["customTitle"],"v":"empty window title"}
{"kind":2,"k":["requests"],"v":[{"requestId":"r1","timestamp":1710000001000,"modelId":"copilot/auto","message":{"text":"hello empty window"},"response":[{"value":"ok"}],"result":{"details":"GPT"}}]}
`
	if err := os.WriteFile(filepath.Join(sessionDir, "s1.jsonl"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}

	p := vscodePlugin{id: "copilot-vc", o: VSCodeOptions{UserDirs: []string{userDir}}}
	res, err := p.Scan(context.Background(), plugin.ScanInput{})
	ss := res.Sessions
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 1 {
		t.Fatalf("sessions=%d %#v", len(ss), ss)
	}
	if ss[0].PluginID != "copilot-vc" || ss[0].AgentType != "copilot-vc" {
		t.Fatalf("bad identity: %#v", ss[0])
	}
	if ss[0].Title != "empty window title" {
		t.Fatalf("title=%q", ss[0].Title)
	}
	if ss[0].CWD != "(unknown)" {
		t.Fatalf("cwd=%q", ss[0].CWD)
	}
	if ss[0].Model != "GPT" {
		t.Fatalf("model=%q", ss[0].Model)
	}
}

func TestScanVSCopilotFindsEmptyWindowFlatJSONSessions(t *testing.T) {
	root := t.TempDir()
	userDir := filepath.Join(root, "User")
	sessionDir := filepath.Join(userDir, "globalStorage", "emptyWindowChatSessions")
	if err := os.MkdirAll(sessionDir, 0700); err != nil {
		t.Fatal(err)
	}
	data := `{
  "customTitle": "flat empty window title",
  "creationDate": 1710000000000,
  "lastMessageDate": 1710000001000,
  "requests": [
    {
      "message": {"text": "hello flat empty window"},
      "response": "ok",
      "result": {"details": "GPT flat"}
    }
  ]
}`
	if err := os.WriteFile(filepath.Join(sessionDir, "s1.json"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}

	p := vscodePlugin{id: "copilot-vc", o: VSCodeOptions{UserDirs: []string{userDir}}}
	res, err := p.Scan(context.Background(), plugin.ScanInput{})
	ss := res.Sessions
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 1 {
		t.Fatalf("sessions=%d %#v", len(ss), ss)
	}
	if ss[0].Title != "flat empty window title" {
		t.Fatalf("title=%q", ss[0].Title)
	}
	if ss[0].Model != "GPT flat" {
		t.Fatalf("model=%q", ss[0].Model)
	}
}

func TestScanJetBrainsCopilotBackfillsUnknownCWDFromNearbySession(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(repo, "app", "Service.php")
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("<?php\n"), 0600); err != nil {
		t.Fatal(err)
	}

	knownDir := filepath.Join(root, "copilot", "known")
	if err := os.MkdirAll(knownDir, 0700); err != nil {
		t.Fatal(err)
	}
	knownData := `{"type":"partition.created","data":{"conversationId":"known","partitionId":1,"source":"panel","createdAt":1710000000000},"timestamp":"2024-03-09T16:00:00Z"}
{"type":"user.message","data":{"content":"known","turnId":"t1"},"timestamp":"2024-03-09T16:00:01Z"}
{"type":"tool.execution_start","data":{"toolCallId":"tc1","toolName":"read_file","arguments":{"filePath":"` + filepath.ToSlash(target) + `"}},"timestamp":"2024-03-09T16:00:02Z"}
`
	if err := os.WriteFile(filepath.Join(knownDir, "partition-1.jsonl"), []byte(knownData), 0600); err != nil {
		t.Fatal(err)
	}

	unknownDir := filepath.Join(root, "copilot", "unknown")
	if err := os.MkdirAll(unknownDir, 0700); err != nil {
		t.Fatal(err)
	}
	unknownData := `{"type":"user.message","data":{"content":"short panel prompt","turnId":"t2"},"timestamp":"2024-03-09T16:05:00Z"}
{"type":"partition.created","data":{"conversationId":"unknown","partitionId":1,"source":"panel","createdAt":1710000300000},"timestamp":"2024-03-09T16:05:00Z"}
{"type":"assistant.turn_end","data":{"turnId":"t2","status":"success"},"timestamp":"2024-03-09T16:05:10Z"}
`
	if err := os.WriteFile(filepath.Join(unknownDir, "partition-1.jsonl"), []byte(unknownData), 0600); err != nil {
		t.Fatal(err)
	}

	p := jetbrainsPlugin{id: "copilot", o: JetBrainsOptions{Dirs: []string{filepath.Join(root, "copilot")}}}
	res, err := p.Scan(context.Background(), plugin.ScanInput{})
	ss := res.Sessions
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]domain.Session{}
	for _, s := range ss {
		byID[s.SessionID] = s
	}
	if byID["unknown"].CWD != repo {
		t.Fatalf("cwd=%q want %q", byID["unknown"].CWD, repo)
	}
}

// TestVSCopilotEventTimestamps guards that VS Code conversation events carry the
// request timestamp, so the UI can show a date for each turn. Both the flat
// .json layout and the reassembled .jsonl layout are covered.
// Response parts must surface in their recorded order: thinking as reasoning,
// tool invocations with arguments from rounds and results from toolCallResults,
// inline file references back in the markdown flow, applied edits as file
// changes, and question carousels as assistant text.
func TestVSCopilotResponsePartsInOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s1.json")
	data := `{
  "creationDate": 1710000000000,
  "requests": [
    {
      "timestamp": 1710000001000,
      "message": {"text": "split the chapter"},
      "response": [
        {"kind": "thinking", "value": "compare both chapters first", "id": "t1"},
        {"kind": "toolInvocationSerialized", "toolCallId": "c1", "toolId": "copilot_readFile",
         "invocationMessage": {"value": "reading file", "supportHtml": false}},
        {"kind": "prepareToolInvocation", "toolName": "copilot_readFile"},
        {"value": "対象は "},
        {"kind": "inlineReference", "name": "ReVIEW/09.re", "inlineReference": {"uri": {"path": "/d:/repo/ReVIEW/09.re"}}},
        {"value": " の章分割だと見ています。\n"},
        {"kind": "textEditGroup", "uri": {"fsPath": "/repo/a.re"}, "edits": [[{"text": "new line 1\nnew line 2\n"}]]},
        {"kind": "questionCarousel", "questions": [
          {"title": "Theme", "message": {"value": "どんな見た目にしますか？"},
           "options": [{"label": "ミニマル"}, {"label": "リッチ"}]}
        ]}
      ],
      "result": {"metadata": {
        "toolCallRounds": [{"toolCalls": [
          {"id": "c1", "name": "read_file", "arguments": "{\"filePath\": \"a.re\"}"},
          {"id": "c2", "name": "grep", "arguments": "{\"pattern\": \"x\"}"}
        ]}],
        "toolCallResults": {"c1": {"content": [{"value": {"node": {"children": [
          {"text": "File: ", "lineBreakBefore": false},
          {"text": "a.re", "lineBreakBefore": false},
          {"text": "1: line one", "lineBreakBefore": true}
        ]}}}]}}
      }}
    }
  ]
}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	ev, _ := parseVSCodeSession(path)
	var got []string
	for _, e := range ev {
		got = append(got, string(e.Kind)+":"+e.ToolName+":"+e.Text)
	}
	want := []string{
		"user::split the chapter",
		"reasoning::compare both chapters first",
		"tool_call:copilot_readFile:{\"filePath\": \"a.re\"}",
		"tool_result::File: a.re\n1: line one",
		"assistant::対象は ReVIEW/09.re の章分割だと見ています。",
		"file_change::*** Begin Patch\n*** Update File: /repo/a.re\n+new line 1\n+new line 2\n*** End Patch",
		"assistant::どんな見た目にしますか？\n- ミニマル\n- リッチ",
		"tool_call:grep:{\"pattern\": \"x\"}",
	}
	if len(got) != len(want) {
		t.Fatalf("events:\n%q\nwant:\n%q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d:\n got %q\nwant %q", i, got[i], want[i])
		}
	}
}

// The .jsonl op log (kind 0 snapshot / kind 1 set / kind 2 append) replays into
// the same layout as flat .json files, preserving response parts, results, and
// the request's modelId.
func TestVSCopilotJSONLOpReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s1.jsonl")
	data := `{"kind":0,"v":{"version":3,"creationDate":1710000000000,"sessionId":"s1","requests":[{"requestId":"r1","timestamp":1710000001000,"modelId":"copilot/gpt","message":{"text":"hello"},"response":[{"kind":"thinking","value":"hm","id":"t1"}]}]}}
{"kind":2,"k":["requests",0,"response"],"v":[{"value":"done\n"}]}
{"kind":1,"k":["requests",0,"result"],"v":{"metadata":{"toolCallRounds":[{"toolCalls":[{"id":"c1","name":"bash","arguments":"{}"}]}],"toolCallResults":{"c1":{"content":[{"value":{"node":{"children":[{"text":"ok"}]}}}]}}}}}
{"kind":2,"k":["requests"],"v":[{"requestId":"r2","timestamp":1710000002000,"message":{"text":"next"},"response":[{"value":"sure"}]}]}
`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	ev, root := parseVSCodeSession(path)
	var got []string
	for _, e := range ev {
		got = append(got, string(e.Kind)+":"+e.Text)
	}
	want := []string{
		"user:hello", "reasoning:hm", "assistant:done", "tool_call:{}", "tool_result:ok",
		"user:next", "assistant:sure",
	}
	if len(got) != len(want) {
		t.Fatalf("events:\n%q\nwant:\n%q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d = %q, want %q", i, got[i], want[i])
		}
	}
	if m := copilotModel(root); m != "copilot/gpt" {
		t.Fatalf("model=%q, want copilot/gpt (modelId must survive the replay)", m)
	}
}

// Each request's model is stamped onto its events (result.details preferred
// over modelId), so the host can show per-turn models when the model changes
// between requests. The session-level model stays the last request's model.
func TestVSCopilotPerTurnModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s1.json")
	data := `{"version":3,"creationDate":1710000000000,"sessionId":"s1","requests":[
  {"requestId":"r1","timestamp":1710000001000,"modelId":"copilot/gpt","message":{"text":"hello"},"response":[{"value":"ok"}],"result":{"details":"GPT-5"}},
  {"requestId":"r2","timestamp":1710000002000,"modelId":"copilot/claude","message":{"text":"next"},"response":[{"value":"sure"}]}
]}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	ev, root := parseVSCodeSession(path)
	if m := copilotModel(root); m != "copilot/claude" {
		t.Fatalf("session model=%q, want copilot/claude (last request)", m)
	}
	want := map[string]string{"hello": "GPT-5", "ok": "GPT-5", "next": "copilot/claude", "sure": "copilot/claude"}
	seen := map[string]bool{}
	for _, e := range ev {
		w, ok := want[e.Text]
		if !ok {
			continue
		}
		seen[e.Text] = true
		if e.Model != w {
			t.Fatalf("event %q model=%q, want %q", e.Text, e.Model, w)
		}
	}
	if len(seen) != len(want) {
		t.Fatalf("only saw %v of expected events %v", seen, want)
	}
}

func TestVSCopilotEventTimestamps(t *testing.T) {
	want := msToTime(float64(1710000001000))

	flat := filepath.Join(t.TempDir(), "s1.json")
	flatData := `{
  "creationDate": 1710000000000,
  "requests": [
    {
      "timestamp": 1710000001000,
      "message": {"text": "explain"},
      "response": "answer",
      "result": {"metadata": {"toolCallRounds": [{"toolCalls": [{"name": "read_file", "arguments": {}}]}]}}
    }
  ]
}`
	if err := os.WriteFile(flat, []byte(flatData), 0600); err != nil {
		t.Fatal(err)
	}

	stream := filepath.Join(t.TempDir(), "s1.jsonl")
	streamData := `{"kind":0,"v":{"version":3,"creationDate":1710000000000,"sessionId":"s1","requests":[]}}
{"kind":2,"k":["requests"],"v":[{"requestId":"r1","timestamp":1710000001000,"modelId":"copilot/auto","message":{"text":"hello"},"response":[{"value":"ok"}]}]}
`
	if err := os.WriteFile(stream, []byte(streamData), 0600); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{flat, stream} {
		ev, _ := parseVSCodeSession(path)
		if len(ev) == 0 {
			t.Fatalf("%s: no events", path)
		}
		for _, e := range ev {
			if !e.Timestamp.Equal(want) {
				t.Errorf("%s: event %q timestamp=%v want %v", filepath.Ext(path), e.Kind, e.Timestamp, want)
			}
		}
	}
}
