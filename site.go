package main

import (
	"bytes"
	"crypto/md5"
	"fmt"
	"github.com/nfnt/resize"
	"image/jpeg"
	"io/ioutil"
	"launchpad.net/goamz/aws"
	"launchpad.net/goamz/s3"
	"log"
	"mime"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"text/template"
	"time"
)

var (
	MsgCopyingFile  = "Copying File: %s"
	MsgGenerateFile = "Generating Page: %s"
	MsgUploadFile   = "Uploading: %s"
	MsgUsingConfig  = "Loading Config: %s"
)

type Site struct {
	Src  string // Directory where Jekyll will look to transform files
	Dest string // Directory where Jekyll will write files to
	Conf Config // Configuration date from the _config.toml file

	posts []Page             // Posts thet need to be generated
	pages []Page             // Pages that need to be generated
	files []string           // Static files to get copied to the destination
	media []string           // Media files to get resized to the destination
	templ *template.Template // Compiled templates
}

func NewSite(src, dest string) (*Site, error) {

	// Parse the _config.toml file
	path := filepath.Join(src, "_config.toml")
	conf, err := ParseConfig(path)
	log.Printf(MsgUsingConfig, path)
	if err != nil {
		return nil, err
	}

	site := Site{
		Src:  src,
		Dest: dest,
		Conf: conf,
	}

	// Recursively process all files in the source directory
	// and parse pages, posts, templates, etc
	if err := site.read(); err != nil {
		return nil, err
	}

	return &site, nil
}

// Reloads the site into memory
func (s *Site) Reload() error {
	s.posts = []Page{}
	s.pages = []Page{}
	s.files = []string{}
	s.media = []string{}
	s.templ = nil
	return s.read()
}

// Prepares the source directory for site generation
func (s *Site) Prep() error {
	return os.MkdirAll(s.Dest, 0755)
}

// Removes the existing site (typically in _site).
func (s *Site) Clear() error {
	return os.RemoveAll(s.Dest)
}

// Generates a static website based on Jekyll standard layout.
func (s *Site) Generate() error {

	// Remove previously generated site, and then (re)create the
	// destination directory
	if err := s.Clear(); err != nil {
		return err
	}
	if err := s.Prep(); err != nil {
		return err
	}

	// Generate all Pages and Posts and static files
	if err := s.writePages(); err != nil {
		return err
	}

	if err := s.resizeMedia(); err != nil {
		return err
	}

	if err := s.writeStatic(); err != nil {
		return err
	}

	log.Printf("Site generation completed!\n")

	return nil
}

// Deploys a site to S3.
func (s *Site) Deploy(user, pass, url string) error {
	auth := aws.Auth{AccessKey: user, SecretKey: pass}
	b := s3.New(auth, aws.USEast).Bucket(url)

	// walks _site directory and uploads file to S3
	walker := func(fn string, fi os.FileInfo, err error) error {
		if fi.IsDir() {
			return nil
		}

		rel, _ := filepath.Rel(s.Dest, fn)
		typ := mime.TypeByExtension(filepath.Ext(rel))
		content, err := ioutil.ReadFile(fn)
		log.Printf(MsgUploadFile, rel)
		if err != nil {
			return err
		}

		// try to upload the file ... sometimes this fails due to amazon
		// issues. If so, we'll re-try
		if err := b.Put(rel, content, typ, s3.PublicRead); err != nil {
			time.Sleep(100 * time.Millisecond) // sleep so that we don't immediately retry
			return b.Put(rel, content, typ, s3.PublicRead)
		}

		// file upload was a success, return nil
		return nil
	}

	return filepath.Walk(s.Dest, walker)
}

func Filter(s []Page, fn func(Page) bool) []Page {
	var p []Page // == nil
	for _, i := range s {
		if fn(i) {
			p = append(p, i)
		}
	}
	return p
}

