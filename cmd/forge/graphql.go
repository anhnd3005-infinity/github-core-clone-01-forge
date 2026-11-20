package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type gqlTokenKind int

const (
	tokName gqlTokenKind = iota
	tokInt
	tokFloat
	tokString
	tokPunct
	tokSpread
)

type gqlToken struct {
	kind gqlTokenKind
	val  string
}

func tokenizeGql(src string) ([]gqlToken, error) {
	tokens := make([]gqlToken, 0, 64)
	i := 0
	n := len(src)
	for i < n {
		c := src[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' || (i+2 < n && src[i] == 0xef && src[i+1] == 0xbb && src[i+2] == 0xbf) {
			if src[i] == 0xef {
				i += 3
			} else {
				i++
			}
			continue
		}
		if c == '#' {
			for i < n && src[i] != '\n' && src[i] != '\r' {
				i++
			}
			continue
		}
		if c == '.' && i+2 < n && src[i:i+3] == "..." {
			tokens = append(tokens, gqlToken{kind: tokSpread, val: "..."})
			i += 3
			continue
		}
		if strings.IndexByte("{}():=@$&![]|", c) >= 0 {
			tokens = append(tokens, gqlToken{kind: tokPunct, val: src[i : i+1]})
			i++
			continue
		}
		if c == '"' {
			if i+2 < n && src[i:i+3] == `"""` {
				i += 3
				var b strings.Builder
				for i < n && !(i+2 < n && src[i:i+3] == `"""`) {
					if src[i] == '\\' && i+3 < n && src[i:i+4] == `\"""` {
						b.WriteString(`"""`)
						i += 4
					} else {
						b.WriteByte(src[i])
						i++
					}
				}
				if !(i+2 < n && src[i:i+3] == `"""`) {
					return nil, fmt.Errorf("unterminated block string")
				}
				i += 3
				tokens = append(tokens, gqlToken{kind: tokString, val: b.String()})
				continue
			}
			i++
			var b strings.Builder
			for i < n && src[i] != '"' {
				if src[i] == '\\' {
					i++
					if i >= n {
						return nil, fmt.Errorf("unterminated escape")
					}
					esc := src[i]
					i++
					switch esc {
					case 'n':
						b.WriteByte('\n')
					case 'r':
						b.WriteByte('\r')
					case 't':
						b.WriteByte('\t')
					case 'b':
						b.WriteByte('\b')
					case 'f':
						b.WriteByte('\f')
					case 'u':
						if i+4 <= n {
							if cp, err := strconv.ParseInt(src[i:i+4], 16, 32); err == nil {
								b.WriteRune(rune(cp))
							}
							i += 4
						}
					default:
						b.WriteByte(esc)
					}
				} else {
					b.WriteByte(src[i])
					i++
				}
			}
			if i >= n || src[i] != '"' {
				return nil, fmt.Errorf("unterminated string")
			}
			i++
			tokens = append(tokens, gqlToken{kind: tokString, val: b.String()})
			continue
		}
		if c == '-' || (c >= '0' && c <= '9') {
			start := i
			if src[i] == '-' {
				i++
			}
			for i < n && src[i] >= '0' && src[i] <= '9' {
				i++
			}
			isFloat := false
			if i < n && (src[i] == '.' || src[i] == 'e' || src[i] == 'E') {
				isFloat = true
				if src[i] == '.' {
					i++
					for i < n && src[i] >= '0' && src[i] <= '9' {
						i++
					}
				}
				if i < n && (src[i] == 'e' || src[i] == 'E') {
					i++
					if i < n && (src[i] == '+' || src[i] == '-') {
						i++
					}
					for i < n && src[i] >= '0' && src[i] <= '9' {
						i++
					}
				}
			}
			kind := tokInt
			if isFloat {
				kind = tokFloat
			}
			tokens = append(tokens, gqlToken{kind: kind, val: src[start:i]})
			continue
		}
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_' {
			start := i
			i++
			for i < n {
				ch := src[i]
				if (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '_' {
					i++
				} else {
					break
				}
			}
			tokens = append(tokens, gqlToken{kind: tokName, val: src[start:i]})
			continue
		}
		return nil, fmt.Errorf("unexpected char %q", c)
	}
	return tokens, nil
}

type gqlArg struct {
	name string
	val  gqlVal
}

type gqlValKind int

const (
	valVar gqlValKind = iota
	valInt
	valFloat
	valStr
	valBool
	valNull
	valEnum
	valList
	valObj
)

type gqlVal struct {
	kind   gqlValKind
	str    string
	num    float64
	intVal int
	boolV  bool
	items  []gqlVal
	fields map[string]gqlVal
}

