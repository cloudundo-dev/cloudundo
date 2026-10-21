// Command awsproxy stands in for the aws CLI in end-to-end tests. It sends
// its arguments and AWS_* settings to the test's fake AWS over HTTP
// (CLOUDUNDO_FAKE_AWS_URL) and prints the answer with its exit code, so
// cloudundo can be run as a real program against the fake.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// Request and Response are the wire format shared with the test server.
type Request struct {
	Args []string `json:"args"`
	Env  []string `json:"env"`
}

type Response struct {
	Stdout []byte `json:"stdout"`
	Stderr string `json:"stderr"`
	Code   int    `json:"code"`
}

func main() {
	var env []string
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "AWS_") {
			env = append(env, e)
		}
	}
	body, _ := json.Marshal(Request{Args: os.Args[1:], Env: env})
	resp, err := http.Post(os.Getenv("CLOUDUNDO_FAKE_AWS_URL"), "application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintln(os.Stderr, "awsproxy:", err)
		os.Exit(255)
	}
	defer resp.Body.Close()
	var r Response
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		fmt.Fprintln(os.Stderr, "awsproxy:", err)
		os.Exit(255)
	}
	os.Stdout.Write(r.Stdout)
	os.Stderr.WriteString(r.Stderr)
	os.Exit(r.Code)
}
