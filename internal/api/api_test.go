package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lab240/modbus-slave/internal/device"
	"github.com/lab240/modbus-slave/internal/profile"
	"github.com/lab240/modbus-slave/internal/server"
)

func setup(t *testing.T, token string) (*httptest.Server, *server.Line) {
	p, err := profile.Load("kth2r")
	if err != nil {
		t.Fatal(err)
	}
	l := server.NewLine(0, "0", []*device.Device{profile.Build(p, profile.Env{Unit: 1, Baud: 57600, Parity: 'N'})},
		server.Faults{Delay: 5 * time.Millisecond}, 20*time.Millisecond, 1)
	ts := httptest.NewServer(New(Info{Version: "test", Profile: "kth2r"}, []*server.Line{l}, token).Handler())
	t.Cleanup(ts.Close)
	return ts, l
}

func call(t *testing.T, method, url, body, token string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out map[string]any
	json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

func TestAPI(t *testing.T) {
	ts, l := setup(t, "")
	code, st := call(t, "GET", ts.URL+"/api/v1/status", "", "")
	if code != 200 || st["profile"] != "kth2r" || len(st["lines"].([]any)) != 1 {
		t.Fatalf("status %d %v", code, st)
	}
	code, f := call(t, "PUT", ts.URL+"/api/v1/lines/0/faults", `{"drop": 0.5, "jitter_ms": 10}`, "")
	if code != 200 || f["drop"] != 0.5 || f["delay_ms"] != 5.0 || f["jitter_ms"] != 10.0 {
		t.Fatalf("faults %d %v", code, f)
	}
	if got := l.Faults(); got.Drop != 0.5 || got.Jitter != 10*time.Millisecond || got.Delay != 5*time.Millisecond {
		t.Fatalf("line faults %+v", got)
	}
	if code, _ := call(t, "PUT", ts.URL+"/api/v1/lines/0/faults", `{"drop": 2}`, ""); code != 422 {
		t.Fatalf("drop 2: %d", code)
	}
	if code, _ := call(t, "PUT", ts.URL+"/api/v1/lines/0/faults", `{"dorp": 0.1}`, ""); code != 422 {
		t.Fatalf("unknown field: %d", code)
	}
	// write the T correction, read it back and see it in T
	if code, r := call(t, "PUT", ts.URL+"/api/v1/lines/0/devices/1/registers", `{"addr": 1279, "values": [-50]}`, ""); code != 200 {
		t.Fatalf("write corr: %d %v", code, r)
	}
	code, r := call(t, "GET", ts.URL+"/api/v1/lines/0/devices/1/registers?addr=0x04FF&count=1", "", "")
	if code != 200 || r["values"].([]any)[0] != float64(uint16(0xFFCE)) {
		t.Fatalf("read corr: %d %v", code, r)
	}
	if code, _ := call(t, "PUT", ts.URL+"/api/v1/lines/0/devices/1/registers", `{"addr": 0, "values": [1]}`, ""); code != 409 {
		t.Fatalf("write read-only: %d", code)
	}
	if code, _ := call(t, "GET", ts.URL+"/api/v1/lines/0/devices/7/registers?addr=0", "", ""); code != 404 {
		t.Fatalf("unit 7: %d", code)
	}
	if code, _ := call(t, "GET", ts.URL+"/api/v1/lines/3/faults", "", ""); code != 404 {
		t.Fatalf("line 3: %d", code)
	}
}

func TestToken(t *testing.T) {
	ts, _ := setup(t, "s3cret")
	if code, _ := call(t, "GET", ts.URL+"/api/v1/status", "", ""); code != 401 {
		t.Fatalf("no token: %d", code)
	}
	if code, _ := call(t, "GET", ts.URL+"/api/v1/status", "", "wrong"); code != 401 {
		t.Fatalf("wrong token: %d", code)
	}
	if code, _ := call(t, "GET", ts.URL+"/api/v1/status", "", "s3cret"); code != 200 {
		t.Fatalf("token: %d", code)
	}
}

func TestListen(t *testing.T) {
	if _, err := Listen("0.0.0.0:0", ""); err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("0.0.0.0 without token: %v", err)
	}
	l, err := Listen("127.0.0.1:0", "")
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
}
