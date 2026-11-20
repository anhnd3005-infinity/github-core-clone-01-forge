package main

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

var (
	bearerPrefix      = []byte("Bearer ")
	restMetaHTTP      = buildHTTPFrameBytes(200, "OK", []byte(`{"installed_version":"1.0.0"}`), "")
	restUnauthHTTP    = buildHTTPFrameBytes(401, "Unauthorized", []byte(`{"message":"Requires authentication"}`), "")
	restNotFoundHTTP  = buildHTTPFrameBytes(404, "Not Found", []byte(`{"message":"Not Found"}`), "")
	restMalformedJSON = buildHTTPFrameBytes(400, "Bad Request", []byte(`{"message":"Problems parsing JSON"}`), "")
	restNoContentHTTP = []byte("HTTP/1.1 204 No Content\r\nContent-Type: application/json\r\nX-GitHub-Request-Id: 1\r\n\r\n")
)

func buildExpectedAuth(rawToken string) []byte {
	tok := strings.TrimSpace(rawToken)
	if tok == "" {
		return nil
	}
	out := make([]byte, len(bearerPrefix)+len(tok))
	copy(out, bearerPrefix)
	copy(out[len(bearerPrefix):], tok)
	return out
}

func IsAuthorized(authHeader []byte, expectedAuth []byte) bool {
	if len(expectedAuth) > 0 {
		if bytes.Equal(authHeader, expectedAuth) {
			return true
		}
		if len(authHeader) > len(expectedAuth) && bytes.HasPrefix(authHeader, bearerPrefix) {
			return bytes.Equal(bytes.TrimSpace(authHeader[len(bearerPrefix):]), expectedAuth[len(bearerPrefix):])
		}
		return false
	}
	return len(authHeader) > len(bearerPrefix) && bytes.HasPrefix(authHeader, bearerPrefix) && len(bytes.TrimSpace(authHeader[len(bearerPrefix):])) > 0
}

func parseASCIIPositiveInt(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	v := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		v = v*10 + int(c-'0')
	}
	if v <= 0 {
		return 0, false
	}
	return v, true
}

func skipJSONSpace(b []byte, i int) int {
	for i < len(b) {
		c := b[i]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			i++
			continue
		}
		break
	}
	return i
}

func scanJSONStringFast(b []byte, idx *int) (string, bool) {
	i := *idx
	if i >= len(b) || b[i] != '"' {
		return "", false
	}
	i++
	start := i
	hasEsc := false
	for i < len(b) {
		c := b[i]
		if c == '"' {
			end := i
			i++
			*idx = i
			if !hasEsc {
				return string(b[start:end]), true
			}
			var decBuf [256]byte
			out := decBuf[:0]
			for j := start; j < end; j++ {
				ch := b[j]
				if ch != '\\' {
					out = append(out, ch)
					continue
				}
				j++
				if j >= end {
					return "", false
				}
				switch b[j] {
				case '"', '\\', '/':
					out = append(out, b[j])
				case 'b':
					out = append(out, '\b')
				case 'f':
					out = append(out, '\f')
				case 'n':
					out = append(out, '\n')
				case 'r':
					out = append(out, '\r')
				case 't':
					out = append(out, '\t')
				default:
					return "", false
				}
			}
			return string(out), true
		}
		if c == '\\' {
			hasEsc = true
			i += 2
			continue
		}
		if c < 0x20 {
			return "", false
		}
		i++
	}
	return "", false
}

