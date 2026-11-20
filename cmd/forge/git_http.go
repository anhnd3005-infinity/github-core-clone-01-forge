package main

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

var (
	v2UploadAdvHTTP        []byte
	pushMainOkSidebandHTTP []byte
	gitNotFoundHTTP        = buildHTTPFrameBytes(404, "Not Found", []byte(`{"message":"Not Found"}`), "")
	gitForbiddenHTTP       = buildHTTPFrameBytes(403, "Forbidden", []byte(`{"message":"Forbidden"}`), "")
	gitBadPktHTTP          = buildHTTPFrameBytes(400, "Bad Request", []byte(`{"message":"Malformed pkt-line"}`), "")
)

func init() {
	var v2 []byte
	v2 = AppendPktLineStr(v2, "version 2\n")
	v2 = AppendPktLineStr(v2, "agent=makefun-forge/1.0\n")
	v2 = AppendPktLineStr(v2, "ls-refs=unborn\n")
	v2 = AppendPktLineStr(v2, "fetch=shallow\n")
	v2 = AppendPktLineStr(v2, "object-format=sha1\n")
	v2 = append(v2, FlushPkt...)
	v2UploadAdvHTTP = buildRawHTTPFrame(200, "OK", "application/x-git-upload-pack-advertisement", v2, "Cache-Control: no-cache\r\n")

	var rep []byte
	rep = AppendPktLineStr(rep, "unpack ok\n")
	rep = AppendPktLineStr(rep, "ok refs/heads/main\n")
	rep = append(rep, FlushPkt...)
	var wrapped []byte
	wrapped = AppendSideband(wrapped, 1, rep)
	wrapped = append(wrapped, FlushPkt...)
	pushMainOkSidebandHTTP = buildRawHTTPFrame(200, "OK", "application/x-git-receive-pack-result", wrapped, "Cache-Control: no-cache\r\n")
}

func buildRawHTTPFrame(statusCode int, statusText, contentType string, body []byte, extraHeaders string) []byte {
	var b strings.Builder
	b.Grow(160 + len(contentType) + len(extraHeaders))
	b.WriteString("HTTP/1.1 ")
	b.WriteString(strconv.Itoa(statusCode))
	b.WriteByte(' ')
	b.WriteString(statusText)
	b.WriteString("\r\nContent-Type: ")
	b.WriteString(contentType)
	b.WriteString("\r\nX-GitHub-Request-Id: fg-ok\r\n")
	if extraHeaders != "" {
		b.WriteString(extraHeaders)
	}
	b.WriteString("Content-Length: ")
	b.WriteString(strconv.Itoa(len(body)))
	b.WriteString("\r\n\r\n")

	hdrLen := b.Len()
	out := make([]byte, hdrLen+len(body))
	copy(out, b.String())
	copy(out[hdrLen:], body)
	return out
}

func formatV0AdvertisementLocked(repo *RepoRecord, service string) []byte {
	type refPair struct {
		name string
		oid  string
	}
	sorted := make([]refPair, 0, len(repo.Refs))
	for k, v := range repo.Refs {
		sorted = append(sorted, refPair{name: k, oid: v})
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].name < sorted[j].name
	})

	caps := "report-status report-status-v2 delete-refs side-band-64k ofs-delta agent=makefun-forge/1.0"
	if service == "git-upload-pack" {
		caps = "multi_ack_detailed side-band-64k ofs-delta shallow thin-pack no-progress include-tag symref=HEAD:refs/heads/" + repo.DefaultBranch + " agent=makefun-forge/1.0"
	}

	var entries []string
	if service == "git-upload-pack" {
		headOID := repo.Refs["refs/heads/"+repo.DefaultBranch]
		if headOID == "" && len(sorted) > 0 {
			headOID = sorted[0].oid
		}
		if headOID != "" {
			entries = append(entries, headOID+" HEAD")
		}
	}
	for _, r := range sorted {
		entries = append(entries, r.oid+" "+r.name)
		if obj := repo.Objects[r.oid]; obj != nil && obj.Type == "tag" {
			peeled := PeelOID(repo.Objects, r.oid)
			if peeled != r.oid {
				entries = append(entries, peeled+" "+r.name+"^{}")
			}
		}
	}

	var pkt []byte
	pkt = AppendPktLineStr(pkt, "# service="+service+"\n")
	pkt = append(pkt, FlushPkt...)
	if len(entries) == 0 {
		pkt = AppendPktLineStr(pkt, ZeroOID+" capabilities^{}\x00"+caps+"\n")
	} else {
		for idx, line := range entries {
			if idx == 0 {
				pkt = AppendPktLineStr(pkt, line+"\x00"+caps+"\n")
			} else {
				pkt = AppendPktLineStr(pkt, line+"\n")
			}
		}
	}
	pkt = append(pkt, FlushPkt...)
	return pkt
}

