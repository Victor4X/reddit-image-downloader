package main

import (
	"bytes"
	"crypto/sha256"
	"flag"
	"fmt"
	"github.com/gosimple/slug"
	"io"
	"io/ioutil"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"text/template"
	"time"
)

var singleTemplate *template.Template
var albumTemplate *template.Template

var outputRoot string

var httpClient http.Client
var redditClient RedditClient
var imgurClient ImgurClient

var skipDuplicates bool
var skipDuplicatesInAlbums bool

var knownUrls = make(map[string]struct{})
var knownHashes = make(map[string]struct{})

var quiet bool
var overwrite bool
var nsfw bool

var throttler *time.Ticker

func main() {
	defaultSingleTemplateStr := `{{.Submission.Subreddit}}/{{.Timestamp}}-{{.Submission.Id}}-{{.Submission.Title | slugify}}{{.Ext}}`
	defaultAlbumTemplateStr := `{{.Submission.Subreddit}}/{{.Timestamp}}-{{.Submission.Id}}-{{.Submission.Title | slugify}}/{{.Num}}-{{.Image.Hash}}{{.Ext}}`

	singleTemplateStr := flag.String("single-template", defaultSingleTemplateStr, "template for image paths, use go template syntax")
	albumTemplateStr := flag.String("album-template", defaultAlbumTemplateStr, "template for image paths in albums, use go template syntax")
	flag.StringVar(&outputRoot, "out", ".", "root output directory")
	flag.BoolVar(&skipDuplicates, "skip-duplicates", true, "skip duplicate single images")
	flag.BoolVar(&skipDuplicatesInAlbums, "skip-duplicates-in-albums", false, "skip duplicate images within imgur albums")
	throttle := flag.Duration("throttle", 2*time.Second, "wait at least this long between requests to the reddit api")
	pageSize := flag.Uint("page-size", 25, "reddit api listing page size")
	search := flag.String("search", "", "search string")
	flag.BoolVar(&quiet, "quiet", false, "don't print every submission (errors and skips are still printed)")
	flag.BoolVar(&overwrite, "overwrite", false, "overwrite existing files")
	flag.BoolVar(&nsfw, "nsfw", false, "include nsfw submissions")
	flag.Parse()

	subreddits := flag.Args()
	if len(subreddits) == 0 {
		_, _ = fmt.Fprintln(os.Stderr, "No subreddits provided. Usage: ")
		_, _ = fmt.Fprintf(os.Stderr, "%s [-album-template=string] [-single-template=string] [-skip-duplicates=(true|false)] [-skip-duplicates-in-albums=(true|false)] [-throttle=duration] [-quiet] subreddits...\n", os.Args[0])
		flag.PrintDefaults()
		return
	}

	if *search == "" {
		search = nil
	}

	throttler = newImmediateTicker(*throttle)

	singleTemplate = template.New("name")
	singleTemplate.Funcs(template.FuncMap{
		"slugify": slugify,
	})
	_, err := singleTemplate.Parse(*singleTemplateStr)
	if err != nil {
		log.Fatalf("error parsing template: %v", err)
	}

	albumTemplate = template.New("name")
	albumTemplate.Funcs(template.FuncMap{
		"slugify": slugify,
	})
	_, err = albumTemplate.Parse(*albumTemplateStr)
	if err != nil {
		log.Fatalf("error parsing template: %v", err)
	}

	httpClient = http.Client{
		Timeout: time.Second * 10,
	}
	redditClient = RedditClient{http: &httpClient}
	imgurClient = ImgurClient{http: &httpClient}

	submissions := make(chan Submission)
	go func() {
		after := make(map[string]string)
		completed := make(map[string]bool)
		for _, sub := range subreddits {
			after[sub] = ""
			completed[sub] = false
		}

		page := 1
		for {
			allCompleted := true
			for _, sub := range subreddits {
				if !completed[sub] {
					allCompleted = false
					<-throttler.C
					log.Printf("fetching page %d on r/%s", page, sub)

					var listing Listing
					var err error

					var rateLimitDuration time.Duration = 0
					for {
						if rateLimitDuration > 0 {
							time.Sleep(rateLimitDuration)
						}
						if search != nil {
							listing, err = redditClient.GetSearch(sub, SearchListingParams{
								After:  after[sub],
								Limit:  int(*pageSize),
								Search: *search,
							})
						} else {
							listing, err = redditClient.GetNew(sub, NewListingParams{
								After: after[sub],
								Limit: int(*pageSize),
							})
						}
						if err == nil {
							break
						} else if err == RateLimited {
							rateLimitDuration += *throttle
							log.Printf("rate limit reached, retrying after %s", rateLimitDuration.String())
						} else {
							log.Printf("fetching failed: %v, retrying", err)
							<-throttler.C
						}
					}

					for _, submission := range listing.Children {
						submissions <- submission
					}

					if listing.After == "" {
						completed[sub] = true
						log.Printf("completed %s", sub)
					} else {
						after[sub] = listing.After
					}
				}
			}
			page += 1

			if allCompleted {
				break
			}
		}
		close(submissions)
	}()

	for submission := range submissions {
		if submission.Nsfw && !nsfw {
			log.Printf("skipping NSFW: %s (%s)", submission.Url, submission.Permalink)
		} else {
			_ = fetchSubmission(submission)
		}
	}
	log.Printf("finished")
}