func parseCreateIssueFast(body []byte) (string, string, bool) {
	i := skipJSONSpace(body, 0)
	if i >= len(body) {
		return "", "", true
	}
	if body[i] != '{' {
		return "", "", false
	}
	i = skipJSONSpace(body, i+1)
	if i < len(body) && body[i] == '}' {
		i = skipJSONSpace(body, i+1)
		return "", "", i == len(body)
	}
	var title, bodyText string
	first := true
	for {
		if !first {
			if i >= len(body) || body[i] != ',' {
				break
			}
			i = skipJSONSpace(body, i+1)
		}
		first = false
		k, ok := scanJSONStringFast(body, &i)
		if !ok {
			return "", "", false
		}
		i = skipJSONSpace(body, i)
		if i >= len(body) || body[i] != ':' {
			return "", "", false
		}
		i = skipJSONSpace(body, i+1)
		val, vOK := scanJSONStringFast(body, &i)
		if !vOK {
			return "", "", false
		}
		switch k {
		case "title":
			title = val
		case "body":
			bodyText = val
		default:
			return "", "", false
		}
		i = skipJSONSpace(body, i)
		if i < len(body) && body[i] == '}' {
			i = skipJSONSpace(body, i+1)
			return title, bodyText, i == len(body)
		}
		if i >= len(body) || body[i] != ',' {
			return "", "", false
		}
	}
	return "", "", false
}

func decodePercent(s string) string {
	if strings.IndexByte(s, '%') < 0 {
		return s
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			hi := unhexByte(s[i+1])
			lo := unhexByte(s[i+2])
			if hi >= 0 && lo >= 0 {
				out = append(out, byte((hi<<4)|lo))
				i += 2
				continue
			}
		}
		out = append(out, s[i])
	}
	return string(out)
}

func unhexByte(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c - 'a' + 10)
	case c >= 'A' && c <= 'F':
		return int(c - 'A' + 10)
	default:
		return -1
	}
}

func queryParam(query, key string) string {
	if query == "" {
		return ""
	}
	for query != "" {
		var pair string
		if amp := strings.IndexByte(query, '&'); amp >= 0 {
			pair = query[:amp]
			query = query[amp+1:]
		} else {
			pair = query
			query = ""
		}
		if eq := strings.IndexByte(pair, '='); eq >= 0 {
			if pair[:eq] == key {
				return decodePercent(pair[eq+1:])
			}
		}
	}
	return ""
}

func jsonFrame(status int, statusText string, val any, extraHeaders string) []byte {
	b, _ := json.Marshal(val)
	return buildHTTPFrameBytes(status, statusText, b, extraHeaders)
}

func storeErrorFrame(err error) []byte {
	if se, ok := err.(*StoreError); ok {
		stText := "Error"
		switch se.Status {
		case 400:
			stText = "Bad Request"
		case 401:
			stText = "Unauthorized"
		case 404:
			stText = "Not Found"
		case 405:
			stText = "Method Not Allowed"
		case 409:
			stText = "Conflict"
		case 422:
			stText = "Unprocessable Entity"
		}
		return jsonFrame(se.Status, stText, map[string]string{"message": se.Message}, "")
	}
	return jsonFrame(500, "Internal Server Error", map[string]string{"message": err.Error()}, "")
}

func parseJSONObject(body []byte) (map[string]json.RawMessage, bool) {
	if len(bytes.TrimSpace(body)) == 0 {
		return map[string]json.RawMessage{}, true
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		return nil, false
	}
	return obj, true
}

