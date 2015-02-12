package main

import (
	"bytes"
	"io"
	"os"
	//	"os/exec"
	//"log"
	"path/filepath"
	"strings"
)

// Appends the extension to the specified file. If the file already has the
// desired extension no changes are made.
func appendExt(fn, ext string) string {
	if strings.HasSuffix(fn, ext) {
		return fn
	}
	return fn + ext
}

// copyTo copies the contents of the file named src to the file named
// by dst. The file will be created if it does not already exist. If the
// destination file exists, all it's contents will be replaced by the contents
// of the source file.
// http://stackoverflow.com/questions/21060945/simple-way-to-copy-a-file-in-golang
func copyTo(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return
	}
	defer in.Close()
	dst_dir := filepath.Dir(dst)
	if _, err := os.Stat(dst_dir); err != nil {
		os.MkdirAll(dst_dir, 0755)
	}
	out, err := os.Create(dst)
	if err != nil {
		return
	}
	defer func() {
		cerr := out.Close()
		if err == nil {
			err = cerr
		}
	}()
	if _, err = io.Copy(out, in); err != nil {
		return
	}
	err = out.Sync()
	return
}

// Returns True if a file has YAML front-end matter.
func hasMatter(fn string) bool {
	sample, _ := sniff(fn, 4)
	return bytes.Equal(sample, []byte("---\n"))
}

// Returns True if the file is a temp file (starts with . or ends with ~).
func isHiddenOrTemp(fn string) bool {
	base := filepath.Base(fn)
	return strings.HasPrefix(base, ".") ||
		strings.HasPrefix(fn, ".") ||
		strings.HasSuffix(base, "~") ||
		fn == "README.md"
}

// Returns True if the file is a template. This is determine by the files
// parent directory (_layout or _include) and the file type (markdown).
func isTemplate(fn string) bool {
	switch {
	case !isHtml(fn):
		return false
	case strings.HasPrefix(fn, "_layouts"):
		return true
	case strings.HasPrefix(fn, "_includes"):
		return true
	}
	return false
}

// Return True if the markup is HTML.
// TODO change this to isMarkup and add .xml, .rss, .atom
func isHtml(fn string) bool {
	switch filepath.Ext(fn) {
	case ".html", ".htm", ".xml", ".rss", ".atom":
		return true
	}
	return false
}

// Returns True if the markup is Markdown.
func isMarkdown(fn string) bool {
	switch filepath.Ext(fn) {
	case ".md", ".markdown":
		return true
	}
	return false
}

// Returns True if the specified file is a Page.
func isPage(fn string) bool {
	switch {
	case strings.HasPrefix(fn, "_"):
		return false
	case !isMarkdown(fn) && !isHtml(fn):
		return false
	case !hasMatter(fn):
		return false
	}
	return true
}

// Returns True if the specified file is a Post.
func isPost(fn string) bool {
	switch {
	case !strings.HasPrefix(fn, "_posts"):
		return false
	case !isMarkdown(fn):
		return false
	case !hasMatter(fn):
		return false
	}
	return true
}

// Returns True if the file is a media content.
func isMedia(fn string) bool {
	return strings.HasPrefix(fn, "_media")
}

// Returns True if the specified file is Static Content, meaning it should
// be included in the site, but not compiled and processed by Jekyll.
//
// NOTE: this assumes that we've already established the file is not markdown
//       and does not have yaml front matter.
func isStatic(fn string) bool {
	return !strings.HasPrefix(fn, "_")
}

// Returns an recursive list of all child directories
func dirs(path string) (paths []string) {
	site := filepath.Join(path, "_site")
	filepath.Walk(path, func(fn string, fi os.FileInfo, err error) error {
		switch {
		case err != nil:
			return nil
		case fi.IsDir() == false:
			return nil
		case strings.HasPrefix(fn, site):
			return nil
		}

		paths = append(paths, fn)
		return nil
	})

	return
}

// Removes the files extension. If the file has no extension the string is
// returned without modification.
func removeExt(fn string) string {
	if ext := filepath.Ext(fn); len(ext) > 0 {
		return fn[:len(fn)-len(ext)]
	}
	return fn
}

// Replaces the files extension with the new extension.
func replaceExt(fn, ext string) string {
	return removeExt(fn) + ext
}

// sniff will extract the first N bytes from a file and return the results.
//
// This is used, for example, by the hasMatter function to check and see
// if the file include YAML without having to read the entire contents of the
// file into memory.
func sniff(fn string, n int) ([]byte, error) {
	f, err := os.Open(fn)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	b := make([]byte, n, n)
	if _, err := io.ReadAtLeast(f, b, n); err != nil {
		return nil, err
	}

	return b, nil
}