func collectObjectClosureLocked(repo *RepoRecord, commitOIDs map[string]struct{}, extraRoots map[string]struct{}) map[string]struct{} {
	seen := make(map[string]struct{}, len(commitOIDs)*4+len(extraRoots)+16)
	stack := make([]string, 0, len(commitOIDs)+len(extraRoots)+16)
	for oid := range extraRoots {
		stack = append(stack, oid)
	}
	for cOID := range commitOIDs {
		obj := repo.Objects[cOID]
		if obj == nil {
			continue
		}
		seen[cOID] = struct{}{}
		if obj.Type == "commit" {
			stack = append(stack, ParseCommit(obj.Data).Tree)
		}
	}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, ok := seen[oid]; ok {
			continue
		}
		obj := repo.Objects[oid]
		if obj == nil {
			continue
		}
		seen[oid] = struct{}{}
		if obj.Type == "tree" {
			for _, e := range ParseTree(obj.Data) {
				if e.Mode == "160000" {
					continue
				}
				if _, ok := seen[e.OID]; !ok {
					stack = append(stack, e.OID)
				}
			}
		} else if obj.Type == "tag" {
			stack = append(stack, ParseTag(obj.Data).Object)
		}
	}
	return seen
}

func buildDeltaFetchPackLocked(
	repo *RepoRecord,
	wants, haves, shallowBoundaries []string,
	depth int,
) (pack []byte, newShallows, unshallows []string) {
	clientShallows := make(map[string]struct{}, len(shallowBoundaries))
	for _, s := range shallowBoundaries {
		clientShallows[s] = struct{}{}
	}

	excludeCommits := make(map[string]struct{}, len(haves)*2)
	hQueue := make([]string, 0, len(haves))
	for _, h := range haves {
		target := PeelOID(repo.Objects, h)
		if repo.Objects[target] != nil {
			hQueue = append(hQueue, target)
		}
	}
	for len(hQueue) > 0 {
		oid := hQueue[len(hQueue)-1]
		hQueue = hQueue[:len(hQueue)-1]
		if _, ok := excludeCommits[oid]; ok {
			continue
		}
		excludeCommits[oid] = struct{}{}
		if _, isShallow := clientShallows[oid]; isShallow {
			continue
		}
		for _, p := range repo.CommitParents[oid] {
			if _, ok := excludeCommits[p]; !ok {
				hQueue = append(hQueue, p)
			}
		}
	}

	wantCommits := make(map[string]struct{}, 16)
	newShallowSet := make(map[string]struct{})
	extraRoots := make(map[string]struct{})

	if depth > 0 {
		type distItem struct {
			oid string
			d   int
		}
		bestDist := make(map[string]int, 32)
		bfs := make([]distItem, 0, 32)
		for _, want := range wants {
			wObj := repo.Objects[want]
			if wObj == nil {
				continue
			}
			if wObj.Type == "tag" {
				extraRoots[want] = struct{}{}
			}
			target := PeelOID(repo.Objects, want)
			tObj := repo.Objects[target]
			if tObj == nil {
				continue
			}
			if tObj.Type == "commit" {
				if prev, ok := bestDist[target]; !ok || 1 < prev {
					bestDist[target] = 1
					bfs = append(bfs, distItem{oid: target, d: 1})
				}
			} else {
				extraRoots[target] = struct{}{}
			}
		}
		for i := 0; i < len(bfs); i++ {
			item := bfs[i]
			if item.d > bestDist[item.oid] {
				continue
			}
			wantCommits[item.oid] = struct{}{}
			parents := repo.CommitParents[item.oid]
			if item.d >= depth {
				if len(parents) > 0 {
					newShallowSet[item.oid] = struct{}{}
				}
				continue
			}
			_, isExcluded := excludeCommits[item.oid]
			_, isClientShallow := clientShallows[item.oid]
			if isExcluded && !isClientShallow {
				continue
			}
			for _, p := range parents {
				nextD := item.d + 1
				if prev, ok := bestDist[p]; !ok || nextD < prev {
					bestDist[p] = nextD
					bfs = append(bfs, distItem{oid: p, d: nextD})
				}
			}
		}
	} else {
		stack := make([]string, 0, len(wants))
		for _, want := range wants {
			wObj := repo.Objects[want]
			if wObj == nil {
				continue
			}
			if wObj.Type == "tag" {
				extraRoots[want] = struct{}{}
			}
			target := PeelOID(repo.Objects, want)
			tObj := repo.Objects[target]
			if tObj == nil {
				continue
			}
			if tObj.Type == "commit" {
				stack = append(stack, target)
			} else {
				extraRoots[target] = struct{}{}
			}
		}
		for len(stack) > 0 {
			oid := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if _, ok := wantCommits[oid]; ok {
				continue
			}
			if _, ok := excludeCommits[oid]; ok {
				continue
			}
			if repo.Objects[oid] == nil {
				continue
			}
			wantCommits[oid] = struct{}{}
			for _, p := range repo.CommitParents[oid] {
				_, seenW := wantCommits[p]
				_, seenE := excludeCommits[p]
				if !seenW && !seenE {
					stack = append(stack, p)
				}
			}
		}
	}

	if len(newShallowSet) > 0 {
		newShallows = make([]string, 0, len(newShallowSet))
		for s := range newShallowSet {
			newShallows = append(newShallows, s)
		}
		sort.Strings(newShallows)
	}
	if depth > 0 {
		for s := range clientShallows {
			if _, wanted := wantCommits[s]; wanted {
				if _, stillShallow := newShallowSet[s]; !stillShallow {
					unshallows = append(unshallows, s)
				}
			}
		}
		sort.Strings(unshallows)
	}

	sendCommits := make(map[string]struct{}, len(wantCommits))
	for oid := range wantCommits {
		if _, ex := excludeCommits[oid]; !ex {
			sendCommits[oid] = struct{}{}
		}
	}
	if len(sendCommits) == 0 && len(extraRoots) == 0 {
		return EmptyPackfile, newShallows, unshallows
	}

	excludedObjs := collectObjectClosureLocked(repo, excludeCommits, nil)
	wantedObjs := collectObjectClosureLocked(repo, sendCommits, extraRoots)
	packObjs := make([]*GitObject, 0, len(wantedObjs))
	for oid := range wantedObjs {
		if _, ex := excludedObjs[oid]; ex {
			continue
		}
		if obj := repo.Objects[oid]; obj != nil {
			packObjs = append(packObjs, obj)
		}
	}
	if len(packObjs) == 0 {
		return EmptyPackfile, newShallows, unshallows
	}
	return BuildPackfile(packObjs), newShallows, unshallows
}

