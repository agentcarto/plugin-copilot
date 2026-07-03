package copilot

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/agentcarto/core/common"
	"github.com/agentcarto/core/domain"
	"github.com/agentcarto/core/scan"
)

// This file holds the private helpers shared by the VS Code and JetBrains
// factories. The editor-specific scanning and parsing live in vscode.go and
// jetbrains.go respectively.

// promptText returns the cleaned, whitespace-folded genuine prompt in text, or
// "" when the message is an editor-injected <context-...> attachment or a
// short single-line slash command (/fix, /explain) rather than real user input.
func promptText(text string) string {
	t := strings.TrimSpace(text)
	if t == "" || strings.HasPrefix(strings.ToLower(t), "<context-") {
		return ""
	}
	if common.IsBareSlashCommand(t) {
		return ""
	}
	return strings.Join(strings.Fields(t), " ")
}

// userEvent builds a user event annotated with the normalized Prompt field.
func userEvent(text string, ts time.Time, rawType string) domain.Event {
	return domain.Event{Kind: domain.EventUser, Text: text, Timestamp: ts, RawType: rawType, Prompt: promptText(text)}
}

// scanEntry runs the standard scan-cache lifecycle for a single target keyed by
// `key` (a file path for VS Code, a session directory for JetBrains): reuse a
// warm result, skip a dead entry, otherwise build a fresh session. It returns
// the session to append and whether one was produced.
func scanEntry(cache *scan.Cache, key string, build func() (domain.Session, bool)) (domain.Session, bool) {
	if s, ok := cache.Reuse(key); ok {
		return s, true
	}
	if cache.Skip(key) {
		return domain.Session{}, false
	}
	s, ok := build()
	if !ok {
		cache.Dead(key)
		return domain.Session{}, false
	}
	cache.Stamp(&s)
	return s, true
}

// msToTime converts a millisecond epoch (stored as float64 or int64) to a Time.
// Non-positive or non-numeric values yield the zero Time.
func msToTime(v any) time.Time {
	switch n := v.(type) {
	case float64:
		if n <= 0 {
			return time.Time{}
		}
		return time.UnixMilli(int64(n))
	case int64:
		if n <= 0 {
			return time.Time{}
		}
		return time.UnixMilli(n)
	}
	return time.Time{}
}

// NormalizeCWD normalizes a file:// or vscode-remote URI into an OS-local path.
func NormalizeCWD(s string) string {
	if s == "" {
		return "(unknown)"
	}
	u, e := url.Parse(s)
	if e != nil {
		return s
	}
	path, _ := url.PathUnescape(u.Path)
	if u.Scheme == "file" {
		// Windows drive paths arrive as "/C:/...": turn them into "C:\...".
		if len(path) >= 3 && path[0] == '/' && path[2] == ':' {
			drive := strings.ToUpper(path[1:2])
			rest := strings.ReplaceAll(path[3:], "/", "\\")
			if rest == "" {
				rest = "\\"
			}
			return drive + ":" + rest
		}
		// A non-drive file path (e.g. a POSIX "/home/u/x") is a logical identifier
		// that must normalize identically on every OS — sessions are browsed
		// cross-platform — so keep its forward slashes rather than rewriting them
		// to the host's separator with filepath.FromSlash.
		return path
	}
	if u.Scheme == "vscode-remote" {
		auth, _ := url.PathUnescape(u.Host)
		kind, host, _ := strings.Cut(auth, "+")
		if kind == "ssh-remote" {
			// Drop the host prefix when the remote is the local machine.
			hn, _ := os.Hostname()
			if host == hn || strings.Split(host, ".")[0] == strings.Split(hn, ".")[0] {
				return path
			}
			return host + ":" + path
		}
		return auth + ":" + path
	}
	return s
}

var (
	fileURIRe        = regexp.MustCompile(`file://[^\s"'<>]+`)
	filePathAttrRe   = regexp.MustCompile(`filePath="([^"]+)"`)
	fileHeaderPathRe = regexp.MustCompile("File `([^`]+)`")
)

// pathsFromText extracts path-like substrings from free-form text: filePath="…"
// attributes, file:// URIs, and "File `…`" headers.
func pathsFromText(s string) []string {
	var out []string
	for _, m := range filePathAttrRe.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	out = append(out, fileURIRe.FindAllString(s, -1)...)
	for _, m := range fileHeaderPathRe.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return out
}

// collectPathFields walks an arbitrary decoded JSON value and appends every
// string field whose key looks like a path or URI to out.
func collectPathFields(v any, out *[]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, v := range x {
			lk := strings.ToLower(k)
			if s := common.String(v); s != "" && (strings.Contains(lk, "path") || strings.Contains(lk, "uri")) {
				*out = append(*out, s)
			}
			collectPathFields(v, out)
		}
	case []any:
		for _, v := range x {
			collectPathFields(v, out)
		}
	}
}

// inferCWDFromPathCandidates picks the most likely working directory from a set
// of candidate paths: each path is resolved to its project root, and the root
// referenced most often wins (ties broken by the shortest root).
func inferCWDFromPathCandidates(paths []string) string {
	counts := map[string]int{}
	for _, raw := range paths {
		p, ok := normalizeLocalPathCandidate(raw)
		if !ok {
			continue
		}
		if root := projectRootForPath(p); root != "" {
			counts[root]++
		}
	}
	best, bestN := "", 0
	for root, n := range counts {
		if n > bestN || (n == bestN && (best == "" || len(root) < len(best))) {
			best, bestN = root, n
		}
	}
	return best
}

// normalizeLocalPathCandidate trims surrounding punctuation and reports whether
// the result is an absolute local path (POSIX or Windows drive), returning the
// cleaned path when so.
func normalizeLocalPathCandidate(s string) (string, bool) {
	s = strings.TrimSpace(strings.Trim(s, ".,;:()[]{}<>\"'"))
	if s == "" {
		return "", false
	}
	if strings.HasPrefix(s, "file://") {
		s = NormalizeCWD(s)
	}
	if filepath.IsAbs(s) {
		return filepath.Clean(s), true
	}
	// Windows drive path such as "C:\..." or "C:/..." (filepath.IsAbs is false
	// for these on non-Windows hosts, so detect them explicitly).
	if len(s) >= 3 && ((s[0] >= 'A' && s[0] <= 'Z') || (s[0] >= 'a' && s[0] <= 'z')) && s[1] == ':' && (s[2] == '\\' || s[2] == '/') {
		return s, true
	}
	return "", false
}

// projectRootForPath walks up from path until it finds a directory containing a
// known project marker, returning that directory (or "" if none is found).
func projectRootForPath(path string) string {
	dir := path
	if st, err := os.Stat(path); err == nil {
		if !st.IsDir() {
			dir = filepath.Dir(path)
		}
	} else if filepath.Ext(path) != "" {
		dir = filepath.Dir(path)
	}
	for {
		if projectMarker(dir) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// projectMarker reports whether dir contains a file that marks a project root.
func projectMarker(dir string) bool {
	for _, name := range []string{".git", "go.mod", "package.json", "composer.json", "pyproject.toml", "Cargo.toml", "Gemfile", "pom.xml", "build.gradle", "build.gradle.kts"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}
