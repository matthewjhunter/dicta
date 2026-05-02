package control

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// Send is a one-shot client: dial the socket at path, write cmd as a single
// NDJSON line, read one Response, close. Suitable for the dicta CLI.
func Send(path string, cmd Command, timeout time.Duration) (Response, error) {
	conn, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		return Response{}, fmt.Errorf("control: dial %s: %w", path, err)
	}
	defer conn.Close()

	if timeout > 0 {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}

	buf, err := json.Marshal(cmd)
	if err != nil {
		return Response{}, fmt.Errorf("control: marshal command: %w", err)
	}
	buf = append(buf, '\n')
	if _, err := conn.Write(buf); err != nil {
		return Response{}, fmt.Errorf("control: write: %w", err)
	}

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 4096), MaxLineBytes)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return Response{}, fmt.Errorf("control: read response: %w", err)
		}
		return Response{}, fmt.Errorf("control: read response: no response")
	}
	var resp Response
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		return Response{}, fmt.Errorf("control: decode response: %w", err)
	}
	return resp, nil
}