func handleUploadPackV2(repo *RepoRecord, packets []PktPacket, rawBody string) ([]byte, error) {
	command := ""
	inArgs := false
	peel := false
	symrefs := false
	unborn := false
	var prefixes []string
	var wants []string
	var haves []string
	var shallow []string
	depth := 0

	for _, pkt := range packets {
		if pkt.Kind == 'D' {
			inArgs = true
			continue
		}
		if pkt.Kind == 'F' {
			break
		}
		if pkt.Kind != 'P' {
			continue
		}
		line := strings.TrimSuffix(string(pkt.Payload), "\n")
		if !inArgs {
			if strings.HasPrefix(line, "command=") {
				command = line[len("command="):]
			}
			continue
		}
		switch command {
		case "ls-refs":
			switch {
			case line == "peel":
				peel = true
			case line == "symrefs":
				symrefs = true
			case line == "unborn":
				unborn = true
			case strings.HasPrefix(line, "ref-prefix "):
				prefixes = append(prefixes, line[len("ref-prefix "):])
			}
		case "fetch":
			switch {
			case strings.HasPrefix(line, "want "):
				wants = append(wants, strings.TrimSpace(line[5:]))
			case strings.HasPrefix(line, "have "):
				haves = append(haves, strings.TrimSpace(line[5:]))
			case strings.HasPrefix(line, "shallow "):
				shallow = append(shallow, strings.TrimSpace(line[8:]))
			case strings.HasPrefix(line, "deepen "):
				if v, err := strconv.Atoi(strings.TrimSpace(line[7:])); err == nil && v > 0 {
					depth = v
				}
			}
		}
	}

	if command == "ls-refs" {
		if cachedAny, ok := repo.LsRefsCache.Load(rawBody); ok {
			cf := cachedAny.(*CachedHTTPFrame)
			repo.mu.RLock()
			ver := repo.RefVersion
			repo.mu.RUnlock()
			if cf.Version == ver {
				return cf.Body, nil
			}
		}

		repo.mu.RLock()
		ver := repo.RefVersion
		matchPrefix := func(name string) bool {
			if len(prefixes) == 0 {
				return true
			}
			for _, p := range prefixes {
				if strings.HasPrefix(name, p) {
					return true
				}
			}
			return false
		}
		var body []byte
		if matchPrefix("HEAD") {
			headRef := "refs/heads/" + repo.DefaultBranch
			headOID := repo.Refs[headRef]
			if headOID != "" {
				if symrefs {
					body = AppendPktLineStr(body, headOID+" HEAD symref-target:"+headRef+"\n")
				} else {
					body = AppendPktLineStr(body, headOID+" HEAD\n")
				}
			} else if unborn {
				if symrefs {
					body = AppendPktLineStr(body, "unborn HEAD symref-target:"+headRef+"\n")
				} else {
					body = AppendPktLineStr(body, "unborn HEAD\n")
				}
			}
		}

		refNames := make([]string, 0, len(repo.Refs))
		for rn := range repo.Refs {
			refNames = append(refNames, rn)
		}
		sort.Strings(refNames)
		for _, rn := range refNames {
			if !matchPrefix(rn) {
				continue
			}
			oid := repo.Refs[rn]
			line := oid + " " + rn
			if peel {
				if obj := repo.Objects[oid]; obj != nil && obj.Type == "tag" {
					peeled := PeelOID(repo.Objects, oid)
					if peeled != oid {
						line += " peeled:" + peeled
					}
				}
			}
			body = AppendPktLineStr(body, line+"\n")
		}
		repo.mu.RUnlock()

		body = append(body, FlushPkt...)
		frame := buildRawHTTPFrame(200, "OK", "application/x-git-upload-pack-result", body, "Cache-Control: no-cache\r\n")
		repo.mu.Lock()
		repo.HasLsRefsCache = true
		repo.mu.Unlock()
		repo.LsRefsCache.Store(rawBody, &CachedHTTPFrame{Version: ver, Body: frame})
		return frame, nil
	}

	if command == "fetch" {
		isDepth1Fast := depth == 1 && len(haves) == 0 && len(shallow) == 0 && len(wants) == 1
		if isDepth1Fast {
			if cachedAny, ok := repo.ShallowCache.Load(wants[0]); ok {
				cf := cachedAny.(*CachedHTTPFrame)
				repo.mu.RLock()
				ver := repo.RefVersion
				repo.mu.RUnlock()
				if cf.Version == ver {
					return cf.Body, nil
				}
			}
		}

		repo.mu.RLock()
		ver := repo.RefVersion
		pack, newShallows, unshallows := buildDeltaFetchPackLocked(repo, wants, haves, shallow, depth)
		repo.mu.RUnlock()

		var respBody []byte
		if len(newShallows) > 0 || len(unshallows) > 0 {
			respBody = AppendPktLineStr(respBody, "shallow-info\n")
			for _, s := range newShallows {
				respBody = AppendPktLineStr(respBody, "shallow "+s+"\n")
			}
			for _, u := range unshallows {
				respBody = AppendPktLineStr(respBody, "unshallow "+u+"\n")
			}
			respBody = append(respBody, DelimPkt...)
		}
		respBody = AppendPktLineStr(respBody, "packfile\n")
		respBody = AppendSideband(respBody, 1, pack)
		respBody = append(respBody, FlushPkt...)

		frame := buildRawHTTPFrame(200, "OK", "application/x-git-upload-pack-result", respBody, "Cache-Control: no-cache\r\n")
		if isDepth1Fast {
			repo.mu.Lock()
			repo.HasShallowCache = true
			repo.mu.Unlock()
			repo.ShallowCache.Store(wants[0], &CachedHTTPFrame{Version: ver, Body: frame})
		}
		return frame, nil
	}

	return nil, ErrMalformedPktLine
}

