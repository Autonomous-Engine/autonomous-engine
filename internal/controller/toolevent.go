package controller

import "encoding/json"

// ToolEvent builds the Event for one executor callback. arguments is the
// tool call's raw JSON; path is read from its "path" (or "file_path")
// field for the file tools. command and exitCode are run_command's
// structured result fields; when command is empty the "command" argument
// is used.
func ToolEvent(callID, tool, arguments string, isError bool, command string, exitCode *int) Event {
	ev := Event{Kind: EventTool, CallID: callID, Tool: tool, IsError: isError, Command: command, ExitCode: exitCode}
	var args map[string]any
	if json.Unmarshal([]byte(arguments), &args) == nil {
		switch tool {
		case "write_file", "edit_file", "insert_after", "append_to_file", "read_file":
			if p, ok := args["path"].(string); ok {
				ev.Path = p
			} else if p, ok := args["file_path"].(string); ok {
				ev.Path = p
			}
		case "run_command":
			if ev.Command == "" {
				ev.Command, _ = args["command"].(string)
			}
		}
	}
	return ev
}
