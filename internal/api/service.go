package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"
)

// unitName is the systemd unit we run in ("modbus-slave.service"), "" when not started by systemd.
func unitName() string {
	if os.Getenv("INVOCATION_ID") == "" {
		return ""
	}
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		for p := parts[2]; p != "/" && p != "."; p = path.Dir(p) {
			if b := path.Base(p); strings.HasSuffix(b, ".service") {
				return b
			}
		}
	}
	return ""
}

func systemctl(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

type serviceState struct {
	Running bool   `json:"running"`           // the emulation answers (false: every port closed, the page stays)
	Unit    string `json:"unit,omitempty"`    // the systemd unit, "" when not run by systemd
	Enabled *bool  `json:"enabled,omitempty"` // the unit starts with the system
}

func (a *API) serviceState() serviceState {
	st := serviceState{Running: a.f.Running(), Unit: a.unit}
	if a.unit != "" {
		out, _ := systemctl("is-enabled", a.unit) // exit code 1 for "disabled"
		on := out == "enabled" || out == "enabled-runtime"
		st.Enabled = &on
	}
	return st
}

func (a *API) getService(w http.ResponseWriter, r *http.Request) { reply(w, a.serviceState()) }

// putService: {"running": false} stops the emulation, {"enabled": true} = systemctl enable.
func (a *API) putService(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Running *bool `json:"running"`
		Enabled *bool `json:"enabled"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || (in.Running == nil && in.Enabled == nil) {
		fail(w, http.StatusUnprocessableEntity, `want {"running": true|false} and/or {"enabled": true|false}`)
		return
	}
	if in.Enabled != nil {
		if a.unit == "" {
			fail(w, http.StatusConflict, "not run by systemd: start with the system is set up there")
			return
		}
		verb := "disable"
		if *in.Enabled {
			verb = "enable"
		}
		if out, err := systemctl(verb, a.unit); err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) || out != "" {
				err = fmt.Errorf("systemctl %s %s: %s", verb, a.unit, strings.TrimSpace(out+" "+err.Error()))
			}
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		log.Printf("api: systemctl %s %s", verb, a.unit)
	}
	if in.Running != nil {
		if err := a.f.SetRunning(*in.Running); err != nil {
			fail(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
	}
	reply(w, a.serviceState())
}