func handleUploadPackV0(repo *RepoRecord, packets []PktPacket) []byte {
	var wants, haves, shallow []string
	depth := 0
	section := 0
	for _, pkt := range packets {
		if pkt.Kind == 'F' {
			section++
			continue
		}
		if pkt.Kind != 'P' {
			continue
		}
		raw := strings.TrimSuffix(string(pkt.Payload), "\n")
		if nul := strings.IndexByte(raw, 0); nul >= 0 {
			raw = raw[:nul]
		}
		clean := strings.TrimSpace(raw)
		if clean == "" {
			continue
		}
		if section == 0 {
			switch {
			case strings.HasPrefix(clean, "want "):
				parts := strings.SplitN(clean[5:], " ", 2)
				wants = append(wants, parts[0])
			case strings.HasPrefix(clean, "shallow "):
				shallow = append(shallow, strings.TrimSpace(clean[8:]))
			case strings.HasPrefix(clean, "deepen "):
				if v, err := strconv.Atoi(strings.TrimSpace(clean[7:])); err == nil && v > 0 {
					depth = v
				}
			}
		} else if strings.HasPrefix(clean, "have ") {
			haves = append(haves, strings.TrimSpace(clean[5:]))
		}
	}

	repo.mu.RLock()
	pack, newShallows, unshallows := buildDeltaFetchPackLocked(repo, wants, haves, shallow, depth)
	repo.mu.RUnlock()

	var respBody []byte
	if len(newShallows) > 0 || len(unshallows) > 0 {
		for _, s := range newShallows {
			respBody = AppendPktLineStr(respBody, "shallow "+s+"\n")
		}
		for _, u := range unshallows {
			respBody = AppendPktLineStr(respBody, "unshallow "+u+"\n")
		}
		respBody = append(respBody, FlushPkt...)
	}
	respBody = AppendPktLineStr(respBody, "NAK\n")
	respBody = AppendSideband(respBody, 1, pack)
	respBody = append(respBody, FlushPkt...)
	return buildRawHTTPFrame(200, "OK", "application/x-git-upload-pack-result", respBody, "Cache-Control: no-cache\r\n")
}

