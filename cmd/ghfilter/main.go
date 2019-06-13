package main

import (
  "fmt"
  "io/ioutil"
  "github.com/google/go-github/v26/github"
)

const (
  EVENT_PATH = ""
  WRITE_PATH = ""
)


func main() {

    data, err := ioutil.ReadFile("./test.json")
    if err != nil {
      fmt.Print(err)
    }

    event, err := github.ParseWebHook("issue_comment" , data)
	  if err != nil {
      fmt.Printf("could not parse = %s\n", err)
      return
    }

    switch e := event.(type) {
    case *github.IssueCommentEvent:
      fmt.Printf("%s", *e.GetComment().Body)
    default:
      fmt.Printf("ghfilter currently only supports issue_comment event")
      return
    }
}
