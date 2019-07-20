package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/sirupsen/logrus"

	apiCoreV1 "k8s.io/api/core/v1"
	prowapi "k8s.io/test-infra/prow/apis/prowjobs/v1"
	"k8s.io/test-infra/prow/config"

	pgithub "k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/prow/kube"
	"k8s.io/test-infra/prow/pjutil"
	"k8s.io/test-infra/prow/pluginhelp"
	"k8s.io/test-infra/prow/plugins"
)

type githubClient interface {
	CreateComment(org, repo string, number int, comment string) error
	GetPullRequest(org, repo string, number int) (*pgithub.PullRequest, error)
	IsMember(org, user string) (bool, error)
	RemoveLabel(org, repo string, number int, label string) error
	AddLabel(org, repo string, number int, label string) error
	GetIssueLabels(org, repo string, number int) ([]pgithub.Label, error)
	GetRef(org, repo, ref string) (string, error)
}

type benchmarkInfo struct {
	prNum   int
	release string
	domain  string
	org     string
	repo    string
	baseSHA string
	pr      *pgithub.PullRequest
	guid    string
	comment pgithub.IssueComment
}

type server struct {
	tokenGenerator func() []byte
	ghc            githubClient
	log            *logrus.Entry
	config         options
	prowconfig     *config.Config
}

type prowjobList struct {
	PJs []prowapi.ProwJob `json:"items"`
}

const pluginName = "prombenchmark"
const benchmarkLabel = "benchmark"
const benchmarkPendingLabel = "pending-benchmark-job"

var benchmarkRe = regexp.MustCompile(`(?mi)^/benchmark\s*(master|[0-9]+\.[0-9]+\.[0-9]+\S*)?\s*$`)
var benchmarkCancelRe = regexp.MustCompile(`(?mi)^/benchmark\s+cancel\s*$`)

const maxTries = 50
const benchmarkCommentTmpl = `Welcome to Prometheus Benchmarking Tool.

The two prometheus versions that will be compared are _**pr-{{ .prNum }}**_ and _**{{ .release }}**_

The logs can be viewed at the links provided in the GitHub check blocks at the end of this conversation

After successfull deployment, the benchmarking metrics can be viewed at :
- [prometheus-meta](http://{{ .domain }}/prometheus-meta) - label **{namespace="prombench-{{ .prNum }}"}**
- [grafana](http://{{ .domain }}/grafana) - template-variable **"pr-number" : {{ .prNum }}**

The Prometheus servers being benchmarked can be viewed at :
- PR - [{{ .domain }}/{{ .prNum }}/prometheus-pr]({{ .domain }}/{{ .prNum }}/prometheus-pr)
- {{ .release }} - [{{ .domain }}/{{ .prNum }}/prometheus-release]({{ .domain }}/{{ .prNum }}/prometheus-release)

To stop the benchmark process comment **/benchmark cancel** .`
const benchmarkCancelComment = `benchmark cancel successful`

func helpProvider(enabledRepos []string) (*pluginhelp.PluginHelp, error) {
	pluginHelp := &pluginhelp.PluginHelp{
		Description: "The prombenchmark external plugin starts prometheus benchmarking tool(prombench).",
	}
	pluginHelp.AddCommand(pluginhelp.Command{
		Usage:       "/benchmark master or /benchmark <RELEASE_NUMBER>(ex:2.3.0-rc.1 | Default: master)",
		Description: "Starts prometheus benchmarking tool. With `release` current master will be compared with previous release. With `pr`, PR will be compared with current master.",
		WhoCanUse:   "Members of the same github org.",
		Examples:    []string{"/benchmark", "/benchmark master", "/benchmark 2.3.0-rc.1", "/benchmark cancel"},
	})
	return pluginHelp, nil
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	eventType, eventGUID, payload, ok, _ := pgithub.ValidateWebhook(w, r, s.tokenGenerator())
	if !ok {
		return
	}
	fmt.Fprint(w, "Event received. Have a nice day.")

	if err := s.handleEvent(eventType, eventGUID, payload); err != nil {
		logrus.WithError(err).Error("Error parsing event.")
	}
}

func (s *server) handleEvent(eventType, eventGUID string, payload []byte) error {

	switch eventType {
	case "issue_comment":
		var ic pgithub.IssueCommentEvent
		if err := json.Unmarshal(payload, &ic); err != nil {
			return err
		}
		go func() {
			if err := s.handleIssueComment(ic); err != nil {
				s.log.WithError(err).Info("Benchmarking failed")
			}
		}()
	default:
		logrus.Debugf("skipping event of type %q", eventType)
	}
	return nil
}