type gqlSel struct {
	kind   byte // 'F'=field, 'I'=inlineFragment, 'S'=fragmentSpread
	name   string
	alias  string
	typeC  string
	args   []gqlArg
	subSet []gqlSel
}

type gqlOp struct {
	opType string
	name   string
	selSet []gqlSel
}

type gqlParser struct {
	toks []gqlToken
	pos  int
}

func (p *gqlParser) peek() (gqlToken, bool) {
	if p.pos >= len(p.toks) {
		return gqlToken{}, false
	}
	return p.toks[p.pos], true
}

func (p *gqlParser) next() (gqlToken, error) {
	if p.pos >= len(p.toks) {
		return gqlToken{}, fmt.Errorf("unexpected EOF")
	}
	t := p.toks[p.pos]
	p.pos++
	return t, nil
}

func (p *gqlParser) expectPunct(v string) error {
	t, err := p.next()
	if err != nil || t.kind != tokPunct || t.val != v {
		return fmt.Errorf("expected %q", v)
	}
	return nil
}

func (p *gqlParser) peekPunct(v string) bool {
	t, ok := p.peek()
	return ok && t.kind == tokPunct && t.val == v
}

func (p *gqlParser) skipTypeRef() error {
	if p.peekPunct("[") {
		p.next()
		if err := p.skipTypeRef(); err != nil {
			return err
		}
		if err := p.expectPunct("]"); err != nil {
			return err
		}
	} else {
		if _, err := p.next(); err != nil {
			return err
		}
	}
	for p.peekPunct("!") {
		p.next()
	}
	return nil
}

func (p *gqlParser) parseValue() (gqlVal, error) {
	t, err := p.next()
	if err != nil {
		return gqlVal{}, err
	}
	if t.kind == tokPunct && t.val == "$" {
		nt, err := p.next()
		if err != nil {
			return gqlVal{}, err
		}
		return gqlVal{kind: valVar, str: nt.val}, nil
	}
	if t.kind == tokInt {
		v, _ := strconv.Atoi(t.val)
		return gqlVal{kind: valInt, intVal: v}, nil
	}
	if t.kind == tokFloat {
		f, _ := strconv.ParseFloat(t.val, 64)
		return gqlVal{kind: valFloat, num: f}, nil
	}
	if t.kind == tokString {
		return gqlVal{kind: valStr, str: t.val}, nil
	}
	if t.kind == tokName {
		switch t.val {
		case "true":
			return gqlVal{kind: valBool, boolV: true}, nil
		case "false":
			return gqlVal{kind: valBool, boolV: false}, nil
		case "null":
			return gqlVal{kind: valNull}, nil
		default:
			return gqlVal{kind: valEnum, str: t.val}, nil
		}
	}
	if t.kind == tokPunct && t.val == "[" {
		var items []gqlVal
		for !p.peekPunct("]") {
			item, err := p.parseValue()
			if err != nil {
				return gqlVal{}, err
			}
			items = append(items, item)
		}
		p.expectPunct("]")
		return gqlVal{kind: valList, items: items}, nil
	}
	if t.kind == tokPunct && t.val == "{" {
		fields := make(map[string]gqlVal)
		for !p.peekPunct("}") {
			kt, err := p.next()
			if err != nil {
				return gqlVal{}, err
			}
			if err := p.expectPunct(":"); err != nil {
				return gqlVal{}, err
			}
			val, err := p.parseValue()
			if err != nil {
				return gqlVal{}, err
			}
			fields[kt.val] = val
		}
		p.expectPunct("}")
		return gqlVal{kind: valObj, fields: fields}, nil
	}
	return gqlVal{}, fmt.Errorf("unexpected value token %q", t.val)
}

func (p *gqlParser) parseArgs() ([]gqlArg, error) {
	if !p.peekPunct("(") {
		return nil, nil
	}
	p.next()
	var out []gqlArg
	for !p.peekPunct(")") {
		nt, err := p.next()
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct(":"); err != nil {
			return nil, err
		}
		val, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		out = append(out, gqlArg{name: nt.val, val: val})
	}
	p.expectPunct(")")
	return out, nil
}

func (p *gqlParser) skipDirectives() error {
	for p.peekPunct("@") {
		p.next()
		if _, err := p.next(); err != nil {
			return err
		}
		if _, err := p.parseArgs(); err != nil {
			return err
		}
	}
	return nil
}

