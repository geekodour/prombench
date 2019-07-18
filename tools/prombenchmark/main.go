package main

import (
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	//"github.com/google/go-github/v26/github"
	"github.com/sirupsen/logrus"
	"gopkg.in/alecthomas/kingpin.v2"
	"k8s.io/test-infra/prow/config/secret"
	pgithub "k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/prow/pluginhelp/externalplugins"
)

type options struct {
	portNo        string
	oauthFile     string
	hmacFile      string
	zone          string
	clusterName   string
	domainName    string
	projectID     string
	jobConfigPath string
}

func main() {

	cfg := options{}
	app := kingpin.New(filepath.Base(os.Args[0]), "prombenchmark - external plugin for prow")
	app.Flag("project-id", "gcp project id").Required().StringVar(&cfg.projectID)
	app.Flag("domain-name", "Domain name for prombench").Required().StringVar(&cfg.domainName)
	app.Flag("cluster-name", "gcp cluster name").Required().StringVar(&cfg.clusterName)
	app.Flag("zone", "gcp zone").Required().StringVar(&cfg.zone)
	app.Flag("oauthfile", "path to github oauth token file").Default("/etc/github/oauth").StringVar(&cfg.oauthFile)
	app.Flag("hmacfile", "path to github hmac token file").Default("/etc/webhook/hmac").StringVar(&cfg.hmacFile)
	app.Flag("job-config-path", "path to job-config directory").Default("/etc/job-config").StringVar(&cfg.jobConfigPath)
	app.Flag("port", "port number to run the server in").Default("8080").StringVar(&cfg.portNo)

	kingpin.MustParse(app.Parse(os.Args[1:]))

	// Ignore SIGTERM so that we don't drop hooks when the pod is removed.
	// We'll get SIGTERM first and then SIGKILL after our graceful termination
	// deadline.
	signal.Ignore(syscall.SIGTERM)

	logrus.SetFormatter(&logrus.JSONFormatter{})
	logrus.SetLevel(logrus.DebugLevel)
	log := logrus.StandardLogger().WithField("plugin", "prombenchmark")

	secretAgent := &secret.Agent{}
	if err := secretAgent.Start([]string{cfg.oauthFile, cfg.hmacFile}); err != nil {
		logrus.WithError(err).Fatal("Error starting secrets agent.")
	}

	githubClient := pgithub.NewClient(secretAgent.GetTokenGenerator(cfg.oauthFile), "https://api.github.com", "https://api.github.com")

	server := &server{
		tokenGenerator: secretAgent.GetTokenGenerator(cfg.hmacFile),
		ghc:            githubClient,
		log:            log,
		config:         cfg,
	}

	http.Handle("/", server)
	externalplugins.ServeExternalPluginHelp(http.DefaultServeMux, log, helpProvider)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%v", cfg.portNo), nil))
}