func (s *server) handleIssueComment(ic pgithub.IssueCommentEvent) error {
	if !ic.Issue.IsPullRequest() || ic.Action != pgithub.IssueCommentActionCreated {
		return nil
	}

	bi := benchmarkInfo{
		prNum:   ic.Issue.Number,
		domain:  s.config.domainName,
		org:     ic.Repo.Owner.Login,
		repo:    ic.Repo.Name,
		guid:    ic.GUID,
		comment: ic.Comment,
	}

	// Only members should be able to run benchmarks.
	ok, err := s.ghc.IsMember(bi.org, ic.Comment.User.Login)
	if err != nil {
		return err
	}
	if !ok {
		resp := "Benchmarking is restricted to org members."
		s.log.Infof("commenting: %v", resp)
		return s.ghc.CreateComment(bi.org, bi.repo, bi.prNum, plugins.FormatICResponse(bi.comment, resp))
	}

	bi.pr, err = s.ghc.GetPullRequest(bi.org, bi.repo, bi.prNum)
	if err != nil {
		return err
	}

	bi.baseSHA, err = s.ghc.GetRef(bi.org, bi.repo, "heads/"+bi.pr.Base.Ref)
	if err != nil {
		return err
	}

	// check comment match
	if benchmarkRe.MatchString(bi.comment.Body) {

		s.log.Info("requested a benchmark start")

		// check labels
		ok, err := s.labelsOk(bi, true)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("label mismatch")
		}

		group := benchmarkRe.FindStringSubmatch(bi.comment.Body)
		version := strings.TrimSpace(group[1])
		var buf bytes.Buffer

		if version == "" || version == "master" {
			bi.release = "master"
		} else {
			bi.release = "v" + version
		}

		// add comment
		parsedTemplate, err := template.New("startBenchmark").Parse(benchmarkCommentTmpl)
		if err != nil {
			s.log.Errorln("error parsing benchmark comment")
		}
		if err := parsedTemplate.Execute(&buf, bi); err != nil {
			s.log.Errorln("error executing benchmark comment")
		}
		s.ghc.CreateComment(bi.org, bi.repo, bi.prNum, plugins.FormatICResponse(bi.comment, buf.String()))

		// add label
		s.log.Infoln("adding benchmark label")
		if err := s.ghc.AddLabel(bi.org, bi.repo, bi.prNum, benchmarkLabel); err != nil {
			s.log.Errorln("could not add label")
			return err
		}

		// trigger prowjob
		err = s.triggerProwJob(bi, "start-benchmark")
		if err != nil {
			s.ghc.CreateComment(bi.org, bi.repo, bi.prNum, plugins.FormatICResponse(bi.comment, fmt.Sprintf("Creation of prombench prowjob failed: %v", err)))
			s.ghc.RemoveLabel(bi.org, bi.repo, bi.prNum, benchmarkLabel)
			return fmt.Errorf("failed to create prowjob to start-benchmark for release %v: %v", bi.release, err)
		}

	} else if benchmarkCancelRe.MatchString(bi.comment.Body) {
		s.log.Info("requested a benchmark cancel")
		ok, err := s.labelsOk(bi, false)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("label mismatch")
		}

		err = s.triggerProwJob(bi, "cancel-benchmark")
		if err != nil {
			s.ghc.CreateComment(bi.org, bi.repo, bi.prNum, plugins.FormatICResponse(bi.comment, fmt.Sprintf("Deletion of prombench failed: %v", err)))
			return fmt.Errorf("failed to create prowjob to cancel-benchmark %v", err)
		}
		return s.ghc.RemoveLabel(bi.org, bi.repo, bi.prNum, benchmarkLabel)
	} else {
		return nil
	}

	return nil
}

func (s *server) triggerProwJob(bi benchmarkInfo, jobName string) error {

	err := s.waitForPrombenchPJsToEnd(bi, jobName)
	if err != nil {
		return err
	}

	var benchmarkPj config.Presubmit
	kr := prowapi.Refs{
		Org:     bi.org,
		Repo:    bi.repo,
		BaseRef: bi.pr.Base.Ref,
		BaseSHA: bi.baseSHA,
		Pulls: []prowapi.Pull{
			{
				Number: bi.prNum,
				Author: bi.pr.User.Login,
				SHA:    bi.pr.Head.SHA,
			},
		},
	}

	envvars := []apiCoreV1.EnvVar{
		{Name: "ZONE", Value: s.config.zone},
		{Name: "PROJECT_ID", Value: s.config.projectID},
		{Name: "CLUSTER_NAME", Value: s.config.clusterName},
		{Name: "DOMAIN_NAME", Value: s.config.domainName},
		{Name: "PR_NUMBER", Value: fmt.Sprintf("%d", bi.prNum)},
		{Name: "RELEASE", Value: bi.release},
	}

	// load yaml from file
	jc, err := config.ReadJobConfig(s.config.jobConfigPath)
	if err != nil {
		return fmt.Errorf("failed to read jobconfig")
	}

	// add the env vars
	for _, job := range jc.Presubmits[bi.pr.Base.Repo.FullName] {
		if job.Name == jobName {
			s.log.Debugf("starting pj: %s", jobName)
			for _, envvar := range envvars {
				job.Spec.Containers[0].Env = append(job.Spec.Containers[0].Env, envvar)
			}
			benchmarkPj = job
			break
		}
	}

	// get the pj labels
	labels := make(map[string]string)
	for k, v := range benchmarkPj.Labels {
		labels[k] = v
	}
	labels[pgithub.EventGUID] = bi.guid // pjs need this

	// k8s client
	k, err := kube.NewClientInCluster("default")
	if err != nil {
		return fmt.Errorf("could not create k8s client")
	}

	// start the prowjob
	pj := pjutil.NewProwJob(pjutil.PresubmitSpec(benchmarkPj, kr), labels, benchmarkPj.Annotations)
	s.log.WithFields(pjutil.ProwJobFields(&pj)).Infof("Creating a new prowjob to %v", jobName)
	if _, err := k.CreateProwJob(pj); err != nil {
		s.log.Infof("failed to create %s prowjob: %v", jobName, err)
		return err
	}
	return nil
}

