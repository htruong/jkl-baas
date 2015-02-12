package main

import (
	"bitbucket.org/kardianos/osext"
	"code.google.com/p/monnand-goconf"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/howeyc/fsnotify"
	"github.com/nu7hatch/gouuid"
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
	"strings"
	//"strings"
	"sync"
	"time"
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

type UploaderQueue struct {
	File    string
	RelPath string
	Conf    DeployConfig
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
	advancedMode    = flag.Bool("multisites", false, "Multistes mode")
	globalconfS     = flag.String("conf", "jekyll-baas.conf", "Global config file")
	sitesconfS      = flag.String("sites", "sites.json", "Sites list file")
	changetoExecDir = flag.Bool("cd", true, "Change to the current directory where the executable is when in single site mode")
	port            = flag.Int("port", 8080, "Webserver/webservice port")
	globalconf      string
	sitesconf       string
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

func jekyllProcessorConsumer(ch chan SiteConf, configwatcher chan bool) {
	log.Printf("Site renderer module launching...\n")
	for {
		log.Printf("Waiting for new changes to process...\n")

		job := <-ch
		log.Printf("--- Got job: %s ---", job.Name)

		src, _ := filepath.Abs(filepath.Join(basedir, sitedir, job.HostName))
		dest, _ := filepath.Abs(filepath.Join(basedir, gendir, job.HostName))
		outd, _ := filepath.Abs(filepath.Join(basedir, outdir, job.HostName))

		allsitesdir, _ := filepath.Abs(filepath.Join(basedir, sitedir))

		log.Printf("The gen dir is %s out dir is %s", dest, outd)

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
			fmt.Printf("Error on site %s while trying to initialize: %v. This site will be temporarily disabled until next tickle!\n", job.Name, err)
			//os.Exit(1)
			continue
		}

		if len(job.BaseURL) != 0 {
			site.Conf.Set("baseurl", job.BaseURL)
		}

		// Generate the static website
		if err := site.Generate(); err != nil {
			fmt.Printf("Error on site %s while trying to generate static content: %v\n", job.Name, err)
			//os.Exit(1)
			continue
		}

		log.Printf(" Done!\n")

		log.Printf("Calculating differences...\n")
		// Now sync it to the outdir

		// The ending slash makes rsync sync the same level directory
		rsynccmd := exec.Command("rsync", "--delete", "--size-only", "--recursive", dest+"/", outd)

		runWithTimeout(rsynccmd)

		log.Printf(" Done!\n")
	}
}

func watch(job SiteConf, uploaderqueue chan UploaderQueue) {

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

	// Get recursive list of directories to watch
	src, _ := filepath.Abs(filepath.Join(basedir, outdir, job.HostName))

	if err := os.MkdirAll(src, 0755); err != nil {
		log.Printf("Makedir %s error?\n", src)
		return
	}

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
				rel, _ := filepath.Rel(src, ev.Name)
				uploaderqueue <- UploaderQueue{
					File:    ev.Name,
					RelPath: rel,
					Conf:    *conf,
				}
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

func s3uploader(ch chan UploaderQueue) {
	for {
		work := <-ch
		conf := work.Conf
		fn := work.File
		rel := work.RelPath
		auth := aws.Auth{
			AccessKey: conf.Key,
			SecretKey: conf.Secret,
		}

		b := s3.New(auth, aws.USEast).Bucket(conf.Bucket)

		fi, err := os.Stat(fn)

		if err != nil {
			log.Printf("[s3uploader] There was an error getting the file %s: %v", fn, err)
			continue
		}

		if fi.IsDir() {
			continue
		}

		log.Printf("[s3uploader] Got file %s, uploading... ", fn)

		typ := mime.TypeByExtension(filepath.Ext(rel))
		content, err := ioutil.ReadFile(fn)
		//log.Printf("[s3uploader] Uploading %s...", rel)
		if err != nil {
			continue
		}

		// try to upload the file ... sometimes this fails due to amazon
		// issues. If so, we'll re-try
		if err := b.Put(rel, content, typ, s3.PublicRead); err != nil {
			time.Sleep(100 * time.Millisecond) // sleep so that we don't immediately retry
			err2 := b.Put(rel, content, typ, s3.PublicRead)
			if err2 != nil {
				log.Printf("[s3] Uploading %s... Failed!\n", fn)
				continue
			}
		}
		log.Printf("[s3] Uploading %s... Success!", fn)

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

var chttp = http.NewServeMux()

func StaticGeneratorHandler(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.URL.Path, ".") {
		chttp.ServeHTTP(w, r)
	} else {
		fmt.Fprintf(w, "StaticGeneratedFilesHandler")
	}
}

var mu sync.RWMutex

func recompile(site *Site) {
	mu.Lock()
	defer mu.Unlock()

	if err := site.Reload(); err != nil {
		fmt.Println(err)
		return
	}

	if err := site.Generate(); err != nil {
		fmt.Println(err)
		return
	}
}

func simpleWatch(site *Site) {

	// Setup the inotify watcher
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		fmt.Println(err)
		return
	}

	// Get recursive list of directories to watch
	for _, path := range dirs(site.Src) {
		if err := watcher.Watch(path); err != nil {
			fmt.Println(err)
			return
		}
	}

	for {
		select {
		case ev := <-watcher.Event:
			// Ignore changes to the _site directoy, hidden, or temp files
			if !strings.HasPrefix(ev.Name, site.Dest) && !isHiddenOrTemp(ev.Name) {
				fmt.Println("Event: ", ev.String())
				recompile(site)
			}
		case err := <-watcher.Error:
			fmt.Println("inotify error:", err)
		}
	}
}

func main() {
	flag.Parse()

	if *advancedMode == false {

		if *changetoExecDir {
			oryza_exec, _ := osext.Executable()
			oryza_dir := filepath.Dir(oryza_exec)
			log.Printf("Changing current directory to %s\n", oryza_dir)
			os.Chdir(oryza_dir)

		}

		// Initialize the Jekyll website
		site, err := NewSite(".", "../_out")
		os.MkdirAll("../_out", 0755)
		if err != nil {
			log.Printf("Error on site while trying to render: %v\n", err)
			os.Exit(1)
		}

		// Generate the static website
		if err := site.Generate(); err != nil {
			log.Printf("Error on site while trying to generate static content: %v\n", err)
			os.Exit(1)
		}

		log.Printf("Site generated successfully. Viva la Oryza!\n")

		log.Printf("Check your site at http://127.0.0.1:8080/\n")

		//		chttp.Handle("/", http.FileServer(http.Dir("./_out")))

		// Normal resources

		go simpleWatch(site)

		http.Handle("/", http.FileServer(http.Dir("../_out/")))

		http.ListenAndServe(fmt.Sprintf(":%d", *port), nil)

		os.Exit(0)
	}

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
	uploaderqueue := make(chan UploaderQueue)

	go configwatch(saveConfig, &allSites)

	go s3uploader(uploaderqueue)

	go jekyllProcessorConsumer(work, saveConfig)

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

		go watch(s, uploaderqueue)
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

	http.HandleFunc("/add/", func(w http.ResponseWriter, r *http.Request) {
		newUUID, _ := uuid.NewV4()
		newSite := SiteConf{
			Name:            r.URL.Query().Get("name"),
			Email:           r.URL.Query().Get("email"),
			BaseURL:         r.URL.Query().Get("baseurl"),
			HostName:        r.URL.Query().Get("hostname"),
			CloneURLType:    r.URL.Query().Get("clonetype"),
			CloneURL:        r.URL.Query().Get("cloneurl"),
			NeedsDeployment: true,
			APISecret:       newUUID.String(),
		}

		go watch(newSite, uploaderqueue)
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
