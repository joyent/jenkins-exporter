package jenkins

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type respWFAPIRaw struct {
	Stages []*respWFAPIStageRaw `json:"stages"`
}

type respWFAPIStageRaw struct {
	Name           string `json:"name"`
	Status         string `json:"status"`
	DurationMillis int64  `json:"durationMillis"`
}

type Stage struct {
	Name     string
	Status   string
	Duration time.Duration
}

func respWFAPIRawToStage(raw *respWFAPIStageRaw) *Stage {
	return &Stage{
		Name:     raw.Name,
		Status:   raw.Status,
		Duration: time.Duration(raw.DurationMillis) * time.Millisecond,
	}
}

func respWFAPIRawToStages(resp *respWFAPIRaw) []*Stage {
	res := make([]*Stage, len(resp.Stages))
	for i, stage := range resp.Stages {
		res[i] = respWFAPIRawToStage(stage)
	}
	return res
}

func (c *Client) wfapiJobBuildURL(fullName string, buildID string) (string, error) {
	// Convert "A/B/C/D" to "job/A/job/B/job/C/job/D"
	var parts []string
	for _, part := range splitPath(fullName) {
		parts = append(parts, "job", part)
	}
	parts = append(parts, buildID, "wfapi")
	return url.JoinPath(c.serverURL, parts...)
}

func splitPath(fullName string) []string {
	if fullName == "" {
		return nil
	}
	// Split by "/" to handle multi-level paths
	var result []string
	for _, part := range strings.Split(fullName, "/") {
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}

func (c *Client) Stages(fullName string, buildID int64) ([]*Stage, error) {
	var resp respWFAPIRaw

	wfapiURL, err := c.wfapiJobBuildURL(fullName, fmt.Sprint(buildID))
	if err != nil {
		return nil, err
	}

	err = c.do(http.MethodGet, wfapiURL, &resp)
	if err != nil {
		return nil, err
	}

	return respWFAPIRawToStages(&resp), nil
}
