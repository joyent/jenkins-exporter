package jenkins

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// https://github.com/jenkinsci/metrics-plugin/blob/master/src/main/java/jenkins/metrics/impl/TimeInQueueAction.java#L85
type actionRawResp struct {
	Class                  string `json:"_class"`
	WaitingTimeMillis      int64  `json:"waitingTimeMillis"`
	BuildableTimeMillis    int64  `json:"buildableTimeMillis"`
	BlockedTimeMillis      int64  `json:"blockedTimeMillis"`
	ExecutingTimeMillis    int64  `json:"executingTimeMillis"`
	BuildingDurationMillis int64  `json:"buildingDurationMillis"`
}

type buildRawResp struct {
	ID       string           `json:"id"`
	Actions  []*actionRawResp `json:"actions"`
	Result   string           `json:"result"`
	Building *bool            `json:"building"`
}

type jobRawResp struct {
	Name              string          `json:"name"`
	FullName          string          `json:"fullName"`
	WorkflowJobBuilds []*buildRawResp `json:"builds"`
	MultiBranchJobs   []*jobRawResp   `json:"jobs"`
}

type respRaw struct {
	Jobs []*jobRawResp `json:"jobs"`
}

type Build struct {
	JobName            string
	MultiBranchJobName string
	FullName           string // Full hierarchical path
	ID                 int64
	BuildableTime      time.Duration
	WaitingTime        time.Duration
	BlockedTime        time.Duration
	ExecutingTime      time.Duration
	BuildingDuration   time.Duration
	Result             string
	Building           bool
	// Build-level environment variables
	GitURL       string
	GitBranch    string
	GitCommit    string
	RepoName     string
}

func (b *Build) FullJobName() string {
	// Use FullName from Jenkins API if available
	if b.FullName != "" {
		return b.FullName
	}
	// Fallback to legacy behavior for backwards compatibility
	if b.MultiBranchJobName != "" {
		return b.MultiBranchJobName + "/" + b.JobName
	}
	return b.JobName
}

func (b *Build) String() string {
	return b.FullJobName() + " #" + fmt.Sprint(b.ID)
}

func (b *buildRawResp) validate() error {
	if b.Building == nil {
		return errors.New("Building field is missing (nil)")
	}

	if !*b.Building && b.Result == "" {
		return errors.New("Building is false but build result is empty")
	}

	return nil
}

func (c *Client) buildRawToBuild(workflowJobName, multibranchJobName, fullName string, rawBuild *buildRawResp) (*Build, error) {
	const metricClass = "jenkins.metrics.impl.TimeInQueueAction"

	for _, a := range rawBuild.Actions {
		if a.Class != metricClass {
			continue
		}

		intID, err := strconv.Atoi(rawBuild.ID)
		if err != nil {
			return nil, fmt.Errorf("could not convert id '%s' to int64", rawBuild.ID)
		}

		b := Build{
			JobName:            workflowJobName,
			MultiBranchJobName: multibranchJobName,
			FullName:           fullName,
			ID:                 int64(intID),
			BuildableTime:      time.Duration(a.BuildableTimeMillis) * time.Millisecond,
			WaitingTime:        time.Duration(a.WaitingTimeMillis) * time.Millisecond,
			BlockedTime:        time.Duration(a.BlockedTimeMillis) * time.Millisecond,
			ExecutingTime:      time.Duration(a.ExecutingTimeMillis) * time.Millisecond,
			BuildingDuration:   time.Duration(a.BuildingDurationMillis) * time.Millisecond,
			Result:             rawBuild.Result,
			Building:           *rawBuild.Building,
		}

		return &b, nil
	}

	return nil, errors.New("could not find metrics in Actions slice")
}

// processJobs recursively processes jobs at any nesting level
func (c *Client) processJobs(jobs []*jobRawResp, parentFullName string, parentName string) []*Build {
	var res []*Build

	for _, job := range jobs {
		// Determine the full path for this job
		fullName := job.FullName
		if fullName == "" {
			// Fallback: build path from parent
			if parentFullName != "" {
				fullName = parentFullName + "/" + job.Name
			} else {
				fullName = job.Name
			}
		}

		// Process builds if this job has any (it's a buildable job/leaf node)
		for _, rawBuild := range job.WorkflowJobBuilds {
			if err := rawBuild.validate(); err != nil {
				c.logger.Printf("skipping build %s/%s: %s", fullName, rawBuild.ID, err)
				continue
			}

			// Determine MultiBranchJobName (parent) and JobName (current)
			multiBranchJobName := parentName
			jobName := job.Name

			b, err := c.buildRawToBuild(jobName, multiBranchJobName, fullName, rawBuild)
			if err != nil {
				c.logger.Printf("skipping build %s/%s: %s", fullName, rawBuild.ID, err)
				continue
			}

			res = append(res, b)
		}

		// Recursively process nested jobs (folders/multibranch projects)
		if len(job.MultiBranchJobs) > 0 {
			nestedBuilds := c.processJobs(job.MultiBranchJobs, fullName, job.Name)
			res = append(res, nestedBuilds...)
		}
	}

	return res
}

