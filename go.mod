module github.com/prometheus/prombench

go 1.12

require (
	cloud.google.com/go v0.39.0
	github.com/google/gofuzz v1.0.0 // indirect
	github.com/googleapis/gnostic v0.2.0 // indirect
	github.com/imdario/mergo v0.3.7 // indirect
	github.com/onsi/ginkgo v1.7.0 // indirect
	github.com/onsi/gomega v1.4.3 // indirect
	github.com/pkg/errors v0.8.1
	github.com/prometheus/client_golang v0.9.4
	github.com/sirupsen/logrus v1.4.2
	google.golang.org/genproto v0.0.0-20190516172635-bb713bdc0e52
	google.golang.org/grpc v1.19.1
	gopkg.in/alecthomas/kingpin.v2 v2.2.6
	gopkg.in/yaml.v2 v2.2.2
	k8s.io/api v0.0.0-20181128191700-6db15a15d2d3
	k8s.io/apiextensions-apiserver v0.0.0-20181128195303-1f84094d7e8e
	k8s.io/apimachinery v0.0.0-20181128191346-49ce2735e507
	k8s.io/client-go v9.0.0+incompatible
	k8s.io/test-infra v0.0.0-20190712214304-3ce030d42897
)