func handleReceivePack(repo *RepoRecord, body []byte) ([]byte, error) {
	packets, consumed, err := DecodePktLines(body, true)
	if err != nil {
		return nil, err
	}

	type pushCmd struct {
		oldOID string
		newOID string
		ref    string
	}
	cmds := make([]pushCmd, 0, 2)
	hasSideband := false
	first := true
	for _, pkt := range packets {
		if pkt.Kind != 'P' {
			continue
		}
		text := strings.TrimSuffix(string(pkt.Payload), "\n")
		if first {
			first = false
			if nul := strings.IndexByte(text, 0); nul >= 0 {
				capStr := text[nul+1:]
				hasSideband = strings.Contains(capStr, "side-band-64k")
				text = text[:nul]
			}
		}
		parts := strings.Split(text, " ")
		if len(parts) != 3 || !IsValidOID(parts[0]) || !IsValidOID(parts[1]) {
			return nil, ErrMalformedPktLine
		}
		cmds = append(cmds, pushCmd{oldOID: parts[0], newOID: parts[1], ref: parts[2]})
	}

	packSlice := body[consumed:]

	var unpacked []*GitObject
	var unpackErr error
	if len(packSlice) > 0 {
		unpacked, unpackErr = UnpackPackfile(packSlice, func(oid string) *GitObject {
			repo.mu.RLock()
			o := repo.Objects[oid]
			repo.mu.RUnlock()
			return o
		})
	}

	if unpackErr != nil {
		var rep []byte
		rep = AppendPktLineStr(rep, "unpack "+unpackErr.Error()+"\n")
		for _, cmd := range cmds {
			rep = AppendPktLineStr(rep, "ng "+cmd.ref+" unpack failed\n")
		}
		rep = append(rep, FlushPkt...)
		var out []byte
		if hasSideband {
			out = AppendSideband(out, 1, rep)
			out = append(out, FlushPkt...)
		} else {
			out = rep
		}
		return buildRawHTTPFrame(200, "OK", "application/x-git-receive-pack-result", out, "Cache-Control: no-cache\r\n"), nil
	}

	repo.mu.Lock()
	for _, obj := range unpacked {
		repo.SaveGitObjectLocked(obj)
	}

	allOk := true
	statuses := make([]string, len(cmds))
	mutated := false
	for i, cmd := range cmds {
		curOID := repo.Refs[cmd.ref]
		if curOID == "" {
			curOID = ZeroOID
		}
		if cmd.newOID == ZeroOID {
			if curOID != ZeroOID && curOID != cmd.oldOID {
				statuses[i] = "ng " + cmd.ref + " failed to lock\n"
				allOk = false
				continue
			}
			delete(repo.Refs, cmd.ref)
			mutated = true
			statuses[i] = "ok " + cmd.ref + "\n"
			continue
		}
		if repo.Objects[cmd.newOID] == nil {
			statuses[i] = "ng " + cmd.ref + " missing object\n"
			allOk = false
			continue
		}
		if cmd.oldOID == ZeroOID {
			if curOID != ZeroOID && curOID != cmd.newOID {
				statuses[i] = "ng " + cmd.ref + " already exists\n"
				allOk = false
				continue
			}
			repo.Refs[cmd.ref] = cmd.newOID
			mutated = true
			statuses[i] = "ok " + cmd.ref + "\n"
			continue
		}
		if curOID != cmd.oldOID || !IsAncestor(repo.CommitParents, cmd.oldOID, cmd.newOID) {
			statuses[i] = "ng " + cmd.ref + " non-fast-forward\n"
			allOk = false
			continue
		}
		repo.Refs[cmd.ref] = cmd.newOID
		mutated = true
		statuses[i] = "ok " + cmd.ref + "\n"
	}
	if mutated {
		repo.BumpRefVersionLocked()
	}
	repo.mu.Unlock()

	if allOk && hasSideband && len(cmds) == 1 && cmds[0].ref == "refs/heads/main" {
		return pushMainOkSidebandHTTP, nil
	}

	var rep []byte
	rep = AppendPktLineStr(rep, "unpack ok\n")
	for _, st := range statuses {
		rep = AppendPktLineStr(rep, st)
	}
	rep = append(rep, FlushPkt...)
	var out []byte
	if hasSideband {
		out = AppendSideband(out, 1, rep)
		out = append(out, FlushPkt...)
	} else {
		out = rep
	}
	return buildRawHTTPFrame(200, "OK", "application/x-git-receive-pack-result", out, "Cache-Control: no-cache\r\n"), nil
}

