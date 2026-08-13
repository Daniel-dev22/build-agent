package main

import (
	"context"
	"net/http"
	"runtime"
	"time"

	kitfleet "github.com/Daniel-dev22/agent-kit-go/fleet"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

type hostInfo struct {
	Name   string `json:"name"`
	Site   string `json:"site"`
	NumCPU int    `json:"num_cpu"`
}

// buildSnapshot is what a dashboard renders per host: who this agent is, plus
// its current and recent builds.
type buildSnapshot struct {
	Host   hostInfo      `json:"host"`
	Builds []buildPublic `json:"builds"`
}

type fleetEnvelope struct {
	Type string        `json:"type"` // "snapshot"
	Data buildSnapshot `json:"data"`
}

type fleetHub struct {
	hub *kitfleet.Hub[fleetEnvelope]
}

func newFleetHub(a *app) *fleetHub {
	build := func(_ context.Context) (fleetEnvelope, error) {
		return fleetEnvelope{
			Type: "snapshot",
			Data: buildSnapshot{
				Host:   hostInfo{Name: a.cfg.NodeName, Site: a.cfg.SiteID, NumCPU: runtime.NumCPU()},
				Builds: a.reg.list(),
			},
		}, nil
	}
	return &fleetHub{hub: kitfleet.NewHub(build, 0)}
}

func (f *fleetHub) Run(ctx context.Context) { f.hub.Run(ctx) }
func (f *fleetHub) Trigger()                { f.hub.Trigger() }

var fleetUpgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

// handleFleetWS streams a greeting snapshot then live coalesced snapshots.
func (a *app) handleFleetWS(c *gin.Context) {
	conn, err := fleetUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	ch, unsub := a.fleet.hub.Subscribe()
	defer unsub()

	if snap, err := a.fleet.hub.SnapshotNow(c.Request.Context()); err == nil {
		_ = conn.WriteJSON(snap)
	}

	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	// drain client control frames so close is detected
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				conn.Close()
				return
			}
		}
	}()
	for {
		select {
		case snap, ok := <-ch:
			if !ok {
				return
			}
			if err := conn.WriteJSON(snap); err != nil {
				return
			}
		case <-ping.C:
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
				return
			}
		}
	}
}
