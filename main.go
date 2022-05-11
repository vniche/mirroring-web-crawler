package main

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sync/semaphore"
)

// fixed list of invalid URLs
var invalidURLs = []string{"/"}

var mutex = &sync.RWMutex{}

var (
	maxWorkers int64
	sem        *semaphore.Weighted
	target     string
	directory  string
	force      bool

	// URLs that are being crawled or already been
	doneURLs []string

	// URLs that are being crawled
	crawlingURLs = make(map[string]int)
)

const (
	TO_BE_CRAWLED = iota
	CRAWLING
)

func init() {
	// parse command line arguments
	flag.StringVar(&target, "url", "", "target URL to crawl")
	flag.StringVar(&directory, "directory", "", "target directory to save crawled files")
	flag.BoolVar(&force, "force", false, "force download of files")
	flag.Int64Var(&maxWorkers, "workers", 10, "number of concurrent workers")
	flag.Parse()

	if target == "" {
		log.Fatal("target URL is required")
	}

	if directory == "" {
		log.Fatal("directory is required")
	}

	// initializes semaphore with the number of workers
	sem = semaphore.NewWeighted(maxWorkers)

	// adds the target to the list of invalid prefixes
	invalidURLs = append(invalidURLs, target)

	// adds the target to the list of crawling URLs
	crawlingURLs[target] = TO_BE_CRAWLED
}

func main() {
	// subscribe for process interruption signals
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	// creates context to propagate cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// creates ticker for crawling (every 500ms)
	ticker := time.NewTicker(time.Millisecond * 500)

	for {
		select {
		case <-quit:
			fmt.Printf("\n\nCrawling stopped\n\n")
			// stops time ticker
			ticker.Stop()
			// cancels context
			cancel()
			return
		case <-ticker.C:
			if len(crawlingURLs) == 0 {
				fmt.Printf("\n\nCrawling finished\n\n")
				return
			}

			// get the next URL to crawl
			var urlToCrawl string
			mutex.RLock()
			for current, state := range crawlingURLs {
				// check if link is already being or already been crawled by this execution
				if state != TO_BE_CRAWLED {
					continue
				}

				urlToCrawl = current
			}
			mutex.RUnlock()

			// if no URL to crawl, continue to next iteration
			if urlToCrawl == "" {
				continue
			}

			// adds the file to the list of files to be ignore on future crawls, associated with a unique ID for the worker
			mutex.Lock()
			crawlingURLs[urlToCrawl] = CRAWLING
			mutex.Unlock()

			// try to acquire a semaphore token for each child link
			if err := sem.Acquire(ctx, 1); err != nil {
				fmt.Printf("failed to acquire semaphore: %s\n", err)
				break
			}

			go func(ctx context.Context, crawlTarget string) {
				defer func(crawlTarget string) {
					mutex.Lock()
					delete(crawlingURLs, crawlTarget)
					mutex.Unlock()

					doneURLs = append(doneURLs, crawlTarget)
					fmt.Printf("releasing semaphore for %s\n", crawlTarget)
					sem.Release(1)
				}(crawlTarget)

				if err := crawl(ctx, crawlTarget); err != nil {
					fmt.Printf("failed to crawl %s: %s\n", crawlTarget, err)
				}
			}(ctx, urlToCrawl)
		}
	}
}

// crawl recursively downloads an entire web page, save to a file and returns all its same-domain links
func crawl(ctx context.Context, targetURL string) error {
	fmt.Printf("crawling %s\n", targetURL)

	// parses target URL to golang's URL type
	parsedURL, err := url.Parse(targetURL)
	if err != nil {
		return err
	}

	// sets the file name based on the URL
	urlPath := parsedURL.Path
	if targetURL == target {
		urlPath = "index.html"
	}

	// sets the file path based on the directory
	filePath := filepath.Join(directory, urlPath)
	if filepath.Ext(urlPath) == "" {
		filePath += ".html"
	}

	// checks if file already exists
	// if url is the same as the target, it is considered as always download
	// if force is set, it will download the file anyway
	if _, err := os.Stat(filePath); targetURL != target && !force && err == nil {
		return fmt.Errorf("%s was already crawled", urlPath)
	}

	// downloads target URL content
	content, err := download(targetURL)
	if err != nil {
		return err
	}

	// saves the file
	err = save(content, filePath)
	if err != nil {
		return err
	}

	// will store all valid links on the page
	childLinks := []string{}

	for _, attribute := range []string{`href`, `src`} {
		// find all links on the page
		regex := regexp.MustCompile(attribute + `="(.*?)"`)
		matches := regex.FindAllString(string(content), -1)

		mutex.Lock()
		// uses label to enable breaking outer loop
	MATCHES:
		// for each link matched on the page
		for _, match := range matches {
			// removes attribute surrounding the URL
			value := strings.TrimPrefix(match, attribute+`="`)[:(len(match)-len(attribute+`="`))-1]

			// checks if the URL is valid
			for _, invalidURL := range invalidURLs {
				if value == invalidURL {
					continue MATCHES
				}
			}

			// ignore if the link is for a font-family source
			for _, invalidPrefix := range []string{"font-family:", "#popup:", "'"} {
				if strings.HasPrefix(value, invalidPrefix) {
					continue MATCHES
				}
			}

			// ignore if the URL is valid but not the same domain
			if strings.HasPrefix(value, "http") {
				if !strings.HasPrefix(value, target) {
					continue
				}

				value = strings.TrimPrefix(value, target)
			}

			// ensure that we are not crawling the same URL twice
			for _, nogoURL := range doneURLs {
				if value == nogoURL {
					continue MATCHES
				}
			}

			// if the URL is relative, make it absolute
			if strings.HasPrefix(value, "/") {
				value = target + value
			}

			// ignores if the value is already on child links to crawl
			for _, childLink := range childLinks {
				if value == childLink {
					continue MATCHES
				}
			}

			fmt.Printf("adding: %s\n", value)
			childLinks = append(childLinks, value)
			crawlingURLs[value] = TO_BE_CRAWLED
		}
		mutex.Unlock()
	}

	return nil
}

// download downloads a web page and returns its content
func download(url string) ([]byte, error) {
	// get the file
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// reads response body
	content, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// checks if response status is ok (HTTP 200 status code)
	if resp.StatusCode != http.StatusOK {
		return nil, err
	}

	return content, nil
}

func save(content []byte, filePath string) error {
	// ensure that directory exists
	err := os.MkdirAll(filepath.Dir(filePath), os.ModePerm)
	if err != nil {
		return err
	}

	// create file
	file, err := os.Create(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	// write the response body to the file
	_, err = file.Write(content)
	if err != nil {
		return err
	}

	return nil
}