func fetchSubmission(submission Submission) error {
	if submission.PostHint == "image" {
		return fetchSingleImage(submission.Url, submission)
	} else if submission.Domain == "imgur.com" {
		return fetchImgur(submission)
	} else {
		return fmt.Errorf("could not fetch %s, unknown service %s", submission.Url, submission.Domain)
	}
}

func fetchSingleImage(u string, submission Submission) error {
	if skipDuplicates {
		_, exists := knownUrls[u]
		if exists {
			log.Printf("skipping %s\n", u)
			return nil
		}
		knownUrls[u] = struct{}{}
	}

	resp, err := httpClient.Get(u)
	if err != nil {
		log.Printf("fetching %s (%s) => %v", u, submission.Permalink, err)
		return err
	}
	defer func() {
		_, _ = io.Copy(ioutil.Discard, resp.Body)
		err := resp.Body.Close()
		if err != nil {
			log.Printf("error closing response body: %v", err)
		}
	}()

	if resp.StatusCode == 404 || (resp.Request.URL.Host == "i.imgur.com" && strings.HasSuffix(resp.Request.URL.Path, "removed.png")) {
		log.Printf("fetching %s (%s) => not found\n", u, submission.Permalink)
		return fmt.Errorf("image not found")
	} else if resp.StatusCode >= 300 {
		log.Printf("fetching %s (%s) => HTTP status %d\n", u, submission.Permalink, resp.StatusCode)
		return fmt.Errorf("status code is not 2XX")
	}

	var data []byte
	if skipDuplicates {
		hasher := sha256.New()
		tee := io.TeeReader(resp.Body, hasher)
		data, err = ioutil.ReadAll(tee)
		if err != nil {
			log.Printf("fetching %s (%s) => %v", u, submission.Permalink, err)
			return err
		}
		hash := hasher.Sum(nil)
		hashString := string(hash)
		_, exists := knownHashes[hashString]
		if exists {
			log.Printf("fetching %s (%s) => hash exists already, skipping", submission.Permalink, u)
			return nil
		}
		knownHashes[string(hash)] = struct{}{}
	} else {
		data, err = ioutil.ReadAll(resp.Body)
		if err != nil {
			log.Printf("fetching %s (%s) => %v", u, submission.Permalink, err)
			return err
		}
	}

	parsedUrl, _ := url.Parse(u)
	ext := path.Ext(parsedUrl.Path)

	contentType := resp.Header.Get("Content-Type")
	if contentType != "" {
		exts, err := mime.ExtensionsByType(contentType)
		if err == nil && len(exts) > 0 {
			if ext == "" {
				ext = exts[0]
			} else {
				valid := false
				for _, e := range exts {
					if e == ext {
						valid = true
						break
					}
				}
				if !valid {
					ext = exts[0]
				}
			}
		}
	}

	created := time.Unix(int64(submission.CreatedUtc), 0)

	templateData := struct {
		Ext        string
		Submission Submission
		Time       time.Time
		Timestamp  string
	}{
		Ext:        ext,
		Submission: submission,
		Time:       created,
		Timestamp:  created.Format("2006-01-02-15-04-05"),
	}

	var name bytes.Buffer
	err = singleTemplate.Execute(&name, templateData)
	if err != nil {
		panic(fmt.Errorf("template error: %v", err))
	}

	p := name.String()

	if !filepath.IsAbs(p) {
		p = outputRoot + "/" + p
	}

	if !overwrite {
		if _, err := os.Stat(p); err == nil || !os.IsNotExist(err) {
			// exists or some error except "not exist"
			log.Printf("fetching %s (%s) => file exists, overwrite disabled", u, submission.Permalink)
			return nil
		}
	}

	dir := filepath.Dir(p)
	_ = os.MkdirAll(dir, os.ModeDir)
	err = ioutil.WriteFile(p, data, os.ModePerm)
	if err != nil {
		log.Printf("fetching %s (%s) => %v", u, submission.Permalink, err)
		return err
	}
	if !quiet {
		log.Printf("fetching %s (%s) => %s", u, submission.Permalink, p)
	}
	return nil
}