func (p *gqlParser) parseSelectionSet() ([]gqlSel, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	out := make([]gqlSel, 0, 8)
	for !p.peekPunct("}") {
		t, ok := p.peek()
		if !ok {
			return nil, fmt.Errorf("unterminated selection set")
		}
		if t.kind == tokSpread {
			p.next()
			after, ok2 := p.peek()
			if !ok2 {
				return nil, fmt.Errorf("unexpected EOF after spread")
			}
			if after.kind == tokName && after.val == "on" {
				p.next()
				tc, err := p.next()
				if err != nil {
					return nil, err
				}
				if err := p.skipDirectives(); err != nil {
					return nil, err
				}
				sub, err := p.parseSelectionSet()
				if err != nil {
					return nil, err
				}
				out = append(out, gqlSel{kind: 'I', typeC: tc.val, subSet: sub})
			} else if after.kind == tokPunct && after.val == "{" {
				sub, err := p.parseSelectionSet()
				if err != nil {
					return nil, err
				}
				out = append(out, gqlSel{kind: 'I', subSet: sub})
			} else {
				fn, err := p.next()
				if err != nil {
					return nil, err
				}
				if err := p.skipDirectives(); err != nil {
					return nil, err
				}
				out = append(out, gqlSel{kind: 'S', name: fn.val})
			}
			continue
		}
		ft, err := p.next()
		if err != nil {
			return nil, err
		}
		name := ft.val
		alias := ""
		if p.peekPunct(":") {
			p.next()
			nt, err := p.next()
			if err != nil {
				return nil, err
			}
			alias = name
			name = nt.val
		}
		args, err := p.parseArgs()
		if err != nil {
			return nil, err
		}
		if err := p.skipDirectives(); err != nil {
			return nil, err
		}
		var sub []gqlSel
		if p.peekPunct("{") {
			sub, err = p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
		}
		out = append(out, gqlSel{kind: 'F', name: name, alias: alias, args: args, subSet: sub})
	}
	p.expectPunct("}")
	return out, nil
}

func parseGqlDocument(toks []gqlToken) ([]gqlOp, map[string]struct {
	typeC  string
	selSet []gqlSel
}, error) {
	p := &gqlParser{toks: toks}
	var ops []gqlOp
	frags := make(map[string]struct {
		typeC  string
		selSet []gqlSel
	})

	for p.pos < len(p.toks) {
		t, _ := p.peek()
		if t.kind == tokPunct && t.val == "{" {
			sub, err := p.parseSelectionSet()
			if err != nil {
				return nil, nil, err
			}
			ops = append(ops, gqlOp{opType: "query", selSet: sub})
			continue
		}
		if t.kind == tokName && t.val == "fragment" {
			p.next()
			fn, err := p.next()
			if err != nil {
				return nil, nil, err
			}
			onTok, err := p.next()
			if err != nil || onTok.val != "on" {
				return nil, nil, fmt.Errorf("expected on")
			}
			tc, err := p.next()
			if err != nil {
				return nil, nil, err
			}
			if err := p.skipDirectives(); err != nil {
				return nil, nil, err
			}
			sub, err := p.parseSelectionSet()
			if err != nil {
				return nil, nil, err
			}
			frags[fn.val] = struct {
				typeC  string
				selSet []gqlSel
			}{typeC: tc.val, selSet: sub}
			continue
		}
		if t.kind == tokName && (t.val == "query" || t.val == "mutation" || t.val == "subscription") {
			opT := "query"
			if t.val == "mutation" {
				opT = "mutation"
			}
			p.next()
			opName := ""
			if nt, ok := p.peek(); ok && nt.kind == tokName {
				opName = nt.val
				p.next()
			}
			if p.peekPunct("(") {
				p.next()
				for !p.peekPunct(")") {
					if err := p.expectPunct("$"); err != nil {
						return nil, nil, err
					}
					if _, err := p.next(); err != nil {
						return nil, nil, err
					}
					if err := p.expectPunct(":"); err != nil {
						return nil, nil, err
					}
					if err := p.skipTypeRef(); err != nil {
						return nil, nil, err
					}
					if p.peekPunct("=") {
						p.next()
						if _, err := p.parseValue(); err != nil {
							return nil, nil, err
						}
					}
				}
				p.expectPunct(")")
			}
			if err := p.skipDirectives(); err != nil {
				return nil, nil, err
			}
			sub, err := p.parseSelectionSet()
			if err != nil {
				return nil, nil, err
			}
			ops = append(ops, gqlOp{opType: opT, name: opName, selSet: sub})
			continue
		}
		return nil, nil, fmt.Errorf("unexpected token %q", t.val)
	}
	return ops, frags, nil
}