// Helper function to traverse the source directory and identify all posts,
// projects, templates, etc and parse.
func (s *Site) read() error {

	// Lists of templates (_layouts, _includes) that we find thate
	// will need to be compiled
	layouts := []string{}

	// func to walk the jekyll directory structure
	walker := func(fn string, fi os.FileInfo, err error) error {
		rel, _ := filepath.Rel(s.Src, fn)
		switch {
		case err != nil:
			return nil

		// Ignore directories
		case fi.IsDir():
			return nil

		// Ignore Hidden or Temp files
		// (starting with . or ending with ~)
		case isHiddenOrTemp(rel):
			return nil

		// Parse Templates
		case isTemplate(rel):
			log.Printf("Processing template %s...", fn)
			layouts = append(layouts, fn)

		// Parse Posts
		case isPost(rel):
			log.Printf("Processing post %s...", fn)
			post, err := ParsePost(rel)
			if err != nil {
				return err
			}
			// TODO: this is a hack to get the posts in rev chronological order
			s.posts = append([]Page{post}, s.posts...) //s.posts, post)

		// Parse Pages
		case isPage(rel):
			log.Printf("Processing page %s...", fn)
			page, err := ParsePage(rel)
			if err != nil {
				return err
			}

			s.pages = append(s.pages, page)

		// Make thumbnails and sane images sizes for media content
		case isMedia(rel):
			log.Printf("Processing media content %s...", fn)
			if strings.HasSuffix(rel, ".jpg") {
				s.media = append(s.media, rel)
			}

		// Move static files, no processing required
		case isStatic(rel):
			s.files = append(s.files, rel)
		}
		return nil
	}

	// Walk the diretory recursively to get a list of all posts,
	// pages, templates and static files.
	err := filepath.Walk(s.Src, walker)
	if err != nil {
		return err
	}

	// Compile all templates found
	//s.templ = template.Must(template.ParseFiles(layouts...))
	s.templ, err = template.New("layouts").Funcs(funcMap).ParseFiles(layouts...)
	if err != nil {
		return err
	}

	// Add the posts, timestamp, etc to the Site Params
	s.Conf.Set("posts", s.posts)

	s.Conf.Set("pages", Filter(s.pages, func(v Page) bool { return len(v.GetTitle()) > 0 }))

	s.Conf.Set("time", time.Now())

	if hostname, err := os.Hostname(); err == nil {
		s.Conf.Set("buildhost", hostname)
	}

	if user, err := user.LookupId(fmt.Sprintf("%d", os.Getuid())); err == nil {
		s.Conf.Set("builduser", user.Username)
	}

	s.calculateTags()
	s.calculateCategories()
	s.calculatePageHierachy()

	return nil
}

// Make thumbnail for the selected file
func MakeThumb(src string, dst_sane string, dst_thumb string) error {
	file, err := os.Open(src)
	defer file.Close()
	if err != nil {
		return err
	}

	img, err := jpeg.Decode(file)
	if err != nil {
		return err
	}

	sane := resize.Resize(1080, 0, img, resize.Bilinear)

	out_sane, err := os.Create(dst_sane)
	if err != nil {
		return err
	}
	defer out_sane.Close()

	jpeg.Encode(out_sane, sane, nil)

	thumb := resize.Thumbnail(200, 200, img, resize.Bilinear)

	out_thumb, err := os.Create(dst_thumb)
	if err != nil {
		return err
	}
	defer out_thumb.Close()

	jpeg.Encode(out_thumb, thumb, nil)
	return nil
}

// Helper function to write all pages and posts to the destination directory
// during site generation.
func (s *Site) writePages() error {

	// There is really no difference between a Page and a Post (other than
	// initial parsing) so we can combine the lists and use the same rendering
	// code for both.
	pages := []Page{}
	pages = append(pages, s.pages...)
	pages = append(pages, s.posts...)

	for _, page := range pages {
		url := page.GetUrl()
		layout := page.GetLayout()

		// is the layout provided? or is it nil /empty?
		//layoutNil := layout == "" || layout == "nil"

		// make sure the posts's parent dir exists
		d := filepath.Join(s.Dest, filepath.Dir(url))
		f := filepath.Join(s.Dest, url)
		if err := os.MkdirAll(d, 0755); err != nil {
			return err
		}

		// if markdown, need to convert to html
		// otherwise just convert raw html to a string
		//var content string
		//if isMarkdown(page.GetExt()) {
		//	content = string(blackfriday.MarkdownCommon(raw))
		//} else {
		//	content = string(raw)
		//}

		//data passed in to each template
		data := map[string]interface{}{
			"site": s.Conf,
			"page": page,
		}

		// treat all non-markdown pages as templates
		content := page.GetContent()
		if isMarkdown(page.GetExt()) == false {
			// this code will add the page to the list of templates,
			// will execute the template, and then set the content
			// to the rendered template
			t, err := s.templ.New(url).Parse(content)
			if err != nil {
				return err
			}
			var buf bytes.Buffer
			err = t.ExecuteTemplate(&buf, url, data)
			if err != nil {
				return err
			}
			content = buf.String()
		}

		// add document body to the map
		data["content"] = content

		// write the template to a buffer
		// NOTE: if template is nil or empty, then we should parse the
		//       content as if it were a template
		var buf bytes.Buffer
		if layout == "" || layout == "nil" {
			//t, err := s.templ.New(url).Parse(content);
			//if err != nil { return err }
			//err = t.ExecuteTemplate(&buf, url, data);
			//if err != nil { return err }

			buf.WriteString(content)
		} else {
			layout = appendExt(layout, ".html")
			s.templ.ExecuteTemplate(&buf, layout, data)
		}

		log.Printf(MsgGenerateFile, url)
		if err := ioutil.WriteFile(f, buf.Bytes(), 0644); err != nil {
			return err
		}
	}

	return nil
}