func (c *Client) respRawToBuilds(raw *respRaw) []*Build {
	return c.processJobs(raw.Jobs, "", "")
}

func (c *Client) Builds() ([]*Build, error) {
	// TODO: is it possible to retrieve only the element in actions with
	// _class = "jenkins.metrics.impl.TimeInQueueAction" that contains the
	// metrics?
	const queryBuilds = "builds[id,result,building,actions[_class,buildableTimeMillis,waitingTimeMillis,blockedTimeMillis,executingTimeMillis,buildingDurationMillis]]"

	// Extended tree query to support up to 4 levels of folder nesting
	// This handles structures like: Folder/Subfolder/Project/Branch (up to 4 levels deep)
	const endpoint = "api/json" +
		"?tree=jobs[name,fullName," +
		"jobs[name,fullName," +
		"jobs[name,fullName," +
		"jobs[name,fullName," + queryBuilds + "]," + queryBuilds +
		"]," + queryBuilds +
		"]," + queryBuilds +
		"]"

	var resp respRaw
	err := c.do("GET", c.serverURL+endpoint, &resp)
	if err != nil {
		return nil, err
	}

	builds := c.respRawToBuilds(&resp)

	return builds, nil
}

// BuildEnvVars represents the environment variables for a specific build
type buildEnvVarsResp struct {
	EnvMap map[string]string `json:"envMap"`
}

// extractRepoName extracts repository name from Git URL
func extractRepoName(gitURL string) string {
	if gitURL == "" {
		return ""
	}

	// Remove .git suffix if present
	url := strings.TrimSuffix(gitURL, ".git")

	// Handle different URL formats:
	// https://github.com/org/repo -> repo
	// git@github.com:org/repo -> repo
	var parts []string
	if strings.Contains(url, "/") {
		parts = strings.Split(url, "/")
	} else if strings.Contains(url, ":") {
		// Handle SSH format git@host:org/repo
		colonIndex := strings.LastIndex(url, ":")
		if colonIndex != -1 {
			path := url[colonIndex+1:]
			parts = strings.Split(path, "/")
		}
	}

	if len(parts) > 0 {
		return parts[len(parts)-1]
	}

	return ""
}

// BuildEnvVars fetches environment variables for a specific build
func (c *Client) BuildEnvVars(jobName, multiBranchJobName string, buildID int64) (*buildEnvVarsResp, error) {
	var endpoint string
	if multiBranchJobName != "" {
		// Multibranch pipeline: /job/{multibranch-job}/job/{branch-job}/{build-id}/injectedEnvVars/api/json
		endpoint = fmt.Sprintf("job/%s/job/%s/%d/injectedEnvVars/api/json", multiBranchJobName, jobName, buildID)
	} else {
		// Regular pipeline: /job/{job-name}/{build-id}/injectedEnvVars/api/json
		endpoint = fmt.Sprintf("job/%s/%d/injectedEnvVars/api/json", jobName, buildID)
	}

	var resp buildEnvVarsResp
	err := c.do("GET", c.serverURL+endpoint, &resp)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch environment variables for build %s/%d: %w", jobName, buildID, err)
	}

	return &resp, nil
}

// EnrichBuildWithEnvVars adds environment variable information to a build
func (c *Client) EnrichBuildWithEnvVars(build *Build) error {
	envVars, err := c.BuildEnvVars(build.JobName, build.MultiBranchJobName, build.ID)
	if err != nil {
		// Log error but don't fail the entire build processing
		c.logger.Printf("Warning: could not fetch environment variables for build %s: %v", build.String(), err)
		return nil
	}

	// Extract Git-related environment variables
	if gitURL, ok := envVars.EnvMap["GIT_URL"]; ok {
		build.GitURL = gitURL
		build.RepoName = extractRepoName(gitURL)
	}
	if gitBranch, ok := envVars.EnvMap["GIT_BRANCH"]; ok {
		build.GitBranch = gitBranch
	}
	if gitCommit, ok := envVars.EnvMap["GIT_COMMIT"]; ok {
		build.GitCommit = gitCommit
	}

	return nil
}
