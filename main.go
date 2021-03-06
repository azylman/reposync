// reposync syncs repos for a GitHub user into a folder on your computer.
package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync"

	"github.com/google/go-github/github"
	"golang.org/x/oauth2"
)

var Version string

func main() {
	versionflag := flag.Bool("version", false, "Shows version and exits")
	user := flag.String("user", "", "GitHub user you'd like to sync a folder with. Must specify this or org")
	userRepoType := flag.String("userrepotype", "all", "For the GitHub user, type of repos you'd like to pull. Can be all, owner, member. Default is all.")
	userRepoForks := flag.Bool("userrepoforks", true, "For the GitHub user, include forks. Default is true.")
	org := flag.String("org", "", "GitHub organization you'd like to sync a folder with. Must specify this or user")
	orgRepoType := flag.String("orgrepotype", "all", "For the GitHub org, type of repos you'd like to pull. Can be all, public, private, forks, sources, member. Default is all.")
	dir := flag.String("dir", "", "Directory to put folders for each repo")
	archivedir := flag.String("archivedir", "", "Directory to move folders in dir that are not associated with a repo")
	token := flag.String("token", "", "GitHub token to use for auth")
	dryrun := flag.Bool("dryrun", false, "Set to true to print actions instead of performing them")
	flag.Parse()
	if *versionflag {
		fmt.Println(Version)
		os.Exit(0)
	}
	if *user == "" && *org == "" {
		log.Fatal("must provide user or org")
	}
	if *dir == "" {
		log.Fatal("must provide dir")
	}
	if *archivedir == "" {
		log.Fatal("must provide archivedir")
	}
	if *token == "" {
		log.Fatal("must provide token")
	}
	rs := RepoSync{
		user:          *user,
		userRepoType:  *userRepoType,
		userRepoForks: *userRepoForks,
		org:           *org,
		orgRepoType:   *orgRepoType,
		workdir:       *dir,
		archivedir:    *archivedir,
		token:         *token,
		dryrun:        *dryrun,
	}
	if err := rs.Sync(); err != nil {
		log.Fatal(err)
	}
}

// Contains computes whether an element is a member of a set of strings.
func Contains(list []string, el string) bool {
	for _, str := range list {
		if str == el {
			return true
		}
	}
	return false
}

// Difference computes the set difference A - B for string sets.
func Difference(a, b []string) []string {
	diff := []string{}
	for _, str := range a {
		if Contains(b, str) {
			continue
		}
		diff = append(diff, str)
	}
	return diff
}

// Task runs a function and logs its progress.
type Task struct {
	task        func() error
	description string
}

func NewTask(task func() error, description string) *Task {
	return &Task{task: task, description: description}
}

func (tws *Task) Run() {
	log.Printf("begin %s", tws.description)
	if err := tws.task(); err != nil {
		log.Printf("error %s: %s", tws.description, err)
	} else {
		log.Printf("finished %s", tws.description)
	}
}

type RepoSync struct {
	org           string
	orgRepoType   string
	user          string
	userRepoType  string
	userRepoForks bool
	workdir       string
	archivedir    string
	token         string
	dryrun        bool
}

func (rs RepoSync) Sync() error {

	// get list of repos for org
	var allRepos []string
	NewTask(func() error {
		ts := oauth2.StaticTokenSource(
			&oauth2.Token{AccessToken: rs.token},
		)
		tc := oauth2.NewClient(oauth2.NoContext, ts)
		client := github.NewClient(tc)
		if rs.org != "" {
			opt := &github.RepositoryListByOrgOptions{
				Type:        rs.orgRepoType,
				ListOptions: github.ListOptions{PerPage: 100},
			}
			for {
				repos, resp, err := client.Repositories.ListByOrg(rs.org, opt)
				if err != nil {
					return err
				}
				for _, repo := range repos {
					if repo.Name == nil {
						continue
					}
					allRepos = append(allRepos, *repo.Name)
				}
				if resp.NextPage == 0 {
					break
				}
				opt.ListOptions.Page = resp.NextPage
			}
		} else if rs.user != "" {
			opt := &github.RepositoryListOptions{
				Type:        rs.userRepoType,
				ListOptions: github.ListOptions{PerPage: 1000},
			}
			for {
				repos, resp, err := client.Repositories.List(rs.user, opt)
				if err != nil {
					return err
				}
				for _, repo := range repos {
					if repo.Name == nil {
						continue
					}
					if rs.userRepoForks == false && repo.Fork != nil && *repo.Fork {
						continue
					}
					allRepos = append(allRepos, *repo.Name)
				}
				if resp.NextPage == 0 {
					break
				}
				opt.ListOptions.Page = resp.NextPage
			}
		}
		return nil
	}, fmt.Sprintf("loading repos for %s %s", rs.org, rs.user)).Run()

	// get list of current repositories checked out, ignoring non-directories and hidden directories
	var currentRepos []string
	NewTask(func() error {
		files, _ := ioutil.ReadDir(rs.workdir)
		for _, f := range files {
			if !f.IsDir() || strings.Index(f.Name(), ".") == 0 {
				continue
			}
			currentRepos = append(currentRepos, f.Name())
		}
		return nil
	}, fmt.Sprintf("loading repos already cloned in %s", rs.workdir)).Run()

	reposToArchive := Difference(currentRepos, allRepos)
	reposToClone := Difference(allRepos, currentRepos)

	if len(reposToArchive)+len(reposToClone) == 0 {
		log.Print("nothing to do!")
		return nil
	}

	var archivers sync.WaitGroup
	if err := os.MkdirAll(rs.archivedir, 0755); err != nil {
		return err
	}
	for _, repo := range reposToArchive {
		archivers.Add(1)
		go func(r string) {
			defer archivers.Done()
			NewTask(func() error {
				if rs.dryrun {
					return nil
				}
				return os.Rename(path.Join(rs.workdir, r), path.Join(rs.archivedir, r))
			}, fmt.Sprintf("archiving %s", r)).Run()
		}(repo)
	}

	var cloners sync.WaitGroup
	for _, repo := range reposToClone {
		cloners.Add(1)
		go func(r string) {
			defer cloners.Done()
			NewTask(func() error {
				if rs.dryrun {
					return nil
				}
				if rs.org != "" {
					if output, err := exec.Command("git", "clone", fmt.Sprintf("git@github.com:%s/%s", rs.org, r), path.Join(rs.workdir, r)).CombinedOutput(); err != nil {
						return fmt.Errorf("%s from %s", err, output)
					}
					return nil
				}
				if output, err := exec.Command("git", "clone", fmt.Sprintf("git@github.com:%s/%s", rs.user, r), path.Join(rs.workdir, r)).CombinedOutput(); err != nil {
					return fmt.Errorf("%s from %s", err, output)
				}
				return nil
			}, fmt.Sprintf("cloning %s", r)).Run()
		}(repo)
	}

	archivers.Wait()
	cloners.Wait()
	return nil
}