// Helper function to resize the jpegs to sane sizes
func (s *Site) resizeMedia() error {

	os.MkdirAll(filepath.Join(s.Dest, "media"), 0755)

	cache_dest := fmt.Sprintf("%x", md5.Sum([]byte(s.Dest)))

	mediaCacheDir := filepath.Join(os.TempDir(), "oryza", cache_dest)

	os.MkdirAll(mediaCacheDir, 0755)

	for i, file := range s.media {
		log.Printf("Automatically resizing media for you (File %d/%d: %s)...\n", i+1, len(s.media), file)

		from := filepath.Join(s.Src, file)

		fnbase := filepath.Base(from)

		sane_cache := filepath.Join(mediaCacheDir, "sane_"+fnbase)
		thumb_cache := filepath.Join(mediaCacheDir, "thumb_"+fnbase)
		//log.Printf("Sane and thumb are cached at %s, %s\n", sane_cache, thumb_cache)

		sane_exists := false
		thumb_exists := false

		if _, err := os.Stat(sane_cache); err == nil {
			sane_exists = true
		}

		if _, err := os.Stat(thumb_cache); err == nil {
			thumb_exists = true
		}

		sane_final := filepath.Join(s.Dest, "media", "sane_"+fnbase)
		thumb_final := filepath.Join(s.Dest, "media", "thumb_"+fnbase)

		if !sane_exists || !thumb_exists {
			if err := MakeThumb(from, sane_cache, thumb_cache); err != nil {
				return err
			}
		}

		if err := copyTo(sane_cache, sane_final); err != nil {
			return err
		}

		if err := copyTo(thumb_cache, thumb_final); err != nil {
			return err
		}

	}

	return nil
}

// Helper function to write all static files to the destination directory
// during site generation. This will also take care of creating any parent
// directories, if necessary.
func (s *Site) writeStatic() error {

	for _, file := range s.files {
		from := filepath.Join(s.Src, file)
		to := filepath.Join(s.Dest, file)
		//log.Printf(MsgCopyingFile, file)
		if err := copyTo(from, to); err != nil {
			return err
		}
	}

	return nil
}

// Helper function to aggregate a list of all categories and their
// related posts.
func (s *Site) calculateCategories() {

	categories := make(map[string][]Page)
	for _, post := range s.posts {
		for _, category := range post.GetCategories() {
			if posts, ok := categories[category]; ok == false {
				categories[category] = append(posts, post)
			} else {
				categories[category] = []Page{post}
			}
		}
	}

	s.Conf.Set("categories", categories)
}

// Helper function to aggregate a list of all tags and their
// related posts.
func (s *Site) calculateTags() {

	tags := make(map[string][]Page)
	for _, post := range s.posts {
		for _, tag := range post.GetTags() {
			if posts, ok := tags[tag]; ok == false {
				tags[tag] = append(posts, post)
			} else {
				tags[tag] = []Page{post}
			}
		}
	}

	s.Conf.Set("tags", tags)
}

func (s *Site) calculatePageHierachy() {
	log.Printf("Processing site hierachy...\n")
	for _, page := range s.pages {
		children := make([]Page, 0)
		if pageID, ok := page["id"].(string); ok {
			log.Printf(" %s...\n", pageID)
			for _, potentialChild := range s.pages {
				if pageChildID, ok := potentialChild["id"].(string); ok {
					if strings.HasPrefix(pageChildID, pageID+"_") {
						children = append(children, potentialChild)
						potentialChild["parent"] = page
						log.Printf("  --> %s...\n", pageChildID)
					}
				}
			}
			page["children"] = children
		}
	}
	log.Printf("Done processing site hierachy!\b")
}
