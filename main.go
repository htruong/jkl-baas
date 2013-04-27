package main

import (
	"code.google.com/p/monnand-goconf"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/howeyc/fsnotify"
	"io"
	"io/ioutil"
	"launchpad.net/goamz/aws"
	"launchpad.net/goamz/s3"
	"log"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	//"strings"
	"time"
	//"sync"
)

type SiteConf struct {
	Name,
	BaseURL,
	HostName,
	CloneURLType,
	CloneURL,
	NeedsDeployment,
	APISecret string
}

type APIResponse struct {
	Code    uint
	Message string
}

var (
	sitedir  = "sites"
	gendir   = "_gen"
	outdir   = "_out"
	basedir  = ""
	s3key    = ""
	s3secret = ""
	verbose  = true
)

var (
	// jekyll blog-as-a-service config files
	globalconf = flag.String("conf", "jekyll-baas.conf", "Global config file")
	sitesconf  = flag.String("sites", "sites.json", "Sites list file")
)

func runWithTimeout(cmd *exec.Cmd) {
	done := make(chan error)
	if verbose {
		stderr, _ := cmd.StderrPipe()
		stdout, _ := cmd.StdoutPipe()
		go io.Copy(os.Stderr, stderr)
		go io.Copy(os.Stdout, stdout)
	}
	cmd.Start()
	go func() {
		done <- cmd.Wait()
	}()
	select {
	case <-time.After(30 * time.Second):
		if err := cmd.Process.Kill(); err != nil {
			log.Fatal("Failed to kill: ", err)
		}
		<-done // allow goroutine to exit
		log.Println("Process killed")
	case err := <-done:
		log.Printf("Process done with error = %v", err)
	}
}

func jekyllProcessorConsumer(ch chan SiteConf) {
	log.Printf("Site renderer module launching...\n")
	for {
		log.Printf("Waiting for new changes to process...\n")

		job := <-ch
		log.Printf("--- Got job --> %s ---", job.Name)

		log.Printf("Pulling from source...")
		// Convert the directory to an absolute path

		src, _ := filepath.Abs(filepath.Join(basedir, sitedir, job.HostName))
		dest, _ := filepath.Abs(filepath.Join(basedir, gendir, job.HostName))
		outd, _ := filepath.Abs(filepath.Join(basedir, outdir, job.HostName))

		gitpullcmd := exec.Command("git", "--git-dir="+src+"/.git", "--work-tree="+src, "pull")

		runWithTimeout(gitpullcmd)

		log.Printf(" Done!\n")
		log.Printf("Generating static site...\n")

		// Change the working directory to the website's source directory
		os.Chdir(src)

		// Initialize the Jekyll website
		site, err := NewSite(src, dest)
		if err != nil {
			fmt.Printf("Error on site %s while trying to initialize: %v. This site will be temporary disabled!\n", job.Name, err)
			//os.Exit(1)
			return
		}

		if len(job.BaseURL) != 0 {
			site.Conf.Set("baseurl", job.BaseURL)
		}

		// Generate the static website
		if err := site.Generate(); err != nil {
			fmt.Printf("Error on site %s while trying to generate static content: %v\n", job.Name, err)
			//os.Exit(1)
			return
		}

		log.Printf(" Done!\n")

		log.Printf("Calculating differences...\n")
		// Now sync it to the outdir
		log.Printf("The gen dir is %s out dir is %s", dest, outd)

		if err := os.MkdirAll(outd, 0755); err != nil {
			log.Printf("Makedir %s error?\n", outd)
			continue
		}
		// The ending slash makes rsync sync the same level directory
		rsynccmd := exec.Command("rsync", "--delete", "--size-only", "--recursive", dest+"/", outd)

		runWithTimeout(rsynccmd)

		log.Printf(" Done!\n")
	}
}

