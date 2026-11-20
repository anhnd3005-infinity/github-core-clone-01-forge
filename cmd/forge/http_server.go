package main

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"strconv"
	"strings"
)

var healthzHTTP = buildHTTPFrameBytes(200, "OK", []byte(`{"ok":true}`), "")

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
			lf, err := br.ReadByte()
			if err != nil {
				return nil, err
			}
			_ = lf
		}
	}
}

func dispatchRequest(req *RawRequest, store *ForgeStore, expectedToken []byte) []byte {
	if req.Method == "GET" && req.Path == "/healthz" {
		return healthzHTTP
	}
	if req.Path == "/api/graphql" {
		if resp, handled := HandleGraphQL(req, store, expectedToken); handled {
			return resp
		}
	} else if req.Path == "/mcp" {
		if resp, handled := HandleMCP(req, store, expectedToken); handled {
			return resp
		}
	} else if req.Path == "/api/v3" || strings.HasPrefix(req.Path, "/api/v3/") {
		if resp, handled := HandleRestAPI(req, store, expectedToken); handled {
			return resp
		}
	} else if strings.Contains(req.Path, ".git/") {
		if resp, handled := HandleGitHTTP(req, store); handled {
			return resp
		}
	}
	return restNotFoundHTTP
}

func handleTCPConn(conn net.Conn, store *ForgeStore, expectedToken []byte) {
	defer conn.Close()
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
	}

	br := bufio.NewReaderSize(conn, 64*1024)
	bodyBuf := make([]byte, 0, 8192)
	var authScratch [256]byte

	for {
		line, err := br.ReadSlice('\n')
		if err != nil {
			return
		}
		line = trimASCIISpace(line)
		if len(line) == 0 {
			continue
		}

		sp1 := bytes.IndexByte(line, ' ')
		if sp1 <= 0 {
			return
		}
		sp2 := bytes.IndexByte(line[sp1+1:], ' ')
		if sp2 <= 0 {
			return
		}
		method := string(line[:sp1])
		target := string(line[sp1+1 : sp1+1+sp2])
		proto := line[sp1+1+sp2+1:]

		if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
			if idx := strings.Index(target, "://"); idx >= 0 {
				rest := target[idx+3:]
				if slash := strings.IndexByte(rest, '/'); slash >= 0 {
					target = rest[slash:]
				} else {
					target = "/"
				}
			}
		}

		path := target
		query := ""
		if q := strings.IndexByte(target, '?'); q >= 0 {
			path = target[:q]
			query = target[q+1:]
		}

		contentLen := -1
		isChunked := false
		expectContinue := false
		closeConn := bytes.Equal(proto, []byte("HTTP/1.0"))
		host := ""
		gitProto := ""
		authSlice := authScratch[:0]

		for {
			hline, hErr := br.ReadSlice('\n')
			if hErr != nil {
				return
			}
			hline = trimASCIISpace(hline)
			if len(hline) == 0 {
				break
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
					host = string(val)
				}
			case 6:
				if asciiEqualFold(key, "expect") {
					if asciiEqualFold(val, "100-continue") {
						expectContinue = true
					}
				}
			case 10:
				if asciiEqualFold(key, "connection") {
					if asciiEqualFold(val, "close") {
						closeConn = true
					} else if asciiEqualFold(val, "keep-alive") {
						closeConn = false
					}
				}
			case 12:
				if asciiEqualFold(key, "git-protocol") {
					gitProto = string(val)
				}
			case 13:
				if asciiEqualFold(key, "authorization") {
					authSlice = append(authSlice[:0], val...)
				}
			case 14:
				if asciiEqualFold(key, "content-length") {
					if cl, ok := parseASCIIInt(val); ok {
						contentLen = cl
					}
				}
			case 17:
				if asciiEqualFold(key, "transfer-encoding") {
					if asciiEqualFold(val, "chunked") || strings.Contains(strings.ToLower(string(val)), "chunked") {
						isChunked = true
					}
				}
			}
		}

		if expectContinue {
			if err := writeAll(conn, []byte("HTTP/1.1 100 Continue\r\n\r\n")); err != nil {
				return
			}
		}

		var body []byte
		if isChunked {
			cb, cErr := readChunkedBody(br, bodyBuf)
			if cErr != nil {
				return
			}
			bodyBuf = cb
			body = cb
		} else if contentLen > 0 {
			if cap(bodyBuf) < contentLen {
				bodyBuf = make([]byte, contentLen)
			}
			body = bodyBuf[:contentLen]
			if _, rErr := io.ReadFull(br, body); rErr != nil {
				return
			}
		}

		req := RawRequest{
			Method:      method,
			Path:        path,
			Query:       query,
			Host:        host,
			AuthHeader:  authSlice,
			GitProtocol: gitProto,
			Body:        body,
			CloseConn:   closeConn,
		}

		resp := dispatchRequest(&req, store, expectedToken)
		if err := writeAll(conn, resp); err != nil {
			return
		}
		if closeConn {
			return
		}
	}
}

func ServeRawTCP(ln net.Listener, store *ForgeStore, expectedToken []byte) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go handleTCPConn(conn, store, expectedToken)
	}
}