func rawString(m map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := m[key]
	if !ok {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

func rawBool(m map[string]json.RawMessage, key string) (bool, bool) {
	raw, ok := m[key]
	if !ok {
		return false, false
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return false, false
	}
	return b, true
}

func rawStringSlice(m map[string]json.RawMessage, key string) ([]string, bool) {
	raw, ok := m[key]
	if !ok {
		return nil, false
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, false
	}
	return list, true
}

func HandleRestAPI(req *RawRequest, store *ForgeStore, expectedToken []byte) ([]byte, bool) {
	if req.Path != "/api/v3" && !strings.HasPrefix(req.Path, "/api/v3/") {
		return nil, false
	}
	if !IsAuthorized(req.AuthHeader, expectedToken) {
		return restUnauthHTTP, true
	}

	rel := "/"
	if len(req.Path) > len("/api/v3") {
		rel = req.Path[len("/api/v3"):]
	}

	if strings.HasPrefix(rel, "/repos/") {
		rest := rel[len("/repos/"):]
		s1 := strings.IndexByte(rest, '/')
		if s1 <= 0 {
			return restNotFoundHTTP, true
		}
		owner := decodePercent(rest[:s1])
		afterOwner := rest[s1+1:]
		s2 := strings.IndexByte(afterOwner, '/')
		if s2 < 0 {
			if afterOwner == "" {
				return restNotFoundHTTP, true
			}
			repoName := decodePercent(afterOwner)
			switch req.Method {
			case "GET":
				repo, err := store.RequireRepo(owner, repoName)
				if err != nil {
					return storeErrorFrame(err), true
				}
				return jsonFrame(200, "OK", repo.JSONView(), ""), true
			case "PATCH":
				obj, ok := parseJSONObject(req.Body)
				if !ok {
					return restMalformedJSON, true
				}
				var descPtr *string
				if d, ok := rawString(obj, "description"); ok {
					descPtr = &d
				}
				var privPtr *bool
				if p, ok := rawBool(obj, "private"); ok {
					privPtr = &p
				}
				var brPtr *string
				if b, ok := rawString(obj, "default_branch"); ok {
					brPtr = &b
				}
				view, err := store.PatchRepo(owner, repoName, descPtr, privPtr, brPtr)
				if err != nil {
					return storeErrorFrame(err), true
				}
				return jsonFrame(200, "OK", view, ""), true
			case "DELETE":
				if err := store.DeleteRepo(owner, repoName); err != nil {
					return storeErrorFrame(err), true
				}
				return restNoContentHTTP, true
			}
			return restNotFoundHTTP, true
		}

		repoName := decodePercent(afterOwner[:s2])
		sub := afterOwner[s2+1:]

		if strings.HasPrefix(sub, "issues/") {
			numPart := sub[len("issues/"):]
			if slash := strings.IndexByte(numPart, '/'); slash < 0 {
				num, okNum := parseASCIIPositiveInt(numPart)
				if !okNum {
					return restNotFoundHTTP, true
				}
				if req.Method == "GET" {
					repo := store.GetRepo(owner, repoName)
					if repo == nil {
						return restNotFoundHTTP, true
					}
					iss := repo.GetIssue(num)
					if iss == nil {
						return restNotFoundHTTP, true
					}
					return iss.HTTP200Frame(), true
				}
				if req.Method == "PATCH" {
					obj, ok := parseJSONObject(req.Body)
					if !ok {
						return restMalformedJSON, true
					}
					var statePtr, titlePtr, bodyPtr *string
					if v, ok := rawString(obj, "state"); ok {
						statePtr = &v
					}
					if v, ok := rawString(obj, "title"); ok {
						titlePtr = &v
					}
					if v, ok := rawString(obj, "body"); ok {
						bodyPtr = &v
					}
					iss, uErr := store.UpdateIssue(owner, repoName, num, statePtr, titlePtr, bodyPtr)
					if uErr != nil {
						return storeErrorFrame(uErr), true
					}
					return iss.HTTP200Frame(), true
				}
				return restNotFoundHTTP, true
			} else {
				numStr := numPart[:slash]
				tail := numPart[slash+1:]
				num, okNum := parseASCIIPositiveInt(numStr)
				if !okNum {
					return restNotFoundHTTP, true
				}
				if tail == "comments" {
					if req.Method == "POST" {
						obj, ok := parseJSONObject(req.Body)
						if !ok {
							return restMalformedJSON, true
						}
						cBody, _ := rawString(obj, "body")
						c, cErr := store.AddComment(owner, repoName, num, cBody)
						if cErr != nil {
							return storeErrorFrame(cErr), true
						}
						return jsonFrame(201, "Created", c, ""), true
					}
					if req.Method == "GET" {
						list, cErr := store.ListComments(owner, repoName, num)
						if cErr != nil {
							return storeErrorFrame(cErr), true
						}
						return jsonFrame(200, "OK", list, ""), true
					}
				}
				return restNotFoundHTTP, true
			}
		}

		if sub == "issues" {
			if req.Method == "POST" {
				if title, bodyText, okFast := parseCreateIssueFast(req.Body); okFast {
					iss, cErr := store.CreateIssue(owner, repoName, title, bodyText, nil, false)
					if cErr != nil {
						return storeErrorFrame(cErr), true
					}
					return iss.http201Frame, true
				}
				var issueReq struct {
					Title  string          `json:"title"`
					Body   string          `json:"body"`
					Labels json.RawMessage `json:"labels"`
				}
				if len(bytes.TrimSpace(req.Body)) > 0 {
					if err := json.Unmarshal(req.Body, &issueReq); err != nil {
						return restMalformedJSON, true
					}
				}
				var labels []string
				hasLabels := len(issueReq.Labels) > 0 && !bytes.Equal(bytes.TrimSpace(issueReq.Labels), []byte("null"))
				if hasLabels {
					if err := json.Unmarshal(issueReq.Labels, &labels); err != nil {
						return storeErrorFrame(&StoreError{Status: 422, Message: "labels must be an array"}), true
					}
				}
				iss, cErr := store.CreateIssue(owner, repoName, issueReq.Title, issueReq.Body, labels, hasLabels)
				if cErr != nil {
					return storeErrorFrame(cErr), true
				}
				return iss.http201Frame, true
			}
			if req.Method == "GET" {
				state := queryParam(req.Query, "state")
				if state == "" {
					state = "open"
				}
				dir := queryParam(req.Query, "direction")
				if dir == "" {
					dir = "desc"
				}
				perPage := 30
				if v, err := strconv.Atoi(queryParam(req.Query, "per_page")); err == nil && v > 0 {
					if v > 100 {
						v = 100
					}
					perPage = v
				}
				page := 1
				if v, err := strconv.Atoi(queryParam(req.Query, "page")); err == nil && v > 0 {
					page = v
				}
				items, lErr := store.ListIssues(owner, repoName, state, dir)
				if lErr != nil {
					return storeErrorFrame(lErr), true
				}
				start := (page - 1) * perPage
				if start > len(items) {
					start = len(items)
				}
				end := start + perPage
				if end > len(items) {
					end = len(items)
				}
				return jsonFrame(200, "OK", items[start:end], ""), true
			}
		}

		if sub == "labels" {
			if req.Method == "POST" {
				obj, ok := parseJSONObject(req.Body)
				if !ok {
					return restMalformedJSON, true
				}
				ln, _ := rawString(obj, "name")
				lc, _ := rawString(obj, "color")
				lbl, lErr := store.CreateLabel(owner, repoName, ln, lc)
				if lErr != nil {
					return storeErrorFrame(lErr), true
				}
				return jsonFrame(201, "Created", lbl, ""), true
			}
			if req.Method == "GET" {
				list, lErr := store.ListLabels(owner, repoName)
				if lErr != nil {
					return storeErrorFrame(lErr), true
				}
				return jsonFrame(200, "OK", list, ""), true
			}
		}

		if strings.HasPrefix(sub, "contents/") || sub == "contents" {
			filePath := ""
			if len(sub) > len("contents/") {
				segs := strings.Split(sub[len("contents/"):], "/")
				for i, s := range segs {
					segs[i] = decodePercent(s)
				}
				filePath = strings.Join(segs, "/")
			}
			switch req.Method {
			case "PUT":
				obj, ok := parseJSONObject(req.Body)
				if !ok {
					return restMalformedJSON, true
				}
				msg, _ := rawString(obj, "message")
				content, hasContent := rawString(obj, "content")
				if !hasContent {
					return storeErrorFrame(&StoreError{Status: 422, Message: "content must be base64 string"}), true
				}
				branch, _ := rawString(obj, "branch")
				wantSHA, _ := rawString(obj, "sha")
				st, out, pErr := store.PutContent(owner, repoName, filePath, msg, content, branch, wantSHA)
				if pErr != nil {
					return storeErrorFrame(pErr), true
				}
				stText := "Created"
				if st == 200 {
					stText = "OK"
				}
				return jsonFrame(st, stText, out, ""), true
			case "GET":
				ref := queryParam(req.Query, "ref")
				out, gErr := store.GetContent(owner, repoName, filePath, ref)
				if gErr != nil {
					return storeErrorFrame(gErr), true
				}
				return jsonFrame(200, "OK", out, ""), true
			case "DELETE":
				obj, ok := parseJSONObject(req.Body)
				if !ok {
					return restMalformedJSON, true
				}
				msg, _ := rawString(obj, "message")
				wantSHA, _ := rawString(obj, "sha")
				branch, _ := rawString(obj, "branch")
				out, dErr := store.DeleteContent(owner, repoName, filePath, msg, wantSHA, branch)
				if dErr != nil {
					return storeErrorFrame(dErr), true
				}
				return jsonFrame(200, "OK", out, ""), true
			}
		}

		if sub == "git/refs" && req.Method == "POST" {
			obj, ok := parseJSONObject(req.Body)
			if !ok {
				return restMalformedJSON, true
			}
			ref, _ := rawString(obj, "ref")
			sha, _ := rawString(obj, "sha")
			out, rErr := store.CreateRef(owner, repoName, ref, sha)
			if rErr != nil {
				return storeErrorFrame(rErr), true
			}
			return jsonFrame(201, "Created", out, ""), true
		}

		if strings.HasPrefix(sub, "git/refs/") && req.Method == "GET" {
			segs := strings.Split(sub[len("git/refs/"):], "/")
			for i, s := range segs {
				segs[i] = decodePercent(s)
			}
			refPath := strings.Join(segs, "/")
			out, rErr := store.GetRef(owner, repoName, refPath)
			if rErr != nil {
				return storeErrorFrame(rErr), true
			}
			return jsonFrame(200, "OK", out, ""), true
		}

		if sub == "branches" && req.Method == "GET" {
			out, bErr := store.ListBranches(owner, repoName)
			if bErr != nil {
				return storeErrorFrame(bErr), true
			}
			return jsonFrame(200, "OK", out, ""), true
		}

		if strings.HasPrefix(sub, "branches/") && req.Method == "GET" {
			segs := strings.Split(sub[len("branches/"):], "/")
			for i, s := range segs {
				segs[i] = decodePercent(s)
			}
			out, bErr := store.GetBranch(owner, repoName, strings.Join(segs, "/"))
			if bErr != nil {
				return storeErrorFrame(bErr), true
			}
			return jsonFrame(200, "OK", out, ""), true
		}

		if sub == "commits" && req.Method == "GET" {
			sha := queryParam(req.Query, "sha")
			out, cErr := store.ListCommits(owner, repoName, sha)
			if cErr != nil {
				return storeErrorFrame(cErr), true
			}
			return jsonFrame(200, "OK", out, ""), true
		}

		if sub == "pulls" {
			if req.Method == "POST" {
				obj, ok := parseJSONObject(req.Body)
				if !ok {
					return restMalformedJSON, true
				}
				title, _ := rawString(obj, "title")
				bodyText, _ := rawString(obj, "body")
				head, _ := rawString(obj, "head")
				base, _ := rawString(obj, "base")
				pr, pErr := store.CreatePull(owner, repoName, title, bodyText, head, base)
				if pErr != nil {
					return storeErrorFrame(pErr), true
				}
				return jsonFrame(201, "Created", pr, ""), true
			}
			if req.Method == "GET" {
				state := queryParam(req.Query, "state")
				if state == "" {
					state = "open"
				}
				perPage := 30
				if v, err := strconv.Atoi(queryParam(req.Query, "per_page")); err == nil && v > 0 {
					if v > 100 {
						v = 100
					}
					perPage = v
				}
				page := 1
				if v, err := strconv.Atoi(queryParam(req.Query, "page")); err == nil && v > 0 {
					page = v
				}
				items, pErr := store.ListPulls(owner, repoName, state)
				if pErr != nil {
					return storeErrorFrame(pErr), true
				}
				start := (page - 1) * perPage
				if start > len(items) {
					start = len(items)
				}
				end := start + perPage
				if end > len(items) {
					end = len(items)
				}
				return jsonFrame(200, "OK", items[start:end], ""), true
			}
		}

		if strings.HasPrefix(sub, "pulls/") {
			rem := sub[len("pulls/"):]
			if slash := strings.IndexByte(rem, '/'); slash < 0 {
				num, err := strconv.Atoi(rem)
				if err != nil || num <= 0 {
					return restNotFoundHTTP, true
				}
				if req.Method == "GET" {
					pr, pErr := store.GetPull(owner, repoName, num)
					if pErr != nil {
						return storeErrorFrame(pErr), true
					}
					return jsonFrame(200, "OK", pr, ""), true
				}
			} else {
				numStr := rem[:slash]
				tail := rem[slash+1:]
				num, err := strconv.Atoi(numStr)
				if err != nil || num <= 0 {
					return restNotFoundHTTP, true
				}
				if tail == "files" && req.Method == "GET" {
					files, fErr := store.GetPullFiles(owner, repoName, num)
					if fErr != nil {
						return storeErrorFrame(fErr), true
					}
					return jsonFrame(200, "OK", files, ""), true
				}
				if tail == "merge" && req.Method == "PUT" {
					obj, ok := parseJSONObject(req.Body)
					if !ok {
						return restMalformedJSON, true
					}
					method, _ := rawString(obj, "merge_method")
					if method == "" {
						method = "squash"
					}
					out, mErr := store.MergePull(owner, repoName, num, method)
					if mErr != nil {
						return storeErrorFrame(mErr), true
					}
					return jsonFrame(200, "OK", out, ""), true
				}
			}
		}

		return restNotFoundHTTP, true
	}

	if req.Method == "GET" && rel == "/meta" {
		return restMetaHTTP, true
	}

	if req.Method == "GET" && rel == "/" {
		host := req.Host
		if host == "" {
			host = "127.0.0.1"
		}
		payload := map[string]string{
			"current_user_url": "http://" + host + "/api/v3/user",
			"rate_limit_url":   "http://" + host + "/api/v3/rate_limit",
		}
		return jsonFrame(200, "OK", payload, "X-OAuth-Scopes: repo, read:org\r\n"), true
	}

	if req.Method == "GET" && rel == "/user" {
		return jsonFrame(200, "OK", store.Viewer, ""), true
	}

	if rel == "/user/repos" {
		if req.Method == "POST" {
			obj, ok := parseJSONObject(req.Body)
			if !ok {
				return restMalformedJSON, true
			}
			name, _ := rawString(obj, "name")
			desc, _ := rawString(obj, "description")
			priv, _ := rawBool(obj, "private")
			repo, err := store.CreateRepo(name, desc, priv)
			if err != nil {
				return storeErrorFrame(err), true
			}
			return jsonFrame(201, "Created", repo.JSONView(), ""), true
		}
		if req.Method == "GET" {
			perPage := 30
			if v, err := strconv.Atoi(queryParam(req.Query, "per_page")); err == nil && v > 0 {
				if v > 100 {
					v = 100
				}
				perPage = v
			}
			page := 1
			if v, err := strconv.Atoi(queryParam(req.Query, "page")); err == nil && v > 0 {
				page = v
			}
			all := store.ListViewerRepos()
			start := (page - 1) * perPage
			if start > len(all) {
				start = len(all)
			}
			end := start + perPage
			if end > len(all) {
				end = len(all)
			}
			slice := all[start:end]
			extra := ""
			if end < len(all) {
				host := req.Host
				if host == "" {
					host = "127.0.0.1"
				}
				extra = "Link: <http://" + host + "/api/v3/user/repos?per_page=" + strconv.Itoa(perPage) + "&page=" + strconv.Itoa(page+1) + ">; rel=\"next\"\r\n"
			}
			return jsonFrame(200, "OK", slice, extra), true
		}
	}

	return restNotFoundHTTP, true
}