func watch(job SiteConf, ch chan SiteConf) {

	// Deploys the static website to S3
	var conf *DeployConfig
	// Read the S3 configuration details if there is a s3 conf file

	path := filepath.Join(basedir, sitedir, job.HostName, "_jekyll_s3.yml")
	fi, err := os.Stat(path)
	if fi != nil && err == nil {
		conf, err = ParseDeployConfig(path)
		if err != nil {
			fmt.Printf("Error on site %s while trying to parse deployment config: %v\n", job.Name, err)
			//os.Exit(1)
			return
		}
	} else {
		// else use the command line args
		conf = &DeployConfig{s3key, s3secret, job.HostName}
	}

	/*
		if err := site.Deploy(conf.Key, conf.Secret, conf.Bucket); err != nil {
			fmt.Printf("Error on site %s while trying to deploy to s3: %v\n", job.Name, err)
			//os.Exit(1)
			return
		}
	*/

	// Get recursive list of directories to watch
	src, _ := filepath.Abs(filepath.Join(basedir, outdir, job.HostName))

	uploaderqueue := make(chan string)

	go func() {
		auth := aws.Auth{conf.Key, conf.Secret}
		b := s3.New(auth, aws.USEast).Bucket(conf.Bucket)
		lastAuth := time.Now()
		for {
			fn := <-uploaderqueue
			fi, err := os.Stat(fn)

			if err != nil {
				log.Printf("There was an error getting the file %s: %v", fn, err)
				continue
			}

			// if it's too long ago, then reauthenticate
			if time.Since(lastAuth).Minutes() > 2 {
				auth = aws.Auth{conf.Key, conf.Secret}
				b = s3.New(auth, aws.USEast).Bucket(conf.Bucket)
			}

			if fi.IsDir() {
				continue
			}

			rel, _ := filepath.Rel(src, fn)
			typ := mime.TypeByExtension(filepath.Ext(rel))
			content, err := ioutil.ReadFile(fn)
			log.Printf("[%s] Uploading %s...", job.HostName, rel)
			if err != nil {
				continue
			}

			// try to upload the file ... sometimes this fails due to amazon
			// issues. If so, we'll re-try
			if err := b.Put(rel, content, typ, s3.PublicRead); err != nil {
				time.Sleep(100 * time.Millisecond) // sleep so that we don't immediately retry
				err2 := b.Put(rel, content, typ, s3.PublicRead)
				if err2 != nil {
					log.Printf(" Failed!\n")
					continue
				}
			}
			log.Printf(" Success!")

			lastAuth = time.Now()

		}
	}()

	// Setup the inotify watcher
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		fmt.Println(err)
		return
	}

	log.Printf("[%s] Watching %s\n", job.HostName, src)
	for _, path := range dirs(src) {
		if err := watcher.Watch(path); err != nil {
			fmt.Println(err)
			return
		}
	}

	for {
		select {
		case ev := <-watcher.Event:
			// Ignore changes to the _site directoy, hidden, or temp files
			if !isHiddenOrTemp(ev.Name) && (ev.IsCreate() || ev.IsModify()) {
				log.Printf("[%s] File created or modified: %s", job.HostName, ev.Name)
				uploaderqueue <- ev.Name
				fi, _ := os.Stat(ev.Name)
				if fi.IsDir() {
					watcher.Watch(ev.Name)
				}
			}

		case err := <-watcher.Error:
			fmt.Println("inotify error:", err)
		}
	}
}

func main() {
	flag.Parse()

	c, err := conf.ReadConfigFile(*globalconf)

	if err != nil {
		log.Fatal("Error while opening global config file: %s. Bailing out!\n", err)
	}

	port, err := c.GetString("general", "port")
	if err != nil {
		port = "9999"
	}

	// base dir
	basedir, err = c.GetString("general", "base_dir")
	if err != nil {
		basedir = ""
	}

	// s3 access key
	s3key, err = c.GetString("s3", "key")
	if err != nil {
		log.Fatal("No default global S3 key found. Configure yours in the global config file!\n", err)
	}

	// s3 secret key
	s3secret, err = c.GetString("s3", "secret")

	if err != nil {
		log.Fatal("No default global S3 secret found. Configure yours in the global config file!\n", err)
	}

	fi, err := os.Open(*sitesconf)

	work := make(chan SiteConf)

	go jekyllProcessorConsumer(work)

	if err != nil {
		fmt.Printf("File error while trying to open the sites config: %v\n", err)
		os.Exit(1)
	}

	log.Printf("Reading all sites configuration... Please wait!\n")

	dec := json.NewDecoder(fi)

	allSites := []SiteConf{}

	for {
		var s SiteConf
		if err := dec.Decode(&s); err == io.EOF {
			break
		} else if err != nil {
			log.Fatal(err)
		}
		log.Printf("[conf] Got site: %s: %s!\n", s.Name, s.HostName)
		allSites = append(allSites, s)
	}

	for _, s := range allSites {
		log.Printf("Site: %s [%s]\n", s.Name, s.HostName)
		go watch(s, work)
		work <- s
	}

	fi.Close()

	// Create the handler to serve from the filesystem
	http.HandleFunc("/update/", func(w http.ResponseWriter, r *http.Request) {
		hostname := r.URL.Query().Get("hostname")
		foundhost := false
		for _, s := range allSites {
			if s.HostName == hostname {
				work <- s
				foundhost = true
				break
			}
		}

		msg := APIResponse{
			Code:    400,
			Message: "Command executed successfully",
		}

		if !foundhost {
			msg = APIResponse{
				Code:    404,
				Message: "Host not found",
			}
		}

		w.Header().Set("Content-Type", "text/javascript")
		w.Header().Set("Cache-Control", "no-cache")
		msgToSend, _ := json.Marshal(msg)

		w.Write(msgToSend)
	})

	fmt.Printf("Starting server on port %s\n", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	os.Exit(0)
}
