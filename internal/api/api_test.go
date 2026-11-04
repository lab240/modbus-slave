package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lab240/modbus-slave/internal/fleet"
)

func setup(t *testing.T, token string) (*httptest.Server, *fleet.Fleet) {
	ctx, cancel := context.WithCancel(context.Background())
	link := filepath.Join(t.TempDir(), "ttyEMU")
	f, err := fleet.New(ctx, fleet.Options{Crazy: fleet.DefaultCrazy}, []fleet.Sensor{
		{Profile: "kth2r", Unit: 1, Transport: "rtu", Port: link, Pty: true, Baud: 9600, Mode: "8N1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(New(Info{Version: "test"}, f, 20, token).Handler())
	t.Cleanup(func() { ts.Close(); cancel(); f.Wait() })
	return ts, f
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

func list(t *testing.T, url string) []map[string]any {
	t.Helper()
	resp, err := http.Get(url + "/api/v1/sensors")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out []map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return out
}

func TestSensors(t *testing.T) {
	ts, f := setup(t, "")
	u := ts.URL + "/api/v1/sensors"
	if s := list(t, ts.URL); len(s) != 1 || s[0]["profile"] != "kth2r" || s[0]["live"] == nil {
		t.Fatalf("list %v", s)
	}
	resp, err := http.Get(u + "?values=1")
	if err != nil {
		t.Fatal(err)
	}
	var withVals []map[string]any
	json.NewDecoder(resp.Body).Decode(&withVals)
	resp.Body.Close()
	if v, ok := withVals[0]["values"].([]any); !ok || len(v) != 2 || v[0].(map[string]any)["addr"] != 768.0 { // T and RH
		t.Fatalf("values %v", withVals[0]["values"])
	}
	port := f.List()[0].Port
	// a second sensor on the same port: the same bus
	code, out := call(t, "POST", u, `{"profile": "generic", "unit": 2, "transport": "rtu", "port": "`+port+`", "baud": 9600}`, "")
	if code != 201 {
		t.Fatalf("add %d %v", code, out)
	}
	if code, out := call(t, "POST", u, `{"profile": "kth2r", "unit": 2, "transport": "rtu", "port": "`+port+`", "baud": 9600}`, ""); code != 422 {
		t.Fatalf("same address twice: %d %v", code, out)
	}
	if code, out := call(t, "POST", u, `{"profile": "nope", "unit": 3}`, ""); code != 422 {
		t.Fatalf("bad template: %d %v", code, out)
	}
	if len(f.Lines()) != 1 {
		t.Fatalf("lines %d, want 1 bus", len(f.Lines()))
	}
	// new speed on one sensor: the other one on that bus too
	code, out = call(t, "PUT", u+"/1", `{"baud": 19200, "mode": "8e1"}`, "")
	if code != 200 || len(out["also"].([]any)) != 1 || f.Get(2).Baud != 19200 || f.Get(2).Mode != "8E1" {
		t.Fatalf("speed %d %v %+v", code, out, f.Get(2))
	}
	// the speed code register follows
	code, out = call(t, "GET", u+"/1/registers?addr=0x0D&count=2", "", "")
	if v := out["values"].([]any); code != 200 || v[0] != 4.0 || v[1] != 1.0 {
		t.Fatalf("speed/parity code %d %v", code, out)
	}
	// sensor 2 to Modbus TCP: a second line
	if code, out := call(t, "PUT", u+"/2", `{"transport": "tcp", "listen": "127.0.0.1:15502"}`, ""); code != 200 {
		t.Fatalf("tcp %d %v", code, out)
	}
	if len(f.Lines()) != 2 {
		t.Fatalf("lines %d, want 2", len(f.Lines()))
	}
	if code, out := call(t, "PUT", u+"/1", `{"crazy": true}`, ""); code != 200 || out["sensor"].(map[string]any)["crazy"] != true {
		t.Fatalf("crazy %d %v", code, out)
	}
	if code, out := call(t, "PUT", u+"/1", `{"id": 7}`, ""); code != 422 {
		t.Fatalf("id is not editable: %d %v", code, out)
	}
	if code, out := call(t, "PUT", u+"/1", `{"port": "/dev/other"}`, ""); code != 422 {
		t.Fatalf("pty port changed: %d %v", code, out)
	}
	// registers: read, write a writable one, refuse a read-only one
	code, out = call(t, "GET", u+"/1/map", "", "")
	if code != 200 || out["name"] != "kth2r" || len(out["holding"].([]any)) != 10 {
		t.Fatalf("map %d %v", code, out)
	}
	if code, _ := call(t, "PUT", u+"/1/registers", `{"addr": 1279, "values": [-50]}`, ""); code != 200 {
		t.Fatalf("write %d", code)
	}
	if code, _ := call(t, "PUT", u+"/1/registers", `{"addr": 768, "values": [1]}`, ""); code != 409 {
		t.Fatalf("write read-only %d", code)
	}
	code, out = call(t, "PUT", ts.URL+"/api/v1/crazy", `{"drop": 0.5}`, "")
	if code != 200 || out["drop"] != 0.5 || f.Crazy().Drop != 0.5 {
		t.Fatalf("crazy faults %d %v", code, out)
	}
	if code, _ := call(t, "PUT", ts.URL+"/api/v1/crazy", `{"drop": 2}`, ""); code != 422 {
		t.Fatalf("drop 2: %d", code)
	}
	if code, _ := call(t, "DELETE", u+"/2", "", ""); code != 204 || len(f.List()) != 1 || len(f.Lines()) != 1 {
		t.Fatalf("delete %d %d", code, len(f.List()))
	}
	if code, out := call(t, "POST", u, `{"profile": "generic", "unit": 9, "transport": "tcp", "listen": "127.0.0.1:15503"}`, ""); code != 201 || out["id"] != 2.0 {
		t.Fatalf("a new sensor takes the free number 2: %d %v", code, out)
	}
	call(t, "DELETE", u+"/2", "", "")
	code, out = call(t, "GET", ts.URL+"/api/v1/status", "", "")
	if code != 200 || out["version"] != "test" || len(out["lines"].([]any)) != 1 || out["running"] != true {
		t.Fatalf("status %d %v", code, out)
	}
	// one sensor stopped: its bus closes; a duplicate address is allowed while it is stopped
	if code, out := call(t, "PUT", u+"/1", `{"stopped": true}`, ""); code != 200 || len(f.Lines()) != 0 {
		t.Fatalf("stop sensor %d %v lines %d", code, out, len(f.Lines()))
	}
	if code, out := call(t, "POST", u, `{"profile": "kth2r", "unit": 1, "transport": "rtu", "port": "`+port+`", "baud": 19200, "mode": "8E1"}`, ""); code != 201 {
		t.Fatalf("add while the other is stopped %d %v", code, out)
	}
	if code, _ := call(t, "PUT", u+"/1", `{"stopped": false}`, ""); code != 422 {
		t.Fatalf("start with a duplicate address: %d", code)
	}
	// the whole emulation
	if code, out := call(t, "PUT", ts.URL+"/api/v1/service", `{"enabled": true}`, ""); code != 409 {
		t.Fatalf("enable outside systemd %d %v", code, out)
	}
	if code, out := call(t, "PUT", ts.URL+"/api/v1/service", `{"running": false}`, ""); code != 200 || out["running"] != false || len(f.Lines()) != 0 {
		t.Fatalf("stop all %d %v", code, out)
	}
	if code, out := call(t, "PUT", ts.URL+"/api/v1/service", `{"running": true}`, ""); code != 200 || out["running"] != true || len(f.Lines()) != 1 {
		t.Fatalf("start all %d %v lines %d", code, out, len(f.Lines()))
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
	resp, err := http.Get(ts.URL + "/") // the page itself needs no token
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("page: %v %v", err, resp.StatusCode)
	}
	resp.Body.Close()
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
