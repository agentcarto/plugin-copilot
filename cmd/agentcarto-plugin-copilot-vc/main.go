// Command agentcarto-plugin-copilot-vc serves AgentCarto's Copilot (VS Code) plugin as a subprocess.
package main

import (
	"github.com/agentcarto/core/plugin"
	"github.com/agentcarto/plugin-copilot"
)

func main() {
	plugin.Serve(copilot.VSCodeFactory{})
}
