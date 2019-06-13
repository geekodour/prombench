package main

import (
  "fmt"
  "os"
	"log"
  "strings"
  "path/filepath"
  "io/ioutil"
  "github.com/google/go-github/v26/github"
	"gopkg.in/alecthomas/kingpin.v2"
)

// TODO: change this to /github later
const (
  EVENT_FILE_PATH = "./test.json"
  WRITE_PATH = "./github/home/ghfilter"
)

func writeArgs(args []string) {
  for i, arg := range args {
    data := []byte(arg)
    filename := fmt.Sprintf("ARG%d",i)
    err := ioutil.WriteFile(filepath.Join(WRITE_PATH,filename), data, 0644)
    if err != nil {
      panic(err)
    }
  }
}

func main() {
    data, err := ioutil.ReadFile(EVENT_FILE_PATH)
    if err != nil {
      fmt.Print(err)
    }
    os.MkdirAll(WRITE_PATH, os.ModePerm)

    event, err := github.ParseWebHook("issue_comment" , data)
	  if err != nil {
	    log.Printf("could not parse = %s\n", err)
      return
    }

    switch e := event.(type) {
    case *github.IssueCommentEvent:
      args := strings.Fields(*e.GetComment().Body)
      writeArgs(args)
    default:
	    log.Printf("simpleargs only supports issue_comment event")
      return
    }
}