func (s *server) waitForPrombenchPJsToEnd(bi benchmarkInfo, jobName string) error {

	//remove label irrespective of function status to not block future jobs
	defer s.ghc.RemoveLabel(bi.org, bi.repo, bi.prNum, benchmarkPendingLabel)
	var pjl prowjobList

	err := getCurrentProwjobs(s.log, s.config.domainName, &pjl)
	if err != nil {
		return err
	}

	if len(pjl.PJs) == 0 {
		return nil
	}

	if !isBenchmarkAllowed(s.log, bi.prNum, &pjl) {
		s.log.Infof("need to wait for %s to finish", jobName)
		comment := fmt.Sprintf("Looks like %s job is already running on this PR. Will start %s job once ongoing job is completed", pjl.PJs[0].Name, jobName)
		s.ghc.AddLabel(bi.org, bi.repo, bi.prNum, benchmarkPendingLabel)
		s.ghc.CreateComment(bi.org, bi.repo, bi.prNum, plugins.FormatICResponse(bi.comment, comment))
	}

	for i := 0; i < maxTries; i++ {
		err := getCurrentProwjobs(s.log, s.config.domainName, &pjl)
		if err != nil {
			return err
		}

		if !isBenchmarkAllowed(s.log, bi.prNum, &pjl) {
			s.log.Debugf("%d: %s is ongoing. Retrying after 30 seconds.", i, pjl.PJs[0].Name)
			retry := time.Second * 30
			time.Sleep(retry)
		} else {
			return nil
		}
	}

	return fmt.Errorf("ongoing %s job was not finished after trying for %d times", pjl.PJs[0].Name, maxTries)

}

func getCurrentProwjobs(l *logrus.Entry, domainName string, pjl *prowjobList) error {
	// TODO: Retries
	resp, err := http.Get("http://" + domainName + "/prowjobs.js")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("status code not 2XX: %v", resp.Status)
	}

	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if err := json.Unmarshal(data, pjl); err != nil {
		return fmt.Errorf("cannot unmarshal data from deck: %v", err)
	}
	return nil
}

func isBenchmarkAllowed(l *logrus.Entry, prNum int, pjl *prowjobList) bool {

	var presubmits []prowapi.ProwJob
	for _, pj := range pjl.PJs {
		if pj.Spec.Type != "presubmit" {
			continue
		}
		if pj.Spec.Refs.Pulls[0].Number != prNum {
			continue
		}
		if pj.Status.State == prowapi.TriggeredState || pj.Status.State == prowapi.PendingState {
			presubmits = append(presubmits, pj)
			break
		}
	}

	if len(presubmits) == 0 {
		l.Info("no prowjobs found. test can be started")
		return true
	}

	return false
}

func (s *server) labelsOk(bi benchmarkInfo, startComment bool) (bool, error) {
	labels, err := s.ghc.GetIssueLabels(bi.org, bi.repo, bi.prNum)
	if err != nil {
		return false, fmt.Errorf("failed to get the labels")
	}
	for _, label := range labels {
		if label.Name == benchmarkLabel && startComment {
			resp := "Looks like benchmarking is already running for this PR.<br/> You can cancel benchmarking by commenting `/benchmark cancel`. :smiley:"
			s.log.Infof("commenting: %v", resp)
			err := s.ghc.CreateComment(bi.org, bi.repo, bi.prNum, plugins.FormatICResponse(bi.comment, resp))
			return false, err
		} else if label.Name == benchmarkPendingLabel {
			resp := "Looks like a job is already lined up for this PR.<br/> Please try again once all pending jobs have finished :smiley:"
			s.log.Infof("commenting: %v", resp)
			err := s.ghc.CreateComment(bi.org, bi.repo, bi.prNum, plugins.FormatICResponse(bi.comment, resp))
			return false, err
		}
	}
	return true, nil
}
