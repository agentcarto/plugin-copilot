package copilot

import "testing"

func TestPromptText(t *testing.T) {
	cases := []struct{ in, want string }{
		{"fix the   bug\nplease", "fix the bug please"},
		{"<context-file path=\"a.go\">...</context-file>", ""},
		{"/fix", ""},
		{"/explain this function in detail because I do not understand it", "/explain this function in detail because I do not understand it"},
		{"", ""},
	}
	for _, c := range cases {
		if got := promptText(c.in); got != c.want {
			t.Errorf("promptText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
