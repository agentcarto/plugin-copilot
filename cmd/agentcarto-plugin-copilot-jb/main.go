// Command agentcarto-plugin-copilot-jb serves AgentCarto's Copilot (JetBrains) plugin as a subprocess.
package main

import (
	"github.com/agentcarto/core/plugin"
	"github.com/agentcarto/plugin-copilot"
)

func main() {
	plugin.Serve(copilot.JetBrainsFactory{})
}
