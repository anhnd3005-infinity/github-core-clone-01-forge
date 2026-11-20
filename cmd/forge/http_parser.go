package main

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"strconv"
	"strings"
	"unsafe"
)

const (
	parseOK = iota
	parseNeedMore
	parseSend100
	parseBad
)

var (
	healthzHTTP  = buildHTTPFrameBytes(200, "OK", []byte(`{"ok":true}`), "")
	crlfcrlfSeq  = []byte("\r\n\r\n")
	lflfSeq      = []byte("\n\n")
	http10Proto  = []byte("HTTP/1.0")
	expect100Msg = []byte("HTTP/1.1 100 Continue\r\n\r\n")
)

type RawRequest struct {
	Method      string
	Path        string
	Query       string
	Host        string
	AuthHeader  []byte
	GitProtocol string
	Body        []byte
	CloseConn   bool
}

func unsafeString(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(&b[0], len(b))
}

func asciiEqualFold(a []byte, lower string) bool {
	if len(a) != len(lower) {
		return false
	}
	for i := 0; i < len(a); i++ {
		c := a[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != lower[i] {
			return false
		}
	}
	return true
}

func asciiContainsFold(haystack []byte, lowerNeedle string) bool {
	n := len(lowerNeedle)
	if n == 0 {
		return true
	}
	if len(haystack) < n {
		return false
	}
	limit := len(haystack) - n
	for i := 0; i <= limit; i++ {
		c := haystack[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c == lowerNeedle[0] && asciiEqualFold(haystack[i:i+n], lowerNeedle) {
			return true
		}
	}
	return false
}

func trimASCIISpace(b []byte) []byte {
	start := 0
	for start < len(b) && (b[start] == ' ' || b[start] == '\t') {
		start++
	}
	end := len(b)
	for end > start && (b[end-1] == ' ' || b[end-1] == '\t' || b[end-1] == '\r' || b[end-1] == '\n') {
		end--
	}
	return b[start:end]
}

func parseASCIIInt(b []byte) (int, bool) {
	if len(b) == 0 {
		return 0, false
	}
	val := 0
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
		val = val*10 + int(c-'0')
	}
	return val, true
}

func internHTTPMethod(m []byte) string {
	switch len(m) {
	case 3:
		if m[0] == 'G' && m[1] == 'E' && m[2] == 'T' {
			return "GET"
		}
		if m[0] == 'P' && m[1] == 'U' && m[2] == 'T' {
			return "PUT"
		}
	case 4:
		if m[0] == 'P' && m[1] == 'O' && m[2] == 'S' && m[3] == 'T' {
			return "POST"
		}
		if m[0] == 'H' && m[1] == 'E' && m[2] == 'A' && m[3] == 'D' {
			return "HEAD"
		}
	case 5:
		if bytes.Equal(m, []byte("PATCH")) {
			return "PATCH"
		}
	case 6:
		if bytes.Equal(m, []byte("DELETE")) {
			return "DELETE"
		}
	case 7:
		if bytes.Equal(m, []byte("OPTIONS")) {
			return "OPTIONS"
		}
	}
	return string(m)
}

func parseHexIntBytes(b []byte) (int, bool) {
	if len(b) == 0 {
		return 0, false
	}
	v := 0
	for _, c := range b {
		var d int
		switch {
		case c >= '0' && c <= '9':
			d = int(c - '0')
		case c >= 'a' && c <= 'f':
			d = int(c - 'a' + 10)
		case c >= 'A' && c <= 'F':
			d = int(c - 'A' + 10)
		default:
			return 0, false
		}
		if v > (1<<29) {
			return 0, false
		}
		v = (v << 4) | d
	}
	return v, true
}

func parseChunkedInPlace(bodyAvail []byte, scratch []byte) (body []byte, consumed int, needMore bool, outScratch []byte, ok bool) {
	scratch = scratch[:0]
	off := 0
	for {
		nl := bytes.IndexByte(bodyAvail[off:], '\n')
		if nl < 0 {
			return nil, 0, true, scratch, true
		}
		line := trimASCIISpace(bodyAvail[off : off+nl])
		if semi := bytes.IndexByte(line, ';'); semi >= 0 {
			line = trimASCIISpace(line[:semi])
		}
		sz, okHex := parseHexIntBytes(line)
		if !okHex || sz < 0 {
			return nil, 0, false, scratch, false
		}
		off += nl + 1
		if sz == 0 {
			for {
				tnl := bytes.IndexByte(bodyAvail[off:], '\n')
				if tnl < 0 {
					return nil, 0, true, scratch, true
				}
				tline := trimASCIISpace(bodyAvail[off : off+tnl])
				off += tnl + 1
				if len(tline) == 0 {
					break
				}
			}
			return scratch, off, false, scratch, true
		}
		if off+sz >= len(bodyAvail) {
			return nil, 0, true, scratch, true
		}
		scratch = append(scratch, bodyAvail[off:off+sz]...)
		off += sz
		if bodyAvail[off] == '\r' {
			off++
			if off >= len(bodyAvail) {
				return nil, 0, true, scratch, true
			}
		}
		if off >= len(bodyAvail) || bodyAvail[off] != '\n' {
			return nil, 0, false, scratch, false
		}
		off++
	}
}

func parseHTTPRequestInPlace(buf []byte, chunkScratch []byte, alreadySent100 bool) (req RawRequest, consumed int, needHint int, nextChunkScratch []byte, status int) {
	nextChunkScratch = chunkScratch
	start := 0
	n := len(buf)
	for start < n && (buf[start] == '\r' || buf[start] == '\n') {
		start++
	}
	if start >= n {
		return req, start, 0, nextChunkScratch, parseNeedMore
	}

	hdrRel := bytes.Index(buf[start:], crlfcrlfSeq)
	sepLen := 4
	if hdrRel < 0 {
		hdrRel = bytes.Index(buf[start:], lflfSeq)
		sepLen = 2
	}
	if hdrRel < 0 {
		return req, start, 0, nextChunkScratch, parseNeedMore
	}

	hdrBytes := buf[start : start+hdrRel]
	bodyStart := start + hdrRel + sepLen

	firstEnd := bytes.IndexByte(hdrBytes, '\n')
	var reqLine []byte
	var restHdr []byte
	if firstEnd < 0 {
		reqLine = trimASCIISpace(hdrBytes)
	} else {
		reqLine = trimASCIISpace(hdrBytes[:firstEnd])
		restHdr = hdrBytes[firstEnd+1:]
	}

	sp1 := bytes.IndexByte(reqLine, ' ')
	if sp1 <= 0 {
		return req, 0, 0, nextChunkScratch, parseBad
	}
	sp2 := bytes.IndexByte(reqLine[sp1+1:], ' ')
	if sp2 <= 0 {
		return req, 0, 0, nextChunkScratch, parseBad
	}
	methodBytes := reqLine[:sp1]
	targetBytes := reqLine[sp1+1 : sp1+1+sp2]
	protoBytes := trimASCIISpace(reqLine[sp1+1+sp2+1:])

	if len(targetBytes) > 8 && (bytes.HasPrefix(targetBytes, []byte("http://")) || bytes.HasPrefix(targetBytes, []byte("https://"))) {
		if idx := bytes.Index(targetBytes, []byte("://")); idx >= 0 {
			after := targetBytes[idx+3:]
			if slash := bytes.IndexByte(after, '/'); slash >= 0 {
				targetBytes = after[slash:]
			} else {
				targetBytes = []byte("/")
			}
		}
	}

	pathBytes := targetBytes
	var queryBytes []byte
	if q := bytes.IndexByte(targetBytes, '?'); q >= 0 {
		pathBytes = targetBytes[:q]
		queryBytes = targetBytes[q+1:]
	}

	req.Method = internHTTPMethod(methodBytes)
	req.Path = unsafeString(pathBytes)
	req.Query = unsafeString(queryBytes)
	req.CloseConn = bytes.Equal(protoBytes, http10Proto)

	contentLen := -1
	isChunked := false
	expectContinue := false

	for len(restHdr) > 0 {
		nl := bytes.IndexByte(restHdr, '\n')
		var hline []byte
		if nl < 0 {
			hline = restHdr
			restHdr = nil
		} else {
			hline = restHdr[:nl]
			restHdr = restHdr[nl+1:]
		}
		hline = trimASCIISpace(hline)
		if len(hline) == 0 {
			continue
		}
		colon := bytes.IndexByte(hline, ':')
		if colon <= 0 {
			continue
		}
		key := hline[:colon]
		val := trimASCIISpace(hline[colon+1:])

		switch len(key) {
		case 4:
			if asciiEqualFold(key, "host") {
				req.Host = unsafeString(val)
			}
		case 6:
			if asciiEqualFold(key, "expect") && asciiEqualFold(val, "100-continue") {
				expectContinue = true
			}
		case 10:
			if asciiEqualFold(key, "connection") {
				if asciiEqualFold(val, "close") {
					req.CloseConn = true
				} else if asciiEqualFold(val, "keep-alive") {
					req.CloseConn = false
				}
			}
		case 12:
			if asciiEqualFold(key, "git-protocol") {
				req.GitProtocol = unsafeString(val)
			}
		case 13:
			if asciiEqualFold(key, "authorization") {
				req.AuthHeader = val
			}
		case 14:
			if asciiEqualFold(key, "content-length") {
				cl, ok := parseASCIIInt(val)
				if !ok {
					return req, 0, 0, nextChunkScratch, parseBad
				}
				contentLen = cl
			}
		case 17:
			if asciiEqualFold(key, "transfer-encoding") && asciiContainsFold(val, "chunked") {
				isChunked = true
			}
		}
	}

	if expectContinue && !alreadySent100 {
		return req, start, 0, nextChunkScratch, parseSend100
	}

	if isChunked {
		cBody, cConsumed, needMore, ns, cOK := parseChunkedInPlace(buf[bodyStart:], nextChunkScratch)
		nextChunkScratch = ns
		if !cOK {
			return req, 0, 0, nextChunkScratch, parseBad
		}
		if needMore {
			return req, start, len(buf) + 4096, nextChunkScratch, parseNeedMore
		}
		req.Body = cBody
		return req, bodyStart + cConsumed, 0, nextChunkScratch, parseOK
	}

	if contentLen > 0 {
		totalNeed := bodyStart + contentLen
		if len(buf) < totalNeed {
			return req, start, totalNeed, nextChunkScratch, parseNeedMore
		}
		req.Body = buf[bodyStart:totalNeed]
		return req, totalNeed, 0, nextChunkScratch, parseOK
	}

	return req, bodyStart, 0, nextChunkScratch, parseOK
}

func dispatchRequest(req *RawRequest, store *ForgeStore, expectedAuth []byte) []byte {
	if strings.HasPrefix(req.Path, "/api/v3") {
		if resp, handled := HandleRestAPI(req, store, expectedAuth); handled {
			return resp
		}
	}
	if req.Method == "GET" && req.Path == "/healthz" {
		return healthzHTTP
	}
	if req.Path == "/api/graphql" {
		if resp, handled := HandleGraphQL(req, store, expectedAuth); handled {
			return resp
		}
	} else if req.Path == "/mcp" {
		if resp, handled := HandleMCP(req, store, expectedAuth); handled {
			return resp
		}
	} else if resp, handled := HandleGitHTTP(req, store); handled {
		return resp
	}
	return restNotFoundHTTP
}

func writeAll(conn net.Conn, data []byte) error {
	for len(data) > 0 {
		n, err := conn.Write(data)
		if err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

func readChunkedBody(br *bufio.Reader, buf []byte) ([]byte, error) {
	buf = buf[:0]
	for {
		line, err := br.ReadSlice('\n')
		if err != nil {
			return nil, err
		}
		line = trimASCIISpace(line)
		if semi := bytes.IndexByte(line, ';'); semi >= 0 {
			line = trimASCIISpace(line[:semi])
		}
		sz64, pErr := strconv.ParseInt(string(line), 16, 64)
		if pErr != nil || sz64 < 0 {
			return nil, pErr
		}
		sz := int(sz64)
		if sz == 0 {
			for {
				tLine, tErr := br.ReadSlice('\n')
				if tErr != nil {
					return nil, tErr
				}
				if len(trimASCIISpace(tLine)) == 0 {
					break
				}
			}
			return buf, nil
		}
		need := len(buf) + sz
		if cap(buf) < need {
			grown := make([]byte, len(buf), need*2)
			copy(grown, buf)
			buf = grown
		}
		oldLen := len(buf)
		buf = buf[:need]
		if _, err := io.ReadFull(br, buf[oldLen:need]); err != nil {
			return nil, err
		}
		cr, err := br.ReadByte()
		if err != nil {
			return nil, err
		}
		if cr == '\r' {
			_, err = br.ReadByte()
			if err != nil {
				return nil, err
			}
		}
	}
}