var gqlSchemaFields = map[string]map[string]bool{
	"Query":                            {"viewer": true, "repository": true, "node": true, "__type": true},
	"Mutation":                         {"createIssue": true, "closeIssue": true, "createPullRequest": true, "mergePullRequest": true, "addComment": true},
	"User":                             {"login": true, "name": true, "id": true},
	"Ref":                              {"name": true},
	"PageInfo":                         {"hasNextPage": true, "endCursor": true},
	"Issue":                            {"id": true, "number": true, "title": true, "state": true, "body": true, "databaseId": true, "createdAt": true},
	"IssueConnection":                  {"nodes": true, "pageInfo": true},
	"PullRequestChangedFile":           {"path": true},
	"PullRequestChangedFileConnection": {"nodes": true},
	"PullRequest":                      {"id": true, "number": true, "headRefName": true, "baseRefName": true, "mergeable": true, "state": true, "merged": true, "files": true, "title": true, "body": true},
	"PullRequestConnection":            {"nodes": true},
	"Repository":                       {"id": true, "databaseId": true, "name": true, "nameWithOwner": true, "owner": true, "defaultBranchRef": true, "viewerPermission": true, "hasIssuesEnabled": true, "issue": true, "issues": true, "pullRequests": true, "pullRequest": true, "description": true, "isPrivate": true},
	"__Type":                           {"name": true, "fields": true, "enumValues": true},
	"__Field":                          {"name": true},
	"__EnumValue":                      {"name": true},
	"CreateIssuePayload":               {"issue": true},
	"CloseIssuePayload":                {"issue": true},
	"CreatePullRequestPayload":         {"pullRequest": true},
	"MergePullRequestPayload":          {"pullRequest": true},
	"Comment":                          {"id": true, "body": true},
	"CommentEdge":                      {"node": true},
	"AddCommentPayload":                {"commentEdge": true},
}

var gqlSchemaLists = map[string][]string{
	"Issue":       {"id", "number", "title", "state", "body", "databaseId", "createdAt"},
	"PullRequest": {"id", "number", "headRefName", "baseRefName", "mergeable", "state", "merged", "files", "title", "body"},
	"Repository":  {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "isPrivate"},
}

var gqlEnumLists = map[string][]string{
	"SearchType":             {"ISSUE", "REPOSITORY", "USER", "DISCUSSION"},
	"IssueState":             {"OPEN", "CLOSED"},
	"PullRequestState":       {"OPEN", "CLOSED", "MERGED"},
	"PullRequestMergeMethod": {"MERGE", "SQUASH", "REBASE"},
	"MergeableState":         {"MERGEABLE", "CONFLICTING", "UNKNOWN"},
	"RepositoryPermission":   {"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"},
	"IssueOrderField":        {"CREATED_AT", "UPDATED_AT", "COMMENTS"},
	"OrderDirection":         {"ASC", "DESC"},
}

type gqlExecCtx struct {
	store  *ForgeStore
	vars   map[string]any
	frags  map[string]struct {
		typeC  string
		selSet []gqlSel
	}
	errors []map[string]string
}

func resolveGqlVal(v gqlVal, vars map[string]any) any {
	switch v.kind {
	case valVar:
		return vars[v.str]
	case valInt:
		return v.intVal
	case valFloat:
		return v.num
	case valStr, valEnum:
		return v.str
	case valBool:
		return v.boolV
	case valNull:
		return nil
	case valList:
		out := make([]any, len(v.items))
		for i, item := range v.items {
			out[i] = resolveGqlVal(item, vars)
		}
		return out
	case valObj:
		out := make(map[string]any, len(v.fields))
		for k, fv := range v.fields {
			out[k] = resolveGqlVal(fv, vars)
		}
		return out
	}
	return nil
}

func evalArgs(args []gqlArg, vars map[string]any) map[string]any {
	out := make(map[string]any, len(args))
	for _, a := range args {
		out[a.name] = resolveGqlVal(a.val, vars)
	}
	return out
}

func collectGqlFields(sels []gqlSel, typeName string, frags map[string]struct {
	typeC  string
	selSet []gqlSel
}) []gqlSel {
	var out []gqlSel
	for _, s := range sels {
		if s.kind == 'F' {
			out = append(out, s)
		} else if s.kind == 'I' {
			if s.typeC == "" || s.typeC == typeName {
				out = append(out, collectGqlFields(s.subSet, typeName, frags)...)
			}
		} else if s.kind == 'S' {
			if fr, ok := frags[s.name]; ok && fr.typeC == typeName {
				out = append(out, collectGqlFields(fr.selSet, typeName, frags)...)
			}
		}
	}
	return out
}

func selectGqlObject(
	typeName string,
	selSet []gqlSel,
	ctx *gqlExecCtx,
	resolveField func(name string, args map[string]any, fieldSel gqlSel) any,
) map[string]any {
	res := make(map[string]any)
	if len(selSet) == 0 {
		return res
	}
	allowed := gqlSchemaFields[typeName]
	fields := collectGqlFields(selSet, typeName, ctx.frags)
	for _, f := range fields {
		key := f.name
		if f.alias != "" {
			key = f.alias
		}
		if f.name == "__typename" {
			res[key] = typeName
			continue
		}
		if allowed != nil && !allowed[f.name] {
			ctx.errors = append(ctx.errors, map[string]string{
				"message": fmt.Sprintf("Field '%s' doesn't exist on type '%s'", f.name, typeName),
			})
			continue
		}
		args := evalArgs(f.args, ctx.vars)
		res[key] = resolveField(f.name, args, f)
	}
	return res
}