func HandleGitHTTP(req *RawRequest, store *ForgeStore) ([]byte, bool) {
	gitIdx := strings.Index(req.Path, ".git/")
	if gitIdx < 0 {
		return nil, false
	}
	slug := req.Path[1:gitIdx]
	slash := strings.IndexByte(slug, '/')
	if slash <= 0 || strings.IndexByte(slug[slash+1:], '/') >= 0 {
		return nil, false
	}
	owner := decodePercent(slug[:slash])
	repoName := decodePercent(slug[slash+1:])
	subPath := req.Path[gitIdx+4:]
	repo := store.GetRepo(owner, repoName)

	if req.Method == "GET" && subPath == "/info/refs" {
		if repo == nil {
			return gitNotFoundHTTP, true
		}
		service := queryParam(req.Query, "service")
		if service == "git-upload-pack" {
			if strings.Contains(req.GitProtocol, "version=2") {
				return v2UploadAdvHTTP, true
			}
			repo.mu.RLock()
			adv := formatV0AdvertisementLocked(repo, "git-upload-pack")
			repo.mu.RUnlock()
			return buildRawHTTPFrame(200, "OK", "application/x-git-upload-pack-advertisement", adv, "Cache-Control: no-cache\r\n"), true
		}
		if service == "git-receive-pack" {
			repo.mu.RLock()
			ver := repo.RefVersion
			if cached := repo.ReceiveRefsCache.Load(); cached != nil && cached.Version == ver {
				repo.mu.RUnlock()
				return cached.Body, true
			}
			adv := formatV0AdvertisementLocked(repo, "git-receive-pack")
			repo.mu.RUnlock()

			frame := buildRawHTTPFrame(200, "OK", "application/x-git-receive-pack-advertisement", adv, "Cache-Control: no-cache\r\n")
			repo.mu.RLock()
			if repo.RefVersion == ver {
				repo.ReceiveRefsCache.Store(&CachedHTTPFrame{Version: ver, Body: frame})
			}
			repo.mu.RUnlock()
			return frame, true
		}
		return gitForbiddenHTTP, true
	}

	if req.Method == "POST" && subPath == "/git-upload-pack" {
		if repo == nil {
			return gitNotFoundHTTP, true
		}
		packets, _, err := DecodePktLines(req.Body, false)
		if err != nil {
			return gitBadPktHTTP, true
		}
		var firstData string
		for _, pkt := range packets {
			if pkt.Kind == 'P' {
				firstData = string(pkt.Payload)
				break
			}
		}
		isV2 := strings.Contains(req.GitProtocol, "version=2") || strings.HasPrefix(firstData, "command=")
		if isV2 {
			frame, vErr := handleUploadPackV2(repo, packets, string(req.Body))
			if vErr != nil {
				return gitBadPktHTTP, true
			}
			return frame, true
		}
		return handleUploadPackV0(repo, packets), true
	}

	if req.Method == "POST" && subPath == "/git-receive-pack" {
		if repo == nil {
			return gitNotFoundHTTP, true
		}
		frame, rErr := handleReceivePack(repo, req.Body)
		if rErr != nil {
			msg, _ := json.Marshal(map[string]string{"message": rErr.Error()})
			return buildHTTPFrameBytes(400, "Bad Request", msg, ""), true
		}
		return frame, true
	}

	if !bytes.HasPrefix([]byte(subPath), []byte("/")) {
		return nil, false
	}
	return gitNotFoundHTTP, true
}
