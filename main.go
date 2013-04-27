package main

import (
	"code.google.com/p/monnand-goconf"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/howeyc/fsnotify"
	//"github.com/nu7hatch/gouuid"
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
	Email,
	BaseURL,
	HostName,
	CloneURLType,
	CloneURL,
	APISecret string
	NeedsDeployment bool
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
	globalconfS = flag.String("conf", "jekyll-baas.conf", "Global config file")
	sitesconfS  = flag.String("sites", "sites.json", "Sites list file")
	globalconf  string
	sitesconf   string
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
	case <-time.After(60 * time.Second):
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

		src, _ := filepath.Abs(filepath.Join(basedir, sitedir, job.HostName))
		dest, _ := filepath.Abs(filepath.Join(basedir, gendir, job.HostName))
		outd, _ := filepath.Abs(filepath.Join(basedir, outdir, job.HostName))

		allsitesdir, _ := filepath.Abs(filepath.Join(basedir, sitedir))

		// Needs initial deployment
		if job.NeedsDeployment {
			log.Printf("Cloning from source...")
			/*
				if err := os.MkdirAll(src, 0755); err != nil {
					log.Printf("Makedir %s error?\n", src)
					continue
				}
			*/
			os.Chdir(allsitesdir)
			gitclonecmd := exec.Command("git", "clone", job.CloneURL, job.HostName)
			runWithTimeout(gitclonecmd)

		} else {

			log.Printf("Pulling from source...")
			// Convert the directory to an absolute path

			gitpullcmd := exec.Command("git", "--git-dir="+src+"/.git", "--work-tree="+src, "pull")

			runWithTimeout(gitpullcmd)
		}

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

		uploaderqueue := make(chan string)

		go watch(job, ch, uploaderqueue)

		if job.NeedsDeployment {
			walker := func(fn string, fi os.FileInfo, err error) error {
				if fi.IsDir() {
					return nil
				}
				uploaderqueue <- fn
				return nil
			}

			filepath.Walk(outd, walker)
		}

		log.Printf(" Done!\n")
	}
}

func watch(job SiteConf, ch chan SiteConf, uploaderqueue chan string) {

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

	go func() {
		auth := aws.Auth{
			AccessKey: conf.Key,
			SecretKey: conf.Secret,
		}
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
				auth = aws.Auth{
					AccessKey: conf.Key,
					SecretKey: conf.Secret,
				}
				b = s3.New(auth, aws.USEast).Bucket(conf.Bucket)
			}

			if fi.IsDir() {
				continue
			}

			rel, _ := filepath.Rel(src, fn)

			log.Printf("[%s] Got file %s, uploading... ", job.HostName, rel)

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
	//os.MkdirAll(src, 0755)

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
					log.Printf("[%s] Trying to watch newly created folder: %s", job.HostName, ev.Name)
					watcher.Watch(ev.Name)
				}
			}

		case err := <-watcher.Error:
			fmt.Println("inotify error:", err)
		}
	}
}

func configwatch(ch chan bool, allSites *[]SiteConf) {
	log.Printf("Sites configuration watcher started.")
	for {
		<-ch
		log.Printf("Saving sites configuration...")

		fi, err := os.Create(sitesconf)

		if err != nil {
			log.Printf("FAILED!")
			continue
		}

		b, _ := json.Marshal(allSites)
		fi.Write(b)
		//log.Printf("About to write '%s' to %s", string(b), *sitesconf)
		fi.Close()
	}
}

func main() {
	flag.Parse()
	globalconf, _ = filepath.Abs(*globalconfS)
	sitesconf, _ = filepath.Abs(*sitesconfS)

	log.Printf("Program started with global config file %s, sites %s\n", globalconf, sitesconf)

	c, err := conf.ReadConfigFile(globalconf)
	allSites := []SiteConf{}

	if err != nil {
		log.Fatalf("Error while opening global config file: %s. Bailing out!\n", err)
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

	fi, err := os.Open(sitesconf)

	work := make(chan SiteConf)
	saveConfig := make(chan bool)

	go jekyllProcessorConsumer(work)

	if err != nil {
		fmt.Printf("File error while trying to open the sites config: %v\n", err)
		os.Exit(1)
	}

	log.Printf("Reading all sites configuration... Please wait!\n")

	dec := json.NewDecoder(fi)

	if err := dec.Decode(&allSites); err == io.EOF {

	} else if err != nil {
		log.Fatal(err)
	}

	for _, s := range allSites {
		log.Printf("Site: %s [%s]\n", s.Name, s.HostName)
		work <- s
	}

	fi.Close()

	go configwatch(saveConfig, &allSites)

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

	http.HandleFunc("/add/", func(w http.ResponseWriter, r *http.Request) {
		//newUUID, _ := uuid.NewV4()
		newSite := SiteConf{
			Name:            r.URL.Query().Get("name"),
			Email:           r.URL.Query().Get("email"),
			BaseURL:         r.URL.Query().Get("baseurl"),
			HostName:        r.URL.Query().Get("hostname"),
			CloneURLType:    r.URL.Query().Get("clonetype"),
			CloneURL:        r.URL.Query().Get("cloneurl"),
			NeedsDeployment: true,
			APISecret:       "shibboleth",
		}

		work <- newSite

		newSite.NeedsDeployment = false

		allSites = append(allSites, newSite)

		msg := APIResponse{
			Code:    200,
			Message: fmt.Sprintf("%s", newSite.APISecret),
		}

		w.Header().Set("Content-Type", "text/javascript")
		w.Header().Set("Cache-Control", "no-cache")
		msgToSend, _ := json.Marshal(msg)

		saveConfig <- true

		w.Write(msgToSend)
	})

	fmt.Printf("Starting server on port %s\n", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	os.Exit(0)
}
