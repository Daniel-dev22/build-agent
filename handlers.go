package main

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func (a *app) handleLiveness(c *gin.Context)  { c.JSON(http.StatusOK, gin.H{"status": "ok"}) }
func (a *app) handleReadiness(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ready"}) }

// handleTriggerBuild accepts a BuildRequest, starts the build in the background
// (detached from the request context per the long-running-spawn rule), and
// returns the job id for the caller to poll.
func (a *app) handleTriggerBuild(c *gin.Context) {
	var req BuildRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Descriptor.Project == "" || len(req.Descriptor.Services) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "descriptor.project and descriptor.services are required"})
		return
	}
	if req.Repo == "" || req.Registry == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "repo and registry are required"})
		return
	}
	if req.Descriptor.Version.Scheme == "external" && req.Version == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "version is required for external version scheme"})
		return
	}
	if req.TriggerKey == "" {
		req.TriggerKey = "ansible"
	}
	b := a.reg.start(context.Background(), req)
	c.JSON(http.StatusAccepted, gin.H{"job_id": b.ID, "state": b.snapshot().State})
}

func (a *app) handleListJobs(c *gin.Context) {
	c.JSON(http.StatusOK, a.reg.list())
}

func (a *app) handleGetJob(c *gin.Context) {
	id := c.Param("id")
	if b, ok := a.reg.get(id); ok {
		c.JSON(http.StatusOK, b.snapshot())
		return
	}
	if bp, ok, _ := a.events.getBuild(id); ok {
		c.JSON(http.StatusOK, bp)
		return
	}
	c.JSON(http.StatusNotFound, gin.H{"error": "job not found"})
}

func (a *app) handleGetJobLog(c *gin.Context) {
	id := c.Param("id")
	b, ok := a.reg.get(id)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "job not found or no live log"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": id, "lines": b.logLines()})
}

func (a *app) handleCancelJob(c *gin.Context) {
	if a.reg.cancelJob(c.Param("id")) {
		c.JSON(http.StatusOK, gin.H{"cancelled": true})
		return
	}
	c.JSON(http.StatusConflict, gin.H{"error": "job not cancellable (unknown or already terminal)"})
}

// handleJobLogsWS streams a job's log: backlog then live lines.
func (a *app) handleJobLogsWS(c *gin.Context) {
	conn, err := fleetUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	ch, backlog, unsub, ok := a.reg.subscribe(c.Param("id"))
	if !ok {
		_ = conn.WriteJSON(gin.H{"type": "error", "error": "job not found"})
		return
	}
	defer unsub()
	for _, line := range backlog {
		if conn.WriteJSON(gin.H{"type": "line", "line": line}) != nil {
			return
		}
	}
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				conn.Close()
				return
			}
		}
	}()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case line, open := <-ch:
			if !open {
				return
			}
			if conn.WriteJSON(gin.H{"type": "line", "line": line}) != nil {
				return
			}
		case <-ping.C:
			if conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)) != nil {
				return
			}
		}
	}
}
