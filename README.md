**jkl-baas** is a static site generator written in [Go](http://www.golang.org),
based on [Jekyll](https://github.com/mojombo/jekyll)

[![Build Status](https://drone.io/drone/jkl/status.png)](https://drone.io/drone/jkl/latest)

Notable similarities between jkl and Jekyll:

* Directory structure
* Use of a TOML dialect front matter in Pages and Posts
* Availability of `site`, `content`, `page` and `posts` variables in templates
* Copies all static files into destination directory

Notable differences between jkl and Jekyll:

* Uses [Go templates](http://www.golang.org/pkg/text/template)
* Only supports a TOML dialect front matter in markup files
* No plugin support

Additional features:

* Deploy to S3

Sites built with jkl-baas:

* My blog/website: http://tnhh.net

--------------------------------------------------------------------------------

### Installation

In order to compile with `go build` you will first need to download
the following dependencies:

```
go get github.com/russross/blackfriday
go get launchpad.net/goamz/aws
go get launchpad.net/goamz/s3
go get github.com/howeyc/fsnotify
```
Once you have compiled `jkl` you can install with the following command:

```sh
sudo install -t /usr/local/bin jkl
```

If you are running x64 linux you can download and install the pre-compiled
binary:

```sh
wget https://github.com/downloads/bradrydzewski/jkl/jkl
sudo install -t /usr/local/bin jkl
```

### Usage

```
Usage: jkl [OPTION]... [SOURCE]

  -h, --help           display this help and exit

Examples:
  jkl                  generates site from current working dir

```

### Documentation

See the official [Jekyll wiki](https://github.com/mojombo/jekyll/wiki)
... just remember that you are using Go templates instead of Liquid templates.

