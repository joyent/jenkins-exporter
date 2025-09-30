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
	Class                  string   `json:"_class"`
	WaitingTimeMillis      int64    `json:"waitingTimeMillis"`
	BuildableTimeMillis    int64    `json:"buildableTimeMillis"`
	BlockedTimeMillis      int64    `json:"blockedTimeMillis"`
	ExecutingTimeMillis    int64    `json:"executingTimeMillis"`
	BuildingDurationMillis int64    `json:"buildingDurationMillis"`
	RemoteUrls             []string `json:"remoteUrls"`
	LastBuiltRevision      *struct {
		SHA1   string `json:"SHA1"`
		Branch []struct {
			Name string `json:"name"`
		} `json:"branch"`
	} `json:"lastBuiltRevision"`
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
	// Git metadata extracted from build actions
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

// FolderPath returns the full folder path (everything except the leaf job name)
func (b *Build) FolderPath() string {
	fullName := b.FullJobName()
	// Find the last slash to separate folder path from job name
	lastSlash := strings.LastIndex(fullName, "/")
	if lastSlash == -1 {
		// No folders, just a root-level job
		return ""
	}
	return fullName[:lastSlash]
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
	const gitClass = "hudson.plugins.git.util.BuildData"

	var metricsFound bool
	var b Build

	for _, a := range rawBuild.Actions {
		if a == nil {
			continue
		}

		// Extract metrics data
		if a.Class == metricClass {
			intID, err := strconv.Atoi(rawBuild.ID)
			if err != nil {
				return nil, fmt.Errorf("could not convert id '%s' to int64", rawBuild.ID)
			}

			b = Build{
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
			metricsFound = true
		}

		// Extract Git data
		if a.Class == gitClass {
			if len(a.RemoteUrls) > 0 {
				b.GitURL = a.RemoteUrls[0]
				b.RepoName = extractRepoName(a.RemoteUrls[0])
			}
			if a.LastBuiltRevision != nil {
				b.GitCommit = a.LastBuiltRevision.SHA1
				if len(a.LastBuiltRevision.Branch) > 0 {
					// Remove "refs/remotes/origin/" prefix
					branchName := a.LastBuiltRevision.Branch[0].Name
					b.GitBranch = strings.TrimPrefix(branchName, "refs/remotes/origin/")
				}
			}
		}
	}

	if !metricsFound {
		return nil, errors.New("could not find metrics in Actions slice")
	}

	return &b, nil
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
	const queryBuilds = "builds[id,result,building,actions[_class,buildableTimeMillis,waitingTimeMillis,blockedTimeMillis,executingTimeMillis,buildingDurationMillis,remoteUrls,lastBuiltRevision[SHA1,branch[name]]]]"

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