func asIntArg(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case float64:
		return int(x), true
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return int(i), true
		}
	}
	return 0, false
}

func asStringArg(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func encodeGqlCursor(idx int) string {
	return base64.RawURLEncoding.EncodeToString([]byte("idx:" + strconv.Itoa(idx)))
}

func decodeGqlCursor(cur string) int {
	b, err := base64.RawURLEncoding.DecodeString(cur)
	if err != nil {
		return -1
	}
	s := string(b)
	if !strings.HasPrefix(s, "idx:") {
		return -1
	}
	n, err := strconv.Atoi(s[4:])
	if err != nil || n < 0 {
		return -1
	}
	return n
}

func projectGqlIssue(iss *IssueRecord, selSet []gqlSel, ctx *gqlExecCtx) map[string]any {
	return selectGqlObject("Issue", selSet, ctx, func(name string, _ map[string]any, _ gqlSel) any {
		switch name {
		case "id":
			return iss.NodeID
		case "databaseId":
			return iss.ID
		case "number":
			return iss.Number
		case "title":
			return iss.Title
		case "body":
			return iss.Body
		case "state":
			return strings.ToUpper(iss.State)
		case "createdAt":
			return iss.CreatedAt
		}
		return nil
	})
}

func projectGqlPull(repo *RepoRecord, pr *PullRecord, selSet []gqlSel, ctx *gqlExecCtx) map[string]any {
	return selectGqlObject("PullRequest", selSet, ctx, func(name string, args map[string]any, fieldSel gqlSel) any {
		switch name {
		case "id":
			return pr.NodeID
		case "number":
			return pr.Number
		case "title":
			return pr.Title
		case "body":
			return pr.Body
		case "headRefName":
			return pr.headRef
		case "baseRefName":
			return pr.baseRef
		case "merged":
			return pr.Merged
		case "state":
			if pr.Merged {
				return "MERGED"
			}
			return strings.ToUpper(pr.State)
		case "mergeable":
			if ctx.store.IsPullMergeable(repo, pr) {
				return "MERGEABLE"
			}
			return "CONFLICTING"
		case "files":
			first := 100
			if f, ok := asIntArg(args["first"]); ok && f >= 0 {
				first = f
			}
			files, _ := ctx.store.GetPullFiles(repo.Owner, repo.Name, pr.Number)
			if first < len(files) {
				files = files[:first]
			}
			return selectGqlObject("PullRequestChangedFileConnection", fieldSel.subSet, ctx, func(cf string, _ map[string]any, cSel gqlSel) any {
				if cf == "nodes" {
					nodes := make([]map[string]any, len(files))
					for i, f := range files {
						fn := f.Filename
						nodes[i] = selectGqlObject("PullRequestChangedFile", cSel.subSet, ctx, func(n string, _ map[string]any, _ gqlSel) any {
							if n == "path" {
								return fn
							}
							return nil
						})
					}
					return nodes
				}
				return nil
			})
		}
		return nil
	})
}

func projectGqlRepo(repo *RepoRecord, selSet []gqlSel, ctx *gqlExecCtx) map[string]any {
	return selectGqlObject("Repository", selSet, ctx, func(name string, args map[string]any, fieldSel gqlSel) any {
		switch name {
		case "id":
			return repo.GlobalID
		case "databaseId":
			return repo.ID
		case "name":
			return repo.Name
		case "nameWithOwner":
			return repo.FullName
		case "description":
			return repo.Description
		case "isPrivate":
			return repo.Private
		case "viewerPermission":
			return "ADMIN"
		case "hasIssuesEnabled":
			return true
		case "owner":
			return selectGqlObject("User", fieldSel.subSet, ctx, func(uf string, _ map[string]any, _ gqlSel) any {
				switch uf {
				case "login":
					return repo.Owner
				case "name":
					return ctx.store.Viewer.Name
				case "id":
					return ctx.store.Viewer.ID
				}
				return nil
			})
		case "defaultBranchRef":
			return selectGqlObject("Ref", fieldSel.subSet, ctx, func(rf string, _ map[string]any, _ gqlSel) any {
				if rf == "name" {
					return repo.DefaultBranch
				}
				return nil
			})
		case "issue":
			num, _ := asIntArg(args["number"])
			found := repo.GetIssue(num)
			if found == nil {
				return nil
			}
			return projectGqlIssue(found, fieldSel.subSet, ctx)
		case "issues":
			items := make([]*IssueRecord, len(repo.Issues))
			copy(items, repo.Issues)

			if rawStates, ok := args["states"].([]any); ok && len(rawStates) > 0 {
				wanted := make(map[string]bool, len(rawStates))
				for _, s := range rawStates {
					wanted[strings.ToUpper(asStringArg(s))] = true
				}
				filtered := make([]*IssueRecord, 0, len(items))
				for _, it := range items {
					if wanted[strings.ToUpper(it.State)] {
						filtered = append(filtered, it)
					}
				}
				items = filtered
			}
			if ob, ok := args["orderBy"].(map[string]any); ok {
				desc := strings.EqualFold(asStringArg(ob["direction"]), "DESC")
				sort.Slice(items, func(i, j int) bool {
					if desc {
						if items[i].Seq != items[j].Seq {
							return items[i].Seq > items[j].Seq
						}
						return items[i].Number > items[j].Number
					}
					if items[i].Seq != items[j].Seq {
						return items[i].Seq < items[j].Seq
					}
					return items[i].Number < items[j].Number
				})
			} else {
				sort.Slice(items, func(i, j int) bool {
					return items[i].Number < items[j].Number
				})
			}

			startIdx := 0
			if after := asStringArg(args["after"]); after != "" {
				if c := decodeGqlCursor(after); c >= 0 {
					startIdx = c + 1
				}
			}
			if startIdx > len(items) {
				startIdx = len(items)
			}
			first := len(items) - startIdx
			if f, ok := asIntArg(args["first"]); ok && f >= 0 && f < first {
				first = f
			}
			pageSlice := items[startIdx : startIdx+first]
			hasNext := startIdx+len(pageSlice) < len(items)
			var endCursor any
			if len(pageSlice) > 0 {
				endCursor = encodeGqlCursor(startIdx + len(pageSlice) - 1)
			}

			return selectGqlObject("IssueConnection", fieldSel.subSet, ctx, func(cf string, _ map[string]any, cSel gqlSel) any {
				if cf == "nodes" {
					nodes := make([]map[string]any, len(pageSlice))
					for i, it := range pageSlice {
						nodes[i] = projectGqlIssue(it, cSel.subSet, ctx)
					}
					return nodes
				}
				if cf == "pageInfo" {
					return selectGqlObject("PageInfo", cSel.subSet, ctx, func(pf string, _ map[string]any, _ gqlSel) any {
						if pf == "hasNextPage" {
							return hasNext
						}
						if pf == "endCursor" {
							return endCursor
						}
						return nil
					})
				}
				return nil
			})
		case "pullRequest":
			num, _ := asIntArg(args["number"])
			found := repo.PullsByNumber[num]
			if found == nil {
				return nil
			}
			cp := *found
			return projectGqlPull(repo, &cp, fieldSel.subSet, ctx)
		case "pullRequests":
			total := len(repo.Pulls)
			first := total
			if f, ok := asIntArg(args["first"]); ok && f >= 0 && f < first {
				first = f
			}
			snaps := make([]*PullRecord, first)
			for i := 0; i < first; i++ {
				cp := *repo.Pulls[i]
				snaps[i] = &cp
			}
			return selectGqlObject("PullRequestConnection", fieldSel.subSet, ctx, func(cf string, _ map[string]any, cSel gqlSel) any {
				if cf == "nodes" {
					nodes := make([]map[string]any, len(snaps))
					for i, pr := range snaps {
						nodes[i] = projectGqlPull(repo, pr, cSel.subSet, ctx)
					}
					return nodes
				}
				return nil
			})
		}
		return nil
	})
}

type gqlRequestDoc struct {
	Query         string         `json:"query"`
	Variables     map[string]any `json:"variables"`
	OperationName string         `json:"operationName"`
}

func executeGqlDoc(store *ForgeStore, doc gqlRequestDoc) map[string]any {
	if strings.TrimSpace(doc.Query) == "" {
		return map[string]any{
			"data":   nil,
			"errors": []map[string]string{{"message": "Missing query string"}},
		}
	}
	toks, err := tokenizeGql(doc.Query)
	if err != nil {
		return map[string]any{
			"data":   nil,
			"errors": []map[string]string{{"message": err.Error()}},
		}
	}
	ops, frags, err := parseGqlDocument(toks)
	if err != nil {
		return map[string]any{
			"data":   nil,
			"errors": []map[string]string{{"message": err.Error()}},
		}
	}
	if len(ops) == 0 {
		return map[string]any{
			"data":   nil,
			"errors": []map[string]string{{"message": "Document contains no operations"}},
		}
	}
	op := ops[0]
	if doc.OperationName != "" {
		for _, candidate := range ops {
			if candidate.name == doc.OperationName {
				op = candidate
				break
			}
		}
	}

	vars := doc.Variables
	if vars == nil {
		vars = map[string]any{}
	}
	ctx := &gqlExecCtx{store: store, vars: vars, frags: frags}

	var data map[string]any
	if op.opType == "mutation" {
		data = selectGqlObject("Mutation", op.selSet, ctx, func(name string, args map[string]any, fieldSel gqlSel) any {
			input, _ := args["input"].(map[string]any)
			if input == nil {
				input = map[string]any{}
			}
			switch name {
			case "createIssue":
				repo := store.GetRepoByGlobalID(asStringArg(input["repositoryId"]))
				if repo == nil {
					ctx.errors = append(ctx.errors, map[string]string{"message": "Could not resolve repository id"})
					return nil
				}
				iss, cErr := store.CreateIssue(repo.Owner, repo.Name, asStringArg(input["title"]), asStringArg(input["body"]), nil, false)
				if cErr != nil {
					ctx.errors = append(ctx.errors, map[string]string{"message": cErr.Error()})
					return nil
				}
				return selectGqlObject("CreateIssuePayload", fieldSel.subSet, ctx, func(pf string, _ map[string]any, pSel gqlSel) any {
					if pf == "issue" {
						return projectGqlIssue(iss, pSel.subSet, ctx)
					}
					return nil
				})
			case "closeIssue":
				repo, iss := store.GetIssueByGlobalID(asStringArg(input["issueId"]))
				if repo == nil || iss == nil {
					ctx.errors = append(ctx.errors, map[string]string{"message": "Could not resolve issue id"})
					return nil
				}
				closed := "closed"
				updated, uErr := store.UpdateIssue(repo.Owner, repo.Name, iss.Number, &closed, nil, nil)
				if uErr != nil {
					ctx.errors = append(ctx.errors, map[string]string{"message": uErr.Error()})
					return nil
				}
				return selectGqlObject("CloseIssuePayload", fieldSel.subSet, ctx, func(pf string, _ map[string]any, pSel gqlSel) any {
					if pf == "issue" {
						return projectGqlIssue(updated, pSel.subSet, ctx)
					}
					return nil
				})
			case "createPullRequest":
				repo := store.GetRepoByGlobalID(asStringArg(input["repositoryId"]))
				if repo == nil {
					ctx.errors = append(ctx.errors, map[string]string{"message": "Could not resolve repository id"})
					return nil
				}
				pr, pErr := store.CreatePull(repo.Owner, repo.Name, asStringArg(input["title"]), asStringArg(input["body"]), asStringArg(input["headRefName"]), asStringArg(input["baseRefName"]))
				if pErr != nil {
					ctx.errors = append(ctx.errors, map[string]string{"message": pErr.Error()})
					return nil
				}
				return selectGqlObject("CreatePullRequestPayload", fieldSel.subSet, ctx, func(pf string, _ map[string]any, pSel gqlSel) any {
					if pf == "pullRequest" {
						return projectGqlPull(repo, pr, pSel.subSet, ctx)
					}
					return nil
				})
			case "mergePullRequest":
				repo, pr := store.GetPullByGlobalID(asStringArg(input["pullRequestId"]))
				if repo == nil || pr == nil {
					ctx.errors = append(ctx.errors, map[string]string{"message": "Could not resolve pull request id"})
					return nil
				}
				method := asStringArg(input["mergeMethod"])
				if method == "" {
					method = "SQUASH"
				}
				if _, mErr := store.MergePull(repo.Owner, repo.Name, pr.Number, method); mErr != nil {
					ctx.errors = append(ctx.errors, map[string]string{"message": mErr.Error()})
					return nil
				}
				snap, _ := store.GetPull(repo.Owner, repo.Name, pr.Number)
				return selectGqlObject("MergePullRequestPayload", fieldSel.subSet, ctx, func(pf string, _ map[string]any, pSel gqlSel) any {
					if pf == "pullRequest" {
						return projectGqlPull(repo, snap, pSel.subSet, ctx)
					}
					return nil
				})
			case "addComment":
				sid := asStringArg(input["subjectId"])
				repo, iss := store.GetIssueByGlobalID(sid)
				var num int
				if repo != nil && iss != nil {
					num = iss.Number
				} else {
					var pr *PullRecord
					repo, pr = store.GetPullByGlobalID(sid)
					if repo != nil && pr != nil {
						num = pr.Number
					}
				}
				if repo == nil {
					ctx.errors = append(ctx.errors, map[string]string{"message": "Could not resolve subject id"})
					return nil
				}
				cm, cErr := store.AddComment(repo.Owner, repo.Name, num, asStringArg(input["body"]))
				if cErr != nil {
					ctx.errors = append(ctx.errors, map[string]string{"message": cErr.Error()})
					return nil
				}
				return selectGqlObject("AddCommentPayload", fieldSel.subSet, ctx, func(pf string, _ map[string]any, pSel gqlSel) any {
					if pf != "commentEdge" {
						return nil
					}
					return selectGqlObject("CommentEdge", pSel.subSet, ctx, func(ef string, _ map[string]any, eSel gqlSel) any {
						if ef != "node" {
							return nil
						}
						return selectGqlObject("Comment", eSel.subSet, ctx, func(cf string, _ map[string]any, _ gqlSel) any {
							if cf == "id" {
								return cm.NodeID
							}
							if cf == "body" {
								return cm.Body
							}
							return nil
						})
					})
				})
			}
			return nil
		})
	} else {
		data = selectGqlObject("Query", op.selSet, ctx, func(name string, args map[string]any, fieldSel gqlSel) any {
			switch name {
			case "viewer":
				return selectGqlObject("User", fieldSel.subSet, ctx, func(uf string, _ map[string]any, _ gqlSel) any {
					switch uf {
					case "login":
						return store.Viewer.Login
					case "name":
						return store.Viewer.Name
					case "id":
						return store.Viewer.ID
					}
					return nil
				})
			case "repository":
				repo := store.GetRepo(asStringArg(args["owner"]), asStringArg(args["name"]))
				if repo == nil {
					return nil
				}
				return projectGqlRepo(repo, fieldSel.subSet, ctx)
			case "node":
				gid := asStringArg(args["id"])
				if repo := store.GetRepoByGlobalID(gid); repo != nil {
					return projectGqlRepo(repo, fieldSel.subSet, ctx)
				}
				if _, iss := store.GetIssueByGlobalID(gid); iss != nil {
					return projectGqlIssue(iss, fieldSel.subSet, ctx)
				}
				if repo, pr := store.GetPullByGlobalID(gid); repo != nil && pr != nil {
					snap, _ := store.GetPull(repo.Owner, repo.Name, pr.Number)
					return projectGqlPull(repo, snap, fieldSel.subSet, ctx)
				}
				return nil
			case "__type":
				tName := asStringArg(args["name"])
				fList, hasF := gqlSchemaLists[tName]
				eList, hasE := gqlEnumLists[tName]
				if !hasF && !hasE {
					return nil
				}
				return selectGqlObject("__Type", fieldSel.subSet, ctx, func(tf string, _ map[string]any, tSel gqlSel) any {
					if tf == "name" {
						return tName
					}
					if tf == "fields" {
						if !hasF {
							return nil
						}
						out := make([]map[string]any, len(fList))
						for i, fn := range fList {
							fName := fn
							out[i] = selectGqlObject("__Field", tSel.subSet, ctx, func(f string, _ map[string]any, _ gqlSel) any {
								if f == "name" {
									return fName
								}
								return nil
							})
						}
						return out
					}
					if tf == "enumValues" {
						if !hasE {
							return nil
						}
						out := make([]map[string]any, len(eList))
						for i, ev := range eList {
							eVal := ev
							out[i] = selectGqlObject("__EnumValue", tSel.subSet, ctx, func(f string, _ map[string]any, _ gqlSel) any {
								if f == "name" {
									return eVal
								}
								return nil
							})
						}
						return out
					}
					return nil
				})
			}
			return nil
		})
	}

	if len(ctx.errors) > 0 {
		return map[string]any{"data": data, "errors": ctx.errors}
	}
	return map[string]any{"data": data}
}

func HandleGraphQL(req *RawRequest, store *ForgeStore, expectedToken []byte) ([]byte, bool) {
	if req.Path != "/api/graphql" {
		return nil, false
	}
	if !IsAuthorized(req.AuthHeader, expectedToken) {
		return restUnauthHTTP, true
	}

	trimmed := bytes.TrimSpace(req.Body)
	if len(trimmed) == 0 {
		return restMalformedJSON, true
	}
	if trimmed[0] == '[' {
		var docs []gqlRequestDoc
		if err := json.Unmarshal(trimmed, &docs); err != nil {
			return restMalformedJSON, true
		}
		results := make([]map[string]any, len(docs))
		for i, d := range docs {
			results[i] = executeGqlDoc(store, d)
		}
		return jsonFrame(200, "OK", results, ""), true
	}

	var doc gqlRequestDoc
	if err := json.Unmarshal(trimmed, &doc); err != nil {
		return restMalformedJSON, true
	}
	return jsonFrame(200, "OK", executeGqlDoc(store, doc), ""), true
}
