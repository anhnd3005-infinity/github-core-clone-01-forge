package main

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

var (
	mcpParseErrorHTTP = buildMcpHTTPFrame(400, "Bad Request", []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error"}}`))
	mcpInvalidReqHTTP = buildMcpHTTPFrame(400, "Bad Request", []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid Request"}}`))
	mcpMethodNotAllowed = buildMcpHTTPFrame(405, "Method Not Allowed", []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Method Not Allowed"}}`))
	mcpAcceptedHTTP   = []byte("HTTP/1.1 202 Accepted\r\nX-GitHub-Request-Id: fg-ok\r\nContent-Length: 0\r\n\r\n")
	mcpNoContentHTTP  = []byte("HTTP/1.1 204 No Content\r\nX-GitHub-Request-Id: fg-ok\r\n\r\n")
)

const mcpToolsArrayJSON = `[{"name":"create_repo","description":"Create a new repository for the authenticated user.","inputSchema":{"type":"object","properties":{"name":{"type":"string","description":"Repository name"},"description":{"type":"string","description":"Repository description"},"private":{"type":"boolean","description":"Whether the repository is private"}},"required":["name"]}},{"name":"get_repo","description":"Fetch metadata for an existing repository by owner and name.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner login"},"repo":{"type":"string","description":"Repository name"}},"required":["owner","repo"]}},{"name":"create_issue","description":"Create a new issue in a repository.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner login"},"repo":{"type":"string","description":"Repository name"},"title":{"type":"string","description":"Issue title"},"body":{"type":"string","description":"Issue body text"}},"required":["owner","repo","title"]}},{"name":"list_issues","description":"List issues for a repository.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner login"},"repo":{"type":"string","description":"Repository name"},"state":{"type":"string","description":"Issue state filter: open, closed, or all"}},"required":["owner","repo"]}},{"name":"create_pull_request","description":"Open a new pull request between head and base branches.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner login"},"repo":{"type":"string","description":"Repository name"},"title":{"type":"string","description":"Pull request title"},"body":{"type":"string","description":"Pull request body"},"head":{"type":"string","description":"Head branch name"},"base":{"type":"string","description":"Base branch name"}},"required":["owner","repo","title","head","base"]}},{"name":"merge_pull_request","description":"Merge an open pull request.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner login"},"repo":{"type":"string","description":"Repository name"},"pull_number":{"type":"integer","description":"Pull request number"},"merge_method":{"type":"string","description":"Merge method (squash, merge, rebase)"}},"required":["owner","repo","pull_number"]}}]`

func buildMcpHTTPFrame(status int, statusText string, body []byte) []byte {
	var b strings.Builder
	b.Grow(160 + len(body))
	b.WriteString("HTTP/1.1 ")
	b.WriteString(strconv.Itoa(status))
	b.WriteByte(' ')
	b.WriteString(statusText)
	b.WriteString("\r\nContent-Type: application/json; charset=utf-8\r\nMCP-Protocol-Version: 2026-07-28\r\nX-GitHub-Request-Id: fg-ok\r\nContent-Length: ")
	b.WriteString(strconv.Itoa(len(body)))
	b.WriteString("\r\n\r\n")
	out := make([]byte, b.Len()+len(body))
	copy(out, b.String())
	copy(out[b.Len():], body)
	return out
}

func mcpToolSuccess(data any) map[string]any {
	b, _ := json.Marshal(data)
	return map[string]any{
		"content": []map[string]string{
			{"type": "text", "text": string(b)},
		},
		"structuredContent": data,
		"isError":           false,
	}
}

func executeMcpTool(store *ForgeStore, name string, args map[string]any) (int, string, map[string]any) {
	shortName := name
	if idx := strings.LastIndex(name, "__"); idx >= 0 {
		shortName = name[idx+2:]
	}
	missing := func(keys ...string) string {
		var m []string
		for _, k := range keys {
			v, ok := args[k]
			if !ok || v == nil {
				m = append(m, k)
				continue
			}
			if s, ok := v.(string); ok && s == "" {
				m = append(m, k)
			}
		}
		return strings.Join(m, ", ")
	}

	switch shortName {
	case "create_repo":
		if miss := missing("name"); miss != "" {
			return -32602, "Missing required argument(s): " + miss, nil
		}
		priv, _ := args["private"].(bool)
		repo, err := store.CreateRepo(asStringArg(args["name"]), asStringArg(args["description"]), priv)
		if err != nil {
			return 0, "", map[string]any{"content": []map[string]string{{"type": "text", "text": err.Error()}}, "isError": true}
		}
		return 0, "", mcpToolSuccess(repo.JSONView())

	case "get_repo":
		if miss := missing("owner", "repo"); miss != "" {
			return -32602, "Missing required argument(s): " + miss, nil
		}
		repo, err := store.RequireRepo(asStringArg(args["owner"]), asStringArg(args["repo"]))
		if err != nil {
			return 0, "", map[string]any{"content": []map[string]string{{"type": "text", "text": err.Error()}}, "isError": true}
		}
		return 0, "", mcpToolSuccess(repo.JSONView())

	case "create_issue":
		if miss := missing("owner", "repo", "title"); miss != "" {
			return -32602, "Missing required argument(s): " + miss, nil
		}
		iss, err := store.CreateIssue(asStringArg(args["owner"]), asStringArg(args["repo"]), asStringArg(args["title"]), asStringArg(args["body"]), nil, false)
		if err != nil {
			return 0, "", map[string]any{"content": []map[string]string{{"type": "text", "text": err.Error()}}, "isError": true}
		}
		return 0, "", mcpToolSuccess(iss)

	case "list_issues":
		if miss := missing("owner", "repo"); miss != "" {
			return -32602, "Missing required argument(s): " + miss, nil
		}
		st := asStringArg(args["state"])
		if st == "" {
			st = "open"
		}
		list, err := store.ListIssues(asStringArg(args["owner"]), asStringArg(args["repo"]), st, "desc")
		if err != nil {
			return 0, "", map[string]any{"content": []map[string]string{{"type": "text", "text": err.Error()}}, "isError": true}
		}
		return 0, "", mcpToolSuccess(list)

	case "create_pull_request":
		if miss := missing("owner", "repo", "title", "head", "base"); miss != "" {
			return -32602, "Missing required argument(s): " + miss, nil
		}
		pr, err := store.CreatePull(asStringArg(args["owner"]), asStringArg(args["repo"]), asStringArg(args["title"]), asStringArg(args["body"]), asStringArg(args["head"]), asStringArg(args["base"]))
		if err != nil {
			return 0, "", map[string]any{"content": []map[string]string{{"type": "text", "text": err.Error()}}, "isError": true}
		}
		return 0, "", mcpToolSuccess(pr)

	case "merge_pull_request":
		if miss := missing("owner", "repo", "pull_number"); miss != "" {
			return -32602, "Missing required argument(s): " + miss, nil
		}
		num, ok := asIntArg(args["pull_number"])
		if !ok || num <= 0 {
			if s, okS := args["pull_number"].(string); okS {
				if parsed, pErr := strconv.Atoi(s); pErr == nil {
					num = parsed
					ok = true
				}
			}
		}
		if !ok || num <= 0 {
			return -32602, "Invalid pull_number", nil
		}
		mm := asStringArg(args["merge_method"])
		if mm == "" {
			mm = "squash"
		}
		merged, err := store.MergePull(asStringArg(args["owner"]), asStringArg(args["repo"]), num, mm)
		if err != nil {
			return 0, "", map[string]any{"content": []map[string]string{{"type": "text", "text": err.Error()}}, "isError": true}
		}
		return 0, "", mcpToolSuccess(merged)

	default:
		return -32601, "Unknown tool: " + name, nil
	}
}

func HandleMCP(req *RawRequest, store *ForgeStore, expectedToken []byte) ([]byte, bool) {
	if req.Path != "/mcp" {
		return nil, false
	}
	if !IsAuthorized(req.AuthHeader, expectedToken) {
		return restUnauthHTTP, true
	}
	if req.Method == "DELETE" {
		return mcpNoContentHTTP, true
	}
	if req.Method != "POST" {
		return mcpMethodNotAllowed, true
	}

	var rawMap map[string]json.RawMessage
	if err := json.Unmarshal(req.Body, &rawMap); err != nil {
		return mcpParseErrorHTTP, true
	}
	if rawMap == nil {
		return mcpInvalidReqHTTP, true
	}

	idBytes := []byte("null")
	hasID := false
	if rawID, ok := rawMap["id"]; ok && len(bytes.TrimSpace(rawID)) > 0 {
		idBytes = bytes.TrimSpace(rawID)
		hasID = true
	}
	method, _ := rawString(rawMap, "method")

	if method == "notifications/initialized" || (!hasID && strings.HasPrefix(method, "notifications/")) {
		return mcpAcceptedHTTP, true
	}

	if method == "server/discover" {
		const resJSON = `{"supportedVersions":["2026-07-28","2025-11-25"],"capabilities":{"tools":{}},"serverInfo":{"name":"makefun-git-forge","version":"1.0.0"},"_meta":{"io.modelcontextprotocol/serverInfo":{"name":"makefun-git-forge","version":"1.0.0"}}}`
		var out bytes.Buffer
		out.WriteString(`{"jsonrpc":"2.0","id":`)
		out.Write(idBytes)
		out.WriteString(`,"result":`)
		out.WriteString(resJSON)
		out.WriteByte('}')
		return buildMcpHTTPFrame(200, "OK", out.Bytes()), true
	}

	if method == "initialize" {
		const resJSON = `{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"makefun-git-forge","version":"1.0.0"}}`
		var out bytes.Buffer
		out.WriteString(`{"jsonrpc":"2.0","id":`)
		out.Write(idBytes)
		out.WriteString(`,"result":`)
		out.WriteString(resJSON)
		out.WriteByte('}')
		return buildMcpHTTPFrame(200, "OK", out.Bytes()), true
	}

	if method == "tools/list" {
		var out bytes.Buffer
		out.Grow(len(mcpToolsArrayJSON) + 64)
		out.WriteString(`{"jsonrpc":"2.0","id":`)
		out.Write(idBytes)
		out.WriteString(`,"result":{"tools":`)
		out.WriteString(mcpToolsArrayJSON)
		out.WriteString(`}}`)
		return buildMcpHTTPFrame(200, "OK", out.Bytes()), true
	}

	if method == "tools/call" {
		var callParams struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if rawP, ok := rawMap["params"]; ok {
			_ = json.Unmarshal(rawP, &callParams)
		}
		if callParams.Arguments == nil {
			callParams.Arguments = map[string]any{}
		}
		code, errMsg, resObj := executeMcpTool(store, callParams.Name, callParams.Arguments)
		var out bytes.Buffer
		out.WriteString(`{"jsonrpc":"2.0","id":`)
		out.Write(idBytes)
		if code != 0 {
			errJSON, _ := json.Marshal(map[string]any{"code": code, "message": errMsg})
			out.WriteString(`,"error":`)
			out.Write(errJSON)
			out.WriteByte('}')
		} else {
			resJSON, _ := json.Marshal(resObj)
			out.WriteString(`,"result":`)
			out.Write(resJSON)
			out.WriteByte('}')
		}
		return buildMcpHTTPFrame(200, "OK", out.Bytes()), true
	}

	errJSON, _ := json.Marshal(map[string]any{"code": -32601, "message": "Method not found: " + method})
	var out bytes.Buffer
	out.WriteString(`{"jsonrpc":"2.0","id":`)
	out.Write(idBytes)
	out.WriteString(`,"error":`)
	out.Write(errJSON)
	out.WriteByte('}')
	return buildMcpHTTPFrame(200, "OK", out.Bytes()), true
}
