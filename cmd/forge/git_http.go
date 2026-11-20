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
	var codeBuf [8]byte
	codeBytes := strconv.AppendInt(codeBuf[:0], int64(statusCode), 10)
	var lenBuf [20]byte
	lenBytes := strconv.AppendInt(lenBuf[:0], int64(len(body)), 10)

	hdrCap := 9 + len(codeBytes) + 1 + len(statusText) + 16 + len(contentType) + 28 + len(extraHeaders) + 18 + len(lenBytes) + 4
	out := make([]byte, 0, hdrCap+len(body))
	out = append(out, "HTTP/1.1 "...)
	out = append(out, codeBytes...)
	out = append(out, ' ')
	out = append(out, statusText...)
	out = append(out, "\r\nContent-Type: "...)
	out = append(out, contentType...)
	out = append(out, "\r\nX-GitHub-Request-Id: 1\r\n"...)
	if extraHeaders != "" {
		out = append(out, extraHeaders...)
	}
	out = append(out, "Content-Length: "...)
	out = append(out, lenBytes...)
	out = append(out, "\r\n\r\n"...)
	out = append(out, body...)
	return out
}

func formatReceiveMainTemplate(oid string) ([]byte, int) {
	const prefix = "001f# service=git-receive-pack\n0000"
	lineSuffix := " refs/heads/main\x00report-status report-status-v2 delete-refs side-band-64k ofs-delta agent=makefun-forge/1.0\n"
	pkt := make([]byte, 0, len(prefix)+4+40+len(lineSuffix)+4)
	pkt = append(pkt, prefix...)
	pktTotal := 4 + 40 + len(lineSuffix)
	pkt = append(pkt, hexDigits[(pktTotal>>12)&0xf], hexDigits[(pktTotal>>8)&0xf], hexDigits[(pktTotal>>4)&0xf], hexDigits[pktTotal&0xf])
	oidRelOff := len(pkt)
	pkt = append(pkt, oid...)
	pkt = append(pkt, lineSuffix...)
	pkt = append(pkt, FlushPkt...)
	frame := buildRawHTTPFrame(200, "OK", "application/x-git-receive-pack-advertisement", pkt, "Cache-Control: no-cache\r\n")
	hdrLen := len(frame) - len(pkt)
	return frame, hdrLen + oidRelOff
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
		if cObj := repo.Objects[oid]; cObj != nil && cObj.Parent1 != "" {
			if _, ok := excludeCommits[cObj.Parent1]; !ok {
				hQueue = append(hQueue, cObj.Parent1)
			}
			for _, p := range cObj.ExtraParents {
				if _, ok := excludeCommits[p]; !ok {
					hQueue = append(hQueue, p)
				}
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
			var p1 string
			var extra []string
			if cObj := repo.Objects[item.oid]; cObj != nil {
				p1 = cObj.Parent1
				extra = cObj.ExtraParents
			}
			if item.d >= depth {
				if p1 != "" {
					newShallowSet[item.oid] = struct{}{}
				}
				continue
			}
			_, isExcluded := excludeCommits[item.oid]
			_, isClientShallow := clientShallows[item.oid]
			if isExcluded && !isClientShallow {
				continue
			}
			if p1 != "" {
				nextD := item.d + 1
				if prev, ok := bestDist[p1]; !ok || nextD < prev {
					bestDist[p1] = nextD
					bfs = append(bfs, distItem{oid: p1, d: nextD})
				}
				for _, p := range extra {
					if prev, ok := bestDist[p]; !ok || nextD < prev {
						bestDist[p] = nextD
						bfs = append(bfs, distItem{oid: p, d: nextD})
					}
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
			cObj := repo.Objects[oid]
			if cObj == nil {
				continue
			}
			wantCommits[oid] = struct{}{}
			if cObj.Parent1 != "" {
				p := cObj.Parent1
				if _, seenW := wantCommits[p]; !seenW {
					if _, seenE := excludeCommits[p]; !seenE {
						stack = append(stack, p)
					}
				}
				for _, ep := range cObj.ExtraParents {
					if _, seenW := wantCommits[ep]; !seenW {
						if _, seenE := excludeCommits[ep]; !seenE {
							stack = append(stack, ep)
						}
					}
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

func handleUploadPackV2(repo *RepoRecord, packets []PktPacket, rawBody []byte) ([]byte, error) {
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
		if repo.lsRefsMap != nil {
			if frame, ok := repo.lsRefsMap[unsafeString(rawBody)]; ok {
				repo.lastLsRefsBody = append(repo.lastLsRefsBody[:0], rawBody...)
				repo.lastLsRefsFrame = frame
				return frame, nil
			}
		}

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

		body = append(body, FlushPkt...)
		frame := buildRawHTTPFrame(200, "OK", "application/x-git-upload-pack-result", body, "Cache-Control: no-cache\r\n")
		bodyCopy := append([]byte(nil), rawBody...)
		repo.lastLsRefsBody = bodyCopy
		repo.lastLsRefsFrame = frame
		if repo.lsRefsMap == nil {
			repo.lsRefsMap = make(map[string][]byte, 4)
		}
		repo.lsRefsMap[string(bodyCopy)] = frame
		return frame, nil
	}

	if command == "fetch" {
		isDepth1Fast := depth == 1 && len(haves) == 0 && len(shallow) == 0 && len(wants) == 1
		if isDepth1Fast && repo.shallowMap != nil {
			if frame, ok := repo.shallowMap[wants[0]]; ok {
				repo.lastFetchBody = append(repo.lastFetchBody[:0], rawBody...)
				repo.lastFetchFrame = frame
				return frame, nil
			}
		}

		pack, newShallows, unshallows := buildDeltaFetchPackLocked(repo, wants, haves, shallow, depth)

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
			bodyCopy := append([]byte(nil), rawBody...)
			repo.lastFetchBody = bodyCopy
			repo.lastFetchFrame = frame
			if repo.shallowMap == nil {
				repo.shallowMap = make(map[string][]byte, 4)
			}
			repo.shallowMap[wants[0]] = frame
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

	pack, newShallows, unshallows := buildDeltaFetchPackLocked(repo, wants, haves, shallow, depth)

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

func isValidOIDBytes(b []byte) bool {
	if len(b) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := b[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func isZeroOIDBytes(b []byte) bool {
	for i := 0; i < len(b); i++ {
		if b[i] != '0' {
			return false
		}
	}
	return len(b) == 40
}

func buildReceiveUnpackErrorFrame(unpackErr error, cmds []string, hasSideband bool) []byte {
	var rep []byte
	rep = AppendPktLineStr(rep, "unpack "+unpackErr.Error()+"\n")
	for _, ref := range cmds {
		rep = AppendPktLineStr(rep, "ng "+ref+" unpack failed\n")
	}
	rep = append(rep, FlushPkt...)
	var out []byte
	if hasSideband {
		out = AppendSideband(out, 1, rep)
		out = append(out, FlushPkt...)
	} else {
		out = rep
	}
	return buildRawHTTPFrame(200, "OK", "application/x-git-receive-pack-result", out, "Cache-Control: no-cache\r\n")
}

func tryHandleReceiveMainFast(repo *RepoRecord, body []byte) ([]byte, bool, error) {
	if len(body) < 92 {
		return nil, false, nil
	}
	pktLen, err := parseHex4(body, 0)
	if err != nil || pktLen < 86 || pktLen+4 > len(body) {
		return nil, false, nil
	}
	if body[pktLen] != '0' || body[pktLen+1] != '0' || body[pktLen+2] != '0' || body[pktLen+3] != '0' {
		return nil, false, nil
	}
	line := body[4:pktLen]
	if len(line) > 0 && line[len(line)-1] == '\n' {
		line = line[:len(line)-1]
	}
	hasSideband := false
	if nul := bytes.IndexByte(line, 0); nul >= 0 {
		hasSideband = bytes.Contains(line[nul+1:], []byte("side-band-64k"))
		line = line[:nul]
	}
	if len(line) <= 82 || line[40] != ' ' || line[81] != ' ' {
		return nil, false, nil
	}
	oldBytes := line[:40]
	newBytes := line[41:81]
	refBytes := line[82:]
	if !bytes.Equal(refBytes, []byte("refs/heads/main")) || !isValidOIDBytes(oldBytes) || !isValidOIDBytes(newBytes) || isZeroOIDBytes(newBytes) {
		return nil, false, nil
	}

	packSlice := body[pktLen+4:]
	var unpacked []*GitObject
	if len(packSlice) > 0 {
		var uErr error
		unpacked, uErr = UnpackPackfile(packSlice, func(oid string) *GitObject {
			return repo.Objects[oid]
		})
		if uErr != nil {
			return buildReceiveUnpackErrorFrame(uErr, []string{"refs/heads/main"}, hasSideband), true, nil
		}
	}
	for _, obj := range unpacked {
		repo.SaveGitObjectLocked(obj)
	}

	newObj := repo.Objects[unsafeString(newBytes)]
	if newObj == nil {
		return nil, false, nil
	}
	curOID := repo.Refs["refs/heads/main"]
	if isZeroOIDBytes(oldBytes) {
		if curOID != "" && curOID != newObj.OID {
			return nil, false, nil
		}
	} else {
		if curOID == "" || len(curOID) != 40 || !bytes.Equal([]byte(curOID), oldBytes) || !IsAncestor(repo.Objects, curOID, newObj.OID) {
			return nil, false, nil
		}
	}

	repo.Refs["refs/heads/main"] = newObj.OID
	repo.BumpRefVersionLocked()

	if hasSideband {
		return pushMainOkSidebandHTTP, true, nil
	}
	rep := AppendPktLineStr(nil, "unpack ok\n")
	rep = AppendPktLineStr(rep, "ok refs/heads/main\n")
	rep = append(rep, FlushPkt...)
	return buildRawHTTPFrame(200, "OK", "application/x-git-receive-pack-result", rep, "Cache-Control: no-cache\r\n"), true, nil
}

func handleReceivePack(repo *RepoRecord, body []byte) ([]byte, error) {
	if frame, handled, fastErr := tryHandleReceiveMainFast(repo, body); handled || fastErr != nil {
		return frame, fastErr
	}

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
			return repo.Objects[oid]
		})
	}

	if unpackErr != nil {
		refs := make([]string, len(cmds))
		for i, c := range cmds {
			refs[i] = c.ref
		}
		return buildReceiveUnpackErrorFrame(unpackErr, refs, hasSideband), nil
	}

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
		if curOID != cmd.oldOID || !IsAncestor(repo.Objects, cmd.oldOID, cmd.newOID) {
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
		service := ""
		if req.Query == "service=git-upload-pack" {
			service = "git-upload-pack"
		} else if req.Query == "service=git-receive-pack" {
			service = "git-receive-pack"
		} else {
			service = queryParam(req.Query, "service")
		}
		if service == "git-upload-pack" {
			if strings.Contains(req.GitProtocol, "version=2") {
				return v2UploadAdvHTTP, true
			}
			adv := formatV0AdvertisementLocked(repo, "git-upload-pack")
			return buildRawHTTPFrame(200, "OK", "application/x-git-upload-pack-advertisement", adv, "Cache-Control: no-cache\r\n"), true
		}
		if service == "git-receive-pack" {
			if len(repo.Refs) == 1 {
				if mainOID := repo.Refs["refs/heads/main"]; len(mainOID) == 40 {
					if repo.receiveMainFrame == nil {
						repo.receiveMainFrame, repo.receiveMainOIDOff = formatReceiveMainTemplate(mainOID)
					} else {
						copy(repo.receiveMainFrame[repo.receiveMainOIDOff:repo.receiveMainOIDOff+40], mainOID)
					}
					return repo.receiveMainFrame, true
				}
			}
			ver := repo.RefVersion
			if repo.receiveRefsFrame != nil && repo.receiveRefsVer == ver {
				return repo.receiveRefsFrame, true
			}
			adv := formatV0AdvertisementLocked(repo, "git-receive-pack")
			frame := buildRawHTTPFrame(200, "OK", "application/x-git-receive-pack-advertisement", adv, "Cache-Control: no-cache\r\n")
			repo.receiveRefsVer = ver
			repo.receiveRefsFrame = frame
			return frame, true
		}
		return gitForbiddenHTTP, true
	}

	if req.Method == "POST" && subPath == "/git-upload-pack" {
		if repo == nil {
			return gitNotFoundHTTP, true
		}
		if len(repo.lastFetchBody) > 0 && bytes.Equal(req.Body, repo.lastFetchBody) {
			return repo.lastFetchFrame, true
		}
		if len(repo.lastLsRefsBody) > 0 && bytes.Equal(req.Body, repo.lastLsRefsBody) {
			return repo.lastLsRefsFrame, true
		}
		packets, _, err := DecodePktLines(req.Body, false)
		if err != nil {
			return gitBadPktHTTP, true
		}
		var firstData []byte
		for _, pkt := range packets {
			if pkt.Kind == 'P' {
				firstData = pkt.Payload
				break
			}
		}
		isV2 := strings.Contains(req.GitProtocol, "version=2") || bytes.HasPrefix(firstData, []byte("command="))
		if isV2 {
			frame, vErr := handleUploadPackV2(repo, packets, req.Body)
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
