// fakeagent is a minimal ACP agent used by client tests: it answers the
// handshake, streams a few session/update notifications, asks for one
// permission, and finishes the prompt whose outcome depends on the answer.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

type msg struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
}

var out = bufio.NewWriter(os.Stdout)

func send(v any) {
	data, _ := json.Marshal(v)
	out.Write(data)
	out.WriteByte('\n')
	out.Flush()
}

func respond(id *json.RawMessage, result any) {
	res, _ := json.Marshal(result)
	send(map[string]any{"jsonrpc": "2.0", "id": id, "result": json.RawMessage(res)})
}

func notifyUpdate(update any) {
	send(map[string]any{"jsonrpc": "2.0", "method": "session/update",
		"params": map[string]any{"sessionId": "sess1", "update": update}})
}

func main() {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	pending := map[int64]*json.RawMessage{} // our request id → in-flight
	var reqID int64 = 100

	for sc.Scan() {
		var m msg
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		// Response to a request we made (the permission request).
		if m.Method == "" && m.ID != nil {
			var id int64
			json.Unmarshal(*m.ID, &id)
			if _, ok := pending[id]; ok {
				delete(pending, id)
				var res struct {
					Outcome struct {
						Outcome  string `json:"outcome"`
						OptionID string `json:"optionId"`
					} `json:"outcome"`
				}
				json.Unmarshal(m.Result, &res)
				finishPrompt(res.Outcome.OptionID == "allow")
			}
			continue
		}
		switch m.Method {
		case "initialize":
			respond(m.ID, map[string]any{"protocolVersion": 1,
				"agentCapabilities": map[string]any{}, "authMethods": []any{}})
		case "session/new":
			respond(m.ID, map[string]any{"sessionId": "sess1"})
		case "session/set_mode":
			respond(m.ID, map[string]any{})
		case "session/prompt":
			promptID = m.ID
			notifyUpdate(map[string]any{"sessionUpdate": "agent_message_chunk",
				"content": map[string]any{"type": "text", "text": "Working on it. "}})
			notifyUpdate(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "t1",
				"title": "run tests", "kind": "execute", "status": "pending"})
			// ask permission for the tool call
			id := reqID
			reqID++
			raw := json.RawMessage(fmt.Sprintf("%d", id))
			pending[id] = &raw
			send(map[string]any{"jsonrpc": "2.0", "id": id, "method": "session/request_permission",
				"params": map[string]any{
					"sessionId": "sess1",
					"toolCall":  map[string]any{"toolCallId": "t1", "title": "run tests", "kind": "execute"},
					"options": []map[string]any{
						{"optionId": "allow", "name": "Allow", "kind": "allow_once"},
						{"optionId": "reject", "name": "Reject", "kind": "reject_once"},
					},
				}})
		}
	}
}

var promptID *json.RawMessage

func finishPrompt(allowed bool) {
	if allowed {
		notifyUpdate(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "t1", "status": "completed"})
		notifyUpdate(map[string]any{"sessionUpdate": "agent_message_chunk",
			"content": map[string]any{"type": "text", "text": "Done.\n```json\n{\"status\":\"ok\",\"summary\":\"tests pass\"}\n```"}})
	} else {
		notifyUpdate(map[string]any{"sessionUpdate": "agent_message_chunk",
			"content": map[string]any{"type": "text", "text": "Tool was rejected.\n```json\n{\"status\":\"error\",\"summary\":\"not permitted\"}\n```"}})
	}
	respond(promptID, map[string]any{"stopReason": "end_turn"})
}
