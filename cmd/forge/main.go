package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func parseEnvEndpoint(raw string) (hostHeader string, dialAddr string, basePath string, err error) {
	u := strings.TrimSpace(strings.TrimRight(raw, "/"))
	rest := u
	if strings.HasPrefix(rest, "http://") {
		rest = rest[len("http://"):]
	} else if strings.HasPrefix(rest, "https://") {
		rest = rest[len("https://"):]
	}
	path := ""
	if slash := strings.IndexByte(rest, '/'); slash >= 0 {
		path = rest[slash:]
		rest = rest[:slash]
	}
	if rest == "" {
		return "", "", "", fmt.Errorf("invalid MAKEFUN_ENV_URL %q", raw)
	}
	hostHeader = rest
	dialAddr = rest
	if !strings.Contains(rest, ":") {
		dialAddr = rest + ":80"
	}
	return hostHeader, dialAddr, path, nil
}

func readHTTPResponse(br *bufio.Reader) (status int, body []byte, closeAfter bool, err error) {
	statusLineBytes, err := br.ReadSlice('\n')
	if err != nil {
		return 0, nil, true, err
	}
	parts := strings.SplitN(string(bytes.TrimSpace(statusLineBytes)), " ", 3)
	if len(parts) < 2 {
		return 0, nil, true, fmt.Errorf("invalid status line")
	}
	status, _ = strconv.Atoi(parts[1])
	closeAfter = strings.HasPrefix(parts[0], "HTTP/1.0")

	contentLen := -1
	isChunked := false
	for {
		hline, hErr := br.ReadSlice('\n')
		if hErr != nil {
			return status, nil, true, hErr
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
		if asciiEqualFold(key, "content-length") {
			if cl, ok := parseASCIIInt(val); ok {
				contentLen = cl
			}
		} else if asciiEqualFold(key, "transfer-encoding") {
			if asciiContainsFold(val, "chunked") {
				isChunked = true
			}
		} else if asciiEqualFold(key, "connection") {
			if asciiEqualFold(val, "close") {
				closeAfter = true
			} else if asciiEqualFold(val, "keep-alive") {
				closeAfter = false
			}
		}
	}

	if isChunked {
		body, cErr := readChunkedBody(br, make([]byte, 0, 256))
		return status, body, closeAfter, cErr
	}
	if contentLen > 0 {
		body := make([]byte, contentLen)
		_, rErr := io.ReadFull(br, body)
		return status, body, closeAfter, rErr
	}
	if contentLen == 0 {
		return status, nil, closeAfter, nil
	}
	body, rErr := io.ReadAll(br)
	return status, body, true, rErr
}

func envRegisterOnce(dialAddr, hostHeader, reqPath string, jsonBody []byte) (int, error) {
	conn, err := net.DialTimeout("tcp", dialAddr, 3*time.Second)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	var reqBuf bytes.Buffer
	reqBuf.WriteString("POST ")
	reqBuf.WriteString(reqPath)
	reqBuf.WriteString(" HTTP/1.1\r\nHost: ")
	reqBuf.WriteString(hostHeader)
	reqBuf.WriteString("\r\nConnection: close\r\nContent-Type: application/json\r\nContent-Length: ")
	reqBuf.WriteString(strconv.Itoa(len(jsonBody)))
	reqBuf.WriteString("\r\n\r\n")
	reqBuf.Write(jsonBody)

	if err := writeAll(conn, reqBuf.Bytes()); err != nil {
		return 0, err
	}
	status, _, _, rErr := readHTTPResponse(bufio.NewReaderSize(conn, 4096))
	return status, rErr
}

func registerPort(dialAddr, hostHeader, basePath string, port int) {
	var payloadBuf [32]byte
	payload := append(payloadBuf[:0], `{"port":`...)
	payload = strconv.AppendInt(payload, int64(port), 10)
	payload = append(payload, '}')

	backoff := 25 * time.Millisecond
	for {
		status, err := envRegisterOnce(dialAddr, hostHeader, basePath+"/register", payload)
		if err == nil && (status == 200 || status == 201 || status == 204 || status == 409) {
			return
		}
		time.Sleep(backoff)
		if backoff < 300*time.Millisecond {
			backoff *= 2
		}
	}
}

func waitUntilDone(dialAddr, hostHeader, basePath string, stopCh <-chan os.Signal, shutdown func()) {
	defer shutdown()

	select {
	case <-stopCh:
		return
	case <-time.After(500 * time.Millisecond):
	}

	reqBytes := []byte("GET " + basePath + "/done HTTP/1.1\r\nHost: " + hostHeader + "\r\nConnection: keep-alive\r\n\r\n")
	var conn net.Conn
	var br *bufio.Reader
	closeClient := func() {
		if conn != nil {
			_ = conn.Close()
			conn = nil
			br = nil
		}
	}
	defer closeClient()

	for {
		select {
		case <-stopCh:
			return
		default:
		}

		if conn == nil {
			c, err := net.DialTimeout("tcp", dialAddr, 3*time.Second)
			if err == nil {
				if tcp, ok := c.(*net.TCPConn); ok {
					_ = tcp.SetNoDelay(true)
				}
				conn = c
				br = bufio.NewReaderSize(c, 4096)
			}
		}

		if conn != nil {
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			if err := writeAll(conn, reqBytes); err != nil {
				closeClient()
			} else {
				status, body, closeAfter, rErr := readHTTPResponse(br)
				if rErr != nil || closeAfter {
					closeClient()
				}
				if rErr == nil && status == 200 {
					if bytes.Contains(body, []byte(`"done":true`)) || bytes.Contains(body, []byte(`"done": true`)) {
						return
					}
				}
			}
		}

		select {
		case <-stopCh:
			return
		case <-time.After(1500 * time.Millisecond):
		}
	}
}

func main() {
	runtime.GOMAXPROCS(2)
	debug.SetGCPercent(-1)
	debug.SetMemoryLimit(768 << 20)

	rawEnv := os.Getenv("MAKEFUN_ENV_URL")
	if strings.TrimSpace(rawEnv) == "" {
		os.Stderr.WriteString("MAKEFUN_ENV_URL is required\n")
		os.Exit(1)
	}
	hostHeader, dialAddr, basePath, err := parseEnvEndpoint(rawEnv)
	if err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}

	expectedAuth := buildExpectedAuth(os.Getenv("MAKEFUN_SHOP_TOKEN"))
	viewerLogin := os.Getenv("MAKEFUN_FORGE_LOGIN")
	if strings.TrimSpace(viewerLogin) == "" {
		viewerLogin = "adaiversen"
	}
	store := NewForgeStore(viewerLogin, os.Getenv("MAKEFUN_TRIAL_NOW"))

	port, runServer, shutdown, err := StartServer(store, expectedAuth)
	if err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}

	stopCh := make(chan os.Signal, 2)
	signal.Notify(stopCh, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		registerPort(dialAddr, hostHeader, basePath, port)
		waitUntilDone(dialAddr, hostHeader, basePath, stopCh, shutdown)
	}()

	runServer()
	os.Exit(0)
}
