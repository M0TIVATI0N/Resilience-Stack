package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func serve(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "d.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return New(socket)
}

func frame(stream byte, text string) []byte {
	header := make([]byte, 8)
	header[0] = stream
	binary.BigEndian.PutUint32(header[4:], uint32(len(text)))
	return append(header, text...)
}

func TestInspectParsesState(t *testing.T) {
	c := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/containers/app-blue/json" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"Name":"/app-blue","Config":{"Image":"repo:abc"},
			"State":{"Status":"running","OOMKilled":true,"Health":{"Status":"healthy"}}}`))
	}))
	info, err := c.Inspect(context.Background(), "app-blue")
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "app-blue" || info.Image != "repo:abc" || info.Health != "healthy" || !info.OOMKilled {
		t.Fatalf("info = %+v", info)
	}
	if _, err := c.Inspect(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestStartAndStopAreIdempotent(t *testing.T) {
	c := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	if err := c.Start(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if err := c.Stop(context.Background(), "x", 5); err != nil {
		t.Fatal(err)
	}
}

func TestErrorsIncludeDaemonMessage(t *testing.T) {
	c := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"no such image"}`, http.StatusInternalServerError)
	}))
	err := c.Start(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "no such image") {
		t.Fatalf("err = %v", err)
	}
}

func TestExecReturnsExitCodeAndOutput(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /containers/rs-nginx/exec", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"Id":"e1"}`))
	})
	mux.HandleFunc("POST /exec/e1/start", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(append(frame(1, "out "), frame(2, "err")...))
	})
	mux.HandleFunc("GET /exec/e1/json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ExitCode":1}`))
	})
	code, output, err := serve(t, mux).Exec(context.Background(), "rs-nginx", "nginx", "-t")
	if err != nil {
		t.Fatal(err)
	}
	if code != 1 || output != "out err" {
		t.Fatalf("code=%d output=%q", code, output)
	}
}

func TestDemuxRejectsTruncatedFrame(t *testing.T) {
	raw := frame(1, "hello")
	if _, err := demux(bytes.NewReader(raw[:len(raw)-2])); err == nil {
		t.Fatal("expected an error for a truncated frame")
	}
}

func TestEventsDecodesStreamAndSendsFilters(t *testing.T) {
	var filters map[string][]string
	c := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.Unmarshal([]byte(r.URL.Query().Get("filters")), &filters)
		_, _ = w.Write([]byte(`{"Action":"oom","Actor":{"Attributes":{"name":"app-green","resilience.version":"bad"}}}` + "\n"))
	}))

	var got []Event
	err := c.Events(context.Background(), "resilience.role=app", []string{"oom", "die"}, func(e Event) { got = append(got, e) })
	if err == nil {
		t.Fatal("Events must report the closed stream")
	}
	if len(got) != 1 || got[0].Action != "oom" || got[0].Name != "app-green" || got[0].Labels["resilience.version"] != "bad" {
		t.Fatalf("events = %+v", got)
	}
	if filters["label"][0] != "resilience.role=app" || len(filters["event"]) != 2 {
		t.Fatalf("filters = %v", filters)
	}
}
