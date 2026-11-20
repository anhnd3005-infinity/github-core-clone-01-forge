package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
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

func envHTTPExchange(method, dialAddr, hostHeader, reqPath string, jsonBody []byte) (int, []byte, error) {
	conn, err := net.DialTimeout("tcp", dialAddr, 3*time.Second)
	if err != nil {
		return 0, nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	var reqBuf bytes.Buffer
	reqBuf.WriteString(method)
	reqBuf.WriteByte(' ')
	reqBuf.WriteString(reqPath)
	reqBuf.WriteString(" HTTP/1.1\r\nHost: ")
	reqBuf.WriteString(hostHeader)
	reqBuf.WriteString("\r\nConnection: close\r\n")
	if len(jsonBody) > 0 {
		reqBuf.WriteString("Content-Type: application/json\r\nContent-Length: ")
		reqBuf.WriteString(strconv.Itoa(len(jsonBody)))
		reqBuf.WriteString("\r\n\r\n")
		reqBuf.Write(jsonBody)
	} else {
		reqBuf.WriteString("\r\n")
	}

	if err := writeAll(conn, reqBuf.Bytes()); err != nil {
		return 0, nil, err
	}

	rawResp, err := io.ReadAll(conn)
	if err != nil && len(rawResp) == 0 {
		return 0, nil, err
	}
	nl := bytes.IndexByte(rawResp, '\n')
	if nl < 0 {
		return 0, nil, fmt.Errorf("malformed env HTTP response")
	}
	statusLine := string(bytes.TrimSpace(rawResp[:nl]))
	parts := strings.SplitN(statusLine, " ", 3)
	if len(parts) < 2 {
		return 0, nil, fmt.Errorf("invalid status line %q", statusLine)
	}
	status, _ := strconv.Atoi(parts[1])
	sep := bytes.Index(rawResp, []byte("\r\n\r\n"))
	bodyOffset := sep + 4
	if sep < 0 {
		sep = bytes.Index(rawResp, []byte("\n\n"))
		bodyOffset = sep + 2
	}
	if sep < 0 || bodyOffset > len(rawResp) {
		return status, nil, nil
	}
	return status, rawResp[bodyOffset:], nil
}

func registerPort(dialAddr, hostHeader, basePath string, port int) {
	payload, _ := json.Marshal(map[string]int{"port": port})
	backoff := 25 * time.Millisecond
	for {
		status, _, err := envHTTPExchange("POST", dialAddr, hostHeader, basePath+"/register", payload)
		if err == nil && (status == 200 || status == 201 || status == 204 || status == 409) {
			return
		}
		time.Sleep(backoff)
		if backoff < 300*time.Millisecond {
			backoff *= 2
		}
	}
}

func waitUntilDone(dialAddr, hostHeader, basePath string, stopCh <-chan os.Signal) {
	backoff := 20 * time.Millisecond
	for {
		select {
		case <-stopCh:
			return
		default:
		}
		status, body, err := envHTTPExchange("GET", dialAddr, hostHeader, basePath+"/done", nil)
		if err == nil && status == 200 {
			if bytes.Contains(body, []byte(`"done":true`)) || bytes.Contains(body, []byte(`"done": true`)) {
				return
			}
		}
		select {
		case <-stopCh:
			return
		case <-time.After(backoff):
		}
		if backoff < 150*time.Millisecond {
			backoff = time.Duration(float64(backoff) * 1.5)
		}
	}
}

func main() {
	debug.SetGCPercent(1000)
	debug.SetMemoryLimit(512 << 20)

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

	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	expectedToken := []byte(os.Getenv("MAKEFUN_SHOP_TOKEN"))
	viewerLogin := os.Getenv("MAKEFUN_FORGE_LOGIN")
	if strings.TrimSpace(viewerLogin) == "" {
		viewerLogin = "adaiversen"
	}
	store := NewForgeStore(viewerLogin, os.Getenv("MAKEFUN_TRIAL_NOW"))

	go ServeRawTCP(ln, store, expectedToken)

	stopCh := make(chan os.Signal, 2)
	signal.Notify(stopCh, syscall.SIGTERM, syscall.SIGINT)

	registerPort(dialAddr, hostHeader, basePath, port)
	waitUntilDone(dialAddr, hostHeader, basePath, stopCh)

	_ = ln.Close()
	os.Exit(0)
}