func fetchImgur(submission Submission) error {
	u, err := url.Parse(submission.Url)
	if err != nil {
		log.Printf("invalid url: %s", submission.Url)
		return err
	}
	if strings.HasPrefix(u.Path, "/a/") {
		albumId := strings.TrimPrefix(u.Path, `/a/`)
		if skipDuplicates {
			_, exists := knownUrls[submission.Url]
			if exists {
				log.Printf("skipping imgur album: %s\n", submission.Url)
				return nil
			}
			knownUrls[submission.Url] = struct{}{}
		}
		album, err := imgurClient.GetAlbum(albumId)
		if err != nil {
			log.Printf("fetching imgur album: %s (%s) => %v", submission.Url, submission.Permalink, err)
			return err
		}

		for i, img := range album.Images {
			u := fmt.Sprintf(`https://i.imgur.com/%s%s`, img.Hash, img.Ext)
			if skipDuplicatesInAlbums {
				_, exists := knownUrls[u]
				if exists {
					log.Printf("skipping %s (%s)\n", u, submission.Permalink)
					continue
				}
				knownUrls[u] = struct{}{}
			}
			resp, err := httpClient.Get(u)
			if err != nil {
				log.Printf("fetching %s (%s) => %v", u, submission.Permalink, err)
				continue
			}
			defer func() {
				_, _ = io.Copy(ioutil.Discard, resp.Body)
				err := resp.Body.Close()
				if err != nil {
					log.Printf("error closing response body: %v", err)
				}
			}()

			if strings.HasSuffix(resp.Request.URL.Path, "removed.png") {
				log.Printf("fetching %s (%s) => not found\n", u, submission.Permalink)
				continue
			} else if resp.StatusCode >= 300 {
				log.Printf("fetching %s (%s) => HTTP status %d", u, submission.Permalink, resp.StatusCode)
				continue
			}

			var data []byte

			if skipDuplicatesInAlbums {
				hasher := sha256.New()
				tee := io.TeeReader(resp.Body, hasher)
				data, err = ioutil.ReadAll(tee)
				if err != nil {
					log.Printf("fetching %s (%s) => %v", u, submission.Permalink, err)
					continue
				}
				hash := hasher.Sum(nil)
				hashString := string(hash)
				_, exists := knownHashes[hashString]
				if exists {
					log.Printf("fetching %s (%s) => hash exists already, skipping\n", u, submission.Permalink)
					continue
				}
				knownHashes[string(hash)] = struct{}{}
			} else {
				data, err = ioutil.ReadAll(resp.Body)
				if err != nil {
					log.Printf("fetching %s (%s) => %v", u, submission.Permalink, err)
					continue
				}
			}

			created := time.Unix(int64(submission.CreatedUtc), 0)

			templateData := struct {
				Ext        string
				Submission Submission
				Image      AlbumImage
				Time       time.Time
				Timestamp  string
				Num        int
			}{
				Ext:        img.Ext,
				Submission: submission,
				Image:      img,
				Time:       created,
				Timestamp:  created.Format("2006-01-02-15-04-05"),
				Num:        i + 1,
			}

			var name bytes.Buffer
			err = albumTemplate.Execute(&name, templateData)
			if err != nil {
				panic(fmt.Errorf("template error: %v", err))
			}

			p := name.String()
			if !filepath.IsAbs(p) {
				p = outputRoot + "/" + p
			}

			if !overwrite {
				if _, err := os.Stat(p); err != nil {
					// exists or some error
					log.Printf("fetching %s (%s) => file exists, overwrite disabled", u, submission.Permalink)
					continue
				}
			}

			dir := filepath.Dir(p)
			_ = os.MkdirAll(dir, os.ModeDir)
			err = ioutil.WriteFile(p, data, os.ModePerm)
			if err != nil {
				log.Printf("fetching %s (%s) => %v", u, submission.Permalink, err)
				continue
			}
			if !quiet {
				log.Printf("fetching %s (%s) => %s\n", u, submission.Permalink, p)
			}
		}
		return nil
	} else {
		imgUrl := `https://i.imgur.com` + u.Path + `.png`
		return fetchSingleImage(imgUrl, submission)
	}
}

func slugify(str string) string {
	return slug.Make(str)
}

func newImmediateTicker(repeat time.Duration) *time.Ticker {
	ticker := time.NewTicker(repeat)
	oc := ticker.C
	nc := make(chan time.Time, 1)
	go func() {
		nc <- time.Now()
		for tm := range oc {
			nc <- tm
		}
	}()
	ticker.C = nc
	return ticker
}